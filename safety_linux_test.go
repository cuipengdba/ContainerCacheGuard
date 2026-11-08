//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestConfigRejectsUnsafeAndUnknown(t *testing.T) {
	c := defaultConfig()
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"/", "/proc", "/dev/shm", "/etc", "/var/lib/containerd"} {
		x := c
		x.Mode = "paths"
		x.Paths = []string{p}
		if x.validate() == nil {
			t.Fatal("accepted", p)
		}
	}
	p := filepath.Join(t.TempDir(), "cfg")
	os.WriteFile(p, []byte(`{"unexpected":true}`), 0600)
	if _, e := loadConfig(p); e == nil {
		t.Fatal("unknown field")
	}
	os.WriteFile(p, []byte(`{} {}`), 0600)
	if _, e := loadConfig(p); e == nil {
		t.Fatal("second document")
	}
}
func TestOpenRejectsSymlinkAndFIFO(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, "real"), 0700)
	os.WriteFile(filepath.Join(d, "real", "file"), []byte("x"), 0600)
	os.Symlink(filepath.Join(d, "real"), filepath.Join(d, "link"))
	if f, e := safeOpen(filepath.Join(d, "link", "file")); e == nil {
		f.Close()
		t.Fatal("parent symlink followed")
	}
	os.Symlink(filepath.Join(d, "real", "file"), filepath.Join(d, "final"))
	if f, e := safeOpen(filepath.Join(d, "final")); e == nil {
		f.Close()
		t.Fatal("final symlink followed")
	}
	fifo := filepath.Join(d, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	f, e := safeOpen(fifo)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = resident(f, 0, 4096); e == nil {
		t.Fatal("FIFO accepted")
	}
}
func TestReclaimPreservesBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	data := bytes.Repeat([]byte("abcdefgh"), 1<<20)
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	f, e := safeOpen(p)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if !supportedFS(f) {
		t.Skip("test FS not supported")
	}
	if e = f.Sync(); e != nil {
		t.Fatal(e)
	}
	before, e := resident(f, 0, int64(len(data)))
	if e != nil {
		t.Fatal(e)
	}
	if before == 0 {
		t.Fatal("written pages unexpectedly nonresident")
	}
	if e = advise(f, 0, int64(len(data))); e != nil {
		t.Fatal(e)
	}
	after, e := resident(f, 0, int64(len(data)))
	if e != nil {
		t.Fatal(e)
	}
	if after >= before {
		t.Fatalf("no decrease: %d -> %d", before, after)
	}
	got, e := os.ReadFile(p)
	if e != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("data changed")
	}
}
func TestTruncateAndReplacedIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	os.WriteFile(p, make([]byte, 1<<20), 0600)
	f, _ := safeOpen(p)
	a, _ := statFile(f)
	os.Truncate(p, 0)
	b, _ := statFile(f)
	if sameFile(a, b) {
		t.Fatal("truncate undetected")
	}
	if r, e := resident(f, 0, 1<<20); e != nil || r != 0 {
		t.Fatal(r, e)
	}
	f.Close()
	os.Rename(p, p+".old")
	os.WriteFile(p, make([]byte, 1<<20), 0600)
	f, _ = safeOpen(p)
	defer f.Close()
	b, _ = statFile(f)
	if sameFile(a, b) {
		t.Fatal("replace undetected")
	}
}
func TestStateLockAndPermissions(t *testing.T) {
	d := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(d, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "state")
	f, e := lockState(p)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if second, e := lockState(p); e == nil {
		second.Close()
		t.Fatal("double lock")
	}
	os.Chmod(filepath.Dir(p), 0777)
	if second, e := lockState(p); e == nil {
		second.Close()
		t.Fatal("world writable state dir")
	}
}
func TestAutoRootsAndExclusion(t *testing.T) {
	d := t.TempDir()
	c := defaultConfig()
	c.AutoRoots = []string{d}
	if roots := discoverRoots(c); len(roots) == 0 {
		t.Fatal("no roots")
	}
	c.ExcludePaths = []string{d}
	for _, p := range discoverRoots(c) {
		if p == d {
			t.Fatal("excluded root")
		}
	}
	if !c.excluded(filepath.Join(d, "pg_wal", "test")) {
		t.Fatal("WAL not excluded")
	}
}
func TestBusinessFreshness(t *testing.T) {
	c := defaultConfig()
	c.BusinessSignal = filepath.Join(t.TempDir(), "signal")
	m := Metrics{}
	business(c, &m)
	if m.Error == "" {
		t.Fatal("missing signal allowed")
	}
	b, _ := json.Marshal(map[string]any{"time": time.Now().Add(-time.Minute), "p99_ms": 1})
	os.WriteFile(c.BusinessSignal, b, 0600)
	m = Metrics{}
	business(c, &m)
	if m.Error == "" {
		t.Fatal("stale signal allowed")
	}
}
func TestEngineObserveAndBoundedIndex(t *testing.T) {
	d := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		os.WriteFile(filepath.Join(d, name), make([]byte, 65536), 0600)
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{d}
	c.MaxFiles = 2
	c.MaxRegions = 2
	c.WorkerBudgetMS = 10
	eng := newEngine(c, []string{d})
	defer eng.close()
	var allow atomic.Bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && eng.stats.IndexRotations == 0 {
		eng.work(context.Background(), &allow)
		time.Sleep(10 * time.Millisecond)
	}
	if eng.stats.Files > 2 || eng.stats.Regions > 2 {
		t.Fatal(eng.stats)
	}
	if eng.stats.AdviceBytes != 0 {
		t.Fatal("observe issued advice")
	}
	if eng.stats.IndexRotations == 0 {
		t.Fatal("bounded index did not rotate to later files")
	}
}
func TestAdviceRequiresAlignedNonempty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	os.WriteFile(p, make([]byte, 65536), 0600)
	f, _ := safeOpen(p)
	defer f.Close()
	if advise(f, 1, 4096) == nil || advise(f, 0, 0) == nil {
		t.Fatal("unsafe range accepted")
	}
}
func TestTokenBucketNoBurstAccumulation(t *testing.T) {
	var b budget
	n := time.Now()
	b.refill(n, 100, 100)
	b.refill(n.Add(time.Hour), 100, 100)
	if !b.take(100) || b.take(1) {
		t.Fatal(b)
	}
}

func TestAccessHintsDoNotObserveTheirOwnScan(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "data")
	if err := os.WriteFile(p, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	e := newEngine(c, []string{d})
	defer e.close()
	e.add(d, 0)
	e.add(p, 0)
	if e.hints == nil || e.hints.fd < 0 {
		t.Skip("inotify unavailable")
	}
	f, err := safeOpen(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resident(f, 0, 65536)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	e.hints.poll(e, time.Now(), time.Now().Add(time.Second))
	if e.hints.events != 0 {
		t.Fatal("own scan triggered access hints", e.hints.events)
	}
	if _, err = os.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	e.hints.poll(e, time.Now(), time.Now().Add(-time.Second))
	if e.hints.events != 0 || e.hints.bufferEnd == 0 {
		t.Fatal("expired budget processed or lost pending events")
	}
	e.hints.poll(e, time.Now(), time.Now().Add(time.Second))
	if e.hints.files[p] == nil || e.hints.events == 0 {
		t.Fatal("real read not observed")
	}
}

func TestAccessHintsWatchNewBackupDirectory(t *testing.T) {
	root := t.TempDir()
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{root}
	e := newEngine(c, []string{root})
	defer e.close()
	e.add(root, 0)
	if e.hints == nil || e.hints.fd < 0 {
		t.Skip("inotify unavailable")
	}
	now := time.Now()
	e.discovery.refill(now, 500, 500)
	e.discovery.refill(now.Add(time.Second), 500, 500)
	newDir := filepath.Join(root, ".inprogress-new-backup")
	if err := os.Mkdir(newDir, 0700); err != nil {
		t.Fatal(err)
	}
	e.hints.poll(e, now, now.Add(time.Second))
	if _, watched := e.hints.paths[newDir]; !watched {
		t.Fatal("new output directory was not watched promptly")
	}
	file := filepath.Join(newDir, "data")
	if err := os.WriteFile(file, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4 && e.hints.files[file] == nil; i++ {
		e.hints.poll(e, now, time.Now().Add(time.Second))
	}
	if e.hints.files[file] == nil {
		t.Fatal("new backup file was not discovered by access hint")
	}
}

func TestSafeOpenWalkFallback(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "f")
	os.WriteFile(p, []byte("x"), 0600)
	f, err := safeOpenWalk(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	link := filepath.Join(d, "link")
	os.Symlink(p, link)
	if f, err := safeOpenWalk(link); err == nil {
		f.Close()
		t.Fatal("fallback followed symlink")
	}
}

func TestRecheckRetainsPartialAdviceButBoundsRetries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pending")
	if err := os.WriteFile(p, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := safeOpen(p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := statFile(f)
	if err != nil {
		t.Fatal(err)
	}
	res, err := resident(f, 0, 65536)
	f.Close()
	if err != nil || res == 0 {
		t.Fatalf("need resident fixture: %d %v", res, err)
	}
	e := newEngine(defaultConfig(), nil)
	defer e.close()
	k := regionKey{id.Dev, id.Ino, 0}
	r := &region{Key: k, Path: p, ID: id, Length: 65536, After: res, Before: res, Check: true, Attempts: 1}
	e.regions[k] = r
	e.ops.tokens = 10
	e.revisit.tokens = 1 << 20
	var allow atomic.Bool
	allow.Store(true)
	now := time.Now()
	e.inspect(r, now, &allow)
	if e.regions[k] != r || r.Check || !r.Due.After(now) || e.stats.DeferredRetries != 1 {
		t.Fatal("candidate lost before delayed invalidation", r, e.stats)
	}
	r.Check = true
	r.Attempts = 3
	r.Due = time.Time{}
	e.inspect(r, now, &allow)
	if e.regions[k] != nil {
		t.Fatal("unbounded retry")
	}
	if e.stats.AdviceBytes != 0 {
		t.Fatal("recheck itself issued advice")
	}
}
