package main

import (
	"testing"
	"time"
)

func TestPolicySafety(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	c.WarmupSeconds = 0
	c.CacheTargetBytes = 100 * MiB
	now := time.Now()
	m := Metrics{Current: 500 * MiB, Limit: 1024 * MiB, File: 300 * MiB, RSS: 20 * MiB}
	tests := []struct {
		name   string
		mutate func(*Config, *Metrics)
		paused bool
		state  string
	}{
		{"budget", func(*Config, *Metrics) {}, false, "CONTROL"},
		{"observe", func(c *Config, m *Metrics) { c.Action = "observe" }, false, "OBSERVE"},
		{"pause", func(*Config, *Metrics) {}, true, "SUSPENDED"},
		{"dirty", func(c *Config, m *Metrics) { m.Dirty = c.MaxDirtyBytes + 1 }, false, "BACKOFF"},
		{"stale", func(c *Config, m *Metrics) { m.Error = "stale" }, false, "SUSPENDED"},
		{"critical", func(c *Config, m *Metrics) { m.Current = m.Limit }, false, "CRITICAL"},
		{"oom", func(c *Config, m *Metrics) { m.OOMKill = 1 }, false, "CRITICAL"},
		{"shmem_not_file", func(c *Config, m *Metrics) { m.Shmem = 250 * MiB }, false, "IDLE"},
		{"unlimited_without_budget", func(c *Config, m *Metrics) { c.CacheTargetBytes = 0; m.Limit = 0 }, false, "SUSPENDED"},
		{"cpu", func(c *Config, m *Metrics) { m.CPUPercent = 6 }, false, "BACKOFF"},
		{"rss", func(c *Config, m *Metrics) { m.RSS = c.MaxRSSBytes + 1 }, false, "BACKOFF"},
		{"io", func(c *Config, m *Metrics) { c.MaxIOBytesPerSecond = 1; m.IOBytesPerSecond = 2 }, false, "BACKOFF"},
		{"file_refault", func(c *Config, m *Metrics) { c.MaxFileRefaultsPerSecond = 1; m.FileRefaultsPerSecond = 2 }, false, "BACKOFF"},
		{"business", func(c *Config, m *Metrics) { m.BusinessPause = true }, false, "BACKOFF"},
		{"pressure_wait", func(c *Config, m *Metrics) { c.Trigger = "pressure" }, false, "IDLE"},
		{"always", func(c *Config, m *Metrics) { c.Trigger = "always"; m.File = 0 }, false, "CONTROL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, y := c, m
			tc.mutate(&x, &y)
			d := decide(x, y, now.Add(-time.Hour), now, tc.paused, 0)
			if d.State != tc.state {
				t.Fatalf("%+v", d)
			}
			if d.Allow != (tc.state == "CONTROL") {
				t.Fatal("unsafe allow", d)
			}
		})
	}
}
func TestMemoryBudgetSubtractsShmemAndUnlimited(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	now := time.Now()
	m := Metrics{Limit: 8 << 30, File: 3 << 30, Shmem: 2 << 30}
	d := decide(c, m, now.Add(-time.Hour), now, false, 0)
	if d.CacheTarget != 2<<30 || d.Allow {
		t.Fatal(d)
	}
}

func TestBoundedCriticalStillHonorsSafety(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	c.WarmupSeconds = 0
	c.CriticalMode = "bounded"
	c.CacheTargetBytes = 100 * MiB
	now := time.Now()
	m := Metrics{Current: 1024 * MiB, Limit: 1024 * MiB, File: 900 * MiB}
	d := decide(c, m, now, now, false, 0)
	if !d.Allow || d.State != "CONTROL_CRITICAL" {
		t.Fatal(d)
	}
	m.Dirty = c.MaxDirtyBytes + 1
	if decide(c, m, now, now, false, 0).Allow {
		t.Fatal("critical bypassed dirty guard")
	}
	m.Dirty = 0
	m.OOMKill = 1
	if decide(c, m, now, now, false, 0).Allow {
		t.Fatal("critical bypassed OOM guard")
	}
}

func TestSharedAncestorPressureUsesPairedUsage(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	c.Trigger = "pressure"
	c.WarmupSeconds = 0
	c.CacheTargetBytes = 100 * MiB
	now := time.Now()
	m := Metrics{
		Current: 100 * MiB, Limit: 1024 * MiB, File: 300 * MiB,
		AncestorCurrent: 900 * MiB, AncestorLimit: 1024 * MiB,
	}
	d := decide(c, m, now.Add(-time.Hour), now, false, 0)
	if d.State != "CONTROL" || !d.Allow {
		t.Fatal("sibling usage did not trigger pressure policy", d)
	}
	m.AncestorCurrent = 1000 * MiB
	d = decide(c, m, now.Add(-time.Hour), now, false, 0)
	if d.State != "CRITICAL" || d.Allow {
		t.Fatal("shared parent near limit did not stop speculative work", d)
	}
	m.AncestorCurrent = 200 * MiB
	d = decide(c, m, now.Add(-time.Hour), now, false, 0)
	if d.State != "IDLE" || d.Allow {
		t.Fatal("low self and parent usage falsely triggered pressure", d)
	}
}
