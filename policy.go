package main

import "time"

type Metrics struct {
	Time             time.Time `json:"time"`
	Cgroup           string    `json:"cgroup"`
	Version          int       `json:"version"`
	Current          int64     `json:"current_bytes"`
	Limit            int64     `json:"limit_bytes"` // 0 means unlimited/unknown, never zero capacity
	File             int64     `json:"file_bytes"`
	Shmem            int64     `json:"shmem_bytes"`
	ActiveFile       int64     `json:"active_file_bytes"`
	InactiveFile     int64     `json:"inactive_file_bytes"`
	Anon             int64     `json:"anon_bytes"`
	Slab             int64     `json:"slab_bytes"`
	Dirty            int64     `json:"dirty_bytes"`
	Writeback        int64     `json:"writeback_bytes"`
	IOBytes          int64     `json:"io_bytes"`
	IOBytesPerSecond float64   `json:"io_bytes_per_second"`
	IOPSI            *float64  `json:"io_psi_avg10"`
	MemoryPSI        *float64  `json:"memory_psi_avg10"`
	NodeAvailable    int64     `json:"host_memavailable_bytes"`
	OOMKill          int64     `json:"oom_kill_total"`
	RSS              int64     `json:"self_rss_bytes"`
	CPUSeconds       float64   `json:"self_cpu_seconds"`
	CPUPercent       float64   `json:"self_cpu_percent"`
	BusinessP99      *float64  `json:"business_p99_ms"`
	BusinessPause    bool      `json:"business_pause"`
	Error            string    `json:"error,omitempty"`
}
type Decision struct {
	State       string `json:"state"`
	Reason      string `json:"reason"`
	Allow       bool   `json:"allow"`
	CacheTarget int64  `json:"cache_target_bytes"`
}

func decide(c Config, m Metrics, started, now time.Time, paused bool, oomBaseline int64) Decision {
	d := Decision{State: "IDLE", Reason: "below_cache_target", CacheTarget: c.CacheTargetBytes}
	if d.CacheTarget == 0 && m.Limit > 0 {
		d.CacheTarget = int64(float64(m.Limit) * c.CacheTargetRatio)
	}
	stop := func(state, reason string) Decision { d.State = state; d.Reason = reason; return d }
	if paused {
		return stop("SUSPENDED", "user_paused")
	}
	if m.Error != "" {
		return stop("SUSPENDED", "metrics_unavailable: "+m.Error)
	}
	if m.RSS > c.MaxRSSBytes || m.CPUPercent > c.MaxCPUPercent {
		return stop("BACKOFF", "self_resource_budget")
	}
	if m.OOMKill > oomBaseline {
		return stop("CRITICAL", "new_oom_kill")
	}
	critical := m.Limit > 0 && float64(m.Current) >= float64(m.Limit)*c.MemoryCriticalRatio
	if critical && c.CriticalMode == "pause" {
		return stop("CRITICAL", "memory_critical_stop_speculative_reclaim")
	}
	if c.MinNodeAvailableBytes > 0 && (m.NodeAvailable <= 0 || m.NodeAvailable < c.MinNodeAvailableBytes) {
		return stop("CRITICAL", "host_memavailable_low_or_unknown")
	}
	if m.BusinessPause {
		return stop("BACKOFF", "business_pause")
	}
	if c.MaxBusinessP99MS > 0 && m.BusinessP99 != nil && *m.BusinessP99 > c.MaxBusinessP99MS {
		return stop("BACKOFF", "business_latency")
	}
	if m.Dirty+m.Writeback > c.MaxDirtyBytes {
		return stop("BACKOFF", "dirty_or_writeback")
	}
	if c.MaxIOBytesPerSecond > 0 && m.IOBytesPerSecond > float64(c.MaxIOBytesPerSecond) {
		return stop("BACKOFF", "io_bandwidth")
	}
	if m.IOPSI != nil && c.MaxIOPSIAvg10 > 0 && *m.IOPSI > c.MaxIOPSIAvg10 {
		return stop("BACKOFF", "io_psi")
	}
	if c.Action == "observe" {
		return stop("OBSERVE", "observation_only")
	}
	if now.Sub(started) < time.Duration(c.WarmupSeconds)*time.Second {
		return stop("OBSERVE", "warmup")
	}
	if c.Trigger != "always" {
		if d.CacheTarget == 0 {
			return stop("SUSPENDED", "unlimited_cgroup_requires_cache_target")
		}
		if max64(0, m.File-m.Shmem) <= d.CacheTarget {
			return d
		}
		if c.Trigger == "pressure" && (m.Limit <= 0 || float64(m.Current) < float64(m.Limit)*c.MemoryHighRatio) {
			return stop("IDLE", "waiting_memory_pressure")
		}
	}
	d.State = "CONTROL"
	d.Reason = "cache_budget_exceeded"
	if c.Trigger == "always" {
		d.Reason = "explicit_continuous_policy"
	}
	d.Allow = true
	if critical {
		d.State = "CONTROL_CRITICAL"
		d.Reason = "explicit_bounded_critical_reclaim"
	}
	return d
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
