//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type snapshotHealth struct {
	Present, Fresh, Valid, Ready bool
	AgeSeconds                   float64
	Reason                       string
}

func evaluateSnapshot(s Status, present bool, now time.Time) snapshotHealth {
	h := snapshotHealth{Present: present && !s.Time.IsZero(), Reason: "waiting_first_sample"}
	if !h.Present {
		return h
	}
	oldest := s.Time
	if !s.Metrics.Time.IsZero() && s.Metrics.Time.Before(oldest) {
		oldest = s.Metrics.Time
	}
	h.AgeSeconds = now.Sub(oldest).Seconds()
	interval := s.IntervalMS
	if interval <= 0 {
		interval = 1000
	}
	window := max64(1000, int64(interval)*3)
	future := s.Time.After(now.Add(time.Second)) || s.Metrics.Time.After(now.Add(time.Second))
	h.Fresh = !future && h.AgeSeconds >= -1 && h.AgeSeconds <= float64(window)/1000
	h.Valid = s.Metrics.Error == "" && !s.Metrics.Time.IsZero() &&
		(s.Metrics.Available == nil || s.Metrics.Available["current_bytes"])
	h.Ready = h.Fresh && h.Valid
	switch {
	case s.Metrics.Error != "":
		h.Reason = "metrics_error"
	case s.Metrics.Time.IsZero():
		h.Reason = "missing_metric_timestamp"
	case s.Metrics.Available != nil && !s.Metrics.Available["current_bytes"]:
		h.Reason = "memory_current_unavailable"
	case future:
		h.Reason = "future_sample"
	case !h.Fresh:
		h.Reason = "stale_sample"
	default:
		h.Reason = "monitoring_ready"
	}
	return h
}

func boolNumber(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// Version labels are bounded build identifiers, never request data or paths.
func buildLabel(value string) string {
	if value == "" || len(value) > 64 {
		return "custom"
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._+-", r)) {
			return "custom"
		}
	}
	return value
}

// Exposition uses only the already-published snapshot; it does not sample the
// filesystem, query applications, dispatch workers, or retain scrape history.
func prometheusSnapshot(s Status, present bool, now time.Time) string {
	h := evaluateSnapshot(s, present, now)
	var out strings.Builder
	out.Grow(12288)
	metric := func(name, help, kind string, value float64) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return
		}
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", name, help, name, kind, name, strconv.FormatFloat(value, 'g', -1, 64))
	}
	metric("ccg_exporter_up", "The CCG HTTP exporter is responding; not database health.", "gauge", 1)
	metric("ccg_sample_present", "A daemon snapshot has been published.", "gauge", boolNumber(h.Present))
	metric("ccg_snapshot_fresh", "Snapshot and metric timestamps are within the freshness window.", "gauge", boolNumber(h.Fresh))
	metric("ccg_metrics_valid", "A metric sample has a timestamp and no collection error.", "gauge", boolNumber(h.Valid))
	metric("ccg_monitoring_ready", "Fresh valid monitoring data; not permission to reclaim or business health.", "gauge", boolNumber(h.Ready))
	if h.Present {
		metric("ccg_sample_age_seconds", "Age of the older snapshot/metric timestamp; may be negative for future data.", "gauge", h.AgeSeconds)
		metric("ccg_last_sample_timestamp_seconds", "Daemon snapshot Unix timestamp.", "gauge", float64(s.Time.UnixNano())/1e9)
	}
	build := s.Version
	if build == "" {
		build = version
	}
	out.WriteString("# HELP ccg_build_info CCG build version.\n# TYPE ccg_build_info gauge\n")
	fmt.Fprintf(&out, "ccg_build_info{version=%q} 1\n", buildLabel(build))
	m := s.Metrics
	ioPSI, memoryPSI := math.NaN(), math.NaN()
	if m.IOPSI != nil {
		ioPSI = *m.IOPSI
	}
	if m.MemoryPSI != nil {
		memoryPSI = *m.MemoryPSI
	}
	fields := []struct {
		key, name, help, kind string
		value                 float64
	}{
		{"current_bytes", "ccg_memory_current_bytes", "Memory charged to the monitored cgroup.", "gauge", float64(m.Current)},
		{"limit_bytes", "ccg_visible_memory_limit_bytes", "Strictest visible finite limit; not necessarily the monitored cgroup's own limit.", "gauge", float64(m.Limit)},
		{"file_bytes", "ccg_memory_file_bytes", "Cgroup file statistic including shmem.", "gauge", float64(m.File)},
		{"anon_bytes", "ccg_memory_anon_bytes", "Cgroup anonymous memory; not reclaimed by CCG.", "gauge", float64(m.Anon)},
		{"shmem_bytes", "ccg_memory_shmem_bytes", "Cgroup shared memory; not reclaimed by CCG.", "gauge", float64(m.Shmem)},
		{"slab_bytes", "ccg_memory_slab_bytes", "Cgroup slab memory; not reclaimed by CCG.", "gauge", float64(m.Slab)},
		{"active_file_bytes", "ccg_memory_active_file_bytes", "Kernel active file LRU, not exact hot-file bytes.", "gauge", float64(m.ActiveFile)},
		{"inactive_file_bytes", "ccg_memory_inactive_file_bytes", "Kernel inactive file LRU, not exact cold-file bytes.", "gauge", float64(m.InactiveFile)},
		{"dirty_bytes", "ccg_memory_dirty_bytes", "Cgroup dirty file pages.", "gauge", float64(m.Dirty)},
		{"writeback_bytes", "ccg_memory_writeback_bytes", "Cgroup file pages under writeback.", "gauge", float64(m.Writeback)},
		{"oom_kill_total", "ccg_cgroup_oom_kills_total", "Cgroup OOM kills when the kernel counter is available.", "counter", float64(m.OOMKill)},
		{"io_bytes", "ccg_cgroup_io_bytes_total", "Sum of visible io.stat read/write bytes; stacked devices may be counted twice.", "counter", float64(m.IOBytes)},
		{"io_bytes_per_second", "ccg_cgroup_io_bytes_per_second", "Sampled visible cgroup IO rate, not a physical-disk benchmark.", "gauge", m.IOBytesPerSecond},
		{"file_refaults", "ccg_cgroup_file_refaults_total", "Kernel file refault counter; not attribution to CCG.", "counter", float64(m.FileRefaults)},
		{"file_refaults_per_second", "ccg_cgroup_file_refaults_per_second", "Valid sampled kernel file-refault rate.", "gauge", m.FileRefaultsPerSecond},
		{"host_memavailable_bytes", "ccg_host_memavailable_bytes", "Visible proc MemAvailable; not kubelet memory.available.", "gauge", float64(m.NodeAvailable)},
		{"self_rss_bytes", "ccg_process_resident_memory_bytes", "CCG process resident memory.", "gauge", float64(m.RSS)},
		{"self_cpu_seconds", "ccg_process_cpu_seconds_total", "CCG process CPU seconds.", "counter", m.CPUSeconds},
		{"self_cpu_percent", "ccg_process_cpu_percent", "Sampled CCG CPU use as percent of one core.", "gauge", m.CPUPercent},
		{"io_psi_avg10", "ccg_io_psi_avg10_percent", "Visible IO PSI some avg10 percentage.", "gauge", ioPSI},
		{"memory_psi_avg10", "ccg_memory_psi_avg10_percent", "Visible memory PSI some avg10 percentage.", "gauge", memoryPSI},
	}
	out.WriteString("# HELP ccg_metric_available Metric is known in a fresh valid snapshot.\n# TYPE ccg_metric_available gauge\n")
	for _, field := range fields {
		ok := h.Ready && m.Available[field.key] && !math.IsNaN(field.value) && !math.IsInf(field.value, 0)
		fmt.Fprintf(&out, "ccg_metric_available{metric=%q} %g\n", field.key, boolNumber(ok))
	}
	for _, field := range fields {
		if h.Ready && m.Available[field.key] {
			metric(field.name, field.help, field.kind, field.value)
		}
	}
	if !h.Ready {
		return out.String()
	}
	if m.Available["file_bytes"] && m.Available["shmem_bytes"] {
		metric("ccg_memory_ordinary_file_bytes", "Approximation max(file-shmem,0); not a reclaim guarantee.", "gauge", float64(max64(0, m.File-m.Shmem)))
	}
	if m.BusinessP99 != nil {
		metric("ccg_business_p99_milliseconds", "Fresh external business p99 signal, not a CCG benchmark.", "gauge", *m.BusinessP99)
	}
	if m.AncestorLimit > 0 {
		metric("ccg_pressured_ancestor_memory_current_bytes", "Memory use of the paired visible pressured ancestor.", "gauge", float64(m.AncestorCurrent))
		metric("ccg_pressured_ancestor_memory_limit_bytes", "Finite limit paired with the pressured ancestor use.", "gauge", float64(m.AncestorLimit))
	}
	metric("ccg_advice_allowed", "Latest valid policy allows advice; does not prove effective reclaim.", "gauge", boolNumber(s.Decision.Allow))
	if s.Protection != nil {
		metric("ccg_protection_ready", "Enabled guards have valid inputs; not permission to reclaim or business health.", "gauge", boolNumber(s.Protection.Ready))
		out.WriteString("# HELP ccg_protection_metric_missing Required protection input is missing or invalid.\n# TYPE ccg_protection_metric_missing gauge\n")
		for _, key := range []string{"metrics_sample", "cgroup_version", "current_bytes", "file_bytes", "shmem_bytes", "dirty_bytes", "writeback_bytes", "self_rss_bytes", "self_cpu_percent", "oom_kill_total", "io_bytes_per_second", "io_psi_avg10", "file_refaults_per_second", "host_memavailable_bytes", "business_p99_ms"} {
			missing := false
			for _, field := range s.Protection.Missing {
				if field == key {
					missing = true
				}
			}
			fmt.Fprintf(&out, "ccg_protection_metric_missing{metric=%q} %g\n", key, boolNumber(missing))
		}
	}
	if s.Logging.Enabled {
		metric("ccg_log_managed_bytes", "Bytes of CCG-managed daily files; excludes runtime-collected stdout logs.", "gauge", float64(s.Logging.ManagedBytes))
		metric("ccg_log_files_deleted_total", "Historical daily files deleted by this daemon instance.", "counter", float64(s.Logging.DeletedFiles))
		metric("ccg_log_current_file_truncations_total", "In-place truncations when the active daily file reaches the total budget.", "counter", float64(s.Logging.Truncations))
	}
	out.WriteString("# HELP ccg_policy_state Latest sampled policy state.\n# TYPE ccg_policy_state gauge\n")
	states := []string{"OBSERVE", "IDLE", "CONTROL", "CONTROL_CRITICAL", "BACKOFF", "CRITICAL", "SUSPENDED", "STOPPED", "UNKNOWN"}
	state := "UNKNOWN"
	for _, known := range states {
		if s.Decision.State == known {
			state = known
		}
	}
	for _, known := range states {
		fmt.Fprintf(&out, "ccg_policy_state{state=%q} %g\n", known, boolNumber(state == known))
	}
	metric("ccg_paused", "CCG is manually paused.", "gauge", boolNumber(s.Paused))
	if s.Decision.CacheTarget > 0 {
		metric("ccg_file_cache_target_bytes", "Configured/derived ordinary file soft target, not a hard limit.", "gauge", float64(s.Decision.CacheTarget))
	}
	metric("ccg_recovery_pending", "A prior risk still requires sampled healthy recovery.", "gauge", boolNumber(s.Recovery.Pending))
	metric("ccg_recovery_healthy_seconds", "Elapsed continuous sampled healthy recovery time.", "gauge", s.Recovery.HealthySeconds)
	metric("ccg_recovery_required_seconds", "Configured sampled healthy recovery window.", "gauge", float64(s.Recovery.RequiredSeconds))
	recoveryEnabled := s.Action == "enforce" && s.Recovery.RequiredSeconds > 0
	metric("ccg_recovery_enabled", "The sampled healthy recovery gate is enabled for enforce mode.", "gauge", boolNumber(recoveryEnabled))
	if recoveryEnabled {
		metric("ccg_guard_stops_total", "Guard-stop stages recorded by the enabled recovery gate since CCG start.", "counter", float64(s.Recovery.GuardStops))
		metric("ccg_recovery_resumes_total", "Completed windows recorded by the enabled recovery gate since CCG start.", "counter", float64(s.Recovery.Resumes))
	}
	c := s.Counters
	metric("ccg_file_advice_bytes_total", "Cumulative kernel-accepted advice range, not bytes freed.", "counter", float64(c.AdviceBytes))
	metric("ccg_observed_net_reduction_bytes_total", "Cumulative observed per-range resident reductions; not strict causal attribution.", "counter", float64(c.NetReductionBytes))
	metric("ccg_observed_reappeared_bytes_total", "Cumulative observed reappearance of advised ranges.", "counter", float64(c.ReappearedBytes))
	metric("ccg_sampled_region_bytes_total", "Cumulative sampled file range including repeated coverage.", "counter", float64(c.ScannedBytes))
	metric("ccg_pending_hint_paths", "Bounded pending discovery paths.", "gauge", float64(c.PendingHints))
	metric("ccg_hint_queue_dropped_total", "Hints dropped because the pending queue was full or expired.", "counter", float64(c.HintDropped))
	metric("ccg_hint_discovery_retries_total", "Attempts to discover paths from the pending hint queue.", "counter", float64(c.HintRetries))
	metric("ccg_indexed_files", "Current bounded file index size.", "gauge", float64(c.Files))
	metric("ccg_tracked_regions", "Current bounded region tracking count.", "gauge", float64(c.Regions))
	metric("ccg_inventory_sweeps_completed_total", "Completed ordinary discovery sweeps since CCG start.", "counter", float64(c.CompletedSweeps))
	metric("ccg_errors_total", "Worker operation errors since CCG start.", "counter", float64(c.Errors))
	metric("ccg_file_advice_rate_bytes_per_second", "Current budgeted advice range rate, not physical IO.", "gauge", float64(c.Rate))
	metric("ccg_worker_busy_seconds", "Age of the current worker job; zero if idle.", "gauge", s.WorkerBusySeconds)
	return out.String()
}
