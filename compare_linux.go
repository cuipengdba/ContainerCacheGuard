//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type CompareLimits struct {
	MinCoverage        float64 `json:"min_coverage"`
	MinFileImprovement float64 `json:"min_file_improvement_percent"`
	MaxIOIncrease      float64 `json:"max_io_increase_percent"`
	MaxP99Increase     float64 `json:"max_p99_increase_percent"`
	MaxP99MS           float64 `json:"max_business_p99_ms"`
	MaxCPU             float64 `json:"max_self_cpu_average_percent"`
	MaxRSS             int64   `json:"max_self_rss_bytes"`
}

func defaultCompareLimits() CompareLimits { return CompareLimits{.9, 20, 10, 5, 100, 3, 128 * MiB} }

type CompareSummary struct {
	Samples                                                                                              int                `json:"samples"`
	FilePeak                                                                                             *int64             `json:"ordinary_file_peak_bytes"`
	FileAverage                                                                                          *float64           `json:"ordinary_file_time_weighted_average_bytes"`
	IOAverage                                                                                            *float64           `json:"visible_cgroup_io_average_bytes_per_second"`
	CPUAverage                                                                                           *float64           `json:"self_cpu_time_weighted_average_percent"`
	RSSPeak                                                                                              *int64             `json:"self_rss_peak_bytes"`
	P99Peak                                                                                              *float64           `json:"business_p99_sample_max_ms"`
	Coverage                                                                                             map[string]float64 `json:"coverage_ratios"`
	samplesActions                                                                                       map[string]int
	profiles, nodes, business, cgroups                                                                   map[string]bool
	fileSeconds, fileIntegral, cpuSeconds, cpuIntegral, ioSeconds, ioBytes, businessSeconds, nodeSeconds float64
	advice, allowed                                                                                      float64
	missing, unsafe, operationErrors                                                                     bool
}
type CompareCheck struct {
	Name  string   `json:"name"`
	State string   `json:"state"`
	Value *float64 `json:"value,omitempty"`
	Limit float64  `json:"limit"`
}
type CompareRow struct {
	InstanceID string          `json:"instance_id"`
	Status     string          `json:"status"`
	Reasons    []string        `json:"reasons"`
	Baseline   *CompareSummary `json:"baseline"`
	Candidate  *CompareSummary `json:"candidate"`
	Checks     []CompareCheck  `json:"checks"`
}
type Comparison struct {
	Version string        `json:"version"`
	Status  string        `json:"status"`
	Limits  CompareLimits `json:"thresholds"`
	Reasons []string      `json:"reasons"`
	Rows    []CompareRow  `json:"instances"`
	Scope   string        `json:"scope"`
}

func compareReport(r OfflineReport) (map[string]*CompareSummary, error) {
	if len(r.Hours) == 0 || len(r.Hours) > reportBucketLimit {
		return nil, errors.New("报告小时分组数量无效")
	}
	seconds := r.WindowEnd.Sub(r.WindowStart).Seconds()
	if r.WindowStart.IsZero() || r.WindowEnd.IsZero() || seconds <= 0 || seconds > 31*24*3600 {
		return nil, errors.New("比较报告必须声明1ns..31天有效from/until窗口")
	}
	subjects := map[string]*CompareSummary{}
	seen := map[string]bool{}
	for _, h := range r.Hours {
		if h == nil || !identifier.MatchString(h.InstanceID) || h.Samples < 1 || h.Samples > 100000000 || h.Cgroup == "" || len(h.Cgroup) > 4096 || (h.CgroupVersion != 1 && h.CgroupVersion != 2) {
			return nil, errors.New("小时分组身份/样本无效；旧无身份日志不能比较")
		}
		hour, e := time.Parse(time.RFC3339, h.Hour)
		if e != nil || hour.Minute() != 0 || hour.Second() != 0 || !hour.Before(r.WindowEnd) || !hour.Add(time.Hour).After(r.WindowStart) {
			return nil, errors.New("小时分组不在声明窗口")
		}
		key := fmt.Sprintf("%s:%d:%d:%d:%s:%s", h.InstanceID, h.CgroupVersion, h.CgroupDevice, h.CgroupInode, h.Cgroup, h.Hour)
		if seen[key] {
			return nil, errors.New("小时分组重复")
		}
		seen[key] = true
		for _, v := range []float64{h.ObservedSeconds, h.AllowedSeconds, h.FileByteSeconds, h.FileCoveredSeconds, h.CPUPercentSeconds, h.CPUCoveredSeconds, h.IOCoveredSeconds, h.BusinessCoveredSeconds, h.NodeCoveredSeconds} {
			if !finiteNonnegative(v) {
				return nil, errors.New("报告含负值或非有限统计")
			}
		}
		if h.ObservedSeconds > 3600.001 || h.AllowedSeconds > h.ObservedSeconds+.001 || h.IOBytesDelta < 0 || h.AdviceBytes < 0 || h.NetReductionBytes < 0 || h.ReappearedBytes < 0 {
			return nil, errors.New("小时统计超出有效范围")
		}
		for _, v := range []float64{h.FileCoveredSeconds, h.CPUCoveredSeconds, h.IOCoveredSeconds, h.BusinessCoveredSeconds, h.NodeCoveredSeconds} {
			if v > h.ObservedSeconds+.001 {
				return nil, errors.New("指标覆盖超过已观察时长")
			}
		}
		for _, v := range []*int64{h.OrdinaryPeak, h.SelfRSSPeak} {
			if v != nil && *v < 0 {
				return nil, errors.New("采样峰值为负")
			}
		}
		if h.BusinessP99Peak != nil && !finiteNonnegative(*h.BusinessP99Peak) {
			return nil, errors.New("业务p99无效")
		}
		x := subjects[h.InstanceID]
		if x == nil {
			x = &CompareSummary{Coverage: map[string]float64{}, samplesActions: map[string]int{}, profiles: map[string]bool{}, nodes: map[string]bool{}, business: map[string]bool{}, cgroups: map[string]bool{}}
			subjects[h.InstanceID] = x
		}
		if len(subjects) > 16 {
			return nil, errors.New("比较最多16实例")
		}
		if len(h.RunIDs) > 128 {
			return nil, errors.New("运行标识超过128")
		}
		if len(h.RunIDs) == 0 {
			x.missing = true
		}
		for _, id := range h.RunIDs {
			if !runIdentifier.MatchString(id) {
				return nil, errors.New("运行标识无效")
			}
		}
		if h.NodePressureSamples < 0 || h.NodePressureSamples > h.Samples {
			return nil, errors.New("节点压力计数无效")
		}
		if h.NodePressureSamples > 0 {
			x.unsafe = true
		}
		sum := 0
		for action, count := range h.Actions {
			if (action != "observe" && action != "enforce") || count < 0 || count > h.Samples {
				return nil, errors.New("动作计数无效")
			}
			sum += count
			x.samplesActions[action] += count
		}
		if sum != h.Samples {
			return nil, errors.New("动作计数与样本不符")
		}
		sum = 0
		for state, count := range h.States {
			if count < 0 || count > h.Samples {
				return nil, errors.New("状态计数无效")
			}
			sum += count
			if count > 0 && (state == "CRITICAL" || state == "SUSPENDED" || state == "STOPPED" || reportState(state) == "OTHER") {
				x.unsafe = true
			}
		}
		if sum != h.Samples {
			return nil, errors.New("状态计数与样本不符")
		}
		sum = 0
		for p, count := range h.Profiles {
			if (p != "strict" && p != "limited-output") || count < 0 || count > h.Samples {
				return nil, errors.New("profile计数无效")
			}
			sum += count
			if count > 0 {
				x.profiles[p] = true
			}
		}
		if sum != h.Samples {
			x.missing = true
		}
		if len(h.NodeIDs) > 16 || len(h.BusinessSources) > 16 || len(h.MissingGuards) > 64 {
			return nil, errors.New("来源/缺项列表过大")
		}
		for _, id := range h.NodeIDs {
			if !identifier.MatchString(id) {
				return nil, errors.New("节点身份无效")
			}
			x.nodes[id] = true
		}
		for _, id := range h.BusinessSources {
			if !identifier.MatchString(id) {
				return nil, errors.New("业务来源无效")
			}
			x.business[id] = true
		}
		for _, count := range h.MissingGuards {
			if count < 0 || count > h.Samples {
				return nil, errors.New("缺项计数无效")
			}
			if count > 0 {
				x.missing = true
			}
		}
		if h.Errors > 0 {
			x.operationErrors = true
		}
		x.cgroups[fmt.Sprintf("%d:%d:%d:%s", h.CgroupVersion, h.CgroupDevice, h.CgroupInode, h.Cgroup)] = true
		x.Samples += h.Samples
		x.fileSeconds += h.FileCoveredSeconds
		x.fileIntegral += h.FileByteSeconds
		x.cpuSeconds += h.CPUCoveredSeconds
		x.cpuIntegral += h.CPUPercentSeconds
		x.ioSeconds += h.IOCoveredSeconds
		x.ioBytes += float64(h.IOBytesDelta)
		x.businessSeconds += h.BusinessCoveredSeconds
		x.nodeSeconds += h.NodeCoveredSeconds
		x.advice += float64(h.AdviceBytes)
		x.allowed += h.AllowedSeconds
		if h.OrdinaryPeak != nil {
			peakInt(&x.FilePeak, *h.OrdinaryPeak)
		}
		if h.SelfRSSPeak != nil {
			peakInt(&x.RSSPeak, *h.SelfRSSPeak)
		}
		if h.BusinessP99Peak != nil {
			peakFloat(&x.P99Peak, *h.BusinessP99Peak)
		}
	}
	for _, x := range subjects {
		for k, v := range map[string]float64{"file": x.fileSeconds, "cpu": x.cpuSeconds, "io": x.ioSeconds, "business": x.businessSeconds, "node": x.nodeSeconds} {
			if !finiteNonnegative(v) || v > seconds+.001 {
				return nil, errors.New("累计覆盖超出窗口（可能重复容器/分组）")
			}
			x.Coverage[k] = v / seconds
		}
		if !finiteNonnegative(x.fileIntegral) || !finiteNonnegative(x.cpuIntegral) || !finiteNonnegative(x.ioBytes) {
			return nil, errors.New("累计统计溢出")
		}
		ratio := func(n, d float64) *float64 {
			if d <= 0 {
				return nil
			}
			v := n / d
			return &v
		}
		x.FileAverage = ratio(x.fileIntegral, x.fileSeconds)
		x.CPUAverage = ratio(x.cpuIntegral, x.cpuSeconds)
		x.IOAverage = ratio(x.ioBytes, x.ioSeconds)
	}
	return subjects, nil
}
func equalIdentity(a, b map[string]bool) bool {
	if len(a) != 1 || len(b) != 1 {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
func compareTelemetry(base, candidate OfflineReport, l CompareLimits) (Comparison, error) {
	out := Comparison{Version: version, Status: "insufficient_evidence", Limits: l, Rows: []CompareRow{}, Reasons: []string{}, Scope: "仅判断匹配日志中的遥测门槛；非因果证明，非完整备份/恢复、等压、K8s事件或零驱逐验收。"}
	if base.SchemaVersion != 2 || candidate.SchemaVersion != 2 {
		out.Reasons = append(out.Reasons, "需要rc.15 schema_version=2报告，旧报告没有覆盖/积分证据")
		return out, nil
	}
	a, e := compareReport(base)
	if e != nil {
		return out, e
	}
	b, e := compareReport(candidate)
	if e != nil {
		return out, e
	}
	if candidate.WindowStart.Before(base.WindowEnd) {
		out.Reasons = append(out.Reasons, "候选窗口须在基线结束后，不能重叠或倒置")
		return out, nil
	}
	if base.WindowEnd.Sub(base.WindowStart) != candidate.WindowEnd.Sub(candidate.WindowStart) {
		out.Reasons = append(out.Reasons, "基线和候选窗口长度不同")
		return out, nil
	}
	ids := map[string]bool{}
	for id := range a {
		ids[id] = true
	}
	for id := range b {
		ids[id] = true
	}
	ordered := []string{}
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	out.Status = "telemetry_thresholds_met"
	for _, id := range ordered {
		x, y := a[id], b[id]
		row := CompareRow{InstanceID: id, Status: "insufficient_evidence", Reasons: []string{}, Baseline: x, Candidate: y, Checks: []CompareCheck{}}
		missing := func(reason string) { row.Reasons = append(row.Reasons, reason) }
		if x == nil || y == nil {
			missing("两份报告实例集合不同")
		} else {
			if x.samplesActions["enforce"] != 0 || y.samplesActions["observe"] != 0 {
				missing("基线须全部observe，候选须全部enforce")
			}
			if x.Samples < 2 || y.Samples < 2 || x.FilePeak == nil || y.FilePeak == nil || x.FileAverage == nil || y.FileAverage == nil || x.CPUAverage == nil || y.CPUAverage == nil || x.RSSPeak == nil || y.RSSPeak == nil || x.P99Peak == nil || y.P99Peak == nil || x.IOAverage == nil || y.IOAverage == nil {
				missing("关键遥测或有效区间不足")
			}
			for _, k := range []string{"file", "cpu", "io", "business", "node"} {
				if x.Coverage[k] < l.MinCoverage || y.Coverage[k] < l.MinCoverage {
					missing(k + "有效覆盖比例不足")
				}
			}
			if !equalIdentity(x.profiles, y.profiles) || !equalIdentity(x.nodes, y.nodes) || !equalIdentity(x.business, y.business) || len(x.cgroups) != 1 || len(y.cgroups) != 1 {
				missing("保护profile、节点/业务来源不一致或单次试验跨cgroup")
			}
			if x.missing || y.missing || x.unsafe || y.unsafe {
				missing("窗口含缺保护输入、暂停/临界/停止/未知状态")
			}
			if y.advice <= 0 || y.allowed <= 0 || x.advice != 0 {
				missing("没有有效observe/enforce回收对照")
			}
			if x.FileAverage != nil && *x.FileAverage <= 0 || x.FilePeak != nil && *x.FilePeak <= 0 {
				missing("基线文件缓存为0，不能计算改善比例")
			}
			if len(row.Reasons) == 0 {
				row.Status = "telemetry_thresholds_met"
				check := func(name string, value *float64, limit float64, minimum bool) {
					state := "met"
					if value == nil || minimum && *value < limit || !minimum && *value > limit {
						state = "exceeded"
						row.Status = "threshold_exceeded"
					}
					row.Checks = append(row.Checks, CompareCheck{name, state, value, limit})
				}
				val := func(v float64) *float64 { return &v }
				check("file_average_reduction_percent", val((1 - *y.FileAverage / *x.FileAverage)*100), l.MinFileImprovement, true)
				check("file_peak_reduction_percent", val((1-float64(*y.FilePeak)/float64(*x.FilePeak))*100), l.MinFileImprovement, true)
				increase := func(a, b float64) *float64 {
					if a == 0 {
						if b == 0 {
							return val(0)
						}
						return nil
					}
					return val((b/a - 1) * 100)
				}
				check("visible_io_increase_percent", increase(*x.IOAverage, *y.IOAverage), l.MaxIOIncrease, false)
				check("business_p99_sample_max_increase_percent", increase(*x.P99Peak, *y.P99Peak), l.MaxP99Increase, false)
				check("business_p99_sample_max_ms", y.P99Peak, l.MaxP99MS, false)
				check("self_cpu_average_percent", y.CPUAverage, l.MaxCPU, false)
				check("self_rss_sample_peak_bytes", val(float64(*y.RSSPeak)), float64(l.MaxRSS), false)
				if x.operationErrors || y.operationErrors {
					row.Status = "threshold_exceeded"
					row.Reasons = append(row.Reasons, "有文件操作错误，不能按无错误试验通过")
				}
			}
		}
		if row.Status == "insufficient_evidence" {
			out.Status = "insufficient_evidence"
		} else if row.Status == "threshold_exceeded" && out.Status != "insufficient_evidence" {
			out.Status = "threshold_exceeded"
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}
func compareCommand(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	base := fs.String("baseline", "", "基线report JSON，必须为observe")
	candidate := fs.String("candidate", "", "候选report JSON，必须为enforce")
	format := fs.String("format", "json", "json或markdown；只输出标准输出，重定向时不要覆盖输入")
	require := fs.Bool("require-thresholds", false, "证据不足/超限时输出结果后返回非零")
	l := defaultCompareLimits()
	fs.Float64Var(&l.MinCoverage, "min-coverage", l.MinCoverage, "每项有效覆盖最低比例(0,1]")
	fs.Float64Var(&l.MinFileImprovement, "min-file-improvement-percent", l.MinFileImprovement, "文件页峰值/时间均值最低改善百分比")
	fs.Float64Var(&l.MaxIOIncrease, "max-io-increase-percent", l.MaxIOIncrease, "可见cgroup IO均值最大增幅百分比")
	fs.Float64Var(&l.MaxP99Increase, "max-p99-increase-percent", l.MaxP99Increase, "业务p99采样最大值最大增幅百分比")
	fs.Float64Var(&l.MaxP99MS, "max-business-p99-ms", l.MaxP99MS, "业务p99采样最大绝对值ms，须按真实SLO调整")
	fs.Float64Var(&l.MaxCPU, "max-cpu-average-percent", l.MaxCPU, "单核CPU时间均值上限")
	fs.Int64Var(&l.MaxRSS, "max-rss-bytes", l.MaxRSS, "自身RSS采样峰值上限")
	if e := fs.Parse(args); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if fs.NArg() != 0 || *base == "" || *candidate == "" || (*format != "json" && *format != "markdown") {
		return errors.New("需要--baseline/--candidate，format为json或markdown")
	}
	for _, v := range []float64{l.MinCoverage, l.MinFileImprovement, l.MaxIOIncrease, l.MaxP99Increase, l.MaxP99MS, l.MaxCPU} {
		if !finiteNonnegative(v) {
			return errors.New("比较阈值必须为有限非负数")
		}
	}
	if l.MinCoverage <= 0 || l.MinCoverage > 1 || l.MinFileImprovement > 100 || l.MaxIOIncrease > 10000 || l.MaxP99Increase > 10000 || l.MaxP99MS <= 0 || l.MaxCPU <= 0 || l.MaxCPU > 100 || l.MaxRSS <= 0 {
		return errors.New("比较阈值超出有效范围")
	}
	var a, b OfflineReport
	if e := decodeRegularJSON(*base, 16<<20, &a); e != nil {
		return e
	}
	if e := decodeRegularJSON(*candidate, 16<<20, &b); e != nil {
		return e
	}
	result, e := compareTelemetry(a, b, l)
	if e != nil {
		return e
	}
	if *format == "json" {
		body, e := json.MarshalIndent(result, "", "  ")
		if e != nil {
			return e
		}
		if _, e = os.Stdout.Write(append(body, '\n')); e != nil {
			return e
		}
	} else {
		var s strings.Builder
		fmt.Fprintf(&s, "# ContainerCacheGuard 离线对比\n\n结论：%s。%s\n\n", result.Status, result.Scope)
		for _, reason := range result.Reasons {
			fmt.Fprintf(&s, "- %s\n", markdownCell(reason))
		}
		s.WriteString("\n| 实例 | 遥测结论 | 证据缺口或错误 |\n| --- | --- | --- |\n")
		for _, row := range result.Rows {
			fmt.Fprintf(&s, "| %s | %s | %s |\n", markdownCell(row.InstanceID), row.Status, markdownCell(strings.Join(row.Reasons, "；")))
		}
		for _, row := range result.Rows {
			s.WriteString("\n" + markdownCell(row.InstanceID) + "：\n\n| 指标 | 值 | 门槛 | 结果 |\n| --- | ---: | ---: | --- |\n")
			for _, check := range row.Checks {
				v := "未定义（基线为0）"
				if check.Value != nil {
					v = fmt.Sprintf("%.3f", *check.Value)
				}
				fmt.Fprintf(&s, "| %s | %s | %.3f | %s |\n", check.Name, v, check.Limit, check.State)
			}
		}
		if _, e = fmt.Print(s.String()); e != nil {
			return e
		}
	}
	if *require && result.Status != "telemetry_thresholds_met" {
		return errors.New("遥测门槛未满足，详见输出；不等同生产验收")
	}
	return nil
}
