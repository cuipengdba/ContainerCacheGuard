//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var version = "dev"
var commit = "unknown"

const help = `ContainerCacheGuard — Linux 容器文件缓存常驻治理

用法：
  containercacheguard init-config --mode auto|paths > config.json
  containercacheguard doctor [--config config.json] [--path /data]
  containercacheguard run --config config.json [--mode auto|paths] [--action observe|enforce]
  containercacheguard status [--state-file /tmp/containercacheguard/status.json]
  containercacheguard scan --path /data [--scan-bytes-per-second 268435456]
  containercacheguard version

auto 自动发现磁盘数据挂载、常用数据目录和 PGDATA；paths 仅处理 --path 或 paths 配置。
--path 可重复。默认 action=observe，不回收；enforce 才发出文件范围释放建议。
--duration 30s 限制运行时长；默认持续运行。--config 支持完整 JSON 参数，未知参数报错。
run 可覆盖 --cache-target-bytes、--reclaim-bytes-per-second、--cgroup-path、--state-file。
SIGUSR1 暂停新释放，SIGUSR2 恢复，SIGTERM/SIGINT 退出；配置修改需重启。
web_listen 可选启用内置只读页面；默认关闭。非回环地址必须配置 web_token_file。
scan 是只读驻留扫描，输出不是热度；可能需要完整遍历，不属于常态轻量监控。

只操作已授权路径下支持的普通磁盘文件。不会删除文件、清空应用堆、清理全机缓存、
修改 cgroup 限额或杀业务。fadvise 可能引起写回；驻留不等于冷热，无法保证零影响。
中文文档：https://github.com/cuipengdba/ContainerCacheGuard
`

type pathsFlag []string

func (p *pathsFlag) String() string     { return strings.Join(*p, ",") }
func (p *pathsFlag) Set(v string) error { *p = append(*p, v); return nil }
func main() {
	if e := entry(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "ContainerCacheGuard:", e)
		os.Exit(1)
	}
}
func entry(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(help)
		return nil
	}
	if args[0] == "version" {
		fmt.Printf("ContainerCacheGuard %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	config := fs.String("config", "", "JSON 配置文件")
	mode := fs.String("mode", "", "auto 或 paths")
	action := fs.String("action", "", "observe 或 enforce")
	cg := fs.String("cgroup-path", "", "目标 memory cgroup 绝对路径")
	state := fs.String("state-file", "", "状态 JSON 文件绝对路径")
	webListen := fs.String("web-listen", "", "只读 Web 监听地址，例如 127.0.0.1:9898")
	webTokenFile := fs.String("web-token-file", "", "Web HTTP Basic 密钥文件绝对路径；用户名 ccg")
	var paths pathsFlag
	fs.Var(&paths, "path", "治理路径，可重复（设置后选择 paths 模式）")
	target := fs.Int64("cache-target-bytes", -1, "容器普通文件缓存软预算，字节")
	rate := fs.Int64("reclaim-bytes-per-second", -1, "每秒建议释放范围字节上限")
	scanRate := fs.Int64("scan-bytes-per-second", -1, "每秒驻留查询覆盖字节上限")
	duration := fs.Duration("duration", 0, "运行时长，例如 5m；0 常驻")
	fs.Usage = func() { fmt.Fprint(os.Stderr, help); fs.PrintDefaults() }
	if e := fs.Parse(args[1:]); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if fs.NArg() > 0 {
		return errors.New("unexpected positional arguments")
	}
	c, e := loadConfig(*config)
	if e != nil {
		return e
	}
	if *mode != "" {
		c.Mode = *mode
	}
	if *action != "" {
		c.Action = *action
	}
	if *cg != "" {
		c.CgroupPath = *cg
	}
	if *state != "" {
		c.StateFile = *state
	}
	if *webListen != "" {
		c.WebListen = *webListen
	}
	if *webTokenFile != "" {
		c.WebTokenFile = *webTokenFile
	}
	if len(paths) > 0 {
		c.Paths = paths
		if *mode == "" {
			c.Mode = "paths"
		}
	}
	if *target >= 0 {
		c.CacheTargetBytes = *target
	}
	if *rate >= 0 {
		c.ReclaimBytesPerSecond = *rate
		if c.InitialReclaimBytesPerSecond > *rate {
			c.InitialReclaimBytesPerSecond = *rate
		}
	}
	if *scanRate >= 0 {
		c.ScanBytesPerSecond = *scanRate
	}
	if *duration < 0 {
		return errors.New("duration must be nonnegative")
	}
	if cmd == "init-config" {
		if c.Mode == "paths" && len(c.Paths) == 0 {
			c.Paths = []string{"/data"}
		}
		return output(c)
	}
	if cmd == "status" {
		b, e := os.ReadFile(c.StateFile)
		if e != nil {
			return e
		}
		var s Status
		if e = json.Unmarshal(b, &s); e != nil {
			return e
		}
		if time.Since(s.Time) > time.Duration(c.IntervalMS*3)*time.Millisecond {
			return fmt.Errorf("状态已过期（%s）；daemon 可能退出或阻塞", s.Time)
		}
		fmt.Print(string(b))
		return nil
	}
	if e = c.validate(); e != nil {
		return e
	}
	roots := normalizeRoots(c, c.Paths)
	if c.Mode == "auto" {
		roots = discoverRoots(c)
	}
	if cmd == "doctor" {
		r, e := newMetricReader(c.CgroupPath)
		result := map[string]any{"version": version, "architecture": runtime.GOARCH, "page_size": os.Getpagesize(), "uid": os.Geteuid(), "mode": c.Mode, "action": c.Action, "roots": roots, "file_cache_backend": "mincore+fadvise", "other_caches": "diagnostic_only", "node_pressure_source": "host MemAvailable (not kubelet Summary)"}
		if e != nil {
			result["metrics_error"] = e.Error()
		} else {
			m := r.sample(c)
			result["metrics"] = m
			if m.Error != "" {
				e = fmt.Errorf("metrics unavailable: %s", m.Error)
			}
		}
		output(result)
		if e != nil {
			return e
		}
		if len(roots) == 0 {
			return errors.New("no eligible roots; set paths or auto_roots")
		}
		return nil
	}
	if cmd == "scan" {
		if len(roots) == 0 {
			return errors.New("no eligible roots")
		}
		return scanCommand(c, roots, *duration)
	}
	if cmd != "run" {
		return fmt.Errorf("unknown command: %s", cmd)
	}
	return runDaemon(c, roots, *duration)
}
func output(v any) error { e := json.NewEncoder(os.Stdout); e.SetIndent("", "  "); return e.Encode(v) }
func atomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".ccg-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(append(b, '\n')); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(name, path)
}

type Status struct {
	Time              time.Time      `json:"time"`
	Version           string         `json:"version"`
	PID               int            `json:"pid"`
	Mode              string         `json:"mode"`
	Action            string         `json:"action"`
	IntervalMS        int            `json:"sample_interval_ms"`
	Decision          Decision       `json:"decision"`
	Metrics           Metrics        `json:"metrics"`
	Counters          Counters       `json:"counters"`
	Roots             []string       `json:"roots"`
	WorkerBusy        bool           `json:"worker_busy"`
	WorkerBusySeconds float64        `json:"worker_busy_seconds"`
	Paused            bool           `json:"paused"`
	Degraded          []string       `json:"degraded"`
	Diagnostics       Diagnostics    `json:"diagnostics"`
	Recovery          RecoveryStatus `json:"recovery"`
}

func business(c Config, m *Metrics) {
	if c.BusinessSignal == "" {
		return
	}
	fd, e := syscall.Open(c.BusinessSignal, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		m.Error = "business signal unreadable"
		return
	}
	f := os.NewFile(uintptr(fd), c.BusinessSignal)
	defer f.Close()
	id, e := statFile(f)
	if e != nil || id.Mode&syscall.S_IFMT != syscall.S_IFREG {
		m.Error = "business signal must be a regular file"
		return
	}
	var v struct {
		Time  time.Time `json:"time"`
		P99MS *float64  `json:"p99_ms"`
		Pause bool      `json:"pause"`
	}
	body, readErr := io.ReadAll(io.LimitReader(f, 16385))
	if readErr != nil || len(body) > 16384 {
		m.Error = "business signal too large or unreadable"
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	e = decoder.Decode(&v)
	var extra any
	if e == nil {
		if decoder.Decode(&extra) != io.EOF {
			e = errors.New("trailing business signal data")
		}
	}
	if e != nil || v.Time.IsZero() || time.Since(v.Time) > time.Duration(c.SignalMaxAgeSeconds)*time.Second || time.Until(v.Time) > time.Second {
		m.Error = "business signal invalid/stale"
		return
	}
	if c.MaxBusinessP99MS > 0 && v.P99MS == nil {
		m.Error = "business signal p99_ms missing"
		return
	}
	if v.P99MS != nil && *v.P99MS < 0 {
		m.Error = "business signal p99_ms must be nonnegative"
		return
	}
	m.BusinessP99 = v.P99MS
	m.BusinessPause = v.Pause
}
func runDaemon(c Config, roots []string, duration time.Duration) error {
	lock, e := lockState(c.StateFile)
	if e != nil {
		return e
	}
	defer lock.Close()
	r, e := newMetricReader(c.CgroupPath)
	if e != nil {
		return e
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(c.MaxRSSBytes * 3 / 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, duration)
		defer stop()
	}
	web, webErr := startWeb(ctx, c)
	if webErr != nil {
		return webErr
	}
	if web != nil {
		defer web.close()
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(sigs)
	var allow atomic.Bool
	work := make(chan struct{}, 1)
	results := make(chan Counters, 1)
	eng := newEngine(c, roots)
	go func() {
		defer eng.close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-work:
				stats := eng.work(ctx, &allow)
				select {
				case results <- stats:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	started := time.Now()
	s := Status{Time: started, Version: version, PID: os.Getpid(), Mode: c.Mode, Action: c.Action, Roots: roots, IntervalMS: c.IntervalMS}
	m := r.sample(c)
	oomBaseline := m.OOMKill
	lastLog := time.Time{}
	busySince := time.Time{}
	var recovery recoveryGate
	nextSample := time.Time{}
	nextWork := time.Time{}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			allow.Store(false)
			s.Time = time.Now()
			s.Decision = Decision{State: "STOPPED", Reason: "shutdown"}
			atomicJSON(c.StateFile, s)
			return nil
		case sig := <-sigs:
			if sig == syscall.SIGUSR1 {
				s.Paused = true
				allow.Store(false)
			} else if sig == syscall.SIGUSR2 {
				s.Paused = false
			} else {
				cancel()
			}
		case stats := <-results:
			s.Counters = stats
			roots = stats.DetectedRoots
			s.Roots = roots
			s.WorkerBusy = false
			busySince = time.Time{}
		case now := <-tick.C:
			if !now.Before(nextSample) {
				m = r.sample(c)
				business(c, &m)
				s.Metrics = m
				s.Time = now
				s.Decision = decide(c, m, started, now, s.Paused, oomBaseline)
				s.Degraded = []string{}
				if m.IOPSI == nil {
					s.Degraded = append(s.Degraded, "io_PSI_unavailable")
				}
				if m.MemoryPSI == nil {
					s.Degraded = append(s.Degraded, "memory_PSI_unavailable")
				}
				if !m.FileRefaultRateValid {
					s.Degraded = append(s.Degraded, "file_refault_rate_unavailable")
				}
				s.Degraded = append(s.Degraded, "no_kubelet_summary", "application_heap_shmem_slab_not_reclaimed")
				if len(roots) == 0 {
					s.Decision.Allow = false
					s.Decision.State = "SUSPENDED"
					s.Decision.Reason = "no_eligible_roots"
				}
				if !busySince.IsZero() {
					s.WorkerBusySeconds = now.Sub(busySince).Seconds()
				} else {
					s.WorkerBusySeconds = 0
				}
				if s.WorkerBusySeconds > 5 {
					s.Decision.Allow = false
					s.Decision.State = "SUSPENDED"
					s.Decision.Reason = "worker_slow_or_blocked"
				}
				s.Decision, s.Recovery = recovery.apply(c, s.Decision, now)
				allow.Store(s.Decision.Allow)
				s.Diagnostics = diagnose(c, m, s.Decision)
				if e = atomicJSON(c.StateFile, s); e != nil {
					allow.Store(false)
					return fmt.Errorf("state write failed; stopping: %w", e)
				}
				if web != nil {
					web.publish(s)
				}
				if now.Sub(lastLog) >= time.Duration(c.LogIntervalSeconds)*time.Second {
					b, _ := json.Marshal(s)
					fmt.Println(string(b))
					lastLog = now
				}
				nextSample = now.Add(time.Duration(c.IntervalMS) * time.Millisecond)
			}
			// A single worker. Even if a syscall blocks, never spawn replacements.
			if !now.Before(nextWork) && !s.WorkerBusy && s.Decision.Reason != "self_resource_budget" && s.Decision.State != "CRITICAL" && s.Metrics.Error == "" {
				s.WorkerBusy = true
				busySince = now
				spacing := 200 * time.Millisecond
				if allow.Load() {
					spacing = 20 * time.Millisecond
				}
				nextWork = now.Add(spacing)
				work <- struct{}{}
			}
		}
	}
}
func scanCommand(c Config, roots []string, duration time.Duration) error {
	started := time.Now()
	report := struct {
		Files, Bytes, Resident, Errors int64
		Complete                       bool
		Seconds                        float64
		Error                          string `json:"error,omitempty"`
	}{Complete: true}
	var b budget
	b.last = started
	b.tokens = float64(c.RegionBytes)
	b.cap = float64(c.RegionBytes)
	deadline := time.Time{}
	if duration > 0 {
		deadline = started.Add(duration)
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if !deadline.IsZero() && time.Now().After(deadline) {
				report.Complete = false
				return context.DeadlineExceeded
			}
			if err != nil {
				report.Errors++
				return nil
			}
			if c.excluded(p) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			f, e := safeOpen(p)
			if e != nil {
				report.Errors++
				return nil
			}
			defer f.Close()
			id, e := statFile(f)
			if e != nil || !supportedFS(f) {
				report.Errors++
				return nil
			}
			report.Files++
			report.Bytes += id.Size
			for off := int64(0); off < id.Size; off += c.RegionBytes {
				if !deadline.IsZero() && time.Now().After(deadline) {
					report.Complete = false
					return context.DeadlineExceeded
				}
				n := min64(c.RegionBytes, id.Size-off)
				for {
					if !deadline.IsZero() && time.Now().After(deadline) {
						report.Complete = false
						return context.DeadlineExceeded
					}
					b.refill(time.Now(), float64(c.ScanBytesPerSecond), float64(c.RegionBytes))
					if b.take(float64(n)) {
						break
					}
					time.Sleep(time.Millisecond * 5)
				}
				r, e := resident(f, off, n)
				if e != nil {
					report.Errors++
					report.Error = e.Error()
					break
				}
				report.Resident += r
			}
			return nil
		})
		if err != nil {
			report.Complete = false
			report.Error = err.Error()
			break
		}
	}
	report.Seconds = time.Since(started).Seconds()
	output(report)
	if report.Errors > 0 || !report.Complete {
		return errors.New("scan incomplete; inspect JSON")
	}
	return nil
}
