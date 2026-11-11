//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func comparisonFixture(t *testing.T, action string, file int64) OfflineReport {
	t.Helper()
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if action == "enforce" {
		at = at.Add(time.Hour)
	}
	a := newReportAggregator(reportOptions{location: time.UTC, maxGap: 30 * time.Second, from: at, until: at.Add(21 * time.Second)})
	c := limitedTestConfig()
	for i := 0; i < 3; i++ {
		s := reportTestSample(at.Add(time.Duration(i)*10*time.Second), 0)
		s.InstanceID = "lab/dbsbackup"
		s.RunID = strings.Repeat("a", 32)
		s.ProtectionProfile = "limited-output"
		s.Action = action
		m := healthyLimitedMetrics(c)
		m.Time = s.Time
		m.Cgroup = "/fixture"
		m.CgroupDevice = 1
		m.CgroupInode = 2
		m.Node.Time = s.Time
		m.BusinessTime = s.Time
		m.File = file
		m.Shmem = 0
		m.Available["io_bytes"] = true
		m.IOBytes = int64(i) * 1000
		m.CPUPercent = .5
		s.Metrics = m
		s.Signals = signals(c, m, s.Time)
		if action == "observe" {
			s.Decision = Decision{State: "OBSERVE"}
		} else {
			s.Decision = Decision{State: "CONTROL", Allow: true}
			s.Counters.AdviceBytes = int64(i) * 1024
		}
		if e := a.add(s); e != nil {
			t.Fatal(e)
		}
	}
	r, e := a.finish()
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestCompareOverflowZeroBaselineAndOverlappingWindowsAreExplicit(t *testing.T) {
	base, candidate := comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
	base.Hours[0].IOBytesDelta = 0
	r, e := compareTelemetry(base, candidate, defaultCompareLimits())
	if e != nil || r.Status != "threshold_exceeded" {
		t.Fatalf("zero IO baseline: %+v %v", r, e)
	}
	v := 1e-300
	base.Hours[0].BusinessP99Peak = &v
	v2 := 1e300
	candidate.Hours[0].BusinessP99Peak = &v2
	r, e = compareTelemetry(base, candidate, defaultCompareLimits())
	if e != nil || r.Status != "threshold_exceeded" {
		t.Fatal(e, r)
	}
	if _, e = json.Marshal(r); e != nil {
		t.Fatal("nonfinite result escaped", e)
	}
	candidate.Hours[0].FileByteSeconds = 1e308
	candidate.Hours[0].FileCoveredSeconds = 1e-9
	if _, e = compareTelemetry(base, candidate, defaultCompareLimits()); e == nil {
		t.Fatal("mean overflow accepted")
	}
	base, candidate = comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
	candidate.WindowStart = base.WindowStart
	candidate.WindowEnd = base.WindowEnd
	candidate.Hours[0].Hour = base.Hours[0].Hour
	r, e = compareTelemetry(base, candidate, defaultCompareLimits())
	if e != nil || r.Status != "insufficient_evidence" {
		t.Fatal("overlap accepted", e, r)
	}
}
func TestCoverageSplitsHoursAndDoesNotInventMissingTime(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 10, 59, 50, 0, a.options.location)
	for i, seconds := range []int{0, 20, 70} {
		s := reportTestSample(at.Add(time.Duration(seconds)*time.Second), 0)
		s.RunID = strings.Repeat("b", 32)
		s.Metrics.Available["io_bytes"] = true
		s.Metrics.IOBytes = int64(i) * 200
		if e := a.add(s); e != nil {
			t.Fatal(e)
		}
	}
	r, e := a.finish()
	if e != nil {
		t.Fatal(e)
	}
	seconds, integral, io := 0.0, 0.0, int64(0)
	for _, h := range r.Hours {
		seconds += h.FileCoveredSeconds
		integral += h.FileByteSeconds
		io += h.IOBytesDelta
	}
	if seconds != 20 || integral != 20*80*float64(MiB) || io != 200 {
		t.Fatalf("seconds=%v integral=%v io=%d", seconds, integral, io)
	}
	if len(r.Hours) != 2 || r.Hours[0].FileCoveredSeconds != 10 || r.Hours[1].FileCoveredSeconds != 10 {
		t.Fatal("hour split incorrect")
	}
}
func TestCoverageUnknownAndRestartAreNotZeroValuedMeasurements(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s := reportTestSample(at.Add(time.Duration(i)*10*time.Second), 0)
		s.RunID = strings.Repeat("c", 32)
		if i == 1 {
			delete(s.Metrics.Available, "file_bytes")
		}
		if i == 2 {
			s.RunID = strings.Repeat("d", 32)
		}
		if e := a.add(s); e != nil {
			t.Fatal(e)
		}
	}
	r, _ := a.finish()
	h := r.Hours[0]
	if h.FileCoveredSeconds != 0 || h.BusinessCoveredSeconds != 0 || h.CPUCoveredSeconds != 0 {
		t.Fatal(h)
	}
}
func TestCompareTelemetryGateIsDistinctFromProductionAcceptance(t *testing.T) {
	base, candidate := comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
	r, e := compareTelemetry(base, candidate, defaultCompareLimits())
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "telemetry_thresholds_met" || len(r.Rows) != 1 || len(r.Rows[0].Checks) != 7 || !strings.Contains(r.Scope, "非完整备份") {
		t.Fatalf("%+v", r)
	}
	if math.Abs(*r.Rows[0].Candidate.FileAverage-50*float64(MiB)) > .01 {
		t.Fatal("time average wrong")
	}
}
func TestCompareRefusesGapsMissingGuardsAndMismatchedSources(t *testing.T) {
	changes := []func(*OfflineReport){func(r *OfflineReport) { r.SchemaVersion = 0 }, func(r *OfflineReport) { r.WindowEnd = r.WindowEnd.Add(time.Second) }, func(r *OfflineReport) { r.Hours[0].NodeIDs = []string{"other-node"} }, func(r *OfflineReport) { r.Hours[0].BusinessSources = []string{"other-probe"} }, func(r *OfflineReport) { r.Hours[0].FileCoveredSeconds = 1; r.Hours[0].FileByteSeconds = float64(MiB) }, func(r *OfflineReport) { r.Hours[0].MissingGuards = map[string]int{"io": 1} }, func(r *OfflineReport) { r.Hours[0].RunIDs = nil }, func(r *OfflineReport) { r.Hours[0].NodePressureSamples = 1 }, func(r *OfflineReport) { r.Hours[0].AdviceBytes = 0 }}
	for i, change := range changes {
		base, candidate := comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
		change(&candidate)
		r, e := compareTelemetry(base, candidate, defaultCompareLimits())
		if e != nil || r.Status != "insufficient_evidence" {
			t.Fatalf("case%d: %+v %v", i, r, e)
		}
	}
}
func TestCompareExposesCacheBenefitWithLatencyOrIORegression(t *testing.T) {
	for _, change := range []func(*HourReport){func(h *HourReport) { v := 200.0; h.BusinessP99Peak = &v }, func(h *HourReport) { h.IOBytesDelta = 100000 }, func(h *HourReport) { h.CPUPercentSeconds = 100; h.CPUCoveredSeconds = 20 }, func(h *HourReport) { v := int64(256 * MiB); h.SelfRSSPeak = &v }, func(h *HourReport) { h.Errors = 1 }} {
		base, candidate := comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
		change(candidate.Hours[0])
		r, e := compareTelemetry(base, candidate, defaultCompareLimits())
		if e != nil || r.Status != "threshold_exceeded" {
			t.Fatalf("%+v %v", r, e)
		}
	}
}
func TestCompareRejectsForgedNegativeDuplicateAndOverflowStatistics(t *testing.T) {
	for _, change := range []func(*OfflineReport){func(r *OfflineReport) { r.Hours = append(r.Hours, r.Hours[0]) }, func(r *OfflineReport) { r.Hours[0].FileByteSeconds = math.Inf(1) }, func(r *OfflineReport) { r.Hours[0].FileCoveredSeconds = -1 }, func(r *OfflineReport) { r.Hours[0].Actions["observe"] = 100000 }, func(r *OfflineReport) { r.Hours[0].IOCoveredSeconds = 1000 }, func(r *OfflineReport) { r.Hours[0].Hour = "2026-11-10T10:00:00Z" }} {
		base, candidate := comparisonFixture(t, "observe", 100*MiB), comparisonFixture(t, "enforce", 50*MiB)
		change(&candidate)
		if _, e := compareTelemetry(base, candidate, defaultCompareLimits()); e == nil {
			t.Fatal("bad report accepted")
		}
	}
}
