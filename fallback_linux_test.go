//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fallbackTestConfig(t *testing.T) Config {
	t.Helper()
	base := t.TempDir()
	makeDir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := defaultConfig()
	c.CacheFallback.Enabled = true
	c.CacheFallback.PGData = makeDir("pgdata")
	c.CacheFallback.BackupDirectories = []string{makeDir("backup")}
	c.CacheFallback.AutoDirectories = []string{makeDir("scratch")}
	return c
}

func TestFallbackDefaultsScopeAndCriticalOptIn(t *testing.T) {
	c := fallbackTestConfig(t)
	if c.CacheFallback.IntervalSeconds != 30 || c.CacheFallback.AutoAction != "report" || c.CacheFallback.AllowBoundedCritical {
		t.Fatal(c.CacheFallback)
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.CacheFallback.AllowBoundedCritical = true
	if c.validate() == nil {
		t.Fatal("critical opt-in accepted when existing mode is pause")
	}
	c.CriticalMode = "bounded"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.CacheFallback.AutoDirectories = []string{filepath.Join(c.CacheFallback.PGData, "nested")}
	if c.validate() == nil {
		t.Fatal("overlapping scopes accepted")
	}
}

func TestFallbackStartsEarlyWithoutReplacingActiveRound(t *testing.T) {
	c := fallbackTestConfig(t)
	e := newEngine(c, nil)
	defer e.close()
	if err := e.initFallback(); err != nil {
		t.Fatal(err)
	}
	f := e.fallback
	var allow atomic.Bool
	now := time.Now()
	run := func(at time.Time, reason string) {
		f.input.Store(&fallbackInput{reason: reason})
		e.fallbackWork(context.Background(), at, time.Now(), &allow)
	}
	run(now, "")
	run(now.Add(time.Second), "ancestor_headroom_warning")
	if f.current.Round != 2 || f.current.Reason != "ancestor_headroom_warning" {
		t.Fatal(f.current)
	}
	first := f.nextRound
	run(now.Add(2*time.Second), "kubelet_headroom_warning")
	if f.current.Round != 2 || f.nextRound != first {
		t.Fatal("second warning renewed active budget")
	}
	if f.c.IntervalSeconds != 30 || first.Sub(now) != 31*time.Second {
		t.Fatal(first, now)
	}
}

func TestFallbackProtectsObservedWriterAndReportOnly(t *testing.T) {
	c := fallbackTestConfig(t)
	c.ProtectSeconds, c.MinFileAgeSeconds = 0, 0
	e := newEngine(c, nil)
	defer e.close()
	if err := e.initFallback(); err != nil {
		t.Fatal(err)
	}
	f := e.fallback
	p := filepath.Join(c.CacheFallback.AutoDirectories[0], "payload")
	if err := os.WriteFile(p, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := safeOpen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	id, err := statFile(file)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e.fallbackRecord(p, "cgroup_open_fd", file, id, now, false)
	item := f.lanes[fallbackAuto].byInode[[2]uint64{id.Dev, id.Ino}]
	item.writerEpoch = 2
	f.proc.lastCleanEpoch = 1
	if e.fallbackFileSafe(p, id, file, now, true) {
		t.Fatal("writer was treated as closed")
	}
	f.proc.lastCleanEpoch = 2
	if !e.fallbackFileSafe(p, id, file, now, true) {
		t.Fatal("quiet candidate was not accepted")
	}
	f.input.Store(&fallbackInput{metrics: Metrics{Time: now}, policy: Decision{Allow: true},
		permission: pathPermissions{Time: now, Tasks: TaskStatus{State: "disabled"}}})
	f.current.At, f.current.Reason, f.nextRound = now, "file_cache_above_target", now.Add(time.Minute)
	r := &region{Path: p, Length: 65536, Fallback: fallbackAuto}
	if e.fallbackAllows(r, now) {
		t.Fatal("report mode granted advice")
	}
	f.c.AutoAction = "enforce"
	if !e.fallbackAllows(r, now) {
		t.Fatal("eligible candidate did not get permission")
	}
	f.lanes[fallbackAuto].stats.RoundRequested = f.c.RoundAdviceBytesPerScope
	if e.fallbackAllows(r, now) {
		t.Fatal("round byte cap not enforced")
	}
}
