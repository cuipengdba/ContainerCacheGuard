//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type KernelConstraint struct {
	Kind      string    `json:"kind"`
	Time      time.Time `json:"time"`
	Available *int64    `json:"available_bytes"`
	Hard      *int64    `json:"hard_bytes"`
	Soft      *int64    `json:"soft_bytes"`
}

// A separate versioned envelope leaves legacy node-signal consumers unchanged.
type KernelSignal struct {
	Schema              int                `json:"schema_version"`
	SourceID            string             `json:"source_id"`
	NodeID              string             `json:"node_id"`
	BootID              string             `json:"boot_id"`
	Time                time.Time          `json:"time"`
	ConfigVersion       string             `json:"config_version"`
	AllocatableRequired *bool              `json:"allocatable_required"`
	MemoryPressure      *bool              `json:"memory_pressure"`
	Constraints         []KernelConstraint `json:"constraints"`
	DiskTime            time.Time          `json:"disk_time"`
	DiskAwaitMS         *float64           `json:"disk_await_ms"`
	DiskQueue           *float64           `json:"disk_queue"`
	BusinessTime        time.Time          `json:"business_time"`
	BusinessSamples     *int               `json:"business_samples"`
	BusinessErrorRate   *float64           `json:"business_error_rate"`
}
type KernelLevel struct {
	Path    string `json:"path"`
	Current int64  `json:"current_bytes"`
	Limit   int64  `json:"limit_bytes"`
	OOMKill int64  `json:"oom_kill_total"`
}
type KernelSample struct {
	Metrics     Metrics       `json:"metrics"`
	Signal      KernelSignal  `json:"signal"`
	Levels      []KernelLevel `json:"levels"`
	SwapMax     int64         `json:"swap_max_bytes"`
	SwapCurrent int64         `json:"swap_current_bytes"`
	Error       string        `json:"error,omitempty"`
}
type KernelObservation struct {
	Seconds      int          `json:"requested_seconds"`
	Time         time.Time    `json:"time"`
	Current      int64        `json:"current_bytes"`
	OrdinaryFile int64        `json:"ordinary_file_bytes"`
	Anon         int64        `json:"anon_bytes"`
	Shmem        int64        `json:"shmem_bytes"`
	Slab         int64        `json:"slab_bytes"`
	Signal       KernelSignal `json:"signal"`
	Error        string       `json:"error,omitempty"`
	Metrics      Metrics      `json:"metrics"`
}
type KernelStatus struct {
	Phase          string              `json:"phase"`
	Reason         string              `json:"reason"`
	Time           time.Time           `json:"time"`
	Episode        string              `json:"episode,omitempty"`
	TargetIdentity string              `json:"target_identity,omitempty"`
	HoldFile       bool                `json:"hold_file_advice"`
	Attempted      bool                `json:"write_attempt_reserved"`
	Request        int64               `json:"request_bytes"`
	EstimatedFile  int64               `json:"estimated_file_budget_bytes"`
	Deficit        int64               `json:"desired_headroom_bytes"`
	Started        time.Time           `json:"started,omitempty"`
	Finished       time.Time           `json:"finished,omitempty"`
	Return         string              `json:"kernel_result,omitempty"`
	Watchdog       bool                `json:"watchdog_expired"`
	Before         *KernelSample       `json:"before,omitempty"`
	Observations   []KernelObservation `json:"observations,omitempty"`
}
type kernelMachine struct {
	Status        KernelStatus
	confirmations int
	healthy       time.Time
	previous      KernelSample
}

func freshKernel(t, now time.Time, age time.Duration) bool {
	return !t.IsZero() && !now.IsZero() && now.Sub(t) >= -time.Second && now.Sub(t) <= age
}
func kernelRead(c Config, r *metricReader, boot string) KernelSample {
	s := KernelSample{Metrics: r.sample(c)}
	business(c, &s.Metrics)
	if err := readTrustedKernelJSON(c.KernelReclaim.SignalFile, 32768, &s.Signal); err != nil {
		s.Error = "kernel signal unreadable/invalid"
	}
	if err := kernelTrustedFile(c.BusinessSignal); err != nil {
		s.Error = "business signal ownership/permissions invalid"
	}
	if _, err := os.Lstat(filepath.Join(r.path, "memory.reclaim")); err != nil {
		s.Error = "memory.reclaim interface unavailable"
	}
	s.SwapMax, _ = number(filepath.Join(r.path, "memory.swap.max"))
	s.SwapCurrent, _ = number(filepath.Join(r.path, "memory.swap.current"))
	for _, n := range []string{"memory.swap.max", "memory.swap.current"} {
		if _, err := number(filepath.Join(r.path, n)); err != nil {
			s.Error = "swap interface unavailable"
		}
	}
	// All visible finite ancestors are checked individually; never sum usage.
	for p, depth := r.path, 0; ; p, depth = filepath.Dir(p), depth+1 {
		limit, err := number(filepath.Join(p, "memory.max"))
		if err != nil {
			if p == r.path {
				s.Error = "memory.max unavailable"
			}
			break
		}
		cur, e := number(filepath.Join(p, "memory.current"))
		if e != nil {
			s.Error = "ancestor usage unavailable"
			break
		}
		events := values(filepath.Join(p, "memory.events"))
		kills, ok := events["oom_kill"]
		if !ok || kills < 0 {
			s.Error = "ancestor OOM events unavailable"
			break
		}
		if limit > 0 || kills > 0 {
			s.Levels = append(s.Levels, KernelLevel{p, cur, limit, kills})
		}
		if depth >= 64 {
			s.Error = "ancestor limit exceeded"
			break
		}
		if p == "/" {
			break
		}
	}
	return s
}
func kernelGuard(c Config, s KernelSample, boot string, now time.Time) string {
	k := c.KernelReclaim
	m := s.Metrics
	n := s.Signal
	if s.Error != "" {
		return s.Error
	}
	if m.Error != "" || m.Version != 2 || !freshKernel(m.Time, now, 2500*time.Millisecond) {
		return "cgroup_metrics_unavailable"
	}
	// Never reinterpret a historical kill as healthy after restarting the agent.
	if !m.Available["oom_kill_total"] || m.OOMKill != 0 {
		return "oom_history_unresolved"
	}
	for _, l := range s.Levels {
		if l.OOMKill != 0 {
			return "ancestor_oom_history_unresolved"
		}
	}
	if !k.RequireSwapDisabled || s.SwapMax != 0 || s.SwapCurrent != 0 {
		return "swap_not_disabled"
	}
	if n.Schema != 2 || n.SourceID != k.SignalSourceID || n.NodeID != c.NodeID || n.BootID != boot || n.ConfigVersion == "" || !freshKernel(n.Time, now, 5*time.Second) || n.AllocatableRequired == nil || n.MemoryPressure == nil || len(n.Constraints) < 1 || len(n.Constraints) > 2 {
		return "kernel_signal_identity_or_age"
	}
	if *n.MemoryPressure {
		return "node_already_in_memory_pressure"
	}
	seen := map[string]bool{}
	for _, q := range n.Constraints {
		if (q.Kind != "node" && q.Kind != "allocatable") || seen[q.Kind] || q.Available == nil || q.Hard == nil || q.Soft == nil || *q.Available < 0 || *q.Hard < 0 || *q.Soft < 0 || max64(*q.Hard, *q.Soft) > math.MaxInt64-(2<<40) || !freshKernel(q.Time, now, 15*time.Second) {
			return "kernel_node_constraint_invalid"
		}
		seen[q.Kind] = true
		if *q.Hard > 0 && *q.Available <= *q.Hard {
			return "too_late_hard_eviction_line"
		}
	}
	if !seen["node"] || (*n.AllocatableRequired && !seen["allocatable"]) {
		return "kernel_node_constraint_missing"
	}
	if !freshKernel(m.BusinessTime, now, 5*time.Second) || m.BusinessSourceID != c.BusinessSourceID || !n.BusinessTime.Equal(m.BusinessTime) || n.BusinessSamples == nil || *n.BusinessSamples < k.MinBusinessSamples || n.BusinessErrorRate == nil || !finiteNonnegative(*n.BusinessErrorRate) || *n.BusinessErrorRate != 0 {
		return "business_coverage_or_errors"
	}
	// Reuse all configured resource/IO/refault/business guards, independently
	// of the old memory-critical pause (which is this backend's trigger).
	guardConfig := c
	guardConfig.NodeSignal = ""
	if k.GuardProfile == "no-psi-guarded" && m.IOPSI == nil {
		if !m.Available["io_psi_interface_absent"] {
			return "psi_missing_not_confirmed_absent"
		}
		guardConfig.MaxIOPSIAvg10 = 0
		if !freshKernel(n.DiskTime, now, 5*time.Second) || n.DiskAwaitMS == nil || n.DiskQueue == nil || !finiteNonnegative(*n.DiskAwaitMS) || !finiteNonnegative(*n.DiskQueue) || *n.DiskAwaitMS > k.MaxDiskAwaitMS || *n.DiskQueue > k.MaxDiskQueue {
			return "disk_alternative_unavailable_or_high"
		}
	}
	guardConfig.ProtectionProfile = "strict"
	if !assessProtection(guardConfig, m).Ready {
		return "kernel_protection_inputs_unavailable"
	}
	if k.GuardProfile == "strict" && (m.MemoryPSI == nil || m.IOPSI == nil) {
		return "kernel_psi_unavailable"
	}
	if m.MemoryPSI != nil && (!finiteNonnegative(*m.MemoryPSI) || *m.MemoryPSI > k.MaxMemoryPSIAvg10) {
		return "memory_psi_high_or_invalid"
	}
	if m.RSS > c.MaxRSSBytes || m.CPUPercent > c.MaxCPUPercent {
		return "self_resource_budget"
	}
	if m.BusinessPause || m.BusinessP99 == nil || *m.BusinessP99 > c.MaxBusinessP99MS {
		return "business_latency_or_pause"
	}
	if m.Dirty > c.MaxDirtyBytes-m.Writeback {
		return "dirty_or_writeback"
	}
	if m.IOBytesPerSecond > float64(c.MaxIOBytesPerSecond) {
		return "io_bandwidth"
	}
	if !m.FileRefaultRateValid || m.FileRefaultsPerSecond > c.MaxFileRefaultsPerSecond {
		return "file_refault_rate"
	}
	if m.IOPSI != nil && c.MaxIOPSIAvg10 > 0 && *m.IOPSI > c.MaxIOPSIAvg10 {
		return "io_psi"
	}
	return ""
}
func kernelRisk(c Config, s, previous KernelSample) (risk, healthy bool, deficit int64) {
	k := c.KernelReclaim
	risk = false
	healthy = true
	for _, l := range s.Levels {
		if l.Limit <= 0 {
			continue
		}
		h := l.Limit - l.Current
		d := max64(k.MinHeadroomBytes-h, l.Current-int64(float64(l.Limit)*k.MemoryRearmRatio))
		hit := float64(l.Current) >= float64(l.Limit)*k.MemoryTriggerRatio || h <= k.MinHeadroomBytes
		for _, p := range previous.Levels {
			dt := s.Metrics.Time.Sub(previous.Metrics.Time).Seconds()
			if p.Path == l.Path && dt > 0 && dt <= 3 && l.Current > p.Current && float64(h) <= float64(l.Current-p.Current)/dt*float64(k.PredictSeconds) {
				hit = true
			}
		}
		if hit {
			risk = true
			deficit = max64(deficit, d)
		}
		if float64(l.Current) > float64(l.Limit)*k.MemoryRearmRatio || h < k.MinHeadroomBytes {
			healthy = false
		}
	}
	for _, q := range s.Signal.Constraints {
		if q.Available == nil || q.Hard == nil || q.Soft == nil {
			healthy = false
			continue
		}
		e := max64(*q.Hard, *q.Soft)
		margin := k.NodeTriggerMarginBytes
		for _, p := range previous.Signal.Constraints {
			dt := q.Time.Sub(p.Time).Seconds()
			if p.Kind == q.Kind && p.Available != nil && dt > 0 && dt <= 30 && *p.Available > *q.Available {
				lead := math.Max(0, s.Metrics.Time.Sub(q.Time).Seconds()) + float64(k.PredictSeconds+k.ConfirmSamples) + float64(k.CallWatchdogMS)/1000
				dynamic := float64(*p.Available-*q.Available) / dt * lead
				if dynamic > float64(margin) {
					margin = int64(math.Min(dynamic, float64(1<<40)))
				}
			}
		}
		if *q.Available <= e+margin {
			risk = true
			deficit = max64(deficit, e+margin+k.NodeRearmExtraBytes-*q.Available)
		}
		if *q.Available < e+margin+k.NodeRearmExtraBytes {
			healthy = false
		}
	}
	return
}
func kernelRequest(c Config, s KernelSample, deficit, nodeBudget int64) (int64, int64) {
	f := max64(0, s.Metrics.File-s.Metrics.Shmem-s.Metrics.Dirty-s.Metrics.Writeback)
	if f < c.KernelReclaim.MinFileEstimateBytes {
		return 0, f
	}
	r := min64(deficit, min64(c.KernelReclaim.RequestMaxBytes, min64(int64(float64(f)*c.KernelReclaim.FileBudgetRatio), nodeBudget)))
	r = r / int64(os.Getpagesize()) * int64(os.Getpagesize())
	if r < c.KernelReclaim.RequestMinBytes {
		return 0, f
	}
	return r, f
}
func readBootID() (string, error) {
	b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	id := strings.TrimSpace(string(b))
	if e != nil || len(id) != 36 {
		return "", errors.New("boot identity unavailable")
	}
	return id, nil
}
