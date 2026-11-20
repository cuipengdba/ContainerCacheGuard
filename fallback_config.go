package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type FallbackConfig struct {
	Enabled                  bool     `json:"enabled"`
	IntervalSeconds          int      `json:"interval_seconds"`
	PGData                   string   `json:"pgdata,omitempty"`
	BackupDirectories        []string `json:"backup_directories"`
	AutoDirectories          []string `json:"auto_directories"`
	AutoAction               string   `json:"auto_action"`
	AllowBoundedCritical     bool     `json:"allow_bounded_critical"`
	ParentHeadroomBytes      int64    `json:"parent_headroom_bytes"`
	NodeHeadroomBytes        int64    `json:"node_headroom_bytes"`
	MinMainReductionBytes    int64    `json:"min_main_reduction_bytes"`
	RoundWorkMSPerScope      int      `json:"round_work_ms_per_scope"`
	RoundEntriesPerScope     int      `json:"round_entries_per_scope"`
	RoundAdviceBytesPerScope int64    `json:"round_advice_bytes_per_scope"`
	ProcEntriesPerPass       int      `json:"proc_entries_per_pass"`
	ExpectedMemoryHighBytes  *int64   `json:"expected_memory_high_bytes,omitempty"`
}

func defaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		IntervalSeconds: 30, AutoAction: "report", ParentHeadroomBytes: 2 << 30,
		NodeHeadroomBytes: 1 << 30, MinMainReductionBytes: 32 * MiB,
		RoundWorkMSPerScope: 150, RoundEntriesPerScope: 1024, RoundAdviceBytesPerScope: 128 * MiB,
		ProcEntriesPerPass: 512,
	}
}

func (f FallbackConfig) pgdata() string {
	if f.PGData != "" {
		return f.PGData
	}
	return os.Getenv("PGDATA")
}

func (c Config) fallbackPolicyConfig() Config {
	c.Trigger, c.PressureBoost = "always", false
	if !c.CacheFallback.AllowBoundedCritical {
		c.CriticalMode = "pause"
	}
	return c
}

func (c Config) validateFallback() error {
	f := c.CacheFallback
	if !f.Enabled {
		return nil
	}
	if f.IntervalSeconds < 5 || f.IntervalSeconds > 300 ||
		f.RoundWorkMSPerScope < 10 || f.RoundWorkMSPerScope > 3000 ||
		f.RoundEntriesPerScope < 4 || f.RoundEntriesPerScope > 16384 ||
		f.RoundAdviceBytesPerScope < c.RegionBytes || f.RoundAdviceBytesPerScope > 4<<30 ||
		f.ProcEntriesPerPass < 8 || f.ProcEntriesPerPass > 4096 ||
		f.ParentHeadroomBytes < 0 || f.ParentHeadroomBytes > 2<<40 ||
		f.NodeHeadroomBytes < 0 || f.NodeHeadroomBytes > 2<<40 ||
		f.MinMainReductionBytes < 0 || c.MaxRegions < 8 ||
		(f.ExpectedMemoryHighBytes != nil && *f.ExpectedMemoryHighBytes < 0) {
		return fmt.Errorf("cache_fallback 时间、工作量、条目或字节预算无效")
	}
	if f.AutoAction != "report" && f.AutoAction != "enforce" {
		return fmt.Errorf("cache_fallback.auto_action 必须为 report 或 enforce")
	}
	if f.AllowBoundedCritical && c.CriticalMode != "bounded" {
		return fmt.Errorf("cache_fallback.allow_bounded_critical 需要 critical_mode=bounded")
	}
	if len(f.BackupDirectories) == 0 || len(f.BackupDirectories)+len(f.AutoDirectories) > 24 ||
		(f.AutoAction == "enforce" && len(f.AutoDirectories) == 0) {
		return fmt.Errorf("cache_fallback 需要备份目录，目录合计最多24个，自动执行需要显式授权目录")
	}
	roots := append([]string{f.pgdata()}, f.BackupDirectories...)
	roots = append(roots, f.AutoDirectories...)
	for i, path := range roots {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || c.excluded(path) || len(path) > 4096 {
			return fmt.Errorf("cache_fallback 需要真实的 PGDATA 和规范、允许的绝对目录: %q", path)
		}
		for _, other := range roots[:i] {
			if beneath(path, other) || beneath(other, path) {
				return fmt.Errorf("cache_fallback 目录不能重复或嵌套: %q", path)
			}
		}
	}
	return nil
}
