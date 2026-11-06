//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"io"
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
	Errors                uint64   `json:"errors"`
	LastError             string   `json:"last_error,omitempty"`
	ScannedBytes          int64    `json:"sampled_region_bytes"`
	ObservedResidentBytes int64    `json:"sampled_resident_bytes_cumulative"`
	AdviceBytes           int64    `json:"advice_bytes"`
	NetReductionBytes     int64    `json:"observed_net_reduction_bytes"`
	ReappearedBytes       int64    `json:"observed_reappeared_bytes"`
	Protected             uint64   `json:"protected_regions"`
	Ineffective           uint64   `json:"ineffective_advice"`
	Sweeps                uint64   `json:"inventory_sweeps"`
	Rate                  int64    `json:"current_advice_rate_bytes_per_second"`
}
type fileEntry struct {
	Path   string
	ID     identity
	Offset int64
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
	c                            Config
	roots                        []string
	dirs                         []dirEntry
	dir                          *os.File
	dirInfo                      dirEntry
	nextSweep                    time.Time
	files                        []*fileEntry
	known                        map[string]*fileEntry
	cursor                       int
	regions                      map[regionKey]*region
	queue                        []*region
	qcursor                      int
	scan, advice, ops, discovery budget
	duty                         budget
	rate                         int64
	nextIncrease                 time.Time
	cooldown                     time.Time
	stats                        Counters
}

func newEngine(c Config, roots []string) *engine {
	return &engine{c: c, roots: roots, known: map[string]*fileEntry{}, regions: map[regionKey]*region{}, rate: c.InitialReclaimBytesPerSecond}
}
func (e *engine) close() {
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
	if id.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		if len(e.dirs) >= e.c.MaxDirectories {
			e.stats.Overflow++
			return
		}
		e.dirs = append(e.dirs, dirEntry{path, id.Dev})
		return
	}
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG || id.Size < int64(os.Getpagesize()) || id.Mode&0111 != 0 {
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
		}
		return
	}
	if len(e.known) >= e.c.MaxFiles {
		e.stats.Overflow++
		return
	}
	n := &fileEntry{Path: path, ID: id}
	e.known[path] = n
	e.files = append(e.files, n)
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
	}
	for time.Now().Before(deadline) && e.discovery.take(1) {
		if e.dir == nil {
			if len(e.dirs) == 0 {
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
			e.add(filepath.Join(e.dirInfo.Path, names[0]), e.dirInfo.Dev)
		}
		if err != nil {
			e.dir.Close()
			e.dir = nil
			if err != io.EOF {
				e.failure(err)
			}
		}
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
	if !e.ops.take(1) || !e.scan.take(float64(r.Length)) {
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
		e.removeRegion(r)
		return
	}
	if res == 0 {
		e.removeRegion(r)
		return
	}
	if now.Sub(time.Unix(0, id.Mtime)) < time.Duration(e.c.MinFileAgeSeconds)*time.Second {
		r.Due = now.Add(time.Duration(e.c.MinFileAgeSeconds) * time.Second)
		return
	}
	if !allow.Load() || now.Before(e.cooldown) || !e.scan.take(float64(r.Length)) || !e.advice.take(float64(r.Length)) {
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
		r.Until = now.Add(time.Duration(e.c.CooldownSeconds) * time.Second)
		e.rate = max64(e.c.InitialReclaimBytesPerSecond, e.rate/2)
	}
	r.Before = res
	r.After = after
	r.Check = true
	r.Due = now.Add(time.Duration(e.c.RecheckSeconds) * time.Second)
}
func (e *engine) work(ctx context.Context, allow *atomic.Bool) Counters {
	now := time.Now()
	e.duty.refill(now, float64(e.c.WorkerBudgetMS)/1000, float64(e.c.WorkerBudgetMS)/1000)
	slice := e.duty.tokens
	if slice > 0.005 {
		slice = 0.005
	}
	if slice < 0.001 {
		return e.stats
	}
	defer func() {
		e.duty.tokens -= time.Since(now).Seconds()
		if e.duty.tokens < 0 {
			e.duty.tokens = 0
		}
	}()
	deadline := now.Add(time.Duration(slice * float64(time.Second)))
	e.scan.refill(now, float64(e.c.ScanBytesPerSecond), float64(e.c.ScanBytesPerSecond))
	e.ops.refill(now, float64(e.c.MaxOperationsPerSecond), float64(e.c.MaxOperationsPerSecond))
	e.discovery.refill(now, float64(e.c.DiscoverEntriesPerSecond), float64(e.c.DiscoverEntriesPerSecond))
	if !allow.Load() {
		e.rate = e.c.InitialReclaimBytesPerSecond
		e.advice.tokens = 0
		e.nextIncrease = now.Add(30 * time.Second)
	} else if now.After(e.nextIncrease) && now.After(e.cooldown) {
		e.rate = min64(e.c.ReclaimBytesPerSecond, e.rate+e.c.InitialReclaimBytesPerSecond)
		e.nextIncrease = now.Add(30 * time.Second)
	}
	e.advice.refill(now, float64(e.rate), float64(e.rate))
	e.discover(now, now.Add(time.Duration(slice/3*float64(time.Second))))
	// Due regions are revisited independently of full-directory coverage.
	count := len(e.queue)
	for i := 0; i < count && time.Now().Before(deadline); i++ {
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
	for len(e.files) > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
		if !e.ops.take(1) {
			break
		}
		if e.cursor >= len(e.files) {
			e.cursor = 0
		}
		entry := e.files[e.cursor]
		if entry.Offset >= entry.ID.Size {
			entry.Offset = 0
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
	e.stats.Files = len(e.files)
	e.stats.Directories = len(e.dirs)
	e.stats.Regions = len(e.regions)
	e.stats.Rate = e.rate
	e.stats.DetectedRoots = append([]string{}, e.roots...)
	return e.stats
}
