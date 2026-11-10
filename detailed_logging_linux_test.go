//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func detailedFixture(now time.Time) Status {
	m := healthyProtectionMetrics()
	m.Time = now
	m.File, m.Shmem = 5*MiB, MiB
	m.Available["anon_bytes"] = true
	m.Anon = 0
	return Status{Time: now, IntervalMS: 1000, PID: 1, Metrics: m,
		Decision: Decision{State: "OBSERVE", Reason: "observation_only"}}
}

func TestDetailedLogsPreserveUnknownAndExplainDecision(t *testing.T) {
	now := time.Now()
	s := detailedFixture(now)
	r := detailedStatusLog(s, nil)
	if r.Event != "status" || r.Level != "INFO" || !strings.Contains(r.Message, "仅观察") || r.Changes != nil {
		t.Fatal(r)
	}
	if r.Summary["ordinary_file_mib"] == nil || *r.Summary["ordinary_file_mib"] != 4 || r.Summary["slab_mib"] != nil || r.Summary["anon_mib"] == nil || *r.Summary["anon_mib"] != 0 {
		t.Fatal("unknown and zero confused", r.Summary)
	}
	s.Metrics.Available["shmem_bytes"] = false
	if detailedStatusLog(s, nil).Summary["ordinary_file_mib"] != nil {
		t.Fatal("ordinary cache fabricated without shmem")
	}
	s.Decision = Decision{State: "SUSPENDED", Reason: "protection_metrics_unavailable:io_psi_avg10"}
	s.Protection = &ProtectionStatus{Missing: []string{"io_psi_avg10"}}
	r = detailedStatusLog(s, nil)
	if r.Level != "WARN" || !strings.Contains(r.Message, "缺失") || r.Protection.Missing[0] != "io_psi_avg10" {
		t.Fatal(r)
	}
	for _, mutate := range []func(*Status){
		func(s *Status) { s.Metrics.Time = now.Add(-10 * time.Second) },
		func(s *Status) { s.Metrics.Time = now.Add(2 * time.Second) },
		func(s *Status) { s.Metrics.Time = time.Time{} },
		func(s *Status) { s.Metrics.Error = "read failed" },
		func(s *Status) { s.Metrics.CPUPercent = math.NaN() },
	} {
		x := detailedFixture(now)
		mutate(&x)
		r = detailedStatusLog(x, nil)
		if x.Metrics.CPUPercent != x.Metrics.CPUPercent {
			if r.Summary["self_cpu_percent"] != nil {
				t.Fatal("nonfinite summary")
			}
		} else if r.Summary["ordinary_file_mib"] != nil {
			t.Fatal("unusable sample summarized as current")
		}
	}
}

func TestDetailedLogChangesDoNotOverclaimReclaim(t *testing.T) {
	now := time.Now()
	p := detailedFixture(now)
	p.Counters = Counters{AdviceBytes: 100, NetReductionBytes: 40, ScannedBytes: 200, ReappearedBytes: 10, Errors: 2}
	s := detailedFixture(now.Add(10 * time.Second))
	s.Counters = Counters{AdviceBytes: 180, NetReductionBytes: 60, ScannedBytes: 500, ReappearedBytes: 15, Errors: 3}
	s.Metrics.File -= MiB
	x := detailedStatusLog(s, &p).Changes
	if !x.Valid || x.Seconds != 10 || x.Advice != 80 || x.Reduction != 20 || x.Scanned != 300 || x.Reappeared != 5 || x.Errors != 1 || x.OrdinaryChange == nil || *x.OrdinaryChange != -MiB {
		t.Fatal(x)
	}
	for _, mutate := range []func(*Status){
		func(s *Status) { s.Counters.Errors = 0 },
		func(s *Status) { s.Counters.AdviceBytes = 0 },
		func(s *Status) { s.Counters.NetReductionBytes = 0 },
		func(s *Status) { s.Time = p.Time },
		func(s *Status) { s.PID = 2 },
		func(s *Status) { s.Metrics.Cgroup = "changed" },
	} {
		copy := s
		mutate(&copy)
		x = detailedStatusLog(copy, &p).Changes
		if x.Valid || x.Advice != 0 || x.Errors != 0 || x.OrdinaryChange != nil {
			t.Fatal("invalid counter baseline used", x)
		}
	}
}

func TestStartupLogDoesNotLeakLegacyTokenSettings(t *testing.T) {
	c := defaultConfig()
	c.WebListen = "sensitive-listen"
	c.WebTokenFile = "sensitive-token-file"
	c.WebHistorySamples = 21600
	b, err := json.Marshal(startupLog(c, []string{"/data"}))
	if err != nil || strings.Contains(string(b), "sensitive") || !strings.Contains(string(b), `"web_service_enabled":false`) {
		t.Fatal(string(b), err)
	}
}

var detailedBenchmarkSink []byte

func BenchmarkDetailedStatusLog(b *testing.B) {
	now := time.Now()
	p := detailedFixture(now)
	s := detailedFixture(now.Add(time.Second))
	s.Protection = &ProtectionStatus{Ready: true}
	s.Logging = LoggingStatus{Enabled: true, ManagedBytes: 4096}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detailedBenchmarkSink, _ = json.Marshal(detailedStatusLog(s, &p))
	}
}
