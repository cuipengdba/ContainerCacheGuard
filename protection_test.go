package main

import (
	"testing"
	"time"
)

func healthyProtectionMetrics() Metrics {
	zero := 0.0
	return Metrics{Time: time.Now(), Version: 2, RSS: 20 * MiB, IOPSI: &zero,
		FileRefaultRateValid: true, FileRefaultsAvailable: true,
		Available: map[string]bool{"current_bytes": true, "file_bytes": true, "shmem_bytes": true,
			"dirty_bytes": true, "writeback_bytes": true, "self_rss_bytes": true,
			"self_cpu_percent": true, "oom_kill_total": true, "io_bytes_per_second": true,
			"io_psi_avg10": true, "file_refaults_per_second": true, "host_memavailable_bytes": true}}
}

func TestEnabledGuardsRejectMissingMetrics(t *testing.T) {
	c := defaultConfig()
	c.Action, c.Trigger, c.WarmupSeconds = "enforce", "always", 0
	c.MaxIOBytesPerSecond = MiB
	c.MaxFileRefaultsPerSecond = 100
	c.MinNodeAvailableBytes = MiB
	for _, key := range []string{"current_bytes", "file_bytes", "shmem_bytes", "dirty_bytes", "writeback_bytes",
		"self_rss_bytes", "self_cpu_percent", "oom_kill_total", "io_bytes_per_second", "io_psi_avg10", "file_refaults_per_second"} {
		t.Run(key, func(t *testing.T) {
			m := healthyProtectionMetrics()
			m.NodeAvailable = 2 * MiB
			delete(m.Available, key)
			d := decide(c, m, m.Time.Add(-time.Hour), m.Time, false, 0)
			if d.Allow || d.State != "SUSPENDED" {
				t.Fatalf("enabled guard treated missing %s as safe zero: %+v", key, d)
			}
		})
	}
}
