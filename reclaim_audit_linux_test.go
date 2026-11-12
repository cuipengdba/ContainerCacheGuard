//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAutoExecutableDataExceptionStaysScoped(t *testing.T) {
	d := t.TempDir()
	pg := filepath.Join(d, "pgdata")
	os.Mkdir(pg, 0700)
	p := filepath.Join(pg, "12345")
	os.WriteFile(p, make([]byte, 8192), 0700)
	c := defaultConfig()
	c.AutoRoots = []string{d}
	c.AccessHints = false
	e := newEngine(c, []string{d})
	defer e.close()
	e.add(p, 0)
	if e.known[p] != nil || e.auditGroup(p).SkipReasons["execute_bit_auto"] != 1 {
		t.Fatal("auto default no longer protects executable files")
	}
	c.ExecutableDataPaths = []string{pg}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	e2 := newEngine(c, []string{d})
	defer e2.close()
	e2.add(p, 0)
	if e2.known[p] == nil {
		t.Fatal("explicit executable data exception ignored")
	}
	other := filepath.Join(d, "program")
	os.WriteFile(other, make([]byte, 8192), 0700)
	e2.add(other, 0)
	lib := filepath.Join(pg, "program.so")
	os.WriteFile(lib, make([]byte, 8192), 0700)
	e2.add(lib, 0)
	wal := filepath.Join(pg, "pg_wal")
	os.Mkdir(wal, 0700)
	e2.add(wal, 0)
	link := filepath.Join(pg, "link")
	os.Symlink(other, link)
	e2.add(link, 0)
	if e2.known[other] != nil || e2.known[lib] != nil || e2.known[link] != nil || e2.known[wal] != nil {
		t.Fatal("exception bypassed another scope rule")
	}
	c.ExecutableDataPaths = []string{filepath.Dir(d)}
	if c.validate() == nil {
		t.Fatal("exception added a new root")
	}
	if e2.auditGroup(link).SkipReasons["symlink_rejected"] != 1 {
		t.Fatal("symlink cause hidden")
	}
}
func TestAuditSeparatesCumulativeWindowQueriesAndUnknownSkips(t *testing.T) {
	c := defaultConfig()
	c.MaxScanHistory = 2
	c.MaxAuditGroups = 3
	e := newEngine(c, []string{"/data"})
	defer e.close()
	id := identity{Dev: 1, Ino: 2, Size: 65536, Mtime: 3, Mode: 0100600}
	e.auditQuery("/data/123", id, 0, 65536, 65536, "initial")
	// Same inode through another name is a repeated query in the bounded window.
	e.auditQuery("/data/456", id, 0, 65536, 65536, "initial")
	e.auditQuery("/data/123", id, 0, 65536, 0, "after")
	e.auditQuery("/data/123", id, 0, 65536, 0, "recheck")
	e.auditSkip("/data/123", "permission_denied", nil)
	e.auditSkip("/data/123", "execute_bit_auto", &id)
	g := e.auditGroup("/data/123")
	if g.InitialBytes != 131072 || g.UniqueBytes != 65536 || g.RepeatBytes != 65536 || g.AfterBytes != 65536 || g.RecheckBytes != 65536 || g.SkippedKnownBytes != 65536 || g.SkippedUnknownSize != 1 {
		t.Fatalf("wrong attribution %+v", g)
	}
	for i := int64(1); i <= 3; i++ {
		e.auditQuery("/data/123", id, i*65536, 65536, 0, "initial")
	}
	if len(e.audit.seen) != 2 || e.audit.snapshot.HistoryEvictions != 2 {
		t.Fatal("scan history is unbounded or hidden")
	}
	e.auditSkip("/data/a.log", "excluded_or_path_length", nil)
	e.auditSkip("/data/a.tar", "excluded_or_path_length", nil)
	if len(e.audit.groups) > 3 || e.audit.snapshot.MergedGroups == 0 {
		t.Fatal("audit groups unbounded")
	}
	s := e.auditSnapshot()
	s.CandidateOutcomes["tampered"] = 1
	s.Groups[0].SkipReasons["tampered"] = 1
	for _, g := range e.audit.groups {
		if g.SkipReasons["tampered"] != 0 {
			t.Fatal("snapshot shares a mutable map")
		}
	}
}
func TestAuditRootMetadataAndCandidateRingAreBounded(t *testing.T) {
	d := t.TempDir()
	c := defaultConfig()
	c.MaxCandidateEvents = 2
	t.Setenv("PGDATA", d)
	e := newEngine(c, []string{d})
	defer e.close()
	e.add(d, 0)
	s := e.auditSnapshot()
	if len(s.Roots) != 1 || s.Roots[0].Inode == 0 || s.Roots[0].Filesystem == "" || s.PGDATAHint != d {
		t.Fatalf("missing real root metadata %+v", s)
	}
	r := &region{CandidateID: 3, Path: filepath.Join(d, "123"), Key: regionKey{1, 2, 0}, AdvisedAt: time.Now().Add(-time.Second)}
	for _, state := range []string{"created", "advised", "completed_terminal"} {
		e.candidateEvent(r, state, 0, time.Now())
	}
	s = e.auditSnapshot()
	if len(s.Events) != 2 || s.Events[0].Sequence != 2 || s.Events[1].Outcome != "completed_terminal" || s.EventsOverwritten != 1 || s.CandidateOutcomes["created"] != 1 {
		t.Fatal("events not bounded or terminal lost", s)
	}
}
func TestExtendedRechecksKeepAdviceIdentityAndDoNotInventTimes(t *testing.T) {
	c := defaultConfig()
	c.ExtendedRecheckSeconds = []int{5, 30, 120}
	e := newEngine(c, nil)
	defer e.close()
	now := time.Now()
	r := &region{CandidateID: 7, Check: true, AdvisedAt: now, Due: now.Add(time.Second)}
	for _, delay := range []time.Duration{5, 30, 120} {
		if !e.scheduleExtendedRecheck(r, now.Add(time.Hour)) || !r.Due.Equal(now.Add(delay*time.Second)) || r.CandidateID != 7 || !r.Check {
			t.Fatal("horizon not retained honestly when late", r)
		}
	}
	if e.scheduleExtendedRecheck(r, now.Add(time.Hour)) {
		t.Fatal("unbounded rechecks")
	}
	for _, bad := range [][]int{{0}, {30, 5}, {5, 5}, {3601}} {
		c.ExtendedRecheckSeconds = bad
		if c.validate() == nil {
			t.Fatal("invalid recheck horizons accepted", bad)
		}
	}
}
func boostTestConfig() Config {
	c := limitedTestConfig()
	c.ProtectionProfile = "strict"
	c.OutputLowReuse = false
	c.PressureBoost = true
	c.CriticalMode = "bounded"
	c.WarmupSeconds = 0
	return c
}
func boostTestMetrics(c Config) Metrics {
	m := healthyLimitedMetrics(c)
	zero := 0.0
	m.IOPSI = &zero
	m.Available["io_psi_avg10"] = true
	m.Current = 90 * MiB
	m.Limit = 100 * MiB
	m.File = 2 * MiB
	return m
}
func TestPressureBoostUsesExistingBudgetAndHonorsEveryVeto(t *testing.T) {
	c := boostTestConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.CacheTargetBytes = 100 * MiB // Pressure pre-empts the soft target, not safety.
	m := boostTestMetrics(c)
	d := decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0)
	if !d.Allow || !d.PressureBoost {
		t.Fatal("healthy early pressure not accelerated", d)
	}
	for name, change := range map[string]func(*Metrics){
		"oom": func(m *Metrics) { m.OOMKill = 1 }, "dirty": func(m *Metrics) { m.Dirty = c.MaxDirtyBytes + 1 },
		"io": func(m *Metrics) { m.IOBytesPerSecond = float64(c.MaxIOBytesPerSecond + 1) }, "latency": func(m *Metrics) { v := 101.0; m.BusinessP99 = &v },
		"refault": func(m *Metrics) { m.FileRefaultsPerSecond = c.MaxFileRefaultsPerSecond + 1 }, "business_pause": func(m *Metrics) { m.BusinessPause = true },
		"node": func(m *Metrics) { *m.Node.MemoryPressure = true }, "stop_line": func(m *Metrics) { *m.Node.AvailableBytes = nodeStopThreshold(c, m.Node) },
		"missing": func(m *Metrics) { delete(m.Available, "oom_kill_total") }, "stale": func(m *Metrics) { m.Node.Time = m.Time.Add(-time.Hour) },
		"self_cpu": func(m *Metrics) { m.CPUPercent = c.MaxCPUPercent + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			n := boostTestMetrics(c)
			change(&n)
			got := decide(c, n, n.Time.Add(-time.Hour), n.Time, false, 0)
			if got.Allow || got.PressureBoost {
				t.Fatal("veto bypassed", got)
			}
		})
	}
	if decide(c, m, m.Time.Add(-time.Hour), m.Time, true, 0).Allow {
		t.Fatal("user pause bypassed")
	}
	c.PressureBoost = false
	if decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0).Allow {
		t.Fatal("disabled boost changed the soft target")
	}
}
func TestPressureBoostAncestorNodeWarningAndRecovery(t *testing.T) {
	c := boostTestConfig()
	m := boostTestMetrics(c)
	m.Current = MiB
	m.AncestorCurrent = 96 * MiB
	m.AncestorLimit = 100 * MiB
	d := decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0)
	if !d.Allow || !d.PressureBoost || d.State != "CONTROL_CRITICAL" {
		t.Fatal("bounded ancestor pressure not reused", d)
	}
	m.AncestorCurrent = 0
	*m.Node.AvailableBytes = nodeStopThreshold(c, m.Node) + 1
	if !pressureBoostWanted(c, m) {
		t.Fatal("early node warning ignored")
	}
	var gate recoveryGate
	gate.apply(c, Decision{State: "CRITICAL", Reason: "new_oom_kill"}, m.Time)
	d, rs := gate.apply(c, d, m.Time.Add(time.Second))
	if d.Allow || rs.GuardStops != 1 {
		t.Fatal("recovery bypassed")
	}
	m.OOMKill = 1
	if decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0).Reason != "new_oom_kill" {
		t.Fatal("OOM history forgotten")
	}
}
func TestPressureBoostRateFallsBackWithoutGrantingPermission(t *testing.T) {
	c := defaultConfig()
	c.AccessHints = false
	e := newEngine(c, nil)
	defer e.close()
	e.nextSweep = time.Now().Add(time.Hour)
	var allow, boost atomic.Bool
	e.boost = &boost
	allow.Store(true)
	boost.Store(true)
	e.duty.tokens = .1
	e.work(context.Background(), &allow)
	if e.rate != c.ReclaimBytesPerSecond {
		t.Fatal("boost did not respect configured maximum", e.rate)
	}
	boost.Store(false)
	e.duty.tokens = .1
	e.work(context.Background(), &allow)
	if e.rate != c.InitialReclaimBytesPerSecond {
		t.Fatal("high rate persisted after pressure")
	}
	boost.Store(true)
	allow.Store(false)
	e.duty.tokens = .1
	e.work(context.Background(), &allow)
	if e.stats.AdviceBytes != 0 || e.rate != c.InitialReclaimBytesPerSecond {
		t.Fatal("boost became permission")
	}
}
func TestMappedHotPagesAreNotProvenColdByLowDiskReads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mapped")
	os.WriteFile(p, make([]byte, MiB), 0600)
	f, err := safeOpen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, int(MiB), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(m)
	var sum byte
	for i := 0; i < len(m); i += os.Getpagesize() {
		sum ^= m[i]
	}
	if sum != 0 {
		t.Fatal("fixture corrupted")
	}
	id, _ := statFile(f)
	c := defaultConfig()
	e := newEngine(c, []string{filepath.Dir(p)})
	defer e.close()
	r := &region{CandidateID: 1, Path: p, ID: id, Key: regionKey{id.Dev, id.Ino, 0}, Length: MiB, Before: MiB, After: 0, Check: true, Due: time.Now().Add(-time.Second)}
	e.regions[r.Key] = r
	e.revisit.tokens = float64(4 * MiB)
	e.ops.tokens = 10
	var allow atomic.Bool // Recheck is permitted while new advice is denied.
	e.inspect(r, time.Now(), &allow)
	if e.stats.Protected != 1 || e.stats.ReappearedBytes != MiB || e.stats.AdviceBytes != 0 || !time.Now().Before(r.Until) {
		t.Fatal("mapped residency did not trigger protection", e.stats)
	}
	if e.audit.snapshot.CandidateOutcomes["reappearance_protected"] != 1 {
		t.Fatal("hot history not linked to candidate")
	}
}
func TestGuardTransitionContainsSubsecondTimesAndOOMHistory(t *testing.T) {
	s := Status{Time: time.Now(), RunID: strings.Repeat("a", 32), IntervalMS: 100, Metrics: Metrics{OOMKill: 4}, Decision: Decision{State: "CRITICAL", Reason: "new_oom_kill"}}
	b, err := json.Marshal(guardTransitionLog(s, nil))
	if err != nil || !strings.Contains(string(b), `"oom_kill_total":4`) || !strings.Contains(string(b), `"sampling_interval_ms":100`) || !strings.Contains(string(b), `"event":"guard_transition"`) {
		t.Fatal("guard evidence missing", string(b), err)
	}
}

func TestLargeFileRoundRobinAndAuditDirectoryOverflow(t *testing.T) {
	d := t.TempDir()
	large := filepath.Join(d, "large")
	small := filepath.Join(d, "small.log")
	os.WriteFile(large, make([]byte, 32*MiB), 0600)
	os.WriteFile(small, make([]byte, 65536), 0600)
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{d}
	c.AccessHints = false
	c.MaxDirectories = 1
	c.Action = "observe"
	c.MaxOperationsPerSecond = 2
	c.RegionBytes = 65536
	e := newEngine(c, c.Paths)
	defer e.close()
	e.add(large, 0)
	e.add(small, 0)
	e.nextSweep = time.Now().Add(time.Hour)
	e.ops.tokens = 2
	e.scan.tokens = float64(MiB)
	e.duty.tokens = .1
	var allow atomic.Bool
	deadline := time.Now().Add(2 * time.Second)
	for e.known[small].Offset == 0 && time.Now().Before(deadline) {
		e.duty.tokens = .1
		e.work(context.Background(), &allow)
		time.Sleep(5 * time.Millisecond)
	}
	if e.known[small].Offset == 0 || e.known[large].Offset > 65536 {
		t.Fatal("large file monopolized the slice", e.known[large], e.known[small])
	}
	for _, name := range []string{"one", "two"} {
		p := filepath.Join(d, name)
		os.Mkdir(p, 0700)
		e.add(p, 0)
	}
	if e.stats.Overflow != 1 || e.auditGroup(filepath.Join(d, "two")).SkipReasons["directory_limit"] != 1 {
		t.Fatal("directory overflow not explicit")
	}
}
