package main

import (
	"testing"
	"time"
)

func hasDiagnostic(d Diagnostics, code string) bool {
	for _, n := range d.Notes {
		if n.Code == code {
			return true
		}
	}
	return false
}

func TestDiagnosticsKeepNonFileMemoryOutsideReclaimScope(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	m := Metrics{Time: time.Now(), Current: 1000 * MiB, Limit: 1024 * MiB,
		File: 500 * MiB, Shmem: 400 * MiB, Anon: 450 * MiB}
	d := diagnose(c, m, Decision{CacheTarget: 80 * MiB})
	if !d.Valid || d.OrdinaryFileBytes != 100*MiB || d.OtherChargedBytes != 900*MiB || d.FileTargetExcessBytes != 20*MiB {
		t.Fatalf("double-counted shmem or attributed all memory to files: %+v", d)
	}
	for _, code := range []string{"non_file_usage_majority", "file_target_exceeded", "advice_not_allowed", "not_eviction_prediction"} {
		if !hasDiagnostic(d, code) {
			t.Fatal("missing explanation", code)
		}
	}
	m.Error = "unreadable memory.stat"
	d = diagnose(c, m, Decision{})
	if d.Valid || d.OrdinaryFileBytes != 0 || hasDiagnostic(d, "non_file_usage_majority") || !hasDiagnostic(d, "metrics_unavailable") {
		t.Fatal("unavailable metrics presented as a valid diagnosis", d)
	}
}
