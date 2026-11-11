//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestLimitedOutputDaemonStopsOnStaleNodeAndRecoversWithoutContentChange(t *testing.T) {
	w := t.TempDir()
	data := filepath.Join(w, "output")
	fake := filepath.Join(w, "metrics")
	for _, p := range []string{data, fake} {
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	payload := []byte(strings.Repeat("content must remain identical\n", 30000))
	file := filepath.Join(data, "backup")
	if e := os.WriteFile(file, payload, 0600); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{"memory.current": "134217728", "memory.max": "max", "memory.stat": "file 134217728\nshmem 0\nfile_dirty 0\nfile_writeback 0\nanon 0\nworkingset_refault_file 0\n", "memory.events": "oom_kill 0", "io.stat": "8:0 rbytes=100 wbytes=100\n"} {
		if e := os.WriteFile(filepath.Join(fake, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	c := limitedTestConfig()
	c.Paths = []string{data}
	c.NodeSignal = filepath.Join(w, "node.json")
	c.BusinessSignal = filepath.Join(w, "business.json")
	c.StateFile = filepath.Join(w, "state/status.json")
	c.LogDirectory = filepath.Join(w, "logs")
	// Internal test seam accelerates waits and excludes race instrumentation
	// cost. CLI production configurations must pass conservative validation.
	c.WarmupSeconds = 0
	c.RecoveryHealthySeconds = 1
	c.IntervalMS = 100
	c.ProtectSeconds = 0
	c.MinFileAgeSeconds = 0
	c.CooldownSeconds = 1
	c.RegionBytes = 65536
	c.MaxCPUPercent = 100
	c.MaxRSSBytes = 512 * MiB
	m := healthyLimitedMetrics(c)
	n := m.Node
	n.Time = time.Now()
	if e := atomicJSON(c.NodeSignal, n); e != nil {
		t.Fatal(e)
	}
	if e := atomicJSON(c.BusinessSignal, map[string]any{"time": time.Now(), "source_id": c.BusinessSourceID, "p99_ms": 1}); e != nil {
		t.Fatal(e)
	}
	factory := func(string) (*metricReader, error) {
		f, e := safeOpen(fake)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		id, e := statFile(f)
		return &metricReader{path: fake, id: id}, e
	}
	oldProcs, oldLimit := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	defer runtime.GOMAXPROCS(oldProcs)
	defer debug.SetMemoryLimit(oldLimit)
	done := make(chan error, 1)
	go func() { done <- runDaemonWithReader(c, []string{data}, 8*time.Second, factory) }()
	defer func() {
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(12 * time.Second):
			t.Error("daemon stuck")
		}
	}()
	wait := func(predicate func(Status) bool) Status {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		var last Status
		for time.Now().Before(deadline) {
			b, e := os.ReadFile(c.StateFile)
			if e == nil && json.Unmarshal(b, &last) == nil && predicate(last) {
				return last
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("state timeout: %+v", last)
		return last
	}
	s := wait(func(s Status) bool { return s.Counters.AdviceBytes > 0 })
	if s.RunID == "" || s.InstanceID != c.InstanceID || s.Metrics.IOPSI != nil {
		t.Fatal("test failed to exercise no-PSI output")
	}
	n.Time = time.Now().Add(-time.Minute)
	if e := atomicJSON(c.NodeSignal, n); e != nil {
		t.Fatal(e)
	}
	s = wait(func(s Status) bool { return s.Metrics.Error != "" && !s.Decision.Allow && !s.WorkerBusy })
	advice := s.Counters.AdviceBytes
	time.Sleep(250 * time.Millisecond)
	s = wait(func(s Status) bool { return !s.Decision.Allow })
	if s.Counters.AdviceBytes != advice {
		t.Fatal("advice continued after stale input")
	}
	n.Time = time.Now()
	if e := atomicJSON(c.NodeSignal, n); e != nil {
		t.Fatal(e)
	}
	wait(func(s Status) bool { return s.Decision.Reason == "waiting_healthy_recovery" })
	wait(func(s Status) bool { return s.Decision.Allow && s.Recovery.Resumes > 0 })
	b, e := os.ReadFile(file)
	if e != nil || string(b) != string(payload) {
		t.Fatal("file content changed", e)
	}
}
