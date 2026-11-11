//go:build linux && (amd64 || arm64)

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

const reportLineLimit = 4 * 1024 * 1024
const reportBucketLimit = 4096

type HourReport struct {
	InstanceID          string             `json:"instance_id,omitempty"`
	RunIDs              []string           `json:"run_ids,omitempty"`
	CgroupDevice        uint64             `json:"cgroup_device,omitempty"`
	CgroupInode         uint64             `json:"cgroup_inode,omitempty"`
	NodeAvailableMin    *int64             `json:"kubelet_memory_available_min_bytes"`
	NodePressureSamples int                `json:"node_memory_pressure_samples"`
	Hour                string             `json:"hour"`
	Cgroup              string             `json:"cgroup"`
	CgroupVersion       int                `json:"cgroup_version"`
	Samples             int                `json:"samples"`
	UsableMemorySamples int                `json:"usable_memory_samples"`
	Actions             map[string]int     `json:"action_samples"`
	States              map[string]int     `json:"state_samples"`
	MissingGuards       map[string]int     `json:"missing_guard_samples"`
	ObservedSeconds     float64            `json:"observed_interval_seconds"`
	AllowedSeconds      float64            `json:"advice_allowed_interval_seconds"`
	StateSeconds        map[string]float64 `json:"state_interval_seconds"`
	UnbridgedIntervals  int                `json:"unbridged_intervals"`
	CounterIntervals    int                `json:"valid_counter_intervals"`
	AdviceBytes         int64              `json:"advice_bytes_delta"`
	NetReductionBytes   int64              `json:"observed_net_reduction_bytes_delta"`
	ReappearedBytes     int64              `json:"observed_reappeared_bytes_delta"`
	Errors              uint64             `json:"errors_delta"`
	OrdinaryPeak        *int64             `json:"ordinary_file_peak_bytes"`
	AnonPeak            *int64             `json:"anon_peak_bytes"`
	ShmemPeak           *int64             `json:"shmem_peak_bytes"`
	SelfRSSPeak         *int64             `json:"self_rss_peak_bytes"`
	HostAvailableMin    *int64             `json:"host_available_min_bytes"`
	IOPeak              *float64           `json:"io_peak_bytes_per_second"`
	CPUPeak             *float64           `json:"self_cpu_peak_percent"`
}
type OfflineReport struct {
	Version       string        `json:"reporter_version"`
	Instance      string        `json:"instance"`
	Timezone      string        `json:"timezone"`
	MaxGapSeconds float64       `json:"max_gap_seconds"`
	SourceFiles   int           `json:"source_files"`
	IgnoredEvents int           `json:"ignored_non_status_events"`
	OutOfOrder    int           `json:"duplicate_or_out_of_order_samples"`
	Hours         []*HourReport `json:"hours"`
	Notes         []string      `json:"notes"`
}
type reportOptions struct {
	location         *time.Location
	from, until      time.Time
	maxGap           time.Duration
	instance         string
	expectedInstance string
}
type reportAggregator struct {
	report      OfflineReport
	options     reportOptions
	buckets     map[string]*HourReport
	previous    map[string]Status
	generations map[string]uint64
	generation  uint64
}

func newReportAggregator(o reportOptions) *reportAggregator {
	return &reportAggregator{options: o, buckets: map[string]*HourReport{}, previous: map[string]Status{}, generations: map[string]uint64{}, report: OfflineReport{
		Version: version, Instance: o.instance, Timezone: o.location.String(), MaxGapSeconds: o.maxGap.Seconds(), Hours: []*HourReport{},
		Notes: []string{
			"仅包含有日志或有效短间隔的小时；没有记录的小时不能视为健康。峰值是日志采样峰值，不是连续真实峰值。",
			"短间隔秒数按前一条日志状态保持到下一条估算；超出max-gap、进程/版本变化、重复和乱序不补算。",
			"累计计数只统计同进程、同版本、同cgroup的连续非回退差值，归入后一条样本所在小时；首样本和空窗不追算。",
			"建议量、区域复查净下降和重新驻留分别统计；并发下不能把净下降全部归因于CCG。",
			"观察模式、暂停、缺样或未记录错误都不能证明业务安全；SQL尾延迟、备份恢复和K8s事件需独立验证。",
			"一次report只提供同一实例的日志；cgroup路径跨容器或主机可能重名，不能混合不同实例后依靠路径自动区分。",
		},
	}}
}
func reportScope(s Status) string {
	return fmt.Sprintf("%s:%d:%d:%d:%s", s.InstanceID, s.Metrics.CgroupDevice, s.Metrics.CgroupInode, s.Metrics.Version, s.Metrics.Cgroup)
}
func (a *reportAggregator) bucket(s Status, at time.Time) (*HourReport, error) {
	hour := at.In(a.options.location).Format("2006-01-02T15:00:00Z07:00")
	key := reportScope(s) + "\x00" + hour
	if h := a.buckets[key]; h != nil {
		return h, nil
	}
	if len(a.buckets) >= reportBucketLimit {
		return nil, errors.New("小时分组超过4096，请用--from/--until缩小范围")
	}
	h := &HourReport{InstanceID: s.InstanceID, CgroupDevice: s.Metrics.CgroupDevice, CgroupInode: s.Metrics.CgroupInode, Hour: hour, Cgroup: s.Metrics.Cgroup, CgroupVersion: s.Metrics.Version, Actions: map[string]int{}, States: map[string]int{}, MissingGuards: map[string]int{}, StateSeconds: map[string]float64{}}
	a.buckets[key] = h
	a.report.Hours = append(a.report.Hours, h)
	return h, nil
}
func peakInt(p **int64, v int64) {
	if v >= 0 && (*p == nil || v > **p) {
		n := v
		*p = &n
	}
}
func minIntValue(p **int64, v int64) {
	if v >= 0 && (*p == nil || v < **p) {
		n := v
		*p = &n
	}
}
func peakFloat(p **float64, v float64) {
	if finiteNonnegative(v) && (*p == nil || v > **p) {
		n := v
		*p = &n
	}
}
func (a *reportAggregator) add(s Status) error {
	if a.options.expectedInstance != "" && (s.InstanceID != a.options.expectedInstance || s.RunID == "") {
		return errors.New("manifest实例与日志身份不符，或缺少run_id；不能合并旧日志")
	}
	if s.InstanceID != "" && !identifier.MatchString(s.InstanceID) {
		return errors.New("日志instance_id无效")
	}
	if s.RunID != "" && !runIdentifier.MatchString(s.RunID) {
		return errors.New("日志run_id无效")
	}
	if s.Time.IsZero() || s.PID <= 0 || s.Version == "" || s.Metrics.Cgroup == "" || (s.Metrics.Version != 1 && s.Metrics.Version != 2) {
		return errors.New("状态记录缺少时间、PID、版本或cgroup身份")
	}
	if len(s.Metrics.Cgroup) > 4096 || len(s.Version) > 128 {
		return errors.New("状态身份字段过长")
	}
	if s.Action != "observe" && s.Action != "enforce" {
		return errors.New("状态action必须为observe或enforce")
	}
	if s.Action == "observe" && s.Decision.Allow {
		return errors.New("观察记录不能声明允许回收")
	}
	if s.Counters.AdviceBytes < 0 || s.Counters.NetReductionBytes < 0 || s.Counters.ReappearedBytes < 0 {
		return errors.New("状态累计计数不能为负数")
	}
	if !a.options.from.IsZero() && s.Time.Before(a.options.from) {
		return nil
	}
	if !a.options.until.IsZero() && !s.Time.Before(a.options.until) {
		return nil
	}
	scope := reportScope(s)
	prev, exists := a.previous[scope]
	if exists && !s.Time.After(prev.Time) {
		a.report.OutOfOrder++
		return nil
	}
	h, err := a.bucket(s, s.Time)
	if err != nil {
		return err
	}
	if s.RunID != "" {
		seen := false
		for _, id := range h.RunIDs {
			if id == s.RunID {
				seen = true
				break
			}
		}
		if !seen {
			if len(h.RunIDs) >= 128 {
				return errors.New("每小时运行身份超过128，请缩小报告范围或检查日志来源")
			}
			h.RunIDs = append(h.RunIDs, s.RunID)
		}
	}
	h.Samples++
	h.Actions[s.Action]++
	state := reportState(s.Decision.State)
	h.States[state]++
	if s.Protection != nil {
		for _, name := range s.Protection.Missing {
			if len(name) > 128 {
				name = "OTHER"
			}
			if len(h.MissingGuards) < 64 || h.MissingGuards[name] > 0 {
				h.MissingGuards[name]++
			}
		}
	}
	if logSampleUsable(s) {
		m := s.Metrics
		if m.Node != nil && m.Node.AvailableBytes != nil {
			minIntValue(&h.NodeAvailableMin, *m.Node.AvailableBytes)
		}
		if m.Node != nil && m.Node.MemoryPressure != nil && *m.Node.MemoryPressure {
			h.NodePressureSamples++
		}
		if m.Available["file_bytes"] && m.Available["shmem_bytes"] && m.File >= 0 && m.Shmem >= 0 {
			peakInt(&h.OrdinaryPeak, max64(0, m.File-m.Shmem))
			h.UsableMemorySamples++
		}
		if m.Available["anon_bytes"] {
			peakInt(&h.AnonPeak, m.Anon)
		}
		if m.Available["shmem_bytes"] {
			peakInt(&h.ShmemPeak, m.Shmem)
		}
		if m.Available["self_rss_bytes"] {
			peakInt(&h.SelfRSSPeak, m.RSS)
		}
		if m.Available["host_memavailable_bytes"] {
			minIntValue(&h.HostAvailableMin, m.NodeAvailable)
		}
		if m.Available["io_bytes_per_second"] {
			peakFloat(&h.IOPeak, m.IOBytesPerSecond)
		}
		if m.Available["self_cpu_percent"] {
			peakFloat(&h.CPUPeak, m.CPUPercent)
		}
	}
	if exists {
		gap := s.Time.Sub(prev.Time)
		continuous := gap > 0 && gap <= a.options.maxGap && s.PID == prev.PID && s.Version == prev.Version && s.RunID == prev.RunID && a.generations[scope] == a.generation
		if continuous {
			// Split time at local-hour boundaries; do not invent duration for gaps.
			for cur := prev.Time; cur.Before(s.Time); {
				local := cur.In(a.options.location)
				step := time.Hour - time.Duration(local.Minute())*time.Minute - time.Duration(local.Second())*time.Second - time.Duration(local.Nanosecond())
				end := cur.Add(step)
				if end.After(s.Time) {
					end = s.Time
				}
				part, e := a.bucket(prev, cur)
				if e != nil {
					return e
				}
				seconds := end.Sub(cur).Seconds()
				part.ObservedSeconds += seconds
				part.StateSeconds[reportState(prev.Decision.State)] += seconds
				if prev.Action == "enforce" && prev.Decision.Allow {
					part.AllowedSeconds += seconds
				}
				cur = end
			}
			x, y := s.Counters, prev.Counters
			if x.AdviceBytes >= y.AdviceBytes && x.NetReductionBytes >= y.NetReductionBytes && x.ReappearedBytes >= y.ReappearedBytes && x.Errors >= y.Errors {
				advice, reduction, again, errs := x.AdviceBytes-y.AdviceBytes, x.NetReductionBytes-y.NetReductionBytes, x.ReappearedBytes-y.ReappearedBytes, x.Errors-y.Errors
				if h.AdviceBytes > math.MaxInt64-advice || h.NetReductionBytes > math.MaxInt64-reduction || h.ReappearedBytes > math.MaxInt64-again || h.Errors > math.MaxUint64-errs {
					return errors.New("小时累计计数溢出")
				}
				h.CounterIntervals++
				h.AdviceBytes += advice
				h.NetReductionBytes += reduction
				h.ReappearedBytes += again
				h.Errors += errs
			} else {
				h.UnbridgedIntervals++
			}
		} else {
			h.UnbridgedIntervals++
		}
	}
	a.previous[scope] = s
	a.generations[scope] = a.generation
	return nil
}
func reportState(s string) string {
	switch s {
	case "OBSERVE", "WARMUP", "IDLE", "RECLAIM", "CONTROL", "CONTROL_CRITICAL", "BACKOFF", "SUSPENDED", "CRITICAL", "STOPPED":
		return s
	}
	return "OTHER"
}
func (a *reportAggregator) read(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), reportLineLimit)
	line := 0
	for scanner.Scan() {
		line++
		b := scanner.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		var envelope struct {
			Event   string           `json:"event"`
			Time    time.Time        `json:"time"`
			Metrics *json.RawMessage `json:"metrics"`
		}
		if err := json.Unmarshal(b, &envelope); err != nil {
			return fmt.Errorf("第%d行不是完整JSON：%w", line, err)
		}
		if envelope.Event != "status" && !(envelope.Event == "" && envelope.Metrics != nil) {
			a.report.IgnoredEvents++
			if envelope.Event == "daemon_started" || envelope.Event == "daemon_stopped" {
				if a.generation == math.MaxUint64 {
					return errors.New("日志生命周期计数溢出")
				}
				a.generation++
			}
			continue
		}
		var s Status
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("第%d行状态无效：%w", line, err)
		}
		if err := a.add(s); err != nil {
			return fmt.Errorf("第%d行：%w", line, err)
		}
	}
	return scanner.Err()
}
func (a *reportAggregator) finish() (OfflineReport, error) {
	if len(a.report.Hours) == 0 {
		return a.report, errors.New("选定范围没有有效状态记录，不能生成健康结论")
	}
	sort.Slice(a.report.Hours, func(i, j int) bool {
		x, y := a.report.Hours[i], a.report.Hours[j]
		if x.Hour != y.Hour {
			return x.Hour < y.Hour
		}
		return x.Cgroup < y.Cgroup
	})
	return a.report, nil
}
func reportFiles(dir string, files []string) ([]string, error) {
	if len(files) > 0 && dir != "" {
		return nil, errors.New("--log-file和--log-directory不能同时使用")
	}
	if dir != "" {
		f, err := safeOpen(dir)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		id, err := statFile(f)
		if err != nil || id.Mode&0170000 != 0040000 {
			return nil, errors.New("日志目录必须为真实目录")
		}
		entries, err := f.Readdirnames(10001)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(entries) > 10000 {
			return nil, errors.New("日志目录条目超过10000")
		}
		for _, name := range entries {
			if managedLogName(name) {
				files = append(files, filepath.Join(dir, name))
			}
		}
	}
	if len(files) == 0 || len(files) > 1024 {
		return nil, errors.New("需要1至1024个日志文件")
	}
	result := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, name := range files {
		p, err := filepath.Abs(name)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, errors.New("重复输入文件")
		}
		seen[p] = true
		result = append(result, p)
	}
	sort.Strings(result)
	return result, nil
}
func managedLogName(s string) bool {
	if !strings.HasPrefix(s, "containercacheguard-") || !strings.HasSuffix(s, ".log") {
		return false
	}
	_, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(s, "containercacheguard-"), ".log"))
	return err == nil
}
func markdownReport(r OfflineReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ContainerCacheGuard 小时统计\n\n实例：%s；时区：%s；来源文件：%d。\n\n", markdownCell(r.Instance), r.Timezone, r.SourceFiles)
	b.WriteString("| 小时 / 实例 | 普通文件缓存采样峰值 MiB | 匿名内存采样峰值 MiB | 宿主可用内存采样最低 MiB | 已观察间隔秒 | 允许回收估算秒 | 建议量 MiB | 区域复查净下降 MiB | 重新驻留 MiB | 模式/状态样本 |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	integer := func(v *int64) string {
		if v == nil {
			return "未知"
		}
		return fmt.Sprintf("%.2f", float64(*v)/float64(MiB))
	}
	for _, h := range r.Hours {
		action, _ := json.Marshal(h.Actions)
		state, _ := json.Marshal(h.States)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.2f | %.2f | %.2f | %.2f | %.2f | %s / %s |\n", h.Hour+" / "+markdownCell(h.InstanceID), integer(h.OrdinaryPeak), integer(h.AnonPeak), integer(h.HostAvailableMin), h.ObservedSeconds, h.AllowedSeconds, float64(h.AdviceBytes)/float64(MiB), float64(h.NetReductionBytes)/float64(MiB), float64(h.ReappearedBytes)/float64(MiB), markdownCell(string(action)), markdownCell(string(state)))
	}
	b.WriteString("\n")
	for _, h := range r.Hours {
		fmt.Fprintf(&b, "- %s，cgroup：%s；保护缺项样本：%s；未衔接间隔：%d。\n", h.Hour, markdownCell(h.Cgroup), jsonText(h.MissingGuards), h.UnbridgedIntervals)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "忽略非状态事件%d条；重复/乱序状态%d条。\n\n", r.IgnoredEvents, r.OutOfOrder)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", note)
	}
	return b.String()
}
func markdownCell(s string) string {
	return strings.NewReplacer("|", "&#124;", "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;").Replace(s)
}
func jsonText(v any) string { b, _ := json.Marshal(v); return markdownCell(string(b)) }
func reportCommand(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	config := fs.String("config", "", "从配置读取默认日志目录，不采集cgroup")
	dir := fs.String("log-directory", "", "每日文件日志目录，不递归扫描")
	var files pathsFlag
	fs.Var(&files, "log-file", "输入JSONL日志，可重复；按文件名排序")
	format := fs.String("format", "markdown", "markdown、json或svg")
	manifest := fs.String("manifest", "", "多实例来源清单JSON，严格绑定instance_id；不能混入旧版无身份日志")
	outputPath := fs.String("output", "", "输出文件；默认标准输出")
	zone := fs.String("timezone", "Asia/Shanghai", "小时分组时区")
	from := fs.String("from", "", "包含的起点，RFC3339")
	until := fs.String("until", "", "不包含的终点，RFC3339")
	maxGap := fs.Duration("max-gap", 30*time.Second, "允许衔接的最大日志间隔，100ms至1h")
	instance := fs.String("instance", "未命名实例", "报表实例名称，仅作标签")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("report不接受位置参数")
	}
	if *format != "json" && *format != "markdown" && *format != "svg" {
		return errors.New("format必须为json、markdown或svg")
	}
	if *maxGap < 100*time.Millisecond || *maxGap > time.Hour {
		return errors.New("max-gap须在100ms至1h之间")
	}
	if len(*instance) > 256 {
		return errors.New("instance长度超过256字节")
	}
	location, err := time.LoadLocation(*zone)
	if err != nil {
		return err
	}
	o := reportOptions{location: location, maxGap: *maxGap, instance: *instance}
	if *from != "" {
		o.from, err = time.Parse(time.RFC3339Nano, *from)
		if err != nil {
			return err
		}
	}
	if *until != "" {
		o.until, err = time.Parse(time.RFC3339Nano, *until)
		if err != nil {
			return err
		}
	}
	if !o.from.IsZero() && !o.until.IsZero() && !o.until.After(o.from) {
		return errors.New("until必须晚于from")
	}
	if *manifest == "" && len(files) == 0 && *dir == "" {
		c, e := loadConfig(*config)
		if e != nil {
			return e
		}
		*dir = c.logDirectory()
	}
	if *dir != "" {
		*dir, err = filepath.Abs(*dir)
		if err != nil {
			return err
		}
	}
	var paths []string
	var result OfflineReport
	if *manifest != "" {
		if *dir != "" || len(files) > 0 || *config != "" {
			return errors.New("manifest不能与config/log-directory/log-file混用")
		}
		result, paths, err = manifestReport(*manifest, o)
	} else {
		paths, err = reportFiles(*dir, files)
		if err == nil {
			result, err = readReportPaths(paths, o)
		}
	}
	if err != nil {
		return err
	}
	var body []byte
	if *format == "json" {
		body, err = json.MarshalIndent(result, "", "  ")
		body = append(body, '\n')
	} else if *format == "svg" {
		body = []byte(svgReport(result))
	} else {
		body = []byte(markdownReport(result))
	}
	if err != nil {
		return err
	}
	if *outputPath == "" {
		_, err = os.Stdout.Write(body)
		return err
	}
	dest, err := filepath.Abs(*outputPath)
	if err != nil {
		return err
	}
	if managedLogName(filepath.Base(dest)) {
		return errors.New("不能将报表写为受管每日日志文件")
	}
	if *manifest != "" {
		if p, e := filepath.Abs(*manifest); e == nil && p == dest {
			return errors.New("报表不能覆盖来源清单")
		}
	}
	for _, p := range paths {
		if dest == p {
			return errors.New("报表不能覆盖输入日志")
		}
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".ccg-report-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}
