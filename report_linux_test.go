//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reportTestSample(at time.Time, advice int64) Status {
	return Status{Time: at, PID: 12, Version: "test", Action: "enforce", IntervalMS: 1000,
		Decision: Decision{State: "CONTROL", Allow: true}, Counters: Counters{AdviceBytes: advice, NetReductionBytes: advice / 2},
		Metrics: Metrics{Time: at, Version: 2, Cgroup: "/not/a/live/cgroup", File: 100 * MiB, Shmem: 20 * MiB, Anon: 30 * MiB,
			Available: map[string]bool{"file_bytes": true, "shmem_bytes": true, "anon_bytes": true}},
	}
}
func reportTestAggregator(t *testing.T) *reportAggregator {
	t.Helper()
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return newReportAggregator(reportOptions{location: zone, maxGap: 30 * time.Second, instance: "DBS"})
}
func TestReportSplitsHoursAndDoesNotAttributeAllAdviceAsReduction(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 10, 59, 50, 0, a.options.location)
	for _, s := range []Status{reportTestSample(at, 100), reportTestSample(at.Add(20*time.Second), 300), reportTestSample(at.Add(30*time.Second), 400)} {
		if err := a.add(s); err != nil {
			t.Fatal(err)
		}
	}
	r, err := a.finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hours) != 2 {
		t.Fatal(r)
	}
	x, y := r.Hours[0], r.Hours[1]
	if x.ObservedSeconds != 10 || y.ObservedSeconds != 20 || x.AdviceBytes != 0 || y.AdviceBytes != 300 || y.NetReductionBytes != 150 {
		t.Fatalf("%+v %+v", x, y)
	}
	if x.HostAvailableMin != nil || *y.OrdinaryPeak != 80*MiB || *y.AnonPeak != 30*MiB {
		t.Fatal("unknown or memory categories corrupted")
	}
	if !strings.Contains(markdownReport(r), "未知") {
		t.Fatal("missing metrics converted to zero")
	}
}
func TestReportGapsRestartsCounterResetsAndDuplicatesAreExplicit(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s := reportTestSample(at, 100)
	a.add(s)
	s = reportTestSample(at.Add(10*time.Second), 120)
	a.add(s)
	a.add(s)
	s = reportTestSample(at.Add(60*time.Second), 200)
	a.add(s)
	s = reportTestSample(at.Add(70*time.Second), 5)
	s.PID = 13
	a.add(s)
	s = reportTestSample(at.Add(80*time.Second), 3)
	s.PID = 13
	a.add(s)
	s = reportTestSample(at.Add(90*time.Second), 13)
	s.PID = 13
	a.add(s)
	r, err := a.finish()
	if err != nil {
		t.Fatal(err)
	}
	h := r.Hours[0]
	if h.Samples != 6 || r.OutOfOrder != 1 || h.AdviceBytes != 30 || h.CounterIntervals != 2 || h.UnbridgedIntervals != 3 || h.ObservedSeconds != 30 {
		t.Fatalf("%+v %+v", r, h)
	}
}
func TestReportKeepsScopesSeparateAndBoundsAggregation(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, path := range []string{"/dbs", "/his"} {
		s := reportTestSample(at, 100)
		s.Metrics.Cgroup = path
		if err := a.add(s); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.report.Hours) != 2 || a.report.Hours[0].AdviceBytes != 0 {
		t.Fatal("mixed cgroups or invented initial delta")
	}
	for i := 0; i < reportBucketLimit; i++ {
		s := reportTestSample(at.Add(time.Duration(i+1)*time.Hour), 0)
		err := a.add(s)
		if i == reportBucketLimit-2 && err == nil {
			t.Fatal("bucket limit bypassed")
		}
		if err != nil {
			return
		}
	}
	t.Fatal("expected bounded aggregation")
}
func TestReportRejectsMalformedOversizedAndInvalidStateLogs(t *testing.T) {
	good := reportTestSample(time.Now(), 0)
	raw, _ := json.Marshal(statusLogRecord{Status: good, Event: "status"})
	for name, input := range map[string][]byte{
		"trailing":         []byte(string(raw) + "{}\n"),
		"partial":          raw[:len(raw)/2],
		"oversized":        []byte(strings.Repeat(" ", reportLineLimit+1)),
		"missing_identity": []byte("{\"event\":\"status\"}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := reportTestAggregator(t).read(bytes.NewReader(input)); err == nil {
				t.Fatal("accepted invalid report input")
			}
		})
	}
	for _, change := range []func(*Status){func(s *Status) { s.Counters.AdviceBytes = -1 }, func(s *Status) { s.Action = "observe" }, func(s *Status) { s.Version = strings.Repeat("x", 129) }} {
		s := good
		change(&s)
		if err := reportTestAggregator(t).add(s); err == nil {
			t.Fatal("invalid sample accepted")
		}
	}
}
func TestReportObserveUnknownAndRangeAreNotHealthClaims(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	a.options.from = at
	a.options.until = at.Add(20 * time.Second)
	s := reportTestSample(at.Add(-time.Second), 100)
	a.add(s)
	s = reportTestSample(at, 0)
	s.Action = "observe"
	s.Decision = Decision{State: "OBSERVE"}
	s.Metrics.Error = "sample failed"
	s.Protection = &ProtectionStatus{Missing: []string{"io_psi_avg10"}}
	a.add(s)
	s.Time = at.Add(10 * time.Second)
	s.Metrics.Time = s.Time
	a.add(s)
	s.Time = at.Add(20 * time.Second)
	a.add(s)
	r, err := a.finish()
	if err != nil {
		t.Fatal(err)
	}
	h := r.Hours[0]
	if h.Samples != 2 || h.OrdinaryPeak != nil || h.AllowedSeconds != 0 || h.MissingGuards["io_psi_avg10"] != 2 {
		t.Fatal(h)
	}
}
func TestReportCounterOverflowFailsInsteadOfWrapping(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Now()
	s := reportTestSample(at, 0)
	a.add(s)
	s = reportTestSample(at.Add(time.Second), math.MaxInt64)
	if err := a.add(s); err != nil {
		t.Fatal(err)
	}
	s = reportTestSample(at.Add(2*time.Second), 0)
	a.add(s)
	s = reportTestSample(at.Add(3*time.Second), 1)
	if err := a.add(s); err == nil {
		t.Fatal("overflow accepted")
	}
}
func TestReportLifecycleEventsResetSamePIDBaseline(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var input bytes.Buffer
	enc := json.NewEncoder(&input)
	enc.Encode(map[string]any{"event": "daemon_started", "time": at.Add(-time.Second)})
	enc.Encode(statusLogRecord{Status: reportTestSample(at, 5), Event: "status"})
	enc.Encode(statusLogRecord{Status: reportTestSample(at.Add(10*time.Second), 10), Event: "status"})
	enc.Encode(map[string]any{"event": "daemon_stopped", "time": at.Add(11 * time.Second)})
	enc.Encode(map[string]any{"event": "daemon_started", "time": at.Add(12 * time.Second)})
	// Same PID/version/path, monotonic counters: only lifecycle events can
	// distinguish the restart from uninterrupted work in these legacy fields.
	enc.Encode(statusLogRecord{Status: reportTestSample(at.Add(20*time.Second), 100), Event: "status"})
	enc.Encode(statusLogRecord{Status: reportTestSample(at.Add(30*time.Second), 110), Event: "status"})
	if err := a.read(&input); err != nil {
		t.Fatal(err)
	}
	r, err := a.finish()
	if err != nil {
		t.Fatal(err)
	}
	h := r.Hours[0]
	if h.AdviceBytes != 15 || h.CounterIntervals != 2 || h.UnbridgedIntervals != 1 || h.ObservedSeconds != 20 || r.IgnoredEvents != 3 {
		t.Fatalf("restart bridged: %+v %+v", r, h)
	}
}
func TestReportCLIIsOfflineAndRefusesUnsafeFiles(t *testing.T) {
	w := t.TempDir()
	source := filepath.Join(w, "events.jsonl")
	s := reportTestSample(time.Now(), 0)
	raw, _ := json.Marshal(statusLogRecord{Status: s, Event: "status"})
	if err := os.WriteFile(source, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(w, "report.json")
	if err := reportCommand([]string{"--log-file", source, "--format", "json", "--output", dest}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var result OfflineReport
	if err = json.Unmarshal(body, &result); err != nil || len(result.Hours) != 1 {
		t.Fatal(err)
	}
	if err = reportCommand([]string{"--log-file", source, "--output", source}); err == nil {
		t.Fatal("overwrote input")
	}
	for _, kind := range []string{"fifo", "symlink"} {
		path := filepath.Join(w, kind)
		if kind == "fifo" {
			err = syscall.Mkfifo(path, 0600)
		} else {
			err = os.Symlink(source, path)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = reportCommand([]string{"--log-file", path}); err == nil {
			t.Fatalf("accepted %s", kind)
		}
	}
	if err = reportCommand([]string{"--log-file", source, "--max-gap", "0s"}); err == nil {
		t.Fatal("unbounded gap accepted")
	}
}
