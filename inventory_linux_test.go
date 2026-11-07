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
