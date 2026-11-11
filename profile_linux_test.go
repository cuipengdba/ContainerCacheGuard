//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func limitedTestConfig() Config {
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{"/backup"}
	c.Action = "enforce"
	c.InstanceID = "lab/dbsbackup"
	c.ProtectionProfile = "limited-output"
	c.OutputLowReuse = true
	c.NodeSignal = "/signals/node.json"
	c.NodeID = "node-uid"
	c.NodeSourceID = "collector-1"
	c.NodeSafetyMarginBytes = 256 * MiB
	c.BusinessSignal = "/signals/dbs.json"
	c.BusinessSourceID = "sql-probe-dbs"
	c.MaxBusinessP99MS = 100
	c.MaxIOBytesPerSecond = 100 * MiB
	c.MaxFileRefaultsPerSecond = 1000
	c.CacheTargetBytes = MiB
	c.ScanBytesPerSecond = 64 * MiB
	c.ReclaimBytesPerSecond = 8 * MiB
	c.InitialReclaimBytesPerSecond = MiB
	c.RegionBytes = MiB
	c.WorkerBudgetMS = 10
	c.DiscoverEntriesPerSecond = 200
	c.MaxOperationsPerSecond = 64
	return c
}
func healthyLimitedMetrics(c Config) Metrics {
	m := healthyProtectionMetrics()
	m.IOPSI = nil
	m.Available["io_psi_interface_absent"] = true
	delete(m.Available, "io_psi_avg10")
	m.File = 2 * MiB
	available, hard, soft, grace := int64(2*1024*MiB), int64(100*MiB), int64(200*MiB), int64(60)
	pressure := false
	m.Node = &NodeSignal{SchemaVersion: 1, Time: m.Time, NodeID: c.NodeID, SourceID: c.NodeSourceID, AvailableBytes: &available, HardBytes: &hard, SoftBytes: &soft, SoftGraceSeconds: &grace, MemoryPressure: &pressure}
	p99 := 1.0
	m.BusinessP99 = &p99
	m.BusinessTime = m.Time
	m.BusinessSourceID = c.BusinessSourceID
	return m
}
func TestLimitedOutputRequiresExplicitConservativeConfiguration(t *testing.T) {
	c := limitedTestConfig()
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	cases := []func(*Config){func(c *Config) { c.Mode = "auto" }, func(c *Config) { c.OutputLowReuse = false }, func(c *Config) { c.InstanceID = "" }, func(c *Config) { c.BusinessSourceID = "" }, func(c *Config) { c.NodeID = "" }, func(c *Config) { c.NodeSignal = "" }, func(c *Config) { c.MaxIOPSIAvg10 = 0 }, func(c *Config) { c.MaxIOBytesPerSecond = 0 }, func(c *Config) { c.MaxFileRefaultsPerSecond = 0 }, func(c *Config) { c.RecoveryHealthySeconds = 0 }, func(c *Config) { c.ReclaimBytesPerSecond = 9 * MiB }, func(c *Config) { c.NodeSafetyMarginBytes = 1 }, func(c *Config) { c.WorkerBudgetMS = 11 }, func(c *Config) { c.MaxBusinessP99MS = math.NaN() }}
	for i, change := range cases {
		v := c
		change(&v)
		if v.validate() == nil {
			t.Fatalf("case %d accepted unsafe profile", i)
		}
	}
	old := defaultConfig()
	if old.profile() != "strict" {
		t.Fatal("default changed")
	}
	m := healthyProtectionMetrics()
	m.IOPSI = nil
	delete(m.Available, "io_psi_avg10")
	if assessProtection(old, m).Ready {
		t.Fatal("strict silently downgraded")
	}
}
func TestLimitedOutputMissingAlternativeInputsNeverAllowsAdvice(t *testing.T) {
	c := limitedTestConfig()
	m := healthyLimitedMetrics(c)
	if p := assessProtection(c, m); !p.Ready || !p.Experimental {
		t.Fatalf("%+v", p)
	}
	if d := decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0); !d.Allow {
		t.Fatalf("healthy explicit profile denied %+v", d)
	}
	for _, key := range []string{"current_bytes", "file_bytes", "shmem_bytes", "dirty_bytes", "writeback_bytes", "self_cpu_percent", "self_rss_bytes", "oom_kill_total", "io_bytes_per_second", "file_refaults_per_second", "io_psi_interface_absent"} {
		v := healthyLimitedMetrics(c)
		delete(v.Available, key)
		if decide(c, v, v.Time.Add(-time.Hour), v.Time, false, 0).Allow {
			t.Fatal("missing guard allowed", key)
		}
	}
	cases := []func(*Metrics){func(m *Metrics) { m.Version = 1 }, func(m *Metrics) { m.Node = nil }, func(m *Metrics) { m.Node.NodeID = "wrong" }, func(m *Metrics) { m.Node.SourceID = "other" }, func(m *Metrics) { m.Node.Time = m.Time.Add(-time.Minute) }, func(m *Metrics) { m.Node.Time = m.Time.Add(3 * time.Second) }, func(m *Metrics) { m.Node.MemoryPressure = nil }, func(m *Metrics) { m.BusinessSourceID = "other" }, func(m *Metrics) { m.BusinessP99 = nil }, func(m *Metrics) { m.IOBytesPerSecond = math.Inf(1) }, func(m *Metrics) { m.Node.HardBytes = nil }}
	for i, change := range cases {
		v := healthyLimitedMetrics(c)
		change(&v)
		if decide(c, v, v.Time.Add(-time.Hour), v.Time, false, 0).Allow {
			t.Fatalf("case%d unsafe advice", i)
		}
	}
}
func TestNodeEvictionPressureAndAvailablePSIStopLimitedAdvice(t *testing.T) {
	c := limitedTestConfig()
	for _, change := range []func(*Metrics){func(m *Metrics) { *m.Node.MemoryPressure = true }, func(m *Metrics) { *m.Node.AvailableBytes = nodeStopThreshold(c, m.Node) }, func(m *Metrics) { v := 80.0; m.IOPSI = &v; m.Available["io_psi_avg10"] = true }, func(m *Metrics) { m.Dirty = c.MaxDirtyBytes + 1 }, func(m *Metrics) { m.BusinessPause = true }, func(m *Metrics) { *m.BusinessP99 = 200 }, func(m *Metrics) { m.FileRefaultsPerSecond = 1001 }, func(m *Metrics) { m.OOMKill = 1 }} {
		m := healthyLimitedMetrics(c)
		change(&m)
		if decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0).Allow {
			t.Fatal("risk allowed")
		}
	}
}
func TestNodeSignalRejectsMalformedStaleUnsafeAndTimeRegression(t *testing.T) {
	c := limitedTestConfig()
	dir := t.TempDir()
	c.NodeSignal = filepath.Join(dir, "node.json")
	m := healthyLimitedMetrics(c)
	b, _ := json.Marshal(m.Node)
	if e := os.WriteFile(c.NodeSignal, b, 0600); e != nil {
		t.Fatal(e)
	}
	m.Node = nil
	nodeSignal(c, &m)
	if m.Error != "" || m.Node == nil {
		t.Fatal(m.Error)
	}
	var g externalGuard
	g.check(&m)
	m.Node.Time = m.Time.Add(-time.Second)
	g.check(&m)
	if !strings.Contains(m.Error, "regressed") {
		t.Fatal("clock rollback accepted")
	}
	for _, body := range []string{string(b) + "{}", strings.Replace(string(b), `"schema_version":1`, `"schema_version":2`, 1), strings.Repeat(" ", 16385), `{"memory_pressure":false}`, `{"x":1}`} {
		os.WriteFile(c.NodeSignal, []byte(body), 0600)
		v := healthyLimitedMetrics(c)
		v.Node = nil
		nodeSignal(c, &v)
		if v.Error == "" {
			t.Fatal("malformed accepted")
		}
	}
	fifo := filepath.Join(dir, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	c.NodeSignal = fifo
	v := healthyLimitedMetrics(c)
	nodeSignal(c, &v)
	if v.Error == "" {
		t.Fatal("FIFO accepted")
	}
}
func TestRunIdentityIsRandomAndLogIdentityIsPreserved(t *testing.T) {
	a, e := newRunID()
	if e != nil {
		t.Fatal(e)
	}
	b, e := newRunID()
	if e != nil || a == b || len(a) != 32 {
		t.Fatal("run ID collision/invalid")
	}
	s := Status{InstanceID: "test/dbs", RunID: a}
	raw, _ := json.Marshal(detailedStatusLog(s, nil))
	if !strings.Contains(string(raw), a) || !strings.Contains(string(raw), "test/dbs") {
		t.Fatal(string(raw))
	}
}

func TestRC13BackupTemplatesAreObserveConservativeAndSourceBound(t *testing.T) {
	for _, role := range []string{"dbsbackup", "hisdbsbackup"} {
		c, e := loadConfig(filepath.Join("config", "rc13-"+role+"-limited-output.json"))
		if e != nil {
			t.Fatal(e)
		}
		if e = c.validate(); e != nil {
			t.Fatal(e)
		}
		if c.Action != "observe" || c.profile() != "limited-output" || !strings.Contains(c.InstanceID, role) || c.BusinessSourceID == "" {
			t.Fatalf("%+v", c)
		}
	}
}
