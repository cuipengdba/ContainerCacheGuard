//go:build linux && (amd64 || arm64)

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"time"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,127}$`)
var runIdentifier = regexp.MustCompile(`^[a-f0-9]{32}$`)

func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (c Config) profile() string {
	if c.ProtectionProfile == "" {
		return "strict"
	}
	return c.ProtectionProfile
}
func (c Config) validateProfile() error {
	for _, s := range []string{c.InstanceID, c.NodeID, c.NodeSourceID, c.BusinessSourceID} {
		if s != "" && !identifier.MatchString(s) {
			return errors.New("身份标识须为1..128字节字母数字及_.:/-，首位为字母数字")
		}
	}
	if c.profile() != "strict" && c.profile() != "limited-output" {
		return errors.New("protection_profile须为strict或limited-output")
	}
	if c.NodeSafetyMarginBytes < 0 {
		return errors.New("node_safety_margin_bytes不能为负")
	}
	if c.NodeSignal != "" && (!filepath.IsAbs(c.NodeSignal) || c.NodeID == "" || c.NodeSourceID == "") {
		return errors.New("node_signal须为绝对路径，同时设置node_id和node_source_id")
	}
	if c.profile() != "limited-output" {
		return nil
	}
	if c.Mode != "paths" || !c.OutputLowReuse || len(c.Paths) == 0 || len(c.AutoRoots) > 0 {
		return errors.New("limited-output仅允许paths，须声明output_low_reuse=true；不支持auto或源PGDATA")
	}
	if c.NodeSignal == "" || c.InstanceID == "" || c.NodeSafetyMarginBytes < 256*MiB || c.BusinessSignal == "" || c.BusinessSourceID == "" || !finiteNonnegative(c.MaxBusinessP99MS) || c.MaxBusinessP99MS <= 0 || c.MaxIOBytesPerSecond <= 0 || !finiteNonnegative(c.MaxFileRefaultsPerSecond) || c.MaxFileRefaultsPerSecond <= 0 {
		return errors.New("limited-output须配置实例、来源绑定的节点/业务信号、正数IO/refault/p99阈值及至少256MiB节点安全余量")
	}
	if !finiteNonnegative(c.MaxIOPSIAvg10) || c.MaxIOPSIAvg10 <= 0 || c.MaxIOPSIAvg10 > 100 || c.MaxDirtyBytes <= 0 || c.CacheTargetBytes <= 0 || c.CriticalMode != "pause" || c.Trigger != "budget" || c.SignalMaxAgeSeconds > 30 || c.IntervalMS > 2000 || c.RecoveryHealthySeconds < 10 || c.WarmupSeconds < 10 || c.MinFileAgeSeconds < 30 || c.ProtectSeconds < 30 || c.CooldownSeconds < 60 {
		return errors.New("limited-output须保留PSI(如可见)/脏页/临界暂停及预算保护；信号<=30秒、采样<=2秒、恢复/预热>=10秒、文件/访问保护>=30秒、冷却>=60秒")
	}
	if c.ScanBytesPerSecond > 64*MiB || c.ReclaimBytesPerSecond > 8*MiB || c.InitialReclaimBytesPerSecond > MiB || c.RegionBytes > MiB || c.MaxOperationsPerSecond > 64 || c.DiscoverEntriesPerSecond > 200 || c.WorkerBudgetMS > 10 || c.MaxCPUPercent > 5 || c.MaxRSSBytes > 128*MiB {
		return errors.New("limited-output预算上限：扫描64MiB/s、建议8MiB/s、初始1MiB/s、区域1MiB、64操作/s、200目录项/s、10ms/片、单核CPU5%、RSS128MiB")
	}
	return nil
}

// All byte quantities are explicitly bytes. Pointers distinguish missing from
// a deliberate zero (disabled threshold); missing pressure never means false.
type NodeSignal struct {
	SchemaVersion    int       `json:"schema_version"`
	SourceID         string    `json:"source_id"`
	NodeID           string    `json:"node_id"`
	Time             time.Time `json:"time"`
	AvailableBytes   *int64    `json:"memory_available_bytes"`
	HardBytes        *int64    `json:"hard_available_bytes"`
	SoftBytes        *int64    `json:"soft_available_bytes"`
	SoftGraceSeconds *int64    `json:"soft_grace_seconds"`
	MemoryPressure   *bool     `json:"memory_pressure"`
}

func validNodeSignal(c Config, n *NodeSignal, now time.Time) bool {
	return n != nil && n.SchemaVersion == 1 && n.SourceID == c.NodeSourceID && n.NodeID == c.NodeID && !n.Time.IsZero() && !now.IsZero() && now.Sub(n.Time) <= time.Duration(c.SignalMaxAgeSeconds)*time.Second && n.Time.Sub(now) <= time.Second && n.AvailableBytes != nil && *n.AvailableBytes >= 0 && n.HardBytes != nil && *n.HardBytes >= 0 && n.SoftBytes != nil && *n.SoftBytes >= 0 && n.SoftGraceSeconds != nil && *n.SoftGraceSeconds >= 0 && n.MemoryPressure != nil && max64(*n.HardBytes, *n.SoftBytes) <= math.MaxInt64-c.NodeSafetyMarginBytes
}
func nodeStopThreshold(c Config, n *NodeSignal) int64 {
	return max64(*n.HardBytes, *n.SoftBytes) + c.NodeSafetyMarginBytes
}
func decodeRegularJSON(path string, limit int64, out any) error {
	f, err := safeOpen(path)
	if err != nil {
		return err
	}
	defer f.Close()
	id, err := statFile(f)
	if err != nil || id.Mode&0170000 != 0100000 {
		return errors.New("信号必须为真实普通文件")
	}
	if id.Size > limit {
		return errors.New("信号超过大小限制")
	}
	d := json.NewDecoder(io.LimitReader(f, limit+1))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("信号包含多余数据")
	}
	return nil
}
func nodeSignal(c Config, m *Metrics) {
	if c.NodeSignal == "" {
		return
	}
	var n NodeSignal
	if err := decodeRegularJSON(c.NodeSignal, 16384, &n); err != nil || !validNodeSignal(c, &n, m.Time) {
		m.Error = "kubelet node signal invalid/stale/source mismatch"
		return
	}
	m.Node = &n
}

type externalGuard struct{ node, business time.Time }

func (g *externalGuard) check(m *Metrics) {
	for _, v := range []struct {
		time time.Time
		last *time.Time
		name string
	}{{m.BusinessTime, &g.business, "business"}, {nodeTime(m.Node), &g.node, "node"}} {
		if v.time.IsZero() {
			continue
		}
		if !v.last.IsZero() && v.time.Before(*v.last) {
			m.Error = fmt.Sprintf("%s signal time regressed", v.name)
			continue
		}
		*v.last = v.time
	}
}
func nodeTime(n *NodeSignal) time.Time {
	if n == nil {
		return time.Time{}
	}
	return n.Time
}
