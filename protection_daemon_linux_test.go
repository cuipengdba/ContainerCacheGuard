//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

func TestDaemonMissingGuardPausesAndRecoversRealFileAdvice(t *testing.T) {
	w := t.TempDir()
	data := filepath.Join(w, "data")
	fake := filepath.Join(w, "metric-fixture")
	for _, p := range []string{data, fake} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	content := make([]byte, 8*MiB)
	file := filepath.Join(data, "payload")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"memory.current": "134217728", "memory.max": "max",
		"memory.stat":   "file 134217728\nshmem 0\nfile_dirty 0\nfile_writeback 0\nanon 0\n",
		"memory.events": "oom_kill 0", "io.stat": ""} {
		if err := os.WriteFile(filepath.Join(fake, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{data}
	c.Action = "enforce"
	c.Trigger = "always"
	c.StateFile = filepath.Join(w, "run/status.json")
	c.IntervalMS = 100
	c.RecoveryHealthySeconds = 2
	c.WarmupSeconds = 0
	c.ProtectSeconds = 0
	c.MinFileAgeSeconds = 0
	c.CooldownSeconds = 1
	c.RecheckSeconds = 1
	c.MaxCPUPercent = 100
	c.RegionBytes = 64 * 1024
	c.LogIntervalSeconds = 60
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	// Only this test constructs a reader for ordinary fixture files. Production
	// CLI rejection of a fake cgroup remains covered by the CLI integration.
	factory := func(string) (*metricReader, error) {
		f, err := safeOpen(fake)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		id, err := statFile(f)
		return &metricReader{path: fake, id: id}, err
	}
	oldProcs, oldLimit := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	defer runtime.GOMAXPROCS(oldProcs)
	defer debug.SetMemoryLimit(oldLimit)
	done := make(chan error, 1)
	go func() { done <- runDaemonWithReader(c, []string{data}, 6*time.Second, factory) }()
	defer func() {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("test daemon failed to stop")
		}
	}()
	wait := func(predicate func(Status) bool) Status {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, err := os.ReadFile(c.StateFile)
			var s Status
			if err == nil && json.Unmarshal(b, &s) == nil && predicate(s) {
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon state deadline", c.StateFile)
		return Status{}
	}
	s := wait(func(s Status) bool { return s.Protection != nil && !s.Protection.Ready })
	if s.Decision.Allow || s.Counters.AdviceBytes != 0 {
		t.Fatal("missing PSI permitted advice", s.Decision)
	}
	if err := os.WriteFile(filepath.Join(fake, "io.pressure"), []byte("some avg10=0.00 avg60=0.00 total=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s = wait(func(s Status) bool { return s.Decision.Reason == "waiting_healthy_recovery" })
	if s.Counters.AdviceBytes != 0 || s.Recovery.HealthySeconds >= 2 {
		t.Fatal("recovery wait bypassed", s.Recovery)
	}
	s = wait(func(s Status) bool { return s.Protection != nil && s.Protection.Ready && s.Counters.AdviceBytes > 0 })
	if s.Recovery.Resumes == 0 {
		t.Fatal("missing-input recovery was not recorded")
	}
	b, err := os.ReadFile(file)
	if err != nil || len(b) != len(content) {
		t.Fatal("file content changed", err)
	}
	for i := range b {
		if b[i] != content[i] {
			t.Fatal("advice modified file content")
		}
	}
}
