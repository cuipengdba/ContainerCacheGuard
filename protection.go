package main

import (
	"math"
	"time"
)

type ProtectionStatus struct {
	Profile      string   `json:"profile"`
	Experimental bool     `json:"experimental"`
	Ready        bool     `json:"ready"`
	Missing      []string `json:"missing_metrics"`
	Disabled     []string `json:"disabled_guards"`
	Unsupported  []string `json:"unsupported_guards"`
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// Configuration decides which guards are required, not whether their missing
// values happen to decode as zero. This assessment never grants permission.
func assessProtection(c Config, m Metrics) ProtectionStatus {
	s := ProtectionStatus{Profile: c.profile(), Experimental: c.profile() == "limited-output", Missing: []string{}, Disabled: []string{}, Unsupported: []string{}}
	need := func(key string, valid bool) {
		if !m.Available[key] || !valid {
			s.Missing = append(s.Missing, key)
		}
	}
	if m.Error != "" || m.Time.IsZero() {
		s.Missing = append(s.Missing, "metrics_sample")
	}
	if m.Version != 1 && m.Version != 2 {
		s.Missing = append(s.Missing, "cgroup_version")
	}
	need("current_bytes", m.Current >= 0)
	need("file_bytes", m.File >= 0)
	need("shmem_bytes", m.Shmem >= 0)
	need("dirty_bytes", m.Dirty >= 0)
	need("writeback_bytes", m.Writeback >= 0)
	need("self_rss_bytes", m.RSS >= 0)
	need("self_cpu_percent", finiteNonnegative(m.CPUPercent))
	if m.Version == 2 {
		need("oom_kill_total", m.OOMKill >= 0)
	} else {
		s.Unsupported = append(s.Unsupported, "oom_kill_total")
	}
	if c.MaxIOBytesPerSecond > 0 {
		need("io_bytes_per_second", finiteNonnegative(m.IOBytesPerSecond))
	} else {
		s.Disabled = append(s.Disabled, "io_bytes_per_second")
	}
	if c.profile() == "limited-output" && m.IOPSI == nil && m.Available["io_psi_interface_absent"] {
		s.Unsupported = append(s.Unsupported, "io_psi_avg10:limited-output-experimental")
	} else if c.MaxIOPSIAvg10 > 0 {
		need("io_psi_avg10", m.IOPSI != nil && finiteNonnegative(*m.IOPSI) && *m.IOPSI <= 100)
	} else {
		s.Disabled = append(s.Disabled, "io_psi_avg10")
	}
	if c.MaxFileRefaultsPerSecond > 0 {
		need("file_refaults_per_second", m.FileRefaultRateValid && finiteNonnegative(m.FileRefaultsPerSecond))
	} else {
		s.Disabled = append(s.Disabled, "file_refaults_per_second")
	}
	if c.MinNodeAvailableBytes > 0 {
		need("host_memavailable_bytes", m.NodeAvailable >= 0)
	} else {
		s.Disabled = append(s.Disabled, "host_memavailable_bytes")
	}
	if c.MaxBusinessP99MS > 0 {
		if c.BusinessSignal == "" || m.BusinessP99 == nil || !finiteNonnegative(*m.BusinessP99) {
			s.Missing = append(s.Missing, "business_p99_ms")
		}
	} else {
		s.Disabled = append(s.Disabled, "business_p99_ms")
	}
	if c.NodeSignal != "" && !validNodeSignal(c, m.Node, m.Time) {
		s.Missing = append(s.Missing, "kubelet_node_signal")
	}
	if c.profile() == "limited-output" {
		if m.Version != 2 {
			s.Missing = append(s.Missing, "limited_output_requires_cgroup_v2")
		}
		if m.BusinessSourceID != c.BusinessSourceID || m.BusinessTime.IsZero() || m.Time.Sub(m.BusinessTime) > time.Duration(c.SignalMaxAgeSeconds)*time.Second || m.BusinessTime.Sub(m.Time) > time.Second {
			s.Missing = append(s.Missing, "business_signal_identity")
		}
	}
	s.Ready = len(s.Missing) == 0
	return s
}
