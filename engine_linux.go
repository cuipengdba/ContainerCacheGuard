//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type Counters struct {
	DetectedRoots         []string `json:"detected_roots"`
	Files                 int      `json:"indexed_files"`
	Directories           int      `json:"pending_directories"`
	Regions               int      `json:"tracked_regions"`
	Discovered            uint64   `json:"discovered_entries"`
	Overflow              uint64   `json:"inventory_overflow"`
	IndexRotations        uint64   `json:"index_rotations"`
	Errors                uint64   `json:"errors"`
	LastError             string   `json:"last_error,omitempty"`
	ScannedBytes          int64    `json:"sampled_region_bytes"`
	ObservedResidentBytes int64    `json:"sampled_resident_bytes_cumulative"`
	AdviceBytes           int64    `json:"advice_bytes"`
	NetReductionBytes     int64    `json:"observed_net_reduction_bytes"`
	ReappearedBytes       int64    `json:"observed_reappeared_bytes"`
	Protected             uint64   `json:"protected_regions"`
	Ineffective           uint64   `json:"ineffective_advice"`
	DeferredRetries       uint64   `json:"deferred_retries"`
	Sweeps                uint64   `json:"inventory_sweeps"`
	CompletedSweeps       uint64   `json:"completed_inventory_sweeps"`
	FastDiscoveryFallback bool     `json:"fast_discovery_fallback"`
	Rate                  int64    `json:"current_advice_rate_bytes_per_second"`
	HintEvents            uint64   `json:"hint_events"`
	HintOverflows         uint64   `json:"hint_overflows"`
	Watches               int      `json:"directory_watches"`
	HintFiles             int      `json:"hint_files"`
	HintsAvailable        bool     `json:"access_hints_available"`
	PendingHints          int      `json:"pending_hint_paths"`
	HintRetries           uint64   `json:"hint_discovery_retries"`
	HintDropped           uint64   `json:"hint_queue_dropped"`
	FileScheduleVisits    uint64   `json:"file_schedule_visits"`
	WaitingRescanVisits   uint64   `json:"waiting_rescan_visits"`
	RecentWriteDeferrals  uint64   `json:"recent_write_deferrals"`
	ChangedCandidates     uint64   `json:"changed_candidates"`
	NonresidentCandidates uint64   `json:"nonresident_candidates"`
	NoProgressBackoffs    uint64   `json:"no_progress_backoffs"`
	SweepInProgress       bool     `json:"inventory_sweep_in_progress"`
	SweepAgeSeconds       float64  `json:"inventory_sweep_age_seconds"`
	LastSweepSeconds      float64  `json:"last_inventory_sweep_seconds"`
	FileScanWaitSeconds   float64  `json:"file_scan_wait_seconds"`
}
type fileEntry struct {
	Path     string
	ID       identity
	Offset   int64
	NextScan time.Time
}
type dirEntry struct {
	Path string
	Dev  uint64
}
type regionKey struct {
	Dev, Ino uint64
	Offset   int64
}
type region struct {
	Key                   regionKey
	Path                  string
	ID                    identity
	Length, After, Before int64
	Due                   time.Time
	Check                 bool
	Attempts              int
	NoProgress            int
	Until                 time.Time
}
type budget struct {
	tokens float64
	rate   float64
	cap    float64
	last   time.Time
}

func (b *budget) refill(now time.Time, rate, capacity float64) {
	if b.last.IsZero() {
		b.last = now
	}
	b.rate = rate
	b.cap = capacity
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
}
func (b *budget) take(n float64) bool {
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

type engine struct {
	c                                     Config
	roots                                 []string
	dirs                                  []dirEntry
	dir                                   *os.File
	dirInfo                               dirEntry
	nextSweep                             time.Time
	sweepOpen                             bool
	sweepStarted                          time.Time
	scanIdleUntil, scanEarliest           time.Time
	scanSkipped                           int
	childFastUnavailable                  bool
	files                                 []*fileEntry
	known                                 map[string]*fileEntry
	cursor                                int
	evictCursor                           int
	regions                               map[regionKey]*region
	queue                                 []*region
	qcursor                               int
	scan, revisit, advice, ops, discovery budget
	hintDiscovery                         budget
	duty                                  budget
	rate                                  int64
	nextIncrease                          time.Time
	cooldown                              time.Time
	stats                                 Counters
	hints                                 *accessHints
}

func newEngine(c Config, roots []string) *engine {
	e := &engine{c: c, roots: roots, known: map[string]*fileEntry{}, regions: map[regionKey]*region{}, rate: c.InitialReclaimBytesPerSecond}
	e.stats.DetectedRoots = append([]string{}, roots...)
	if c.AccessHints {
		e.hints = newAccessHints()
	}
	return e
}
func (e *engine) close() {
	if e.hints != nil {
		e.hints.close()
	}
	if e.dir != nil {
		e.dir.Close()
	}
}
func (e *engine) failure(err error) { e.stats.Errors++; e.stats.LastError = err.Error() }
func (e *engine) add(path string, rootDev uint64) {
	if e.c.excluded(path) || len(path) > 4096 {
		return
	}
	f, err := safeOpen(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.failure(err)
		}
		return
	}
	defer f.Close()
	id, err := statFile(f)
	if err != nil {
		e.failure(err)
		return
	}
	if !supportedFS(f) || (rootDev != 0 && id.Dev != rootDev) {
		return
	}
	e.addIdentity(path, rootDev, id)
}
func (e *engine) addChild(parent *os.File, name string, rootDev uint64) {
	path := filepath.Join(e.dirInfo.Path, name)
	if e.c.excluded(path) || len(path) > 4096 {
		return
	}
	if e.childFastUnavailable {
		e.add(path, rootDev)
		return
	}
	id, err := statChild(parent, name)
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
			e.childFastUnavailable = true
			e.stats.FastDiscoveryFallback = true
			e.add(path, rootDev)
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			e.failure(err)
		}
		return
	}
	e.addIdentity(path, rootDev, id)
}
func (e *engine) addIdentity(path string, rootDev uint64, id identity) {
	if rootDev != 0 && id.Dev != rootDev {
		return
	}
	if id.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		if e.hints != nil {
			e.hints.watch(path, id.Dev, e.c.MaxWatches)
		}
		if len(e.dirs) >= e.c.MaxDirectories {
			e.stats.Overflow++
			return
		}
		e.dirs = append(e.dirs, dirEntry{path, id.Dev})
		return
	}
	// Vendor PostgreSQL data files can be mode 0700. In an explicit paths
	// scope, an execute bit is not evidence that a file is program code.
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG || id.Size < int64(os.Getpagesize()) ||
		(e.c.Mode == "auto" && id.Mode&0111 != 0) {
		return
	}
	base := filepath.Base(path)
	if strings.Contains(base, ".so") || strings.HasSuffix(base, ".a") {
		return
	}
	if old := e.known[path]; old != nil {
		if !sameFile(old.ID, id) {
			old.ID = id
			old.Offset = 0
			old.NextScan = time.Time{}
			e.invalidateScanWait()
		}
		return
	}
	n := &fileEntry{Path: path, ID: id}
	e.invalidateScanWait()
	if len(e.known) >= e.c.MaxFiles {
		// Keep a bounded working index while still visiting later files in a
		// directory. A permanently full index would starve every file beyond
		// the first MaxFiles entries on the next sweep as well.
		if len(e.files) == 0 {
			e.stats.Overflow++
			return
		}
		e.evictCursor %= len(e.files)
		old := e.files[e.evictCursor]
		delete(e.known, old.Path)
		e.files[e.evictCursor] = n
		e.evictCursor = (e.evictCursor + 1) % len(e.files)
		e.known[path] = n
		e.stats.IndexRotations++
		return
	}
	e.known[path] = n
	e.files = append(e.files, n)
}
func (e *engine) finishSweep(now time.Time) {
	if !e.sweepOpen {
		return
	}
	e.stats.CompletedSweeps++
	e.stats.LastSweepSeconds = maxFloat(0, now.Sub(e.sweepStarted).Seconds())
	e.sweepOpen = false
	// Very large trees can take longer than rescan_seconds to traverse. Start
	// the quiet interval after completion so they do not rescan continuously.
	e.nextSweep = now.Add(time.Duration(e.c.RescanSeconds) * time.Second)
}
func (e *engine) discover(now time.Time, deadline time.Time) {
	if e.dir == nil && len(e.dirs) == 0 {
		if now.Before(e.nextSweep) {
			return
		}
		if e.c.Mode == "auto" {
			e.roots = discoverRoots(e.c)
		} else {
			e.roots = normalizeRoots(e.c, e.c.Paths)
		}
		// Drop vanished entries during normal scanning; index limits remain hard.
		for _, p := range e.roots {
			e.add(p, 0)
		}
		e.nextSweep = now.Add(time.Duration(e.c.RescanSeconds) * time.Second)
		e.stats.Sweeps++
		e.sweepOpen = true
		e.sweepStarted = now
	}
	for time.Now().Before(deadline) && e.discovery.take(1) {
		if e.dir == nil {
			if len(e.dirs) == 0 {
				e.finishSweep(time.Now())
				return
			}
			e.dirInfo = e.dirs[0]
			e.dirs[0] = dirEntry{}
			e.dirs = e.dirs[1:]
			f, err := safeOpen(e.dirInfo.Path)
			if err != nil {
				e.failure(err)
				continue
			}
			id, err := statFile(f)
			if err != nil || id.Dev != e.dirInfo.Dev || id.Mode&syscall.S_IFMT != syscall.S_IFDIR {
				f.Close()
				continue
			}
			e.dir = f
		}
		names, err := e.dir.Readdirnames(1)
		if len(names) > 0 {
			e.stats.Discovered++
			e.addChild(e.dir, names[0], e.dirInfo.Dev)
		}
		if err != nil {
			e.dir.Close()
			e.dir = nil
			if err != io.EOF {
				e.failure(err)
			}
		}
	}
	if e.dir == nil && len(e.dirs) == 0 && e.sweepOpen {
		e.finishSweep(time.Now())
	}
}
func (e *engine) track(f *fileEntry, offset, length, res int64, now time.Time) {
	if res == 0 {
		return
	}
	k := regionKey{f.ID.Dev, f.ID.Ino, offset}
	if r := e.regions[k]; r != nil {
		return
	}
	if len(e.regions) >= e.c.MaxRegions {
		e.stats.Overflow++
		return
	}
	r := &region{Key: k, Path: f.Path, ID: f.ID, Length: length, Before: res, Due: now.Add(time.Duration(e.c.ProtectSeconds) * time.Second)}
	e.regions[k] = r
	e.queue = append(e.queue, r)
}
func (e *engine) removeRegion(r *region) { delete(e.regions, r.Key) }
func (e *engine) inspect(r *region, now time.Time, allow *atomic.Bool) {
	if now.Before(r.Due) || now.Before(r.Until) {
		return
	}
	// Do not spend the sampling budget repeatedly on a candidate that cannot
	// receive advice this tick. Post-advice checks still run while paused.
	if !r.Check && (!allow.Load() || now.Before(e.cooldown) || e.advice.tokens < float64(r.Length)) {
		return
	}
	needed := float64(r.Length)
	if !r.Check {
		needed *= 2 // Before and immediately after DONTNEED.
	}
	if e.revisit.tokens < needed || !e.ops.take(1) || !e.revisit.take(float64(r.Length)) {
		return
	}
	f, err := safeOpen(r.Path)
	if err != nil {
		e.removeRegion(r)
		if !errors.Is(err, os.ErrNotExist) {
			e.failure(err)
		}
		return
	}
	defer f.Close()
	id, err := statFile(f)
	if err != nil || !sameFile(id, r.ID) || !supportedFS(f) {
		e.stats.ChangedCandidates++
		e.removeRegion(r)
		return
	}
	res, err := resident(f, r.Key.Offset, r.Length)
	if err != nil {
		e.failure(err)
		e.removeRegion(r)
		return
	}
	e.stats.ScannedBytes += r.Length
	e.stats.ObservedResidentBytes += res
	if r.Check {
		again := max64(0, res-r.After)
		e.stats.ReappearedBytes += again
		if float64(again) > float64(max64(1, r.Before-r.After))*e.c.MaxReappearanceRatio {
			e.stats.Protected++
			r.Until = now.Add(time.Duration(e.c.CooldownSeconds) * time.Second)
			r.Check = false
			e.rate = max64(e.c.InitialReclaimBytesPerSecond, e.rate/2)
			e.cooldown = now.Add(time.Duration(e.c.RecheckSeconds) * time.Second)
			return
		}
		// DONTNEED may start asynchronous writeback without invalidating every
		// page immediately. Retain a still-resident candidate for a bounded
		// retry instead of waiting for a potentially long whole-tree sweep.
		// A region that reappeared above was protected before this branch.
		if res > 0 && r.After > 0 && r.Attempts < 3 {
			r.Check = false
			r.Before = res
			r.Due = now.Add(time.Duration(e.c.RecheckSeconds) * time.Second)
			e.stats.DeferredRetries++
			return
		}
		if r.NoProgress > 0 && r.Attempts >= 3 && res > 0 {
			// Keep bounded history through the cooldown. Dropping it here would let
			// an ordinary rescan immediately recreate the same ineffective candidate.
			r.Check = false
			r.Attempts = 0
			r.Due = now.Add(noProgressDelay(e.c, r.NoProgress))
			r.Until = r.Due
			e.stats.NoProgressBackoffs++
			return
		}
		e.removeRegion(r)
		return
	}
	if res == 0 {
		e.stats.NonresidentCandidates++
		e.removeRegion(r)
		return
	}
	if now.Sub(time.Unix(0, id.Mtime)) < time.Duration(e.c.MinFileAgeSeconds)*time.Second {
		e.stats.RecentWriteDeferrals++
		r.Due = now.Add(time.Duration(e.c.MinFileAgeSeconds) * time.Second)
		return
	}
	if !allow.Load() || now.Before(e.cooldown) || !e.revisit.take(float64(r.Length)) || !e.advice.take(float64(r.Length)) {
		return
	}
	// Revalidate immediately before advice. A concurrent writer can still race;
	// fadvise is advisory and this is deliberately not a clean-page guarantee.
	check, err := statFile(f)
	if err != nil || !sameFile(id, check) {
		e.removeRegion(r)
		return
	}
	if !allow.Load() {
		return
	}
	if err = advise(f, r.Key.Offset, r.Length); err != nil {
		e.failure(err)
		r.Due = now.Add(time.Duration(e.c.CooldownSeconds) * time.Second)
		return
	}
	e.stats.AdviceBytes += r.Length
	r.Attempts++
	after, err := resident(f, r.Key.Offset, r.Length)
	if err != nil {
		e.failure(err)
		e.removeRegion(r)
		return
	}
	e.stats.ScannedBytes += r.Length
	e.stats.ObservedResidentBytes += after
	reduction := max64(0, res-after)
	e.stats.NetReductionBytes += reduction
	if reduction == 0 {
		e.stats.Ineffective++
		r.NoProgress = minInt(4, r.NoProgress+1)
		r.Until = now.Add(noProgressDelay(e.c, r.NoProgress))
		e.rate = max64(e.c.InitialReclaimBytesPerSecond, e.rate/2)
	} else {
		r.NoProgress = 0
	}
	r.Before = res
	r.After = after
	r.Check = true
	r.Due = now.Add(time.Duration(e.c.RecheckSeconds) * time.Second)
}
func (e *engine) work(ctx context.Context, allow *atomic.Bool) Counters {
	now := time.Now()
	dutyRate := float64(e.c.WorkerBudgetMS) / 1000
	// Explicit observation needs the configured discovery budget to measure
	// coverage on large directories. Enforce mode still scans only slowly
	// while a safety gate has denied new advice.
	if e.c.Action != "observe" && !allow.Load() && dutyRate > 0.005 {
		dutyRate = 0.005
	}
	e.duty.refill(now, dutyRate, dutyRate)
	slice := e.duty.tokens
	if slice > 0.005 {
		slice = 0.005
	}
	if slice < 0.001 {
		return e.snapshot(now)
	}
	defer func() {
		e.duty.tokens -= time.Since(now).Seconds()
		if e.duty.tokens < 0 {
			e.duty.tokens = 0
		}
	}()
	deadline := now.Add(time.Duration(slice * float64(time.Second)))
	// Give candidate rechecks a reserved share of the configured sampling rate.
	// Otherwise continuous file scanning can consume every token before advice.
	revisitRate := float64(e.c.ScanBytesPerSecond) * 0.25
	scanRate := float64(e.c.ScanBytesPerSecond) - revisitRate
	e.scan.refill(now, scanRate, math.Max(scanRate, float64(e.c.RegionBytes)))
	e.revisit.refill(now, revisitRate, math.Max(revisitRate, float64(2*e.c.RegionBytes)))
	e.ops.refill(now, float64(e.c.MaxOperationsPerSecond), float64(e.c.MaxOperationsPerSecond))
	// Keep event-driven discovery independent of the ordinary tree walk.
	// Otherwise a large old backup consumes all discovery tokens before a
	// newly-created backup directory can be watched. The sum of both rates
	// remains discover_entries_per_second.
	hintRate := 0.0
	if e.hints != nil && e.hints.fd >= 0 {
		hintRate = float64(e.c.DiscoverEntriesPerSecond) * 0.25
	}
	ordinaryRate := float64(e.c.DiscoverEntriesPerSecond) - hintRate
	e.discovery.refill(now, ordinaryRate, math.Max(1, ordinaryRate))
	e.hintDiscovery.refill(now, hintRate, math.Max(1, hintRate))
	if !allow.Load() {
		e.rate = e.c.InitialReclaimBytesPerSecond
		e.advice.tokens = 0
		e.nextIncrease = now.Add(30 * time.Second)
	} else if now.After(e.nextIncrease) && now.After(e.cooldown) {
		e.rate = min64(e.c.ReclaimBytesPerSecond, e.rate+e.c.InitialReclaimBytesPerSecond)
		e.nextIncrease = now.Add(30 * time.Second)
	}
	e.advice.refill(now, float64(e.rate), float64(e.rate))
	e.discover(now, now.Add(time.Duration(slice*0.15*float64(time.Second))))
	if e.hints != nil {
		e.hints.poll(e, now, now.Add(time.Duration(slice*0.30*float64(time.Second))))
	}
	// Due regions are revisited independently of full-directory coverage.
	count := len(e.queue)
	for i := 0; i < count && time.Now().Before(now.Add(time.Duration(slice*0.70*float64(time.Second)))); i++ {
		if len(e.queue) == 0 {
			break
		}
		if e.qcursor >= len(e.queue) {
			e.qcursor = 0
		}
		r := e.queue[e.qcursor]
		if e.regions[r.Key] != r {
			last := len(e.queue) - 1
			e.queue[e.qcursor] = e.queue[last]
			e.queue[last] = nil
			e.queue = e.queue[:last]
			continue
		}
		e.qcursor++
		if ctx.Err() != nil {
			break
		}
		e.inspect(r, now, allow)
	}
	if e.hints != nil {
		e.hints.sample(e, now, now.Add(time.Duration(slice*0.90*float64(time.Second))))
	}
	for !now.Before(e.scanIdleUntil) && len(e.files) > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
		if !e.ops.take(1) {
			break
		}
		if e.cursor >= len(e.files) {
			e.cursor = 0
		}
		entry := e.files[e.cursor]
		e.stats.FileScheduleVisits++
		if now.Before(entry.NextScan) {
			e.cursor++
			if e.deferScan(entry.NextScan) {
				break
			}
			continue
		}
		e.invalidateScanWait()
		if entry.Offset >= entry.ID.Size {
			entry.Offset = 0
			entry.NextScan = now.Add(time.Duration(e.c.RescanSeconds) * time.Second)
			e.cursor++
			continue
		}
		length := min64(e.c.RegionBytes, entry.ID.Size-entry.Offset)
		length = length / int64(os.Getpagesize()) * int64(os.Getpagesize())
		if length == 0 {
			entry.Offset = 0
			e.cursor++
			continue
		}
		if !e.scan.take(float64(length)) {
			break
		}
		f, err := safeOpen(entry.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				delete(e.known, entry.Path)
				last := len(e.files) - 1
				e.files[e.cursor] = e.files[last]
				e.files[last] = nil
				e.files = e.files[:last]
				e.invalidateScanWait()
			} else {
				e.failure(err)
				e.cursor++
			}
			continue
		}
		id, err := statFile(f)
		if err != nil || id.Mode&syscall.S_IFMT != syscall.S_IFREG || !supportedFS(f) {
			f.Close()
			e.cursor++
			continue
		}
		if !sameFile(id, entry.ID) {
			entry.ID = id
			entry.Offset = 0
			entry.NextScan = time.Time{}
			f.Close()
			e.cursor++
			continue
		}
		res, err := resident(f, entry.Offset, length)
		f.Close()
		if err != nil {
			e.failure(err)
			e.cursor++
			continue
		}
		e.stats.ScannedBytes += length
		e.stats.ObservedResidentBytes += res
		e.track(entry, entry.Offset, length, res, now)
		entry.Offset += length
	}
	return e.snapshot(now)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func noProgressDelay(c Config, streak int) time.Duration {
	streak = minInt(4, streak)
	if streak < 1 {
		streak = 1
	}
	multiplier := int64(1 << uint(streak-1))
	seconds := int64(c.CooldownSeconds)
	if seconds > int64(math.MaxInt64)/int64(time.Second)/multiplier {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds*multiplier) * time.Second
}
func (e *engine) invalidateScanWait() {
	e.scanIdleUntil, e.scanEarliest = time.Time{}, time.Time{}
	e.scanSkipped = 0
}

// Account across worker slices, so a large sleeping index is traversed once
// per wakeup rather than once per tiny time slice. New/changed entries reset it.
func (e *engine) deferScan(until time.Time) bool {
	e.stats.WaitingRescanVisits++
	if e.scanEarliest.IsZero() || until.Before(e.scanEarliest) {
		e.scanEarliest = until
	}
	e.scanSkipped++
	if e.scanSkipped >= len(e.files) {
		e.scanIdleUntil = e.scanEarliest
		e.scanSkipped = 0
		e.scanEarliest = time.Time{}
		return true
	}
	return false
}
func (e *engine) snapshot(now time.Time) Counters {
	e.stats.SweepInProgress = e.sweepOpen
	e.stats.SweepAgeSeconds = 0
	if e.sweepOpen {
		e.stats.SweepAgeSeconds = maxFloat(0, now.Sub(e.sweepStarted).Seconds())
	}
	e.stats.FileScanWaitSeconds = maxFloat(0, e.scanIdleUntil.Sub(now).Seconds())
	e.stats.Files = len(e.files)
	e.stats.Directories = len(e.dirs)
	e.stats.Regions = len(e.regions)
	e.stats.Rate = e.rate
	if e.hints != nil {
		e.stats.HintEvents = e.hints.events
		e.stats.HintsAvailable = e.hints.fd >= 0
		e.stats.HintOverflows = e.hints.overflows
		e.stats.Watches = len(e.hints.watches)
		e.stats.HintFiles = len(e.hints.files)
		e.stats.PendingHints = len(e.hints.pending)
		e.stats.HintRetries = e.hints.retries
		e.stats.HintDropped = e.hints.dropped
	}
	e.stats.DetectedRoots = append([]string{}, e.roots...)
	return e.stats
}
