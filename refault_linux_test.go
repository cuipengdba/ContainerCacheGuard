//go:build linux && (amd64 || arm64)

package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMetricCollectionFailureInvalidatesPreviousBaseline(t *testing.T) {
	r := metricReader{path: filepath.Join(t.TempDir(), "missing"), previous: Metrics{
		Time: time.Now(), Version: 2, FileRefaultsAvailable: true, FileRefaults: 100,
	}}
	m := r.sample(defaultConfig())
	if m.Error == "" || !r.previous.Time.IsZero() {
		t.Fatal("failed collection retained a baseline across an observation gap")
	}
}

func TestRefaultRateRequiresContinuousUsableCounters(t *testing.T) {
	previous := Metrics{Time: time.Now(), Cgroup: "/example", Version: 2, FileRefaultsAvailable: true, FileRefaults: 100}
	base := previous
	base.Time = base.Time.Add(2 * time.Second)
	base.FileRefaults = 120
	for _, tc := range []struct {
		name   string
		mutate func(*Metrics, *Metrics)
		valid  bool
	}{
		{"continuous", func(*Metrics, *Metrics) {}, true},
		{"first_sample", func(m, p *Metrics) { *p = Metrics{} }, false},
		{"reset_counter", func(m, p *Metrics) { m.FileRefaults = 1 }, false},
		{"missing_field", func(m, p *Metrics) { m.FileRefaultsAvailable = false }, false},
		{"previous_missing", func(m, p *Metrics) { p.FileRefaultsAvailable = false }, false},
		{"changed_scope", func(m, p *Metrics) { m.Cgroup = "/other" }, false},
		{"failed_previous", func(m, p *Metrics) { p.Error = "read failed" }, false},
		{"same_timestamp", func(m, p *Metrics) { m.Time = p.Time }, false},
		{"cgroup_v1", func(m, p *Metrics) { m.Version = 1 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, p := base, previous
			tc.mutate(&m, &p)
			setRefaultRate(&m, p)
			if m.FileRefaultRateValid != tc.valid {
				t.Fatalf("valid=%v, want %v", m.FileRefaultRateValid, tc.valid)
			}
			if tc.valid && m.FileRefaultsPerSecond != 10 {
				t.Fatal("wrong rate", m.FileRefaultsPerSecond)
			}
		})
	}
}
