package main

// These observations explain the measured scope. They neither attribute a
// workload's root cause nor predict kubelet eviction, and never enable advice.
type DiagnosticNote struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Diagnostics struct {
	Valid                 bool             `json:"valid"`
	OrdinaryFileBytes     int64            `json:"ordinary_file_bytes"`
	OtherChargedBytes     int64            `json:"other_charged_bytes"`
	FileTargetExcessBytes int64            `json:"file_target_excess_bytes"`
	Notes                 []DiagnosticNote `json:"notes"`
}

func diagnose(c Config, m Metrics, d Decision) Diagnostics {
	x := Diagnostics{Notes: []DiagnosticNote{}}
	add := func(code, message string) { x.Notes = append(x.Notes, DiagnosticNote{code, message}) }
	if m.Error != "" || m.Time.IsZero() {
		add("metrics_unavailable", "指标缺失或不可用，不能据此判断内存构成是否安全；先核对cgroup和采集状态。")
		return x
	}
	if m.Available != nil && (!m.Available["current_bytes"] || !m.Available["file_bytes"] || !m.Available["shmem_bytes"]) {
		add("memory_breakdown_incomplete", "缺少文件页或共享内存分类，无法可靠估算普通文件页与其它计费用量；已知指标仍可单独查看。")
		return x
	}
	x.Valid = true
	x.OrdinaryFileBytes = max64(0, m.File-m.Shmem)
	x.OtherChargedBytes = max64(0, m.Current-x.OrdinaryFileBytes)
	if d.CacheTarget > 0 {
		x.FileTargetExcessBytes = max64(0, x.OrdinaryFileBytes-d.CacheTarget)
	}
	add("file_scope_only", "CCG只能对授权文件的普通文件页提出释放建议；匿名内存、共享内存和slab不由此接口清理。")
	if memoryAtRatio(m, c.MemoryHighRatio) && m.Current > 0 && x.OtherChargedBytes > m.Current/2 {
		add("non_file_usage_majority", "可见内存组已达到高水位，当前容器超过一半用量属于普通文件页以外；只回收文件缓存可能不足，应同时检查业务查询、共享内存与容量预算。")
	}
	if x.FileTargetExcessBytes > 0 {
		add("file_target_exceeded", "普通文件页超过配置的软目标；目标不是硬上限，只有保护条件允许且找到合格范围时才可能下降。")
	}
	if c.Action == "enforce" && !d.Allow {
		add("advice_not_allowed", "当前策略暂停新释放建议，原因见decision.reason；enforce标志不表示正在持续回收。")
	}
	if d.Reason == "waiting_healthy_recovery" {
		add("healthy_recovery_wait", "风险解除后仍需连续有效的健康采样达到配置窗口；等待期间不发出新释放建议，中途异常或采样空窗会重新计时。")
	}
	if !m.FileRefaultRateValid {
		add("refault_unknown", "当前文件页refault速率未知，可能缺少连续样本或内核字段；未知不能解释为零重读。")
	}
	add("not_eviction_prediction", "本地统计未接入Kubelet驱逐信号；没有OOM或文件页下降都不能单独证明业务无中断。")
	return x
}
