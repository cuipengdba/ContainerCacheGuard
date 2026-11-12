//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
  containercacheguard compare --baseline baseline.json --candidate candidate.json [--format json|markdown]
  containercacheguard check-config --config config.json
  containercacheguard doctor [--config config.json] [--path /data] [--require-ready]
  containercacheguard report --log-directory /var/log/containercacheguard [--format markdown|json|svg]
  containercacheguard run --config config.json [--mode auto|paths] [--action observe|enforce]
  containercacheguard reclaim-agent --config agent.json [--duration 5m]
  containercacheguard status [--state-file /tmp/containercacheguard/status.json]
  containercacheguard scan --path /data [--scan-bytes-per-second 268435456]
  containercacheguard version

auto 自动发现磁盘数据挂载、常用数据目录和 PGDATA；paths 仅处理 --path 或 paths 配置。
auto默认跳过执行位文件；executable_data_paths可对显式数据根下的指定路径放开此一项规则。
coverage_audit按目录/类型记录扫描/跳过/驻留/建议及候选事件；窗口唯一查询不等于完整覆盖。
pressure_boost显式启用后在真实保护允许时加速至配置上限，需strict+bounded及完整替代信号。
--path 可重复。默认 action=observe，不回收；enforce 才发出文件范围释放建议。
--duration 30s 限制运行时长；默认持续运行。--config 支持完整 JSON 参数，未知参数报错。
run 可覆盖 --cache-target-bytes、--reclaim-bytes-per-second、--cgroup-path、--state-file。
SIGUSR1 暂停新释放，SIGUSR2 恢复，SIGTERM/SIGINT 退出；配置修改需重启。
不提供Web/HTTP服务、不监听端口；通过每日日志和status状态文件查看。
kernel_reclaim默认关闭；显式启用后由受限节点reclaim-agent通过Unix Socket串行执行。
每压力事件至多一次memory.reclaim尝试；覆盖整个目标cgroup，不受路径排除约束。
日志默认每天一个文件，最多30个日历日、合计1GB；目录/天数/容量/时区可配置。
scan 是只读驻留扫描，输出不是热度；可能需要完整遍历，不属于常态轻量监控。
doctor --require-ready采集两个样本，保护输入不齐返回非零；不会启用PSI或关闭保护。
report离线读取每日日志，生成小时统计；--manifest严格绑定多实例来源，支持独立SVG图片。
instance_id标识部署实例；每次run启动生成独立随机run_id，日志/状态带可见cgroup设备与inode。
protection_profile默认strict；limited-output为显式实验策略，仅指定低复用备份产物，
须配置来源绑定的kubelet与业务信号、替代保护和更小预算；缺项即暂停，不能承诺不被驱逐。

常态文件后端只操作已授权路径下支持的普通磁盘文件。不会删除文件、清空应用堆、清理全机缓存、
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
	if cmd == "reclaim-agent" {
		return kernelAgentCommand(args[1:])
	}
	if cmd == "internal-kernel-write" {
		return kernelWriteCommand(args[1:])
	}
	if cmd == "compare" {
		return compareCommand(args[1:])
	}
	if cmd == "report" {
		return reportCommand(args[1:])
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	config := fs.String("config", "", "JSON 配置文件")
	mode := fs.String("mode", "", "auto 或 paths")
	action := fs.String("action", "", "observe 或 enforce")
	cg := fs.String("cgroup-path", "", "目标 memory cgroup 绝对路径")
	state := fs.String("state-file", "", "状态 JSON 文件绝对路径")
	webListen := fs.String("web-listen", "", "已停用，仅兼容旧启动命令；不会监听HTTP端口")
	webTokenFile := fs.String("web-token-file", "", "已停用，仅兼容旧启动命令；不会读取令牌文件")
	var paths pathsFlag
	fs.Var(&paths, "path", "治理路径，可重复（设置后选择 paths 模式）")
	target := fs.Int64("cache-target-bytes", -1, "容器普通文件缓存软预算，字节")
	rate := fs.Int64("reclaim-bytes-per-second", -1, "每秒建议释放范围字节上限")
	scanRate := fs.Int64("scan-bytes-per-second", -1, "每秒驻留查询覆盖字节上限")
	duration := fs.Duration("duration", 0, "运行时长，例如 5m；0 常驻")
	requireReady := fs.Bool("require-ready", false, "仅doctor：双样本保护输入预检，缺项返回非零")
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
	if *requireReady && cmd != "doctor" {
		return errors.New("--require-ready is only supported by doctor")
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
	if cmd == "check-config" {
		return output(map[string]any{"valid": true, "mode": c.Mode, "action": c.Action, "protection_profile": c.profile(), "instance_id": c.InstanceID, "scope": "仅离线配置校验；未读取数据目录、cgroup或外部信号"})
	}
	roots := normalizeRoots(c, c.Paths)
	if c.Mode == "auto" {
		roots = discoverRoots(c)
	}
	if cmd == "doctor" {
		invocationID, idErr := newRunID()
		if idErr != nil {
			return idErr
		}
		r, e := newMetricReader(c.CgroupPath)
		result := map[string]any{"version": version, "architecture": runtime.GOARCH, "page_size": os.Getpagesize(), "uid": os.Geteuid(), "mode": c.Mode, "action": c.Action, "roots": roots, "file_cache_backend": "mincore+fadvise", "other_caches": "diagnostic_only", "node_pressure_source": "host MemAvailable proxy; optional source-bound kubelet JSON", "instance_id": c.InstanceID, "protection_profile": c.profile()}
		result["web_service_enabled"] = false
		result["run_id"] = invocationID
		result["deprecated_config_fields"] = c.deprecatedWebFields()
		var m Metrics
		readerErr := e
		if e != nil {
			result["metrics_error"] = e.Error()
		} else {
			m = r.sample(c)
			if *requireReady {
				time.Sleep(time.Duration(c.IntervalMS) * time.Millisecond)
				m = r.sample(c)
			}
			business(c, &m)
			nodeSignal(c, &m)
			result["metrics"] = m
			result["protection"] = assessProtection(c, m)
			result["signals"] = signals(c, m, time.Now())
			if c.KernelReclaim.Enabled {
				boot, bootErr := readBootID()
				ks := kernelRead(c, r, boot)
				if *requireReady {
					time.Sleep(time.Duration(c.IntervalMS) * time.Millisecond)
					ks = kernelRead(c, r, boot)
				}
				reason := kernelGuard(c, ks, boot, time.Now())
				if bootErr != nil {
					reason = "boot_id_unavailable"
				}
				risk, healthy, deficit := kernelRisk(c, ks, KernelSample{})
				request, estimate := kernelRequest(c, ks, deficit, 1<<30)
				result["kernel_reclaim"] = map[string]any{"read_only": true, "guard_reason": reason, "risk": risk, "healthy": healthy, "estimated_request_bytes": request, "file_estimate_bytes": estimate, "write_permission_verified": false, "sample": ks}
				if *requireReady && reason != "" {
					e = fmt.Errorf("kernel reclaim not ready: %s", reason)
				}
			}
			if m.Error != "" {
				e = fmt.Errorf("metrics unavailable: %s", m.Error)
			}
		}
		pre := preflight(c, m, readerErr, os.ReadFile)
		result["preflight"] = pre
		if err := output(result); err != nil {
			return err
		}
		if e != nil {
			return e
		}
		if len(roots) == 0 {
			return errors.New("no eligible roots; set paths or auto_roots")
		}
		if *requireReady && !pre.ProtectionReady {
			return errors.New("保护输入未就绪；详情见preflight和protection，未关闭任何保护")
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
	KernelReclaim     *KernelStatus     `json:"kernel_reclaim,omitempty"`
	Signals           SignalStatus      `json:"signals"`
	InstanceID        string            `json:"instance_id,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	ProtectionProfile string            `json:"protection_profile,omitempty"`
	Time              time.Time         `json:"time"`
	Version           string            `json:"version"`
	PID               int               `json:"pid"`
	Mode              string            `json:"mode"`
	Action            string            `json:"action"`
	IntervalMS        int               `json:"sample_interval_ms"`
	WebServiceEnabled bool              `json:"web_service_enabled"` // Always false; no HTTP listener exists.
	Decision          Decision          `json:"decision"`
	Metrics           Metrics           `json:"metrics"`
	Counters          Counters          `json:"counters"`
	Roots             []string          `json:"roots"`
	WorkerBusy        bool              `json:"worker_busy"`
	WorkerBusySeconds float64           `json:"worker_busy_seconds"`
	Paused            bool              `json:"paused"`
	Degraded          []string          `json:"degraded"`
	Diagnostics       Diagnostics       `json:"diagnostics"`
	Recovery          RecoveryStatus    `json:"recovery"`
	Protection        *ProtectionStatus `json:"protection,omitempty"`
	Logging           LoggingStatus     `json:"logging"`
}

func business(c Config, m *Metrics) {
	if c.BusinessSignal == "" {
		return
	}
	fail := func(issue string) { m.Error = issue; m.BusinessSignalError = issue }
	var v struct {
		Time     time.Time `json:"time"`
		P99MS    *float64  `json:"p99_ms"`
		Pause    bool      `json:"pause"`
		SourceID string    `json:"source_id"`
	}
	e := readRegularJSON(c.BusinessSignal, 16384, &v, false)
	if e == nil {
		m.BusinessTime = v.Time
	}
	if e != nil || v.Time.IsZero() || time.Since(v.Time) > time.Duration(c.SignalMaxAgeSeconds)*time.Second || time.Until(v.Time) > time.Second {
		fail("business signal invalid/stale")
		return
	}
	if c.MaxBusinessP99MS > 0 && v.P99MS == nil {
		fail("business signal p99_ms missing")
		return
	}
	if v.P99MS != nil && !finiteNonnegative(*v.P99MS) {
		fail("business signal p99_ms must be nonnegative")
		return
	}
	if c.BusinessSourceID != "" && v.SourceID != c.BusinessSourceID {
		fail("business signal source mismatch")
		return
	}
	m.BusinessSourceID, m.BusinessTime = v.SourceID, v.Time
	m.BusinessP99 = v.P99MS
	m.BusinessPause = v.Pause
}
func runDaemon(c Config, roots []string, duration time.Duration) error {
	return runDaemonWithReader(c, roots, duration, newMetricReader)
}

// The factory is an internal test seam; the CLI always uses the real-cgroup
// validating factory above. No flag or configuration can select a fake reader.
func runDaemonWithReader(c Config, roots []string, duration time.Duration, readerFactory func(string) (*metricReader, error)) (runErr error) {
	runID, e := newRunID()
	if e != nil {
		return e
	}
	lock, e := lockState(c.StateFile)
	if e != nil {
		return e
	}
	defer lock.Close()
	var allow atomic.Bool
	var boost atomic.Bool
	logs, e := newDailyLogger(c, os.Stdout, time.Now)
	if e != nil {
		return fmt.Errorf("daily logs unavailable; no advice started: %w", e)
	}
	defer func() {
		allow.Store(false)
		event := map[string]any{"time": time.Now(), "event": "daemon_stopped", "instance_id": c.InstanceID, "run_id": runID, "level": "INFO", "message": "CCG已停止；不接管数据库或业务进程。"}
		if runErr != nil {
			event["error"] = runErr.Error()
			event["level"] = "ERROR"
		}
		if err := logs.WriteJSON(event); runErr == nil && err != nil {
			runErr = err
		}
		if err := logs.Close(); runErr == nil && err != nil {
			runErr = err
		}
	}()
	startEvent := startupLog(c, roots)
	startEvent["instance_id"], startEvent["run_id"], startEvent["protection_profile"] = c.InstanceID, runID, c.profile()
	if e = logs.WriteJSON(startEvent); e != nil {
		return e
	}
	if fields := c.deprecatedWebFields(); len(fields) > 0 {
		if e = logs.WriteJSON(map[string]any{"time": time.Now(), "event": "deprecated_web_settings_ignored", "level": "WARN", "fields": fields, "message": "Web服务已取消；旧Web配置已忽略，不监听端口，也不读取令牌文件。"}); e != nil {
			return e
		}
	}
	r, e := readerFactory(c.CgroupPath)
	if e != nil {
		return e
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(c.MaxRSSBytes * 3 / 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kernelClient := newKernelClient(ctx, c)
	if duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, duration)
		defer stop()
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(sigs)
	work := make(chan struct{}, 1)
	results := make(chan Counters, 1)
	eng := newEngine(c, roots)
	eng.boost = &boost
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
	s := Status{InstanceID: c.InstanceID, RunID: runID, ProtectionProfile: c.profile(), Time: started, Version: version, PID: os.Getpid(), Mode: c.Mode, Action: c.Action, Roots: roots, IntervalMS: c.IntervalMS}
	m := r.sample(c)
	oomBaseline := m.OOMKill
	oomBaselineReady := m.Available["oom_kill_total"]
	lastLog := time.Time{}
	var previousLog *Status
	var previousDecision *Decision
	busySince := time.Time{}
	var recovery recoveryGate
	var external externalGuard
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
			if e = atomicJSON(c.StateFile, s); e != nil {
				return fmt.Errorf("final state write failed: %w", e)
			}
			return logs.WriteJSON(detailedStatusLog(s, previousLog))
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
				// File/state logging can wait for storage. Do not leave an old
				// permission armed while the sampler is waiting for IO.
				allow.Store(false)
				if e = logs.Maintain(); e != nil {
					allow.Store(false)
					return fmt.Errorf("daily log maintenance failed; stopping advice: %w", e)
				}
				m = r.sample(c)
				business(c, &m)
				nodeSignal(c, &m)
				external.check(&m)
				if !oomBaselineReady && m.Available["oom_kill_total"] {
					oomBaseline, oomBaselineReady = m.OOMKill, true
				}
				s.Metrics = m
				now = time.Now()
				s.Time = now
				s.Signals = signals(c, m, now)
				protection := assessProtection(c, m)
				s.Protection = &protection
				s.Logging = logs.Status()
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
				if m.Node == nil {
					s.Degraded = append(s.Degraded, "no_kubelet_summary")
				}
				if c.profile() == "limited-output" {
					s.Degraded = append(s.Degraded, "experimental_limited_output_not_field_accepted")
				}
				s.Degraded = append(s.Degraded, "application_heap_shmem_slab_not_reclaimed")
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
				s.KernelReclaim = kernelClient.sample(c, s.Paused, !s.WorkerBusy, now)
				if s.KernelReclaim != nil && s.KernelReclaim.HoldFile && c.Action == "enforce" {
					s.Decision.Allow = false
					s.Decision.State = "SUSPENDED"
					s.Decision.Reason = "kernel_reclaim_owns_target_or_unavailable"
				}
				// A guard can clear Allow after policy evaluation. It must also
				// clear the rate boost; the worker has no separate permission.
				if !s.Decision.Allow {
					s.Decision.PressureBoost = false
				}
				if previousDecision == nil || *previousDecision != s.Decision {
					if e = logs.WriteJSON(guardTransitionLog(s, previousDecision)); e != nil {
						allow.Store(false)
						return e
					}
					copyDecision := s.Decision
					previousDecision = &copyDecision
				}
				s.Diagnostics = diagnose(c, m, s.Decision)
				if e = atomicJSON(c.StateFile, s); e != nil {
					allow.Store(false)
					return fmt.Errorf("state write failed; stopping: %w", e)
				}
				changed := previousLog == nil || s.Decision != previousLog.Decision || s.Paused != previousLog.Paused || s.Metrics.Error != previousLog.Metrics.Error
				if now.Sub(lastLog) >= time.Duration(c.LogIntervalSeconds)*time.Second || (changed && now.Sub(lastLog) >= time.Second) {
					if e = logs.WriteJSON(detailedStatusLog(s, previousLog)); e != nil {
						allow.Store(false)
						return fmt.Errorf("daily log write failed; stopping advice: %w", e)
					}
					lastLog = now
					logged := s
					previousLog = &logged
				}
				boost.Store(s.Decision.Allow && s.Decision.PressureBoost)
				allow.Store(s.Decision.Allow && adviceFresh(c, m, time.Now()))
				nextSample = now.Add(time.Duration(c.IntervalMS) * time.Millisecond)
			}
			if !adviceFresh(c, s.Metrics, time.Now()) {
				allow.Store(false)
			}
			// A single worker. Even if a syscall blocks, never spawn replacements.
			if !now.Before(nextWork) && !s.WorkerBusy && s.Decision.Reason != "self_resource_budget" && s.Decision.State != "CRITICAL" && s.Metrics.Error == "" &&
				s.Metrics.Available["self_rss_bytes"] && s.Metrics.Available["self_cpu_percent"] &&
				(c.Action == "observe" || (s.Protection != nil && s.Protection.Ready)) {
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
