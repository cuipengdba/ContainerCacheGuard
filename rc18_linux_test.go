//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRC18RolesDoNotExpandOrConflictAndShareBudget(t *testing.T) {
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{"/data", "/backup"}
	c.PathPolicies = []PathPolicy{{Path: "/data", Role: "database-source"}, {Path: "/backup", Role: "backup-output", LowReuse: true}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	source, out := c.roleConfig(c.PathPolicies[0]), c.roleConfig(c.PathPolicies[1])
	if source.ProtectSeconds < 60 || source.MinFileAgeSeconds < 60 || out.ReclaimBytesPerSecond != source.ReclaimBytesPerSecond || out.RegionBytes != c.RegionBytes {
		t.Fatal("role changed budget or source protection")
	}
	c.PathPolicies[1].Path = "/outside"
	if c.validate() == nil {
		t.Fatal("expanded scope")
	}
	c.PathPolicies[1].Path = "/data/output"
	if c.validate() == nil {
		t.Fatal("conflicting nested roles")
	}
	c.PathPolicies[1] = PathPolicy{Path: "/data/wal", Role: "excluded"}
	if c.validate() != nil || !c.excluded("/data/wal/segment") {
		t.Fatal("excluded role")
	}
}

func TestRC18AliasesAreDeduplicatedAndConflictsCannotAdvise(t *testing.T) {
	w := t.TempDir()
	a, b := filepath.Join(w, "a"), filepath.Join(w, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	p, q := filepath.Join(a, "payload"), filepath.Join(b, "payload")
	if err := os.WriteFile(p, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(p, q); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{a, b}
	c.PathPolicies = []PathPolicy{{Path: a, Role: "database-source"}, {Path: b, Role: "backup-output"}}
	e := newEngine(c, c.Paths)
	defer e.close()
	e.add(p, 0)
	e.add(q, 0)
	if len(e.files) != 1 || len(e.aliasDenied) != 1 {
		t.Fatal("alias duplicated or conflict not retained")
	}
	r := e.c.roleConfig(e.c.pathPolicy(p))
	if r.ProtectSeconds < 60 {
		t.Fatal("source policy")
	}
}
func TestRC18RootReplacementRevokesAdvice(t *testing.T) {
	w := t.TempDir()
	p := filepath.Join(w, "output")
	os.Mkdir(p, 0700)
	c := defaultConfig()
	c.Mode = "paths"
	c.Paths = []string{p}
	c.PathPolicies = []PathPolicy{{Path: p, Role: "backup-output"}}
	e := newEngine(c, c.Paths)
	defer e.close()
	if err := e.bindRoleRoots(); err != nil {
		t.Fatal(err)
	}
	if !e.roleRootUnchanged(filepath.Join(p, "file")) {
		t.Fatal("unchanged root denied")
	}
	os.Rename(p, p+"-old")
	os.Mkdir(p, 0700)
	if e.roleRootUnchanged(filepath.Join(p, "file")) {
		t.Fatal("replacement granted")
	}
}
func TestRC18PriorityStillVisitsSource(t *testing.T) {
	e := newEngine(defaultConfig(), nil)
	defer e.close()
	e.c.PathPolicies = []PathPolicy{{Path: "/backup", Role: "backup-output"}}
	source := &region{Path: "/data/x", Key: regionKey{Dev: 1, Ino: 1}}
	out := &region{Path: "/backup/x", Key: regionKey{Dev: 1, Ino: 2}}
	e.queue = []*region{source, out}
	e.regions[source.Key] = source
	e.regions[out.Key] = out
	for i := 0; i < 12; i++ {
		got := e.preferredCandidate(0)
		if (i+1)%4 == 0 && got != 0 {
			t.Fatal("source starved")
		}
		if (i+1)%4 != 0 && got != 1 {
			t.Fatal("output not preferred")
		}
	}
}
func taskFixture(t *testing.T) (Config, TaskRecord) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	boot, _ := readBootID()
	start, _ := processStart(os.Getpid())
	id, _ := newRunID()
	c := defaultConfig()
	c.InstanceID = "same-container"
	c.TaskSourceID = "dbs-tasks"
	c.TaskSignalDirectory = dir
	return c, TaskRecord{Schema: 1, SourceID: c.TaskSourceID, InstanceID: c.InstanceID, BootID: boot, Time: time.Now(), ID: id, Sequence: 1, Stage: "verify", Roots: []string{"/backup"}, PID: os.Getpid(), ProcessStart: start}
}
func TestRC18TaskIdentityOverlapStaleAndOverflow(t *testing.T) {
	c, r := taskFixture(t)
	path := filepath.Join(c.TaskSignalDirectory, r.ID+".json")
	atomicJSON(path, r)
	g := taskMonitor{}
	s := g.read(c, time.Now())
	if s.State != "ready" || !taskProtects(s, "/backup/a") || taskProtects(s, "/data/a") {
		t.Fatalf("bad task scope: %+v", s)
	}
	second := r
	second.ID, _ = newRunID()
	second.Stage = "backup"
	atomicJSON(filepath.Join(c.TaskSignalDirectory, second.ID+".json"), second)
	if s = g.read(c, time.Now()); len(s.Active) != 2 || !taskProtects(s, "/backup/a") {
		t.Fatal("overlap lost verify")
	}
	r.Sequence = 2
	r.Time = time.Now().Add(-time.Minute)
	atomicJSON(path, r)
	if s = g.read(c, time.Now()); s.State != "unknown" || !taskProtects(s, "/data/a") {
		t.Fatal("stale treated idle")
	}
	r.Time = time.Now()
	r.ProcessStart = "0"
	atomicJSON(path, r)
	if g.read(c, time.Now()).State != "unknown" {
		t.Fatal("PID reuse admitted")
	}
	c.MaxActiveTasks = 1
	if g.read(c, time.Now()).State != "unknown" {
		t.Fatal("directory overflow admitted")
	}
}
func TestRC18TaskRegressionsAndUnsafeFiles(t *testing.T) {
	c, r := taskFixture(t)
	path := filepath.Join(c.TaskSignalDirectory, r.ID+".json")
	r.Sequence = 5
	atomicJSON(path, r)
	g := taskMonitor{}
	if g.read(c, time.Now()).State != "ready" {
		t.Fatal("initial task")
	}
	r.Sequence = 4
	atomicJSON(path, r)
	if g.read(c, time.Now()).State != "unknown" {
		t.Fatal("sequence regressed")
	}
	os.Remove(path)
	outside := filepath.Join(t.TempDir(), "data")
	atomicJSON(outside, r)
	os.Symlink(outside, path)
	if g.read(c, time.Now()).State != "unknown" {
		t.Fatal("symlink accepted")
	}
}

func kubeFixture(t *testing.T) ([]byte, []byte, []byte, time.Time) {
	t.Helper()
	now := time.Now()
	node := map[string]any{"metadata": map[string]any{"uid": "node-uid", "name": "node-1"}, "status": map[string]any{"nodeInfo": map[string]any{"bootID": "boot-test", "kubeletVersion": "v1.31.0", "operatingSystem": "linux"}, "conditions": []any{map[string]any{"type": "MemoryPressure", "status": "False", "lastHeartbeatTime": now}}}}
	summary := map[string]any{"node": map[string]any{"nodeName": "node-1", "memory": map[string]any{"time": now, "availableBytes": int64(4 << 30), "workingSetBytes": int64(4 << 30)}, "systemContainers": []any{map[string]any{"name": "pods", "memory": map[string]any{"time": now, "availableBytes": int64(1 << 30), "workingSetBytes": int64(3 << 30)}}}}}
	config := map[string]any{"kubeletconfig": map[string]any{"evictionHard": map[string]string{"memory.available": "10%"}, "evictionSoft": map[string]string{"memory.available": "512Mi"}, "evictionSoftGracePeriod": map[string]string{"memory.available": "1m"}, "enforceNodeAllocatable": []string{"pods"}, "cgroupsPerQOS": true}}
	a, _ := json.Marshal(node)
	b, _ := json.Marshal(summary)
	c, _ := json.Marshal(config)
	return a, b, c, now
}
func TestRC18CollectorUsesRealConstraintCapacityAndRawTime(t *testing.T) {
	a, b, c, now := kubeFixture(t)
	n, l, e := collectNodeEnvelope(a, b, c, "kubelet", "boot-test", now)
	if e != nil {
		t.Fatal(e)
	}
	nodeHard, _ := memoryQuantity("10%", 8<<30)
	allocHard, _ := memoryQuantity("10%", 4<<30)
	if len(n.Constraints) != 2 || *n.Constraints[0].Hard != nodeHard || *n.Constraints[1].Hard != allocHard || *n.Constraints[1].Soft != 0 || !n.Time.Equal(now) || !l.Time.Equal(now) || len(n.ConfigVersion) != 64 || !*n.AllocatableRequired {
		t.Fatalf("wrong constraints %+v", n)
	}
	if *l.AvailableBytes != 1<<30 {
		t.Fatal("legacy adapter hid allocatable headroom")
	}
}
func TestRC18CollectorFailsClosedForUnknownInputs(t *testing.T) {
	a, b, c, now := kubeFixture(t)
	cases := [][3][]byte{{[]byte(strings.ReplaceAll(string(a), "v1.31.0", "v1.99.0")), b, c}, {a, []byte(strings.ReplaceAll(string(b), "\"pods\"", "\"other\"")), c}, {a, b, []byte(`{"kubeletconfig":{}}`)}, {[]byte(strings.ReplaceAll(string(a), "\"False\"", "\"Unknown\"")), b, c}}
	for i, v := range cases {
		if _, _, e := collectNodeEnvelope(v[0], v[1], v[2], "source", "boot-test", now); e == nil {
			t.Fatal("accepted case", i)
		}
	}
	if _, _, e := collectNodeEnvelope(a, b, c, "source", "boot-test", now.Add(time.Minute)); e == nil {
		t.Fatal("renewed old timestamp")
	}
	if _, _, e := collectNodeEnvelope(a, b, c, "source", "other-boot", now); e == nil {
		t.Fatal("wrong host")
	}
}
func TestRC18QuantityAndDiskMetricsNeverInventColdness(t *testing.T) {
	for value, want := range map[string]int64{"1Gi": 1 << 30, "0": 0, "0.5Ki": 512, "0.1": 1, "10%": 100} {
		got, e := memoryQuantity(value, 1000)
		if e != nil || got != want {
			t.Fatalf("%s got %d %v", value, got, e)
		}
	}
	for _, value := range []string{"-1", "NaN", "101%", "999999999999999999999999999Gi"} {
		if _, e := memoryQuantity(value, 1000); e == nil {
			t.Fatal(value)
		}
	}
	before := map[string]diskCounter{"8:0": {Reads: 10, Writes: 10, ReadMS: 10, WriteMS: 10, Weighted: 100}}
	after := map[string]diskCounter{"8:0": {Reads: 20, Writes: 20, ReadMS: 30, WriteMS: 30, Weighted: 300}}
	a, q, e := diskInterval(before, after, []string{"8:0"}, 2)
	if e != nil || a != 2 || q != .1 {
		t.Fatal(a, q, e)
	}
	if _, _, e = diskInterval(after, before, []string{"8:0"}, 2); e == nil {
		t.Fatal("counter reset")
	}
	mounts := "1 0 8:0 / /data rw - ext4 /dev/sda rw\n2 1 0:1 / /data/overlay rw - overlay overlay rw"
	if dev, e := deviceForPath("/data/db", mounts); e != nil || dev != "8:0" {
		t.Fatal(dev, e)
	}
	if _, e := deviceForPath("/data/overlay/db", mounts); e == nil {
		t.Fatal("guessed overlay device")
	}
}
func TestRC18KernelFairnessDoesNotGrantMissingAck(t *testing.T) {
	now := time.Now()
	makeTarget := func(name string, at time.Time, current int64) *kernelTarget {
		return &kernelTarget{registration: kernelRegistration{Name: name, Priority: 10}, prepared: at, machine: kernelMachine{Status: KernelStatus{Phase: "PREPARING", Episode: name, Request: 128 * MiB}}, heartbeat: kernelHeartbeat{AckEpisode: name, FileIdle: true}, sample: KernelSample{Levels: []KernelLevel{{Current: current, Limit: 100}}}}
	}
	old, newer := makeTarget("old", now.Add(-time.Minute), 90), makeTarget("new", now, 99)
	if chooseKernelTarget([]*kernelTarget{newer, old}) != old {
		t.Fatal("registration order starved waiting target")
	}
	old.heartbeat.FileIdle = false
	if chooseKernelTarget([]*kernelTarget{newer, old}) != newer {
		t.Fatal("fairness bypassed ack")
	}
	old.heartbeat.FileIdle = true
	old.prepared = now
	old.sample.Levels[0].Current = 80
	if chooseKernelTarget([]*kernelTarget{old, newer}) != newer {
		t.Fatal("risk not ranked")
	}
}
func TestRC18PeriodicLogsReferenceSnapshotsWithoutMutatingEvidence(t *testing.T) {
	s := Status{KernelReclaim: &KernelStatus{Episode: "episode", Before: &KernelSample{}, Observations: []KernelObservation{{Seconds: 1}}}}
	r := periodicStatusLog(defaultConfig(), s, nil)
	if r.KernelReclaim.Before != nil || len(r.KernelReclaim.Observations) != 0 || s.KernelReclaim.Before == nil || len(s.KernelReclaim.Observations) != 1 || r.KernelReclaim.Episode != "episode" {
		t.Fatal("lost reference or mutated evidence")
	}
	c := defaultConfig()
	c.LogKernelSnapshots = true
	if periodicStatusLog(c, s, nil).KernelReclaim.Before == nil {
		t.Fatal("full snapshot opt-in")
	}
}

func TestRC18NoPSIOutputPermissionNeverLeaksToSource(t *testing.T) {
	c := limitedTestConfig()
	c.ProtectionProfile = "strict"
	c.OutputLowReuse = false
	c.Paths = []string{"/backup", "/data"}
	c.WarmupSeconds = 10
	c.RecoveryHealthySeconds = 10
	c.PathPolicies = []PathPolicy{{Path: "/backup", Role: "backup-output", LowReuse: true, LimitedOutput: true}, {Path: "/data", Role: "database-source"}}
	boot, _ := readBootID()
	m := healthyLimitedMetrics(c)
	c.KernelReclaim.SignalFile = filepath.Join(t.TempDir(), "node.json")
	c.KernelReclaim.SignalSourceID = "node-disk"
	c.KernelReclaim.DiskDevices = []string{"8:0"}
	c.KernelReclaim.MaxDiskAwaitMS = 10
	c.KernelReclaim.MaxDiskQueue = 1
	n := KernelSignal{Schema: 2, NodeID: c.NodeID, SourceID: c.KernelReclaim.SignalSourceID, BootID: boot, Time: m.Time, DiskTime: m.Time, DiskDevices: []string{"8:0"}, DiskAwaitMS: kp(0.0), DiskQueue: kp(0.0), BusinessTime: m.Time, BusinessSamples: kp(100), BusinessErrorRate: kp(0.0)}
	if e := atomicJSON(c.KernelReclaim.SignalFile, n); e != nil {
		t.Fatal(e)
	}
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	roles, d := roleDecisions(c, m, m.Time.Add(-time.Hour), m.Time, false, 0, decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0), map[string]*recoveryGate{})
	if !d.Allow || roles[0].Decision.Allow || !roles[1].Decision.Allow || roles[2].Decision.Allow {
		t.Fatalf("permission leak or output not allowed: %+v", roles)
	}
	control := &roleControl{}
	control.snapshot.Store(&pathPermissions{Time: m.Time, Roles: roles, Tasks: TaskStatus{State: "disabled"}})
	e := newEngine(c, c.Paths)
	defer e.close()
	e.roles = control
	if e.roleAllows("/data/base/1", m.Time) || !e.roleAllows("/backup/1", m.Time) || e.roleAllows("/backup/1", m.Time.Add(6*time.Second)) {
		t.Fatal("role or age boundary")
	}
	n.DiskDevices = []string{"8:1"}
	atomicJSON(c.KernelReclaim.SignalFile, n)
	_, d = roleDecisions(c, m, m.Time.Add(-time.Hour), m.Time, false, 0, decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0), map[string]*recoveryGate{})
	if d.Allow {
		t.Fatal("wrong physical disk allowed")
	}
}
