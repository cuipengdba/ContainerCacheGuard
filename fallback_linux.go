//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	fallbackFixed = 1
	fallbackAuto  = 2
)

type FallbackScopeStatus struct {
	Entries        int               `json:"round_entries"`
	RoundWorkUS    int64             `json:"round_work_us"`
	RoundRequested int64             `json:"round_requested_bytes"`
	Queries        int64             `json:"initial_queried_bytes_total"`
	Advice         int64             `json:"advice_bytes_total"`
	Immediate      int64             `json:"immediate_decrease_bytes_total"`
	DelayedNet     int64             `json:"delayed_net_bytes_total"`
	Reappeared     int64             `json:"reappeared_bytes_total"`
	Rechecks       uint64            `json:"delayed_rechecks_total"`
	Discovery      map[string]uint64 `json:"discoveries_total"`
	Skipped        map[string]uint64 `json:"skipped_total"`
}

type FallbackFinding struct {
	Path    string    `json:"path"`
	Scope   string    `json:"scope"`
	Source  string    `json:"source"`
	Size    int64     `json:"size_bytes"`
	At      time.Time `json:"discovered_at"`
	AfterMS int64     `json:"after_start_ms"`
}

type FallbackStatus struct {
	Round              uint64              `json:"round"`
	At                 time.Time           `json:"evaluated_at"`
	Reason             string              `json:"trigger"`
	Permission         string              `json:"permission"`
	ParentRemaining    *int64              `json:"parent_remaining_bytes"`
	KubeletHeadroom    *int64              `json:"kubelet_headroom_above_stop_bytes"`
	KubeletSignal      *NodeSignal         `json:"kubelet_signal,omitempty"`
	MemoryHigh         *int64              `json:"memory_high_bytes"`
	MemoryHighExpected *int64              `json:"memory_high_expected_bytes"`
	MemoryHighStatus   string              `json:"memory_high_status"`
	ProcCoverage       string              `json:"proc_coverage"`
	ProcExamined       uint64              `json:"proc_entries_examined_total"`
	ProcGaps           uint64              `json:"proc_gaps_total"`
	ProcWorkUS         int64               `json:"proc_work_us_total"`
	Findings           []FallbackFinding   `json:"recent_findings"`
	Fixed              FallbackScopeStatus `json:"fixed"`
	Auto               FallbackScopeStatus `json:"auto"`
}

type fallbackInput struct {
	metrics    Metrics
	policy     Decision
	signals    SignalStatus
	reason     string
	permission pathPermissions
}

type fallbackRoot struct {
	path  string
	id    identity
	mount string
}
type fallbackFile struct {
	path, source string
	id           identity
	offset       int64
	protected    time.Time
	writerEpoch  uint64
	sources      uint8
}
type fallbackDir struct {
	file *os.File
	path string
}
type fallbackScope struct {
	roots          []fallbackRoot
	walkRoot       int
	dirs           []fallbackDir
	walkAgain      time.Time
	byInode        map[[2]uint64]*fallbackFile
	files          []*fallbackFile
	cursor, evict  int
	fast           []*fallbackFile
	fastCursor     int
	fastEvict      int
	visits         uint64
	ops, discovery budget
	pending        int
	stats          FallbackScopeStatus
}
type fallbackEngine struct {
	c             FallbackConfig
	input         atomic.Pointer[fallbackInput]
	lanes         [3]fallbackScope
	current       FallbackStatus
	closedWALRoot string
	started       time.Time
	nextRound     time.Time
	lastScan      int64
	lastReduction int64
	stalledRounds int
	priority      int
	proc          fallbackProc
	procOps       budget
}

func (e *engine) initFallback() error {
	if !e.c.CacheFallback.Enabled {
		return nil
	}
	f := &fallbackEngine{c: e.c.CacheFallback, started: time.Now()}
	f.current.ProcCoverage = "pending"
	pgdata := f.c.pgdata()
	for _, name := range []string{"pg_wal", "pg_xlog"} {
		if actual, err := filepath.EvalSymlinks(filepath.Join(pgdata, name)); err == nil && !beneath(actual, pgdata) {
			// Account for a directly mounted target of the PGDATA WAL symlink.
			f.closedWALRoot = actual
			break
		}
	}
	for i, paths := range [][]string{append([]string{pgdata}, f.c.BackupDirectories...), f.c.AutoDirectories} {
		lane := &f.lanes[i+1]
		lane.byInode = make(map[[2]uint64]*fallbackFile)
		lane.stats.Discovery, lane.stats.Skipped = make(map[string]uint64), make(map[string]uint64)
		for _, path := range paths {
			file, err := safeOpen(path)
			if err != nil {
				return fmt.Errorf("cache_fallback root %q: %w", path, err)
			}
			id, err := statFile(file)
			mount := mountID(file)
			ok := err == nil && id.Mode&syscall.S_IFMT == syscall.S_IFDIR && supportedFS(file) && mount != ""
			file.Close()
			if !ok {
				return fmt.Errorf("cache_fallback root %q must be a supported directory with a known mount ID", path)
			}
			lane.roots = append(lane.roots, fallbackRoot{path, id, mount})
			if e.hints != nil {
				e.hints.watch(path, id.Dev, e.c.MaxWatches)
			}
		}
	}
	e.fallback = f
	return nil
}

func mountID(file *os.File) string {
	data, err := os.ReadFile("/proc/self/fdinfo/" + strconv.FormatUint(uint64(file.Fd()), 10))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if value, ok := strings.CutPrefix(line, "mnt_id:"); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func (f *fallbackEngine) scope(path string) (int, *fallbackRoot) {
	for lane := fallbackFixed; lane <= fallbackAuto; lane++ {
		for i := range f.lanes[lane].roots {
			r := &f.lanes[lane].roots[i]
			if beneath(path, r.path) {
				return lane, r
			}
		}
	}
	return fallbackAuto, nil
}

func (f *fallbackEngine) close() {
	for i := fallbackFixed; i <= fallbackAuto; i++ {
		for _, d := range f.lanes[i].dirs {
			d.file.Close()
		}
	}
	f.proc.close()
}

func (e *engine) fallbackExcluded(path string) bool {
	f := e.fallback
	return e.c.excluded(path) || len(path) > 4096 || (f.closedWALRoot != "" && beneath(path, f.closedWALRoot)) ||
		strings.Contains("/"+strings.Trim(path, "/")+"/", "/pg_wal/") ||
		strings.Contains("/"+strings.Trim(path, "/")+"/", "/pg_xlog/")
}

func sameRoot(file *os.File, root *fallbackRoot) bool {
	id, err := statFile(file)
	return err == nil && id.Dev == root.id.Dev && id.Ino == root.id.Ino &&
		id.Mode == root.id.Mode && id.UID == root.id.UID && mountID(file) == root.mount
}

func (e *engine) fallbackRecord(path, source string, file *os.File, id identity, now time.Time, active bool) {
	f := e.fallback
	i, root := f.scope(path)
	l := &f.lanes[i]
	if e.fallbackExcluded(path) {
		l.stats.Skipped["path_excluded"]++
		return
	}
	if e.c.fileSkipReason(path, id) != "" || id.Mode&0111 != 0 && !e.c.executableData(path) {
		l.stats.Skipped["file_type"]++
		return
	}
	key := [2]uint64{id.Dev, id.Ino}
	old := l.byInode[key]
	if root == nil {
		l.stats.Skipped["outside_allowed_roots"]++
		return
	}
	var st syscall.Stat_t
	if statErr := syscall.Fstat(int(file.Fd()), &st); statErr != nil || st.Nlink != 1 ||
		id.Dev != root.id.Dev || mountID(file) != root.mount || !supportedFS(file) {
		l.stats.Skipped["mount_link_or_fs"]++
		return
	}
	if old != nil && old.path != path {
		l.stats.Skipped["duplicate_inode"]++
		return
	}
	var sourceBit uint8 = 1
	switch source {
	case "access_hint":
		sourceBit = 2
	case "cgroup_open_fd":
		sourceBit = 4
	}
	if old == nil || old.sources&sourceBit == 0 {
		f.finding(i, path, source, id.Size, now)
	}
	if old == nil {
		old = &fallbackFile{path: path, source: source, id: id}
		files, cursor := &l.files, &l.evict
		limit := minInt(e.c.MaxFiles, 1024)
		if source != "directory_walk" {
			files, cursor = &l.fast, &l.fastEvict
		}
		if len(*files) == limit {
			stale := (*files)[*cursor]
			delete(l.byInode, [2]uint64{stale.id.Dev, stale.id.Ino})
			(*files)[*cursor] = old
			*cursor = (*cursor + 1) % limit
			l.stats.Skipped["inventory_rotated"]++
		} else {
			*files = append(*files, old)
		}
		l.byInode[key] = old
	} else if !sameFile(old.id, id) {
		old.id, old.offset = id, 0
	}
	old.sources |= sourceBit
	if sourceBit == 4 || (sourceBit == 2 && old.sources&4 == 0) {
		old.source = source
	}
	if active {
		quiet := e.c.roleConfig(e.c.pathPolicy(path)).ProtectSeconds
		old.protected = now.Add(time.Duration(max64(1, int64(quiet))) * time.Second)
	}
}

func (f *fallbackEngine) finding(i int, path, source string, size int64, now time.Time) {
	f.lanes[i].stats.Discovery[source]++
	entry := FallbackFinding{path, fallbackScopeName(i), source, size, now, now.Sub(f.started).Milliseconds()}
	if len(f.current.Findings) == 16 {
		copy(f.current.Findings, f.current.Findings[1:])
		f.current.Findings[15] = entry
	} else {
		f.current.Findings = append(f.current.Findings, entry)
	}
}

func fallbackScopeName(i int) string {
	if i == fallbackFixed {
		return "fixed"
	}
	return "auto"
}

func (e *engine) fallbackHint(path, source string, now time.Time) {
	f := e.fallback
	if f == nil || e.fallbackExcluded(path) {
		return
	}
	_, root := f.scope(path)
	if root == nil {
		return
	}
	if file, err := safeOpen(path); err == nil {
		defer file.Close()
		if id, err := statFile(file); err == nil {
			e.fallbackRecord(path, source, file, id, now, true)
		}
	}
}

// One directory entry per call. Open cursors survive the round boundary.
func (e *engine) fallbackWalk(l *fallbackScope, now time.Time) {
	if len(l.roots) == 0 || now.Before(l.walkAgain) {
		return
	}
	if len(l.dirs) == 0 {
		if l.walkRoot == len(l.roots) {
			l.walkRoot = 0
			l.walkAgain = now.Add(time.Duration(e.fallback.c.IntervalSeconds) * time.Second)
			return
		}
		root := l.roots[l.walkRoot]
		l.walkRoot++
		file, err := safeOpen(root.path)
		if err == nil {
			if sameRoot(file, &root) {
				l.dirs = append(l.dirs, fallbackDir{file, root.path})
				return
			}
			file.Close()
		}
		l.stats.Skipped["root_unavailable_or_replaced"]++
		return
	}
	d := l.dirs[len(l.dirs)-1]
	names, err := d.file.Readdirnames(1)
	if err != nil {
		d.file.Close()
		l.dirs = l.dirs[:len(l.dirs)-1]
		if err != io.EOF {
			l.stats.Skipped["directory_read"]++
		}
	}
	if len(names) == 0 {
		return
	}
	path := filepath.Join(d.path, names[0])
	id, err := statChild(d.file, names[0])
	if err != nil || e.fallbackExcluded(path) {
		l.stats.Skipped["entry_unavailable_or_excluded"]++
		return
	}
	file, err := safeOpen(path)
	if err != nil {
		l.stats.Skipped["entry_open"]++
		return
	}
	check, err := statFile(file)
	_, root := e.fallback.scope(path)
	if err != nil || root == nil || !sameFile(check, id) {
		file.Close()
		l.stats.Skipped["entry_changed"]++
		return
	}
	if id.Mode&syscall.S_IFMT == syscall.S_IFDIR && id.Dev == root.id.Dev && mountID(file) == root.mount && len(l.dirs) < 32 {
		l.dirs = append(l.dirs, fallbackDir{file, path})
		if e.hints != nil {
			e.hints.watch(path, id.Dev, e.c.MaxWatches)
		}
		return
	}
	defer file.Close()
	e.fallbackRecord(path, "directory_walk", file, id, now, false)
}

func (e *engine) fallbackWork(ctx context.Context, now, deadline time.Time, allow *atomic.Bool) {
	f := e.fallback
	input := f.input.Load()
	if input == nil {
		return
	}
	f.current.Permission = input.policy.Reason
	f.current.KubeletHeadroom = input.signals.NodeHeadroomBytes
	f.current.KubeletSignal = nil
	if input.signals.NodeHeadroomBytes != nil {
		f.current.KubeletSignal = input.metrics.Node
	}
	f.current.ParentRemaining = parentRemaining(input.metrics)
	f.current.MemoryHigh = input.metrics.MemoryHigh
	f.current.MemoryHighExpected = f.c.ExpectedMemoryHighBytes
	f.current.MemoryHighStatus = "unconfigured"
	if f.c.ExpectedMemoryHighBytes != nil {
		f.current.MemoryHighStatus = "unavailable"
		if input.metrics.MemoryHigh != nil {
			f.current.MemoryHighStatus = "matched"
			if *input.metrics.MemoryHigh != *f.c.ExpectedMemoryHighBytes {
				f.current.MemoryHighStatus = "drift"
			}
		}
	}
	earlyWarning := f.current.Reason == "" && (input.reason == "ancestor_headroom_warning" || input.reason == "kubelet_headroom_warning")
	if !now.Before(f.nextRound) || earlyWarning {
		f.current.Round++
		f.current.At = now
		f.nextRound = now.Add(time.Duration(f.c.IntervalSeconds) * time.Second)
		mainScan := e.stats.ScannedBytes - e.stats.FallbackScannedBytes
		mainReduction := e.stats.MainRecheckedNetBytes
		f.current.Reason = input.reason
		if f.current.Round > 1 && mainScan-f.lastScan >= e.c.RegionBytes &&
			f.c.MinMainReductionBytes > 0 && mainReduction-f.lastReduction < f.c.MinMainReductionBytes {
			f.stalledRounds = minInt(2, f.stalledRounds+1)
		} else {
			f.stalledRounds = 0
		}
		if input.reason == "" && f.stalledRounds >= 2 {
			f.current.Reason = "main_no_rechecked_progress"
		}
		f.lastScan, f.lastReduction = mainScan, mainReduction
		for i := fallbackFixed; i <= fallbackAuto; i++ {
			s := &f.lanes[i].stats
			s.Entries, s.RoundWorkUS, s.RoundRequested = 0, 0, 0
		}
	}
	// Continue sampling for short-lived open files even when today's directory
	// walk or advice round has spent its quota.
	procUntil := now.Add(deadline.Sub(now) / 3)
	procStart := time.Now()
	procRate := float64(e.c.MaxOperationsPerSecond) * .10
	f.procOps.refill(now, procRate, maxFloat(1, procRate))
	for time.Now().Before(procUntil) && !now.Before(f.proc.next) && f.procOps.take(1) && e.ops.take(1) {
		f.proc.step(e, input.metrics, now)
	}
	f.current.ProcWorkUS += time.Since(procStart).Microseconds()
	f.priority++
	for j := range 2 {
		i := 1 + (f.priority+j)%2
		l := &f.lanes[i]
		opsRate := float64(e.c.MaxOperationsPerSecond) * .20
		walkRate := float64(e.c.DiscoverEntriesPerSecond) * .20
		l.ops.refill(now, opsRate, maxFloat(1, opsRate))
		l.discovery.refill(now, walkRate, maxFloat(1, walkRate))
		start := time.Now()
		until := deadline
		if j == 0 {
			until = start.Add(deadline.Sub(start) / 2)
		}
		canWalk := len(l.roots) > 0 && !now.Before(l.walkAgain)
		canScan := f.current.Reason != "" && input.policy.Allow &&
			(i == fallbackFixed || f.c.AutoAction == "enforce") && (len(l.fast) != 0 || len(l.files) != 0) &&
			l.stats.RoundRequested < f.c.RoundAdviceBytesPerScope
		if !canWalk && !canScan {
			continue
		}
		// Spread quota across the round so active files and large trees do not
		// exhaust it before a file closes and the protection period expires.
		pace := minInt(f.c.RoundEntriesPerScope, 32+int(now.Sub(f.current.At).Seconds()*float64(f.c.RoundEntriesPerScope)/float64(f.c.IntervalSeconds)))
		for ctx.Err() == nil && time.Now().Before(until) && l.stats.Entries < pace &&
			l.stats.RoundWorkUS+time.Since(start).Microseconds() < int64(f.c.RoundWorkMSPerScope)*1000 {
			if !l.ops.take(1) || !e.ops.take(1) {
				break
			}
			l.stats.Entries++
			if canWalk && (l.stats.Entries%2 != 0 || !canScan) && l.discovery.take(1) {
				e.fallbackWalk(l, now)
			} else if canScan {
				e.fallbackScan(l, i, now, allow)
			} else {
				break
			}
		}
		l.stats.RoundWorkUS += time.Since(start).Microseconds()
	}
}

func (e *engine) fallbackScan(l *fallbackScope, lane int, now time.Time, allow *atomic.Bool) {
	files, cursor := l.files, &l.cursor
	l.visits++
	if len(l.fast) > 0 && (len(files) == 0 || l.visits%4 != 0) {
		files, cursor = l.fast, &l.fastCursor
	}
	if len(files) == 0 {
		return
	}
	*cursor %= len(files)
	entry := files[*cursor]
	*cursor++
	if now.Before(entry.protected) || entry.writerEpoch > e.fallback.proc.lastCleanEpoch {
		l.stats.Skipped["recent_access_or_open_fd"]++
		return
	}
	page := int64(os.Getpagesize())
	length := min64(e.c.RegionBytes, entry.id.Size-entry.offset) / page * page
	if length <= 0 {
		entry.offset = 0
		return
	}
	if l.stats.RoundRequested+length > e.fallback.c.RoundAdviceBytesPerScope || !e.scan.take(float64(length)) {
		return
	}
	file, err := safeOpen(entry.path)
	if err != nil {
		l.stats.Skipped["candidate_open"]++
		return
	}
	defer file.Close()
	id, err := statFile(file)
	if err != nil || !sameFile(id, entry.id) {
		l.stats.Skipped["candidate_identity_changed"]++
		return
	}
	if !e.fallbackFileSafe(entry.path, id, file, now, true) {
		return
	}
	key := regionKey{id.Dev, id.Ino, entry.offset}
	offset := entry.offset
	entry.offset += length
	if r := e.regions[key]; r != nil {
		if r.Fallback == 0 && !r.Check && sameFile(r.ID, id) && e.fallbackSlotAvailable(lane) {
			r.Fallback, r.DiscoverySource = lane, entry.source
			l.pending++
			e.inspect(r, now, allow)
			return
		}
		l.stats.Skipped["region_already_indexed"]++
		return
	}
	res, err := resident(file, offset, length)
	if err != nil {
		l.stats.Skipped["residency_unavailable"]++
		return
	}
	l.stats.Queries += length
	e.stats.ScannedBytes += length
	e.stats.FallbackScannedBytes += length
	e.stats.ObservedResidentBytes += res
	e.auditQuery(entry.path, id, offset, length, res, "initial")
	r := e.trackRegion(&fileEntry{Path: entry.path, ID: id}, offset, length, res, now, lane, entry.source)
	if r != nil {
		e.inspect(r, now, allow)
	}
}

func (e *engine) fallbackFileSafe(path string, id identity, file *os.File, now time.Time, requireInventory bool) bool {
	f := e.fallback
	i, root := f.scope(path)
	l := &f.lanes[i]
	if root == nil || e.fallbackExcluded(path) {
		l.stats.Skipped["outside_allowed_roots"]++
		return false
	}
	var st syscall.Stat_t
	if syscall.Fstat(int(file.Fd()), &st) != nil || st.Nlink != 1 || id.Dev != root.id.Dev || mountID(file) != root.mount || !supportedFS(file) {
		l.stats.Skipped["mount_link_or_fs"]++
		return false
	}
	policy := e.c.roleConfig(e.c.pathPolicy(path))
	if beneath(path, f.c.pgdata()) {
		policy.ProtectSeconds = int(max64(60, int64(policy.ProtectSeconds)))
		policy.MinFileAgeSeconds = int(max64(60, int64(policy.MinFileAgeSeconds)))
	}
	old := l.byInode[[2]uint64{id.Dev, id.Ino}]
	atime := time.Unix(st.Atim.Sec, st.Atim.Nsec)
	mtime := time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
	ctime := time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	if (requireInventory && old == nil) ||
		(old != nil && (old.path != path || now.Before(old.protected) || old.writerEpoch > f.proc.lastCleanEpoch)) ||
		now.Sub(atime) < time.Duration(policy.ProtectSeconds)*time.Second ||
		now.Sub(mtime) < time.Duration(policy.MinFileAgeSeconds)*time.Second ||
		now.Sub(ctime) < time.Duration(policy.MinFileAgeSeconds)*time.Second {
		l.stats.Skipped["recent_access_write_or_missing_identity"]++
		return false
	}
	rf, err := safeOpen(root.path)
	if err == nil {
		defer rf.Close()
		if sameRoot(rf, root) {
			return true
		}
	}
	l.stats.Skipped["root_unavailable_or_replaced"]++
	return false
}

func parentRemaining(m Metrics) *int64 {
	if m.AncestorLimit <= 0 {
		return nil
	}
	bytes := m.AncestorLimit - m.AncestorCurrent
	return &bytes
}

func fallbackTrigger(c Config, m Metrics, s SignalStatus, target int64) string {
	f := c.CacheFallback
	if s.NodeHeadroomBytes != nil && f.NodeHeadroomBytes > 0 && *s.NodeHeadroomBytes <= f.NodeHeadroomBytes {
		return "kubelet_headroom_warning"
	}
	if headroom := parentRemaining(m); headroom != nil && f.ParentHeadroomBytes > 0 &&
		*headroom <= max64(f.ParentHeadroomBytes, m.AncestorLimit-int64(float64(m.AncestorLimit)*c.MemoryHighRatio)) {
		return "ancestor_headroom_warning"
	}
	if target > 0 && m.Available["file_bytes"] && m.Available["shmem_bytes"] && max64(0, m.File-m.Shmem) > target {
		return "file_cache_above_target"
	}
	return ""
}

func (e *engine) fallbackAllows(r *region, now time.Time) bool {
	f := e.fallback
	in := f.input.Load()
	if in == nil || !in.policy.Allow || !adviceFresh(e.c, in.metrics, now) ||
		now.Before(f.current.At) || !now.Before(f.nextRound) || f.current.Reason == "" ||
		(r.Fallback == fallbackAuto && f.c.AutoAction != "enforce") {
		return false
	}
	l := &f.lanes[r.Fallback]
	return l.stats.RoundRequested+r.Length <= f.c.RoundAdviceBytesPerScope &&
		l.stats.RoundWorkUS < int64(f.c.RoundWorkMSPerScope)*1000 &&
		e.pathAllowed(r.Path, now, &in.permission)
}

func (e *engine) regionAllowed(r *region, allow *atomic.Bool, now time.Time) bool {
	if e.fallback != nil && e.fallbackExcluded(r.Path) {
		return false
	}
	if r.Fallback != 0 {
		return e.fallbackAllows(r, now)
	}
	return allow.Load() && e.roleAllows(r.Path, now)
}

func (e *engine) fallbackSlotAvailable(lane int) bool {
	if e.fallback == nil {
		return true
	}
	f := e.fallback
	reserve := e.c.MaxRegions / 4
	if lane != 0 {
		return f.lanes[lane].pending < reserve
	}
	return len(e.regions)-f.lanes[fallbackFixed].pending-f.lanes[fallbackAuto].pending < e.c.MaxRegions-2*reserve
}

func (e *engine) fallbackSnapshot() *FallbackStatus {
	if e.fallback == nil {
		return nil
	}
	f := e.fallback
	s := f.current
	s.Findings = append([]FallbackFinding(nil), s.Findings...)
	copyScope := func(i int) FallbackScopeStatus {
		v := f.lanes[i].stats
		v.Discovery = cloneCounts(v.Discovery)
		v.Skipped = cloneCounts(v.Skipped)
		return v
	}
	s.Fixed, s.Auto = copyScope(fallbackFixed), copyScope(fallbackAuto)
	return &s
}

func cloneCounts(src map[string]uint64) map[string]uint64 {
	dst := make(map[string]uint64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
