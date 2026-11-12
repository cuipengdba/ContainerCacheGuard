package main

import (
	"errors"
	"path/filepath"
)

// KernelConfig is opt-in. It never changes a memory, IO, swap or CPU limit.
type KernelConfig struct {
	Enabled                   bool    `json:"enabled"`
	Executor                  string  `json:"executor"`
	GuardProfile              string  `json:"guard_profile"`
	Scope                     string  `json:"scope"`
	Socket                    string  `json:"socket"`
	Target                    string  `json:"target"`
	SignalFile                string  `json:"signal_file"`
	SignalSourceID            string  `json:"signal_source_id"`
	MaxCallsPerEpisode        int     `json:"max_calls_per_episode"`
	MemoryTriggerRatio        float64 `json:"memory_trigger_ratio"`
	MemoryRearmRatio          float64 `json:"memory_rearm_ratio"`
	MinHeadroomBytes          int64   `json:"min_headroom_bytes"`
	PredictSeconds            int     `json:"predict_seconds"`
	NodeTriggerMarginBytes    int64   `json:"node_trigger_margin_bytes"`
	NodeRearmExtraBytes       int64   `json:"node_rearm_extra_bytes"`
	ConfirmSamples            int     `json:"confirm_samples"`
	RequestMinBytes           int64   `json:"request_min_bytes"`
	RequestMaxBytes           int64   `json:"request_max_bytes"`
	FileBudgetRatio           float64 `json:"file_budget_ratio"`
	MinFileEstimateBytes      int64   `json:"min_file_estimate_bytes"`
	CallWatchdogMS            int     `json:"call_watchdog_ms"`
	RecheckSeconds            []int   `json:"recheck_seconds"`
	RearmHealthySeconds       int     `json:"rearm_healthy_seconds"`
	MinEpisodeIntervalSeconds int     `json:"min_episode_interval_seconds"`
	RequireSwapDisabled       bool    `json:"require_swap_disabled"`
	StateDirectory            string  `json:"state_directory"`
	MaxDiskAwaitMS            float64 `json:"max_disk_await_ms"`
	MaxDiskQueue              float64 `json:"max_disk_queue"`
	MaxMemoryPSIAvg10         float64 `json:"max_memory_psi_avg10"`
	MinBusinessSamples        int     `json:"min_business_samples"`
}

func defaultKernelConfig() KernelConfig {
	return KernelConfig{Executor: "node-agent", GuardProfile: "strict", Scope: "cgroup", Socket: "/run/containercacheguard/reclaim.sock", MaxCallsPerEpisode: 1, MemoryTriggerRatio: .90, MemoryRearmRatio: .85, MinHeadroomBytes: 1 << 30, PredictSeconds: 20, NodeTriggerMarginBytes: 1 << 30, NodeRearmExtraBytes: 512 * MiB, ConfirmSamples: 3, RequestMinBytes: 64 * MiB, RequestMaxBytes: 512 * MiB, FileBudgetRatio: .25, MinFileEstimateBytes: 512 * MiB, CallWatchdogMS: 2000, RecheckSeconds: []int{1, 5, 30, 120}, RearmHealthySeconds: 60, MinEpisodeIntervalSeconds: 300, RequireSwapDisabled: true, StateDirectory: "/var/lib/containercacheguard/reclaim", MinBusinessSamples: 20, MaxMemoryPSIAvg10: 10}
}
func (c Config) validateKernel() error {
	k := c.KernelReclaim
	if !k.Enabled {
		return nil
	}
	fail := func() error {
		return errors.New("kernel_reclaim配置无效：须显式cgroup范围、每事件1次、完整保护/身份/可信信号、无swap及有界请求；见rc.17手册")
	}
	if k.Executor != "node-agent" || k.Scope != "cgroup" || k.MaxCallsPerEpisode != 1 || !k.RequireSwapDisabled || !identifier.MatchString(k.Target) || k.Target == "" || !identifier.MatchString(k.SignalSourceID) || k.SignalSourceID == "" || c.NodeID == "" || c.BusinessSourceID == "" || c.BusinessSignal == "" || c.MaxBusinessP99MS <= 0 || !finiteNonnegative(c.MaxBusinessP99MS) || c.MaxIOBytesPerSecond <= 0 || c.MaxFileRefaultsPerSecond <= 0 || !finiteNonnegative(c.MaxFileRefaultsPerSecond) || c.MaxDirtyBytes <= 0 {
		return fail()
	}
	if k.GuardProfile != "strict" && k.GuardProfile != "no-psi-guarded" {
		return fail()
	}
	if k.GuardProfile == "strict" && c.MaxIOPSIAvg10 <= 0 {
		return fail()
	}
	if !finiteNonnegative(k.MaxMemoryPSIAvg10) || k.MaxMemoryPSIAvg10 <= 0 || k.MaxMemoryPSIAvg10 > 100 {
		return fail()
	}
	if !finiteNonnegative(k.MemoryTriggerRatio) || !finiteNonnegative(k.MemoryRearmRatio) || k.MemoryRearmRatio < .1 || k.MemoryTriggerRatio <= k.MemoryRearmRatio || k.MemoryTriggerRatio >= 1 || !finiteNonnegative(k.FileBudgetRatio) || k.FileBudgetRatio <= 0 || k.FileBudgetRatio > .5 {
		return fail()
	}
	if k.RequestMinBytes < 64*MiB || k.RequestMaxBytes < k.RequestMinBytes || k.RequestMaxBytes > 1<<30 || k.RequestMinBytes%65536 != 0 || k.RequestMaxBytes%65536 != 0 || k.MinFileEstimateBytes < 256*MiB || k.MinFileEstimateBytes > 1<<40 || k.MinHeadroomBytes < 64*MiB || k.MinHeadroomBytes > 1<<40 || k.NodeTriggerMarginBytes < 256*MiB || k.NodeTriggerMarginBytes > 1<<40 || k.NodeRearmExtraBytes < 64*MiB || k.NodeRearmExtraBytes > 1<<40 {
		return fail()
	}
	if k.ConfirmSamples < 3 || k.ConfirmSamples > 60 || k.PredictSeconds < 1 || k.PredictSeconds > 120 || k.CallWatchdogMS < 100 || k.CallWatchdogMS > 5000 || k.RearmHealthySeconds < 60 || k.RearmHealthySeconds > 3600 || k.MinEpisodeIntervalSeconds < 300 || k.MinEpisodeIntervalSeconds > 86400 || c.IntervalMS > 1000 || c.WarmupSeconds < 10 || k.MinBusinessSamples < 20 || k.MinBusinessSamples > 1000000 {
		return fail()
	}
	if k.MaxDiskAwaitMS < 0 || !finiteNonnegative(k.MaxDiskAwaitMS) || k.MaxDiskQueue < 0 || !finiteNonnegative(k.MaxDiskQueue) {
		return fail()
	}
	if k.GuardProfile == "no-psi-guarded" && (k.RequestMaxBytes > 128*MiB || k.MaxDiskAwaitMS <= 0 || k.MaxDiskQueue <= 0) {
		return fail()
	}
	if len(k.RecheckSeconds) == 0 || len(k.RecheckSeconds) > 8 {
		return fail()
	}
	last := 0
	for _, n := range k.RecheckSeconds {
		if n <= last || n > 3600 {
			return fail()
		}
		last = n
	}
	for _, p := range []string{k.Socket, k.SignalFile, k.StateDirectory} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return fail()
		}
	}
	return nil
}
