//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func hintFixture(t *testing.T) (*engine, string) {
	t.Helper()
	root := t.TempDir()
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{root}
	e := newEngine(c, c.Paths)
	t.Cleanup(e.close)
	e.add(root, 0)
	if e.hints == nil || e.hints.fd < 0 {
		t.Skip("inotify unavailable")
	}
	return e, root
}

func TestNewDirectoryHintSurvivesExhaustedBudget(t *testing.T) {
	e, root := hintFixture(t)
	path := filepath.Join(root, "new-backup")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Even a busy ordinary walker must not discard the event. There are no
	// hint tokens until a later tick, and no ordinary sweep is run by this test.
	e.discovery.tokens = 1000
	e.hints.poll(e, now, now.Add(time.Second))
	if e.hints.pending[path] == nil {
		t.Fatal("new directory event was lost when hint tokens were exhausted")
	}
	if _, ok := e.hints.paths[path]; ok {
		t.Fatal("discovery bypassed its own budget")
	}
	e.hintDiscovery.tokens = 1
	e.hints.poll(e, now, time.Now().Add(time.Second))
	if _, ok := e.hints.paths[path]; !ok || e.hints.pending[path] != nil || e.hints.retries != 1 {
		t.Fatal("directory was not watched after budget recovered")
	}
	file := filepath.Join(path, "data")
	if err := os.WriteFile(file, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	e.hintDiscovery.tokens = 2
	for i := 0; i < 5 && e.hints.files[file] == nil; i++ {
		e.hints.poll(e, now, time.Now().Add(time.Second))
	}
	if e.hints.files[file] == nil {
		t.Fatal("file in the new backup directory was not discovered")
	}
	if e.stats.AdviceBytes != 0 {
		t.Fatal("hint discovery must not authorize eviction")
	}
}

func TestPendingFileHintsDeduplicateAndAreBounded(t *testing.T) {
	e, root := hintFixture(t)
	e.c.MaxHintFiles = 2
	now := time.Now()
	dev := e.hints.watches[e.hints.paths[root]].Dev
	for _, name := range []string{"a", "a", "b", "c"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, make([]byte, 65536), 0600); err != nil {
			t.Fatal(err)
		}
		e.hints.discoverOrDefer(e, path, dev, false, now)
	}
	if len(e.hints.pending) != 2 || len(e.hints.pendingQueue) != 2 || e.hints.dropped != 1 {
		t.Fatalf("queue did not remain bounded: pending=%d queue=%d dropped=%d",
			len(e.hints.pending), len(e.hints.pendingQueue), e.hints.dropped)
	}
	e.hintDiscovery.tokens = 1
	e.hints.retryPending(e, now, time.Now().Add(time.Second))
	if len(e.hints.pending) != 1 || e.hints.files[filepath.Join(root, "a")] == nil || e.hintDiscovery.tokens != 0 {
		t.Fatal("retry did not respect FIFO and the one-entry budget")
	}
	for _, p := range e.hints.pending {
		p.expires = now.Add(-time.Second)
	}
	e.hints.retryPending(e, now, time.Now().Add(time.Second))
	if len(e.hints.pending) != 0 || len(e.hints.pendingQueue) != 0 || e.hints.dropped != 2 {
		t.Fatal("expired hints accumulated without discovery tokens")
	}
}

func TestDeferredHintRevalidatesSymlink(t *testing.T) {
	e, root := hintFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "replaced")
	if err := os.WriteFile(path, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dev := e.hints.watches[e.hints.paths[root]].Dev
	e.hints.discoverOrDefer(e, path, dev, false, now)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	e.hintDiscovery.tokens = 1
	e.hints.retryPending(e, now, time.Now().Add(time.Second))
	if e.known[path] != nil || e.hints.files[path] != nil || len(e.hints.pending) != 0 {
		t.Fatal("deferred discovery followed a replaced symbolic link")
	}
}
