package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MiB int64 = 1 << 20

// Config has no executable hooks and never changes cgroup limits.
type Config struct {
	Mode                         string   `json:"mode"`
	Action                       string   `json:"action"`
	Paths                        []string `json:"paths"`
	AutoRoots                    []string `json:"auto_roots"`
	ExcludePaths                 []string `json:"exclude_paths"`
	ExcludeNames                 []string `json:"exclude_names"`
	CgroupPath                   string   `json:"cgroup_path"`
	StateFile                    string   `json:"state_file"`
	BusinessSignal               string   `json:"business_signal"`
	SignalMaxAgeSeconds          int      `json:"signal_max_age_seconds"`
	MaxBusinessP99MS             float64  `json:"max_business_p99_ms"`
	IntervalMS                   int      `json:"interval_ms"`
	LogIntervalSeconds           int      `json:"log_interval_seconds"`
	WarmupSeconds                int      `json:"warmup_seconds"`
	ProtectSeconds               int      `json:"protect_seconds"`
	MinFileAgeSeconds            int      `json:"min_file_age_seconds"`
	CooldownSeconds              int      `json:"cooldown_seconds"`
	RescanSeconds                int      `json:"rescan_seconds"`
	CacheTargetBytes             int64    `json:"cache_target_bytes"`
	CacheTargetRatio             float64  `json:"cache_target_ratio"`
	Trigger                      string   `json:"trigger"`
	MemoryHighRatio              float64  `json:"memory_high_ratio"`
	MemoryCriticalRatio          float64  `json:"memory_critical_ratio"`
	CriticalMode                 string   `json:"critical_mode"`
	MaxDirtyBytes                int64    `json:"max_dirty_bytes"`
	MaxIOBytesPerSecond          int64    `json:"max_io_bytes_per_second"`
	MaxIOPSIAvg10                float64  `json:"max_io_psi_avg10"`
	MinNodeAvailableBytes        int64    `json:"min_node_available_bytes"`
	ScanBytesPerSecond           int64    `json:"scan_bytes_per_second"`
	ReclaimBytesPerSecond        int64    `json:"reclaim_bytes_per_second"`
	InitialReclaimBytesPerSecond int64    `json:"initial_reclaim_bytes_per_second"`
	RegionBytes                  int64    `json:"region_bytes"`
	DiscoverEntriesPerSecond     int      `json:"discover_entries_per_second"`
	MaxFiles                     int      `json:"max_files"`
	MaxDirectories               int      `json:"max_directories"`
	MaxRegions                   int      `json:"max_regions"`
	MaxOperationsPerSecond       int      `json:"max_operations_per_second"`
	MaxCPUPercent                float64  `json:"max_cpu_percent"`
	MaxRSSBytes                  int64    `json:"max_rss_bytes"`
	MaxReappearanceRatio         float64  `json:"max_reappearance_ratio"`
	RecheckSeconds               int      `json:"recheck_seconds"`
	WorkerBudgetMS               int      `json:"worker_budget_ms"`
	AccessHints                  bool     `json:"access_hints"`
	MaxWatches                   int      `json:"max_watches"`
	MaxHintFiles                 int      `json:"max_hint_files"`
}

func defaultConfig() Config {
	return Config{
		Mode: "auto", Action: "observe", Paths: []string{}, AutoRoots: []string{}, ExcludePaths: []string{},
		ExcludeNames: []string{"pg_wal", "pg_xlog", "global", "pg_xact", "pg_multixact", "pg_subtrans", "pg_replslot", "pg_logical", "pg_dynshmem", "pg_stat_tmp", "pg_notify", "pg_serial", "pg_snapshots", "lost+found"},
		StateFile:    "/tmp/containercacheguard/status.json", SignalMaxAgeSeconds: 10,
		IntervalMS: 1000, LogIntervalSeconds: 10, WarmupSeconds: 60, ProtectSeconds: 30, MinFileAgeSeconds: 30, CooldownSeconds: 60, RescanSeconds: 60,
		CacheTargetRatio: 0.25, Trigger: "budget", MemoryHighRatio: 0.85, MemoryCriticalRatio: 0.95, CriticalMode: "pause", MaxDirtyBytes: 128 * MiB, MaxIOPSIAvg10: 10,
		ScanBytesPerSecond: 256 * MiB, ReclaimBytesPerSecond: 64 * MiB, InitialReclaimBytesPerSecond: 4 * MiB, RegionBytes: 4 * MiB,
		DiscoverEntriesPerSecond: 500, MaxFiles: 150000, MaxDirectories: 20000, MaxRegions: 8192, MaxOperationsPerSecond: 1000,
		MaxCPUPercent: 5, MaxRSSBytes: 128 * MiB, MaxReappearanceRatio: 0.25, RecheckSeconds: 5, WorkerBudgetMS: 25,
		AccessHints: true, MaxWatches: 1024, MaxHintFiles: 2048,
	}
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	if path == "" {
		return c, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, errors.New("配置只能包含一个 JSON 对象")
	}
	return c, nil
}
func (c Config) validate() error {
	if c.Mode != "auto" && c.Mode != "paths" {
		return errors.New("mode 必须为 auto 或 paths")
	}
	if c.Action != "observe" && c.Action != "enforce" {
		return errors.New("action 必须为 observe 或 enforce")
	}
	if c.Mode == "paths" && len(c.Paths) == 0 {
		return errors.New("paths 模式至少需要一个路径")
	}
	if c.Mode == "auto" && len(c.Paths) > 0 {
		return errors.New("auto 模式使用 auto_roots；paths 只用于 paths 模式")
	}
	if c.Trigger != "budget" && c.Trigger != "pressure" && c.Trigger != "always" {
		return errors.New("trigger 必须为 budget、pressure 或 always")
	}
	if c.CriticalMode != "pause" && c.CriticalMode != "bounded" {
		return errors.New("critical_mode 必须为 pause 或 bounded")
	}
	if c.IntervalMS < 100 || c.IntervalMS > 60000 || c.WorkerBudgetMS < 1 || c.WorkerBudgetMS > 1000 {
		return errors.New("interval_ms 范围100..60000，worker_budget_ms范围1..1000")
	}
	if c.LogIntervalSeconds < 1 || c.RescanSeconds < 1 || c.RecheckSeconds < 1 || c.SignalMaxAgeSeconds < 1 {
		return errors.New("日志、重扫、复查、信号有效期必须为正数")
	}
	if c.WarmupSeconds < 0 || c.ProtectSeconds < 0 || c.MinFileAgeSeconds < 0 || c.CooldownSeconds < 1 {
		return errors.New("保护时间不能为负；冷却时间必须为正")
	}
	if c.CacheTargetBytes < 0 || c.MaxDirtyBytes < 0 || c.MaxIOBytesPerSecond < 0 || c.MinNodeAvailableBytes < 0 {
		return errors.New("字节阈值不能为负")
	}
	if c.CacheTargetRatio <= 0 || c.CacheTargetRatio >= 1 || c.MemoryHighRatio <= 0 || c.MemoryCriticalRatio <= c.MemoryHighRatio || c.MemoryCriticalRatio >= 1 {
		return errors.New("比例需满足 0<target<1，0<high<critical<1")
	}
	if c.MaxReappearanceRatio <= 0 || c.MaxReappearanceRatio > 1 || c.MaxCPUPercent <= 0 || c.MaxCPUPercent > 100 || c.MaxIOPSIAvg10 < 0 {
		return errors.New("CPU、PSI 或重新驻留比例无效")
	}
	if c.RegionBytes < 65536 || c.RegionBytes > 64*MiB || c.RegionBytes%65536 != 0 {
		return errors.New("region_bytes 为 64KiB 的整数倍，范围64KiB..64MiB，兼容4K/64K页")
	}
	if c.ScanBytesPerSecond < c.RegionBytes || c.ScanBytesPerSecond > 16<<30 || c.InitialReclaimBytesPerSecond < c.RegionBytes || c.ReclaimBytesPerSecond < c.InitialReclaimBytesPerSecond || c.ReclaimBytesPerSecond > 16<<30 {
		return errors.New("扫描与初始回收速率至少一个 region；最大回收不得低于初始值；上限16GiB/s")
	}
	if c.MaxFiles < 1 || c.MaxFiles > 1000000 || c.MaxDirectories < 1 || c.MaxDirectories > 100000 || c.MaxRegions < 1 || c.MaxRegions > 1000000 || c.MaxOperationsPerSecond < 1 || c.MaxOperationsPerSecond > 100000 || c.DiscoverEntriesPerSecond < 1 || c.DiscoverEntriesPerSecond > 100000 || c.MaxRSSBytes < 16*MiB {
		return errors.New("索引/操作/内存预算超出有效范围")
	}
	if c.MaxWatches < 1 || c.MaxWatches > 16384 || c.MaxHintFiles < 1 || c.MaxHintFiles > 100000 {
		return errors.New("max_watches范围1..16384，max_hint_files范围1..100000")
	}
	for _, p := range append(append(append([]string{}, c.Paths...), c.AutoRoots...), c.ExcludePaths...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("路径必须为规范绝对路径: %q", p)
		}
	}
	for _, p := range append(append([]string{}, c.Paths...), c.AutoRoots...) {
		if forbiddenRoot(p) {
			return fmt.Errorf("禁止治理系统或根目录: %s", p)
		}
	}
	if !filepath.IsAbs(c.StateFile) || (c.BusinessSignal != "" && !filepath.IsAbs(c.BusinessSignal)) {
		return errors.New("state_file/business_signal 必须为绝对路径")
	}
	return nil
}
func beneath(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}
func forbiddenRoot(p string) bool {
	if p == "/" || p == "/var" || p == "/var/lib" || p == "/opt" || p == "/usr" || p == "/tmp" || p == "/run" || p == "/home" || p == "/app" {
		return true
	}
	for _, x := range []string{"/proc", "/sys", "/dev", "/etc", "/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/lib", "/lib64", "/boot", "/var/lib/docker", "/var/lib/containerd", "/var/lib/kubelet"} {
		if beneath(p, x) {
			return true
		}
	}
	return false
}
func (c Config) excluded(p string) bool {
	if forbiddenRoot(p) {
		return true
	}
	for _, x := range c.ExcludePaths {
		if beneath(p, x) {
			return true
		}
	}
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		for _, n := range c.ExcludeNames {
			if part == n {
				return true
			}
		}
	}
	return false
}
