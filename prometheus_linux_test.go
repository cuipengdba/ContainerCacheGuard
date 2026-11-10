//go:build linux && (amd64 || arm64)

package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func metricsFixture(now time.Time) Status {
	return Status{Time: now, IntervalMS: 1000, Version: "0.2.0-rc.9", Mode: "paths", Action: "observe",
		Decision: Decision{State: "OBSERVE", Reason: "observation_only"},
		Metrics: Metrics{Time: now, Version: 2, Current: 100, File: 0, Shmem: 0,
			Available: map[string]bool{"current_bytes": true, "file_bytes": true, "shmem_bytes": true}},
		Counters: Counters{AdviceBytes: 1234}}
}

func TestPrometheusSnapshotValidity(t *testing.T) {
	now := time.Now()
	s := metricsFixture(now)
	body := prometheusSnapshot(s, true, now)
	for _, expected := range []string{"ccg_monitoring_ready 1\n", "ccg_advice_allowed 0\n",
		"ccg_memory_file_bytes 0\n", "ccg_memory_ordinary_file_bytes 0\n", "ccg_file_advice_bytes_total 1234\n",
		"ccg_metric_available{metric=\"slab_bytes\"} 0\n"} {
		if !strings.Contains(body, expected) {
			t.Fatal("missing metric or zero confused with absent data", expected)
		}
	}
	for _, name := range []string{"ccg_memory_slab_bytes", "ccg_cgroup_oom_kills_total", "ccg_cgroup_file_refaults_per_second", "ccg_file_cache_target_bytes"} {
		if strings.Contains(body, "\n"+name+" ") {
			t.Fatal("unknown metric was exported as zero", name)
		}
	}
	for _, tc := range []struct {
		name    string
		present bool
		mutate  func(*Status)
	}{
		{"before_first", false, func(*Status) {}},
		{"stale", true, func(s *Status) { s.Time = now.Add(-10 * time.Second) }},
		{"metrics_stale", true, func(s *Status) { s.Metrics.Time = now.Add(-10 * time.Second) }},
		{"error", true, func(s *Status) { s.Metrics.Error = "private-marker" }},
		{"future_snapshot", true, func(s *Status) { s.Time = now.Add(2 * time.Second) }},
		{"future_metrics", true, func(s *Status) { s.Metrics.Time = now.Add(2 * time.Second) }},
		{"missing_timestamp", true, func(s *Status) { s.Metrics.Time = time.Time{} }},
		{"missing_current", true, func(s *Status) { s.Metrics.Available["current_bytes"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := metricsFixture(now)
			tc.mutate(&x)
			body := prometheusSnapshot(x, tc.present, now)
			if !strings.Contains(body, "ccg_monitoring_ready 0\n") ||
				strings.Contains(body, "\nccg_memory_file_bytes ") ||
				strings.Contains(body, "\nccg_file_advice_bytes_total ") || strings.Contains(body, "private-marker") {
				t.Fatal("invalid monitoring snapshot looked healthy or leaked error text")
			}
		})
	}
}

func TestPrometheusFormatAndLabelsStayBounded(t *testing.T) {
	now := time.Now()
	s := metricsFixture(now)
	s.Version = "private-marker\nINJECTED 1"
	s.Metrics.Cgroup, s.Counters.LastError, s.Decision.Reason = "private-marker", "private-marker", "private-marker"
	s.Decision.State = "private-marker"
	body := prometheusSnapshot(s, true, now)
	if strings.Contains(body, "private-marker") || strings.Contains(body, "INJECTED") || !strings.Contains(body, "ccg_build_info{version=\"custom\"} 1") {
		t.Fatal("unbounded/private labels escaped into exposition")
	}
	sample := regexp.MustCompile(`^(ccg_[a-z0-9_]+)(?:\{(?:[a-z_]+="[A-Za-z0-9._+-]+"(?:,[a-z_]+="[A-Za-z0-9._+-]+")*)\})? [-+0-9.eE]+$`)
	types, helps := map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		parts := strings.Fields(line)
		if strings.HasPrefix(line, "# HELP ") {
			if helps[parts[2]] {
				t.Fatal("duplicate HELP", parts[2])
			}
			helps[parts[2]] = true
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			if types[parts[2]] || (parts[3] != "counter" && parts[3] != "gauge") {
				t.Fatal("bad or duplicate metric type", line)
			}
			types[parts[2]] = true
			continue
		}
		match := sample.FindStringSubmatch(line)
		if match == nil || !types[match[1]] || !helps[match[1]] || seen[parts[0]] {
			t.Fatal("bad sample, missing metadata or duplicate labelset", line)
		}
		seen[parts[0]] = true
	}
	if len(body) > 32768 || len(seen) > 128 {
		t.Fatal("snapshot output lost its size/cardinality bound", len(body), len(seen))
	}
}

var benchmarkExposition string

func BenchmarkPrometheusSnapshot(b *testing.B) {
	now := time.Now()
	s := metricsFixture(now)
	// Availability of the common v2 fields creates a representative complete
	// snapshot; this measures encoding only, not HTTP or the whole daemon.
	for _, key := range []string{"anon_bytes", "slab_bytes", "active_file_bytes", "inactive_file_bytes", "dirty_bytes", "writeback_bytes", "oom_kill_total", "file_refaults", "file_refaults_per_second", "host_memavailable_bytes", "self_rss_bytes", "self_cpu_seconds", "self_cpu_percent", "io_bytes", "io_bytes_per_second"} {
		s.Metrics.Available[key] = true
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkExposition = prometheusSnapshot(s, true, now)
	}
}
