//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"time"
)

// Status remains embedded at the top level for existing JSON log consumers.
// Summary and interval changes use only existing snapshots, never extra scans.
type statusLogRecord struct {
	Status
	Event   string              `json:"event"`
	Level   string              `json:"level"`
	Message string              `json:"message"`
	Summary map[string]*float64 `json:"summary"`
	Changes *logChanges         `json:"since_previous_log"`
}

type logChanges struct {
	Valid          bool    `json:"valid"`
	Reason         string  `json:"reason,omitempty"`
	Seconds        float64 `json:"seconds"`
	Scanned        int64   `json:"sampled_region_bytes"`
	Advice         int64   `json:"advice_bytes"`
	Reduction      int64   `json:"observed_net_reduction_bytes"`
	Reappeared     int64   `json:"observed_reappeared_bytes"`
	Errors         uint64  `json:"errors"`
	OrdinaryChange *int64  `json:"ordinary_file_bytes_change"`
}

func startupLog(c Config, roots []string) map[string]any {
	return map[string]any{
		"time": time.Now(), "event": "daemon_started", "level": "INFO",
		"message": "CCG已启动，Web服务已取消；通过每日日志及状态文件查看。释放建议量不等于实际回收量，普通文件页以外的内存不由CCG清理。",
		"version": version, "commit": commit, "architecture": runtime.GOARCH, "pid": os.Getpid(),
		"mode": c.Mode, "action": c.Action, "roots": roots, "exclude_paths": c.ExcludePaths,
		"executable_data_paths": c.ExecutableDataPaths, "exclude_names": c.ExcludeNames,
		"pressure_boost": c.PressureBoost, "critical_mode": c.CriticalMode,
		"extended_recheck_seconds": c.ExtendedRecheckSeconds,
		"state_file":               c.StateFile, "web_service_enabled": false,
		"budgets": map[string]any{"cache_target_bytes": c.CacheTargetBytes, "cache_target_ratio": c.CacheTargetRatio,
			"reclaim_bytes_per_second": c.ReclaimBytesPerSecond, "scan_bytes_per_second": c.ScanBytesPerSecond,
			"max_cpu_percent": c.MaxCPUPercent, "max_rss_bytes": c.MaxRSSBytes, "sample_interval_ms": c.IntervalMS,
			"log_interval_seconds": c.LogIntervalSeconds, "log_keep_days": c.LogKeepDays,
			"log_max_total_bytes": c.LogMaxTotalBytes, "log_timezone": c.LogTimezone},
	}
}

func detailedStatusLog(s Status, previous *Status) statusLogRecord {
	r := statusLogRecord{Status: s, Event: "status", Level: "INFO", Message: explainDecision(s.Decision), Summary: map[string]*float64{}}
	if s.Decision.State == "BACKOFF" || s.Decision.State == "SUSPENDED" {
		r.Level = "WARN"
	}
	if s.Decision.State == "CRITICAL" || s.Metrics.Error != "" {
		r.Level = "ERROR"
	}
	m := s.Metrics
	usable := logSampleUsable(s)
	add := func(name, key string, value, divisor float64) {
		r.Summary[name] = nil
		if usable && m.Available[key] && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			n := math.Round(value/divisor*100) / 100
			r.Summary[name] = &n
		}
	}
	for _, field := range []struct {
		name, key string
		value     int64
	}{
		{"current_mib", "current_bytes", m.Current}, {"file_including_shmem_mib", "file_bytes", m.File},
		{"anon_mib", "anon_bytes", m.Anon}, {"shmem_mib", "shmem_bytes", m.Shmem},
		{"slab_mib", "slab_bytes", m.Slab}, {"active_file_mib", "active_file_bytes", m.ActiveFile},
		{"inactive_file_mib", "inactive_file_bytes", m.InactiveFile}, {"dirty_mib", "dirty_bytes", m.Dirty},
		{"writeback_mib", "writeback_bytes", m.Writeback}, {"self_rss_mib", "self_rss_bytes", m.RSS},
		{"host_available_mib", "host_memavailable_bytes", m.NodeAvailable},
	} {
		add(field.name, field.key, float64(field.value), float64(MiB))
	}
	add("ordinary_file_mib", "file_bytes", float64(max64(0, m.File-m.Shmem)), float64(MiB))
	if !m.Available["shmem_bytes"] {
		r.Summary["ordinary_file_mib"] = nil
	}
	add("io_mib_per_second", "io_bytes_per_second", m.IOBytesPerSecond, float64(MiB))
	add("self_cpu_percent", "self_cpu_percent", m.CPUPercent, 1)
	if previous != nil {
		x := &logChanges{Seconds: s.Time.Sub(previous.Time).Seconds(), Reason: "baseline_unavailable_or_counter_reset"}
		a, b := s.Counters, previous.Counters
		if x.Seconds > 0 && s.PID == previous.PID && s.RunID == previous.RunID && reportScope(s) == reportScope(*previous) && m.Version == previous.Metrics.Version &&
			a.ScannedBytes >= b.ScannedBytes && a.AdviceBytes >= b.AdviceBytes && a.NetReductionBytes >= b.NetReductionBytes &&
			a.ReappearedBytes >= b.ReappearedBytes && a.Errors >= b.Errors {
			x.Valid, x.Reason = true, ""
			x.Scanned, x.Advice, x.Reduction = a.ScannedBytes-b.ScannedBytes, a.AdviceBytes-b.AdviceBytes, a.NetReductionBytes-b.NetReductionBytes
			x.Reappeared, x.Errors = a.ReappearedBytes-b.ReappearedBytes, a.Errors-b.Errors
			p := previous.Metrics
			if usable && logSampleUsable(*previous) && m.Available["file_bytes"] && m.Available["shmem_bytes"] && p.Available["file_bytes"] && p.Available["shmem_bytes"] {
				delta := max64(0, m.File-m.Shmem) - max64(0, p.File-p.Shmem)
				x.OrdinaryChange = &delta
			}
		}
		r.Changes = x
	}
	return r
}

func logSampleUsable(s Status) bool {
	m := s.Metrics
	age := s.Time.Sub(m.Time)
	return !s.Time.IsZero() && !m.Time.IsZero() && m.Error == "" && age >= -time.Second &&
		age <= time.Duration(max64(1000, int64(s.IntervalMS)*3))*time.Millisecond
}

func explainDecision(d Decision) string {
	text := map[string]string{
		"protected_pressure_boost":                  "内存进入预警区，按显式压力增强配置处理已有合格候选；仍受全部保护及配置速率上限约束。",
		"kubelet_eviction_headroom_low_or_pressure": "kubelet实际内存余量接近驱逐线或节点已报告MemoryPressure，停止新释放建议；应由外部流程止损备份。",
		"kubelet_node_signal_unavailable":           "kubelet节点信号缺失、过期或来源不符，暂停新释放建议。",
		"observation_only":                          "仅观察，不执行文件缓存释放。", "warmup": "启动预热中，暂不释放。",
		"below_cache_target": "普通文件页未超过软目标，当前无需释放。", "user_paused": "已按信号暂停新的释放建议。",
		"waiting_healthy_recovery": "风险已缓解，等待连续健康采样达到恢复窗口。",
		"dirty_or_writeback":       "脏页或写回量较高，暂停新建议以免增加写回压力。",
		"self_resource_budget":     "CCG自身CPU或内存超预算，退让。", "io_bandwidth": "可见cgroup IO速率过高，退让。",
		"io_psi": "IO等待压力超过阈值，退让。", "file_refault_rate": "文件页重新读入速率偏高，退让。",
		"business_pause": "业务信号要求暂停。", "business_latency": "业务尾延迟超过阈值，退让。",
		"new_oom_kill": "检测到新的cgroup OOM kill，停止新建议并检查容量及业务。",
		"memory_critical_stop_speculative_reclaim": "内存达到临界水位，按配置停止推测性回收。",
		"host_memavailable_low_or_unknown":         "宿主可用内存低于安全线或未知，停止新建议。",
		"no_eligible_roots":                        "没有可处理的目录，等待发现或检查路径权限。",
		"worker_slow_or_blocked":                   "工作线程较慢或阻塞，停止分派新动作。",
		"unlimited_cgroup_requires_cache_target":   "未看到有限内存上限，需要显式设置文件缓存软目标。",
		"waiting_memory_pressure":                  "文件页超过软目标，但尚未满足所选压力触发条件。",
		"cache_budget_exceeded":                    "文件页超过软目标，保护条件允许，正在有界处理合格文件范围。",
		"explicit_continuous_policy":               "按显式持续策略处理合格文件范围，仍受全部保护与预算约束。",
		"explicit_bounded_critical_reclaim":        "按显式临界有界策略处理合格范围，仍需观察业务影响。",
		"shutdown":                                 "CCG正在退出，已停止新的释放建议。",
	}
	if s, ok := text[d.Reason]; ok {
		return s
	}
	if strings.HasPrefix(d.Reason, "protection_metrics_unavailable:") {
		return "关键保护指标缺失，暂停新回收；缺项见protection.missing，未知值不能当作零。"
	}
	if strings.HasPrefix(d.Reason, "metrics_unavailable:") {
		return "指标采集失败，暂停新回收；详情见metrics.error。"
	}
	return fmt.Sprintf("策略状态%s，原因见decision.reason；是否允许新建议以decision.allow为准。", d.State)
}
