//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSleepingIndexWaitSurvivesSlicesAndWakesForChanges(t *testing.T) {
	e := newEngine(defaultConfig(), nil)
	defer e.close()
	now := time.Now()
	e.files = make([]*fileEntry, 100000)
	for i := range e.files {
		e.files[i] = &fileEntry{NextScan: now.Add(time.Hour)}
	}
	for i := range e.files {
		complete := e.deferScan(e.files[i].NextScan)
		if complete != (i == len(e.files)-1) {
			t.Fatalf("unexpected full-pass decision at %d", i)
		}
	}
	if !e.scanIdleUntil.Equal(now.Add(time.Hour)) {
		t.Fatal("wait deadline not cached")
	}
	e.nextSweep = now.Add(time.Hour)
	var allow atomic.Bool
	before := e.stats.FileScheduleVisits
	for i := 0; i < 10; i++ {
		e.duty.tokens = .1
		e.work(context.Background(), &allow)
	}
	if e.stats.FileScheduleVisits != before {
		t.Fatal("sleeping index repeatedly walked")
	}
	path := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	e.add(path, 0)
	if !e.scanIdleUntil.IsZero() || e.scanSkipped != 0 {
		t.Fatal("new file did not invalidate cached wait")
	}
}
func TestScanWaitUsesEarliestDeadlineAndIdentityChanges(t *testing.T) {
	c := defaultConfig()
	c.Mode = "paths"
	e := newEngine(c, nil)
	defer e.close()
	e.files = make([]*fileEntry, 3)
	now := time.Now()
	for _, delay := range []time.Duration{time.Hour, time.Minute, 2 * time.Hour} {
		e.deferScan(now.Add(delay))
	}
	if !e.scanIdleUntil.Equal(now.Add(time.Minute)) {
		t.Fatal("earliest wakeup lost")
	}
	e.known["/data/a"] = &fileEntry{ID: identity{Dev: 1, Ino: 1, Size: 8192, Mode: 0100600}}
	e.addIdentity("/data/a", 1, identity{Dev: 1, Ino: 2, Size: 8192, Mode: 0100600})
	if !e.scanIdleUntil.IsZero() {
		t.Fatal("identity change did not wake scanner")
	}
}
func TestNoProgressBackoffBoundedAndCandidateRetained(t *testing.T) {
	c := defaultConfig()
	c.CooldownSeconds = 2
	for streak, want := range map[int]time.Duration{0: 2 * time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 16 * time.Second, 100: 16 * time.Second} {
		if got := noProgressDelay(c, streak); got != want {
			t.Fatalf("streak %d: %s", streak, got)
		}
		extreme := c
		extreme.CooldownSeconds = math.MaxInt64
		if noProgressDelay(extreme, 4) != time.Duration(math.MaxInt64) {
			t.Fatal("cooldown overflowed")
		}
	}
	// A real resident file is only queried here: allow=false prohibits advice.
	path := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	e := newEngine(c, nil)
	defer e.close()
	e.add(path, 0)
	entry := e.known[path]
	if entry == nil {
		t.Fatal("file not indexed")
	}
	e.track(entry, 0, 8192, 8192, time.Now().Add(-time.Hour))
	var r *region
	for _, v := range e.regions {
		r = v
	}
	r.Check = true
	r.Attempts = 3
	r.NoProgress = 3
	r.After = 8192
	r.Before = 8192
	e.revisit.tokens = 1 << 20
	e.ops.tokens = 100
	now := time.Now()
	var allow atomic.Bool
	e.inspect(r, now, &allow)
	if e.stats.AdviceBytes != 0 || e.regions[r.Key] != r || r.Check || !r.Due.Equal(now.Add(8*time.Second)) || e.stats.NoProgressBackoffs != 1 {
		t.Fatalf("retention bypassed: %+v %+v", r, e.stats)
	}
}
func TestScanWaitInvalidatedByHintIdentityChange(t *testing.T) {
	c := defaultConfig()
	c.Mode = "paths"
	path := filepath.Join(t.TempDir(), "changing")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	e := newEngine(c, nil)
	defer e.close()
	e.add(path, 0)
	now := time.Now()
	e.known[path].NextScan = now.Add(time.Hour)
	e.scanIdleUntil = now.Add(time.Hour)
	e.hints.rememberFile(e, path, now)
	if err := os.WriteFile(path, make([]byte, 16384), 0600); err != nil {
		t.Fatal(err)
	}
	e.scan.tokens = 1 << 20
	e.ops.tokens = 100
	e.hints.sample(e, now, time.Now().Add(time.Second))
	if !e.scanIdleUntil.IsZero() || !e.known[path].NextScan.IsZero() {
		t.Fatal("hint identity change retained stale wait")
	}
}
func BenchmarkSleepingIndex(b *testing.B) {
	for _, count := range []int{1000, 100000, 1000000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			e := &engine{files: make([]*fileEntry, count)}
			wake := time.Now().Add(time.Hour)
			entry := &fileEntry{NextScan: wake}
			for i := 0; i < count; i++ {
				e.files[i] = entry
				e.deferScan(wake)
			}
			now := time.Now()
			b.Run("baseline_walk", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					waiting := 0
					for _, entry := range e.files {
						if now.Before(entry.NextScan) {
							waiting++
						}
					}
					benchmarkWaiting = waiting
				}
			})
			b.Run("cached_wait", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if !now.Before(e.scanIdleUntil) {
						b.Fatal("cached index unexpectedly awake")
					}
				}
			})
		})
	}
}

var benchmarkWaiting int
