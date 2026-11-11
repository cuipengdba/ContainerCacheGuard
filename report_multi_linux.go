//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

type reportManifest struct {
	Sources []reportSource `json:"sources"`
}
type reportSource struct {
	InstanceID string   `json:"instance_id"`
	LogFiles   []string `json:"log_files"`
}

func readReportPaths(paths []string, o reportOptions) (OfflineReport, error) {
	a := newReportAggregator(o)
	a.report.SourceFiles = len(paths)
	for _, p := range paths {
		f, e := safeOpen(p)
		if e != nil {
			return OfflineReport{}, e
		}
		id, e := statFile(f)
		if e != nil || id.Mode&0170000 != 0100000 {
			f.Close()
			return OfflineReport{}, fmt.Errorf("日志必须是普通文件：%s", p)
		}
		e = a.read(io.LimitReader(f, id.Size))
		f.Close()
		if e != nil {
			return OfflineReport{}, fmt.Errorf("%s：%w", p, e)
		}
	}
	return a.finish()
}
func manifestReport(path string, o reportOptions) (OfflineReport, []string, error) {
	var manifest reportManifest
	if e := decodeRegularJSON(path, 1<<20, &manifest); e != nil {
		return OfflineReport{}, nil, e
	}
	if len(manifest.Sources) < 1 || len(manifest.Sources) > 16 {
		return OfflineReport{}, nil, errors.New("manifest须含1..16个来源")
	}
	result := newReportAggregator(o).report
	result.Instance = "多实例（按清单身份隔离）"
	result.Notes[len(result.Notes)-1] = "多实例清单逐项校验instance_id/run_id；不接受无身份旧日志。cgroup设备/inode只在该实例内解释。"
	var all []string
	seenIDs, seenFiles := map[string]bool{}, map[string]bool{}
	for _, source := range manifest.Sources {
		if !identifier.MatchString(source.InstanceID) || seenIDs[source.InstanceID] {
			return OfflineReport{}, nil, errors.New("清单instance_id无效或重复")
		}
		seenIDs[source.InstanceID] = true
		for _, p := range source.LogFiles {
			if !filepath.IsAbs(p) {
				return OfflineReport{}, nil, errors.New("清单日志须为绝对路径")
			}
		}
		paths, e := reportFiles("", source.LogFiles)
		if e != nil {
			return OfflineReport{}, nil, e
		}
		for _, p := range paths {
			if seenFiles[p] {
				return OfflineReport{}, nil, errors.New("清单重复使用日志文件")
			}
			seenFiles[p] = true
			all = append(all, p)
		}
		if len(all) > 1024 {
			return OfflineReport{}, nil, errors.New("总文件数超过1024")
		}
		options := o
		options.expectedInstance = source.InstanceID
		part, e := readReportPaths(paths, options)
		if e != nil {
			return OfflineReport{}, nil, e
		}
		result.Hours = append(result.Hours, part.Hours...)
		if len(result.Hours) > reportBucketLimit {
			return OfflineReport{}, nil, errors.New("总小时分组超过4096")
		}
		result.SourceFiles += part.SourceFiles
		result.IgnoredEvents += part.IgnoredEvents
		result.OutOfOrder += part.OutOfOrder
	}
	sort.Slice(result.Hours, func(i, j int) bool {
		a, b := result.Hours[i], result.Hours[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		return a.InstanceID < b.InstanceID
	})
	return result, all, nil
}

// Standalone SVG: no script, network, external fonts, web service or hidden
// interpolation across absent hours. Every bar describes a sampled hour.
func svgReport(r OfflineReport) string {
	var b strings.Builder
	height := 220 + len(r.Hours)*160
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="1240" height="%d" viewBox="0 0 1240 %d" role="img"><title>ContainerCacheGuard 离线小时统计</title><rect width="1240" height="100%%" fill="#f3f6fa"/><g font-family="sans-serif" fill="#17243b">`, height, height)
	text := func(x, y int, size int, value string) {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="%d">%s</text>`, x, y, size, html.EscapeString(value))
	}
	text(28, 40, 25, "ContainerCacheGuard · 容器缓存小时统计")
	text(28, 69, 15, "采样峰值；无日志的小时不显示，不能视为健康。时区："+r.Timezone)
	text(28, 95, 14, "蓝：普通文件页　橙：匿名内存　紫：共享内存；三项分别计量，不能将共享内存重复相加。")
	text(28, 120, 14, "允许回收≠回收成功；未验证SQL尾延迟、完整备份恢复或K8s驱逐。")
	if len(r.Hours) == 0 {
		text(28, 170, 20, "没有有效样本，无法得出结论。")
	}
	for i, h := range r.Hours {
		y := 150 + i*160
		fmt.Fprintf(&b, `<g><title>%s</title><rect x="18" y="%d" width="1204" height="150" rx="8" fill="white"/>`, html.EscapeString(fmt.Sprintf("实例%s；cgroup=%s；dev/inode=%d/%d；运行标识=%s", h.InstanceID, h.Cgroup, h.CgroupDevice, h.CgroupInode, strings.Join(h.RunIDs, ","))), y)
		label := h.InstanceID
		if label == "" {
			label = "旧日志：未绑定实例"
		}
		if len(label) > 80 {
			label = label[:80] + "…"
		}
		text(30, y+24, 16, h.Hour+" | "+label)
		vals := []*int64{h.OrdinaryPeak, h.AnonPeak, h.ShmemPeak}
		names := []string{"普通文件", "匿名内存", "共享内存"}
		colors := []string{"#377dcb", "#dc8737", "#8d65bb"}
		largest := int64(1)
		for _, v := range vals {
			if v != nil && *v > largest {
				largest = *v
			}
		}
		for j, v := range vals {
			yy := y + 46 + j*23
			value := "未知"
			if v != nil {
				value = fmt.Sprintf("%.1f MiB", float64(*v)/float64(MiB))
				w := 450 * float64(*v) / float64(largest)
				fmt.Fprintf(&b, `<rect x="135" y="%d" width="%.2f" height="14" fill="%s"/>`, yy-12, w, colors[j])
			}
			text(30, yy, 13, names[j])
			text(595, yy, 13, value)
		}
		node := "未知"
		if h.NodeAvailableMin != nil {
			node = fmt.Sprintf("%.1f MiB", float64(*h.NodeAvailableMin)/float64(MiB))
		}
		text(760, y+48, 13, "kubelet余量最低："+node)
		text(760, y+71, 13, fmt.Sprintf("CCG RSS峰值：%s | CPU峰值：%s", svgMiB(h.SelfRSSPeak), svgPercent(h.CPUPeak)))
		text(760, y+94, 13, fmt.Sprintf("样本 %d | 缺保护项 %d | 未衔接间隔 %d", h.Samples, len(h.MissingGuards), h.UnbridgedIntervals))
		io := "未知"
		if h.IOPeak != nil {
			io = fmt.Sprintf("%.1f MiB/s", *h.IOPeak/float64(MiB))
		}
		pressure := "未知"
		if h.NodeAvailableMin != nil {
			pressure = fmt.Sprintf("%d", h.NodePressureSamples)
		}
		text(760, y+117, 13, "IO峰值："+io+" | MemoryPressure样本："+pressure)
		state := fmt.Sprintf("状态 %s | 模式 %s", jsonText(h.States), jsonText(h.Actions))
		if len(state) > 150 {
			state = state[:150] + "…（完整状态见JSON）"
		}
		text(30, y+125, 13, state)
		b.WriteString("</g>")
	}
	text(28, height-24, 13, "仅文件范围释放建议；匿名/共享内存不由本工具清理。跨小时柱长各自归一化，请按数值比较。")
	b.WriteString("</g></svg>\n")
	return b.String()
}
func svgMiB(v *int64) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%.1f MiB", float64(*v)/float64(MiB))
}
func svgPercent(v *float64) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%.2f%%", *v)
}
