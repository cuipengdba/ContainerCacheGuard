//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexRotatesPastCapacity(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 6)
	for i := range paths {
		paths[i] = filepath.Join(root, string(rune('a'+i)))
		if err := os.WriteFile(paths[i], []byte(strings.Repeat("x", 8192)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{root}
	c.MaxFiles = 3
	e := newEngine(c, c.Paths)
	defer e.close()
	for _, path := range paths {
		e.add(path, 0)
		if _, ok := e.known[path]; !ok {
			t.Fatalf("new file %s was starved by a full index", path)
		}
		if len(e.files) > c.MaxFiles || len(e.known) > c.MaxFiles {
			t.Fatalf("working index exceeded its bound: files=%d known=%d", len(e.files), len(e.known))
		}
	}
	if e.stats.IndexRotations != 3 {
		t.Fatalf("expected three index rotations, got %d", e.stats.IndexRotations)
	}
	for _, path := range paths[:3] {
		e.add(path, 0)
		if _, ok := e.known[path]; !ok {
			t.Fatalf("file %s could not reenter index on later sweep", path)
		}
	}
}

func TestCompletedSweepIsReportedAfterTraversal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two", "three", "four"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("x", 8192)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{root}
	c.MaxFiles = 2
	c.WarmupSeconds = 0
	e := newEngine(c, c.Paths)
	defer e.close()
	var allow atomic.Bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.stats.CompletedSweeps == 0 {
		e.work(context.Background(), &allow)
		time.Sleep(10 * time.Millisecond)
	}
	if e.stats.CompletedSweeps != 1 || e.stats.IndexRotations == 0 {
		t.Fatalf("full traversal was not observable: %+v", e.stats)
	}
}

func TestFastDiscoverySkipsLinksAndEmptyFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside"), []byte(strings.Repeat("x", 8192)), 0600); err != nil {
		t.Fatal(err)
	}
	// The vendor PG14 cluster on the target uses 0700 even for relation data.
	if err := os.WriteFile(filepath.Join(root, "valid"), []byte(strings.Repeat("x", 8192)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside"), filepath.Join(root, "link-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link-dir")); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{root}
	c.Action = "observe"
	e := newEngine(c, c.Paths)
	defer e.close()
	var allow atomic.Bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.stats.CompletedSweeps == 0 {
		e.work(context.Background(), &allow)
		time.Sleep(10 * time.Millisecond)
	}
	if e.stats.CompletedSweeps != 1 {
		t.Fatalf("discovery did not complete: %+v", e.stats)
	}
	if len(e.known) != 1 || e.known[filepath.Join(root, "valid")] == nil {
		t.Fatalf("discovery indexed an empty file or link: %+v", e.known)
	}
}

func TestAutoDiscoveryStillSkipsExecutableMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "program-like")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 8192)), 0700); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Mode = "auto"
	e := newEngine(c, []string{root})
	defer e.close()
	e.add(path, 0)
	if len(e.known) != 0 {
		t.Fatal("auto mode indexed executable-mode file")
	}
}

func TestLargeSweepWaitsAfterCompletion(t *testing.T) {
	c := defaultConfig()
	c.RescanSeconds = 120
	e := newEngine(c, nil)
	e.sweepOpen = true
	e.nextSweep = time.Now().Add(-time.Hour) // Traversal exceeded its old deadline.
	now := time.Now()
	e.finishSweep(now)
	if e.stats.CompletedSweeps != 1 || e.nextSweep.Before(now.Add(120*time.Second)) {
		t.Fatalf("completed sweep restarted immediately: next=%s count=%d",
			e.nextSweep, e.stats.CompletedSweeps)
	}
}
