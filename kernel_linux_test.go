//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func kp[T any](v T) *T { return &v }
func kernelFixture() (Config, KernelSample) {
	c := defaultConfig()
	c.Action = "enforce"
	c.IntervalMS = 1000
	c.WarmupSeconds = 10
	c.MaxIOBytesPerSecond = 100 * MiB
	c.MaxFileRefaultsPerSecond = 10000
	c.MaxBusinessP99MS = 100
	c.BusinessSignal = "/run/business.json"
	c.BusinessSourceID = "real-sql-api"
	c.NodeID = "node-test"
	k := &c.KernelReclaim
	k.Enabled = true
	k.Target = "hisdbs"
	k.SignalFile = "/run/kernel.json"
	k.SignalSourceID = "kubelet-real"
	m := healthyProtectionMetrics()
	m.MemoryPSI = kp(0.0)
	m.BusinessP99 = kp(5.0)
	m.BusinessTime = m.Time
	m.BusinessSourceID = c.BusinessSourceID
	m.Current = 9 << 30
	m.Limit = 10 << 30
	m.File = 4 << 30
	m.Shmem = 1 << 30
	s := KernelSample{Metrics: m, Levels: []KernelLevel{{Path: "/sys/fs/cgroup/test", Current: 9 << 30, Limit: 10 << 30}}, Signal: KernelSignal{Schema: 2, SourceID: k.SignalSourceID, NodeID: c.NodeID, BootID: "boot-test", Time: m.Time, ConfigVersion: "verified-config-1", AllocatableRequired: kp(false), MemoryPressure: kp(false), Constraints: []KernelConstraint{{Kind: "node", Time: m.Time, Available: kp(int64(8 << 30)), Hard: kp(int64(256 * MiB)), Soft: kp(int64(512 * MiB))}}, BusinessTime: m.Time, BusinessSamples: kp(100), BusinessErrorRate: kp(0.0)}}
	return c, s
}

func TestKernelConfigurationIsOptInAndBounded(t *testing.T) {
	c, s := kernelFixture()
	_ = s
	if defaultConfig().KernelReclaim.Enabled {
		t.Fatal("kernel backend enabled by default")
	}
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	cases := []func(*Config){func(c *Config) { c.KernelReclaim.Scope = "paths" }, func(c *Config) { c.KernelReclaim.MaxCallsPerEpisode = 2 }, func(c *Config) { c.KernelReclaim.RequestMaxBytes = 2 << 30 }, func(c *Config) { c.KernelReclaim.RequireSwapDisabled = false }, func(c *Config) { c.KernelReclaim.MemoryRearmRatio = .95 }, func(c *Config) { c.KernelReclaim.RecheckSeconds = []int{5, 1} }, func(c *Config) { c.MaxIOBytesPerSecond = 0 }, func(c *Config) { c.BusinessSourceID = "" }, func(c *Config) { c.KernelReclaim.GuardProfile = "no-psi-guarded" }, func(c *Config) { c.KernelReclaim.FileBudgetRatio = math.NaN() }, func(c *Config) { c.KernelReclaim.Socket = "/tmp/../bad.sock" }}
	for i, f := range cases {
		v := c
		f(&v)
		if v.validate() == nil {
			t.Fatalf("accepted unsafe config %d", i)
		}
	}
}

func TestKernelGatesNeverTreatUnknownAsHealthy(t *testing.T) {
	c, s := kernelFixture()
	if r := kernelGuard(c, s, "boot-test", s.Metrics.Time); r != "" {
		t.Fatal(r)
	}
	cases := map[string]func(*KernelSample){
		"oom": func(s *KernelSample) { s.Metrics.OOMKill = 3 }, "parent_oom": func(s *KernelSample) { s.Levels[0].OOMKill = 1 },
		"swap": func(s *KernelSample) { s.SwapMax = 1 }, "version": func(s *KernelSample) { s.Metrics.Version = 1 },
		"boot": func(s *KernelSample) { s.Signal.BootID = "replaced" }, "source": func(s *KernelSample) { s.Signal.SourceID = "spoof" },
		"node_stale":     func(s *KernelSample) { s.Signal.Constraints[0].Time = s.Metrics.Time.Add(-20 * time.Second) },
		"hard_line":      func(s *KernelSample) { s.Signal.Constraints[0].Available = kp(int64(1)) },
		"node_pressure":  func(s *KernelSample) { s.Signal.MemoryPressure = kp(true) },
		"alloc_missing":  func(s *KernelSample) { s.Signal.AllocatableRequired = kp(true) },
		"business_count": func(s *KernelSample) { s.Signal.BusinessSamples = kp(0) }, "errors": func(s *KernelSample) { s.Signal.BusinessErrorRate = kp(.01) },
		"pause": func(s *KernelSample) { s.Metrics.BusinessPause = true }, "p99": func(s *KernelSample) { s.Metrics.BusinessP99 = kp(200.0) },
		"dirty": func(s *KernelSample) { s.Metrics.Writeback = c.MaxDirtyBytes + 1 }, "io": func(s *KernelSample) { s.Metrics.IOBytesPerSecond = 1e12 },
		"refault": func(s *KernelSample) { s.Metrics.FileRefaultRateValid = false }, "psi": func(s *KernelSample) { s.Metrics.IOPSI = nil },
		"rss": func(s *KernelSample) { s.Metrics.RSS = c.MaxRSSBytes + 1 }, "cpu": func(s *KernelSample) { s.Metrics.CPUPercent = c.MaxCPUPercent + 1 },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, s := kernelFixture()
			f(&s)
			if r := kernelGuard(c, s, "boot-test", s.Metrics.Time); r == "" {
				t.Fatal("unsafe sample permitted")
			}
		})
	}
	for _, key := range []string{"current_bytes", "file_bytes", "shmem_bytes", "dirty_bytes", "writeback_bytes", "self_rss_bytes", "self_cpu_percent", "oom_kill_total", "io_bytes_per_second", "file_refaults_per_second"} {
		_, s := kernelFixture()
		delete(s.Metrics.Available, key)
		if kernelGuard(c, s, "boot-test", s.Metrics.Time) == "" {
			t.Fatal(key)
		}
	}
}

func TestKernelNoPSIRequiresConfirmedAbsenceAndFreshAlternatives(t *testing.T) {
	c, s := kernelFixture()
	k := &c.KernelReclaim
	k.GuardProfile = "no-psi-guarded"
	k.RequestMaxBytes = 128 * MiB
	k.MaxDiskAwaitMS = 20
	k.MaxDiskQueue = 2
	s.Metrics.IOPSI = nil
	s.Metrics.MemoryPSI = nil
	s.Signal.DiskTime = s.Metrics.Time
	s.Signal.DiskAwaitMS = kp(2.0)
	s.Signal.DiskQueue = kp(.1)
	if c.validate() != nil {
		t.Fatal("valid no-psi config rejected")
	}
	if kernelGuard(c, s, "boot-test", s.Metrics.Time) == "" {
		t.Fatal("unconfirmed absence")
	}
	s.Metrics.Available["io_psi_interface_absent"] = true
	s.Metrics.Available["memory_psi_interface_absent"] = true
	if r := kernelGuard(c, s, "boot-test", s.Metrics.Time); r != "" {
		t.Fatal(r)
	}
	s.Signal.DiskTime = s.Metrics.Time.Add(-10 * time.Second)
	if kernelGuard(c, s, "boot-test", s.Metrics.Time) == "" {
		t.Fatal("stale disk alternate")
	}
}

func TestKernelRiskUsesEachAncestorAndRealNodeConstraint(t *testing.T) {
	c, s := kernelFixture()
	risk, _, d := kernelRisk(c, s, KernelSample{})
	if !risk || d <= 0 {
		t.Fatal("missing cgroup risk")
	}
	s.Levels[0].Current = 1 << 30
	s.Levels = append(s.Levels, KernelLevel{Path: "parent", Current: 19 << 30, Limit: 20 << 30})
	if r, _, _ := kernelRisk(c, s, KernelSample{}); !r {
		t.Fatal("parent pressure ignored")
	}
	s.Levels = nil
	s.Signal.Constraints[0].Available = kp(int64(1 << 30))
	if r, h, _ := kernelRisk(c, s, KernelSample{}); !r || h {
		t.Fatal("node pressure ignored")
	}
	s.Signal.Constraints[0].Available = kp(int64(8 << 30))
	prev := s
	prev.Signal.Constraints = append([]KernelConstraint{}, s.Signal.Constraints...)
	prev.Signal.Constraints[0].Available = kp(int64(10 << 30))
	prev.Signal.Constraints[0].Time = s.Metrics.Time.Add(-1 * time.Second)
	if r, _, _ := kernelRisk(c, s, prev); !r {
		t.Fatal("rapid node fall did not advance trigger")
	}
}

func TestKernelRequestIsOnlyEstimateAndRespectsAllCaps(t *testing.T) {
	c, s := kernelFixture()
	r, f := kernelRequest(c, s, 2<<30, 1<<30)
	if f != 3<<30 || r != 512*MiB {
		t.Fatal(r, f)
	}
	r, _ = kernelRequest(c, s, 2<<30, 128*MiB)
	if r != 128*MiB {
		t.Fatal(r)
	}
	r, _ = kernelRequest(c, s, 1*MiB, 1<<30)
	if r != 0 {
		t.Fatal("below min")
	}
	s.Metrics.Shmem = s.Metrics.File
	if r, _ = kernelRequest(c, s, 2<<30, 1<<30); r != 0 {
		t.Fatal("shmem budget mistaken for file")
	}
}

func TestKernelControlWriteNeverRetriesPartialOrInterruptedCall(t *testing.T) {
	for _, result := range []struct {
		n   int
		err error
	}{{4, nil}, {0, syscall.EINTR}, {0, syscall.EAGAIN}, {1, nil}, {0, syscall.EPERM}} {
		count := 0
		e := kernelWriteOnce(3, []byte("1234"), func(fd int, b []byte) (int, error) { count++; return result.n, result.err })
		if count != 1 {
			t.Fatal("write retry", count)
		}
		if result.err != nil && !errors.Is(e, result.err) {
			t.Fatal(e)
		}
		if result.err == nil && result.n != 4 && !errors.Is(e, io.ErrShortWrite) {
			t.Fatal(e)
		}
	}
}

func TestKernelJournalPersistsReservationAndRestartsUnknown(t *testing.T) {
	c, s := kernelFixture()
	now := s.Metrics.Time
	id := strings.Repeat("a", 64)
	st := KernelStatus{Phase: "RESERVED", TargetIdentity: id, Attempted: true, Episode: "episode-1", Started: now, Request: 128 * MiB, Before: &s, HoldFile: true}
	j := kernelJournal{Schema: 1, Targets: map[string]KernelStatus{id: st}, Budget: []kernelBudgetEntry{{now, 128 * MiB}}}
	p := filepath.Join(t.TempDir(), "journal.json")
	if e := durableKernelJSON(p, j); e != nil {
		t.Fatal(e)
	}
	var loaded kernelJournal
	if e := trustedKernelJSON(p, &loaded); e != nil {
		t.Fatal(e)
	}
	if e := validateKernelJournal(loaded); e != nil {
		t.Fatal(e)
	}
	r := restoreKernelStatus(loaded.Targets[id])
	if r.Phase != "UNKNOWN" || !r.Attempted || !r.HoldFile {
		t.Fatal(r)
	}
	// Even sustained healthy samples must not automatically retry an unknown.
	sample := s
	sample.Levels = nil
	target := &kernelTarget{c: c, machine: kernelMachine{Status: r}}
	a := &kernelAgent{targets: []*kernelTarget{target}, readSample: func(*kernelTarget) KernelSample { return sample }, boot: "boot-test", started: now.Add(-time.Hour), lastEvent: map[string]string{}}
	c.LogDirectory = t.TempDir()
	os.Chmod(c.LogDirectory, 0700)
	c.StateFile = filepath.Join(c.LogDirectory, "status.json")
	var err error
	a.logs, err = newDailyLogger(c, io.Discard, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer a.logs.Close()
	for i := 0; i < 3; i++ {
		if e := a.tick(now.Add(time.Duration(i) * time.Second)); e != nil {
			t.Fatal(e)
		}
	}
	if !target.machine.Status.Attempted || target.machine.Status.Phase != "UNKNOWN" {
		t.Fatal("unknown episode was rearmed")
	}
	j.Budget[0].Bytes = -1
	if validateKernelJournal(j) == nil {
		t.Fatal("negative budget")
	}
}

func TestKernelNodeBudgetSerialIntervalAndClockRegression(t *testing.T) {
	now := time.Now()
	a := kernelAgent{journal: kernelJournal{Budget: []kernelBudgetEntry{{now.Add(-40 * time.Second), 512 * MiB}}}}
	if a.remaining(now) != 512*MiB {
		t.Fatal("rolling budget")
	}
	a.journal.Budget = append(a.journal.Budget, kernelBudgetEntry{now.Add(-2 * time.Second), 64 * MiB})
	if a.remaining(now) != 0 {
		t.Fatal("serial interval ignored")
	}
	if a.remaining(now.Add(-time.Hour)) != 0 {
		t.Fatal("clock regression restored budget")
	}
	if a.remaining(now.Add(6*time.Minute)) != 1<<30 {
		t.Fatal("expired request budget not restored")
	}
}

func TestKernelSignalRegressionAndTrustedInput(t *testing.T) {
	_, s := kernelFixture()
	p := s.Signal
	n := p
	n.Time = p.Time.Add(-time.Second)
	if !kernelSignalRegressed(n, p) {
		t.Fatal("time regression")
	}
	n = p
	n.ConfigVersion = "changed"
	if !kernelSignalRegressed(n, p) {
		t.Fatal("config change needs review")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "signal.json")
	os.WriteFile(path, []byte(`{}`), 0666)
	os.Chmod(path, 0666)
	var v KernelSignal
	if trustedKernelJSON(path, &v) == nil {
		t.Fatal("writable source")
	}
	os.Chmod(path, 0600)
	os.Symlink(path, filepath.Join(dir, "link"))
	if trustedKernelJSON(filepath.Join(dir, "link"), &v) == nil {
		t.Fatal("symlink source")
	}
	if kernelLeaf(dir) == nil || kernelLeaf("/sys/fs/cgroup") == nil {
		t.Fatal("fake/root cgroup accepted")
	}
	a := kernelAgent{}
	if _, e := a.authenticated(kernelMessage{peer: &syscall.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid())}, request: kernelHeartbeat{Target: "forged"}}); e == nil {
		t.Fatal("unregistered peer")
	}
}

func TestKernelSocketHandlesFragmentsAndRejectsDuplicateKeys(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages := make(chan kernelMessage, 1)
	go func() {
		for {
			c, e := l.AcceptUnix()
			if e != nil {
				return
			}
			go serveKernelConn(c, messages, ctx)
		}
	}()
	c, e := net.Dial("unix", sock)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte(`{"target":"his`))
	time.Sleep(10 * time.Millisecond)
	c.Write([]byte("dbs\",\"action\":\"enforce\"}\n"))
	select {
	case msg := <-messages:
		if msg.request.Target != "hisdbs" {
			t.Fatal(msg.request)
		}
		msg.reply <- KernelStatus{Phase: "ARMED", Time: time.Now()}
	case <-time.After(time.Second):
		t.Fatal("fragmented frame lost")
	}
	var status KernelStatus
	if json.NewDecoder(c).Decode(&status) != nil || status.Phase != "ARMED" {
		t.Fatal("missing reply")
	}
	bad, _ := net.Dial("unix", sock)
	defer bad.Close()
	bad.SetDeadline(time.Now().Add(time.Second))
	bad.Write([]byte("{\"target\":\"a\",\"target\":\"b\"}\n"))
	buf := make([]byte, 1)
	if n, _ := bad.Read(buf); n != 0 {
		t.Fatal("duplicate key accepted")
	}
}

func TestKernelDisabledHasNoClientAndOutputIsBounded(t *testing.T) {
	c := defaultConfig()
	if newKernelClient(context.Background(), c) != nil {
		t.Fatal("disabled backend connects")
	}
	var b cappedKernelOutput
	b.Write([]byte(strings.Repeat("x", 10000)))
	if b.Len() != 4096 {
		t.Fatal(b.Len())
	}
}

func TestKernelFourTargetsPrioritizeBackupsOnlyAfterWorkerAck(t *testing.T) {
	var targets []*kernelTarget
	for i, name := range []string{"dbs", "hisdbs", "dbsbackup", "hisdbsbackup"} {
		priority := 10 + i
		if i >= 2 {
			priority = i - 2
		}
		targets = append(targets, &kernelTarget{registration: kernelRegistration{Name: name, Priority: priority}, machine: kernelMachine{Status: KernelStatus{Phase: "PREPARING", Episode: name, Request: 128 * MiB}}, heartbeat: kernelHeartbeat{AckEpisode: name, FileIdle: true}})
	}
	if got := chooseKernelTarget(targets); got != targets[2] {
		t.Fatal("backup priority")
	}
	targets[2].heartbeat.FileIdle = false
	if got := chooseKernelTarget(targets); got != targets[3] {
		t.Fatal("active worker admitted")
	}
	targets[3].heartbeat.AckEpisode = "old-episode"
	if got := chooseKernelTarget(targets); got != targets[0] {
		t.Fatal("old ack admitted")
	}
}

func testKernelAgent(t *testing.T, c Config, sample *KernelSample) (*kernelAgent, *kernelTarget) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	c.LogDirectory = filepath.Join(dir, "logs")
	c.StateFile = filepath.Join(dir, "status.json")
	logs, e := newDailyLogger(c, io.Discard, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { logs.Close() })
	r := &metricReader{id: identity{Dev: 1, Ino: 2}}
	target := &kernelTarget{c: c, r: r, registration: kernelRegistration{Name: "hisdbs"}}
	a := &kernelAgent{targets: []*kernelTarget{target}, readSample: func(*kernelTarget) KernelSample { return *sample }, boot: "boot-test", started: time.Now().Add(-time.Hour), lastEvent: map[string]string{}, journal: kernelJournal{Schema: 1, Targets: map[string]KernelStatus{}}, journalPath: filepath.Join(dir, "journal.json"), logs: logs}
	target.machine.Status = KernelStatus{TargetIdentity: kernelKey(a.boot, r)}
	return a, target
}

func TestKernelRearmRequiresBothSustainedHealthAndEpisodeInterval(t *testing.T) {
	c, sample := kernelFixture()
	now := sample.Metrics.Time
	s0 := sample
	s0.Levels = nil
	sample = s0
	a, target := testKernelAgent(t, c, &sample)
	target.machine.Status = KernelStatus{Phase: "WAIT_RECOVERY", TargetIdentity: kernelKey(a.boot, target.r), Episode: "one", Attempted: true, Started: now.Add(-200 * time.Second), Finished: now.Add(-121 * time.Second), Request: 128 * MiB, Before: &s0}
	setTime := func(sec int, unhealthy bool) {
		ts := now.Add(time.Duration(sec) * time.Second)
		sample.Metrics.Time = ts
		sample.Metrics.BusinessTime = ts
		sample.Signal.Time = ts
		sample.Signal.BusinessTime = ts
		sample.Signal.Constraints[0].Time = ts
		sample.Metrics.BusinessPause = unhealthy
		if e := a.tick(ts); e != nil {
			t.Fatal(e)
		}
	}
	setTime(0, false)
	setTime(50, true)
	setTime(60, false)
	setTime(110, false)
	if !target.machine.Status.Attempted {
		t.Fatal("rearmed before sustained health")
	}
	setTime(120, false)
	if target.machine.Status.Attempted || target.machine.Status.Phase != "ARMED" || len(a.journal.Targets) != 0 {
		t.Fatal("did not rearm after both conditions")
	}
	if len(s0.Levels) != 0 {
		t.Fatal("fixture mutated")
	}
}

func TestKernelWatchdogRetainsBusyUntilActualExit(t *testing.T) {
	c, sample := kernelFixture()
	a, target := testKernelAgent(t, c, &sample)
	target.machine.Status = KernelStatus{Phase: "INFLIGHT", TargetIdentity: kernelKey(a.boot, target.r), Episode: "blocked", Attempted: true, Started: time.Now().Add(-10 * time.Second), Request: 128 * MiB, Before: &sample}
	cmd := exec.Command("sleep", "10")
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer cmd.Process.Kill()
	a.busy = target
	a.child = cmd
	begin := time.Now()
	if e := a.supervise(time.Now()); e != nil {
		t.Fatal(e)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("watchdog blocked sampler")
	}
	if a.busy != target || !target.machine.Status.Watchdog {
		t.Fatal("watchdog incorrectly cleared in-flight scope")
	}
	if e := cmd.Wait(); e == nil {
		t.Fatal("test child was not terminated")
	}
	if e := a.supervise(time.Now()); e != nil {
		t.Fatal("watchdog retried action")
	}
	if r := restoreKernelStatus(target.machine.Status); r.Phase != "UNKNOWN" || !r.Attempted {
		t.Fatal("lost uncertain attempt")
	}
}

func TestKernelHistoryBlocksFileAdviceAndSnapshotDoesNotDrift(t *testing.T) {
	c, sample := kernelFixture()
	a, target := testKernelAgent(t, c, &sample)
	sample.Metrics.OOMKill = 3
	if e := a.tick(sample.Metrics.Time); e != nil {
		t.Fatal(e)
	}
	if !target.machine.Status.HoldFile || target.machine.Status.Reason != "oom_history_unresolved" {
		t.Fatal("historical OOM cleared")
	}
	sample.Metrics.OOMKill = 0
	target.sample = sample
	target.machine.Status.Request = 128 * MiB
	target.machine.Status.Episode = "before-copy"
	target.registration.Cgroup = "/path-that-does-not-exist-ccg-test"
	if e := a.reserve(target, time.Now()); e == nil {
		t.Fatal("unexpected spawn against absent cgroup")
	}
	original := target.machine.Status.Before.Metrics.Current
	target.sample = KernelSample{}
	if target.machine.Status.Before.Metrics.Current != original {
		t.Fatal("pre-reclaim snapshot drifted")
	}
	var j kernelJournal
	if e := trustedKernelJSON(a.journalPath, &j); e != nil {
		t.Fatal(e)
	}
	if len(j.Targets) != 1 || len(j.Budget) != 1 {
		t.Fatal("reservation missing before spawn")
	}
}
