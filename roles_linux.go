//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Roles never introduce scan roots or allocate another copy of the budgets.
type PathPolicy struct {
	Path              string `json:"path"`
	Role              string `json:"role"`
	ProtectSeconds    int    `json:"protect_seconds"`
	MinFileAgeSeconds int    `json:"min_file_age_seconds"`
	LowReuse          bool   `json:"low_reuse"`
	LimitedOutput     bool   `json:"limited_output"`
}
type RoleDecision struct {
	Path       string         `json:"path"`
	Role       string         `json:"role"`
	Decision   Decision       `json:"decision"`
	Recovery   RecoveryStatus `json:"recovery"`
	ValidUntil time.Time      `json:"valid_until,omitempty"`
}
type pathPermissions struct {
	Time  time.Time
	Roles []RoleDecision
	Tasks TaskStatus
}
type roleControl struct {
	snapshot atomic.Pointer[pathPermissions]
}

func (c Config) validateRoles() error {
	if len(c.PathPolicies) > 64 {
		return errors.New("path_policies最多64项")
	}
	for i, p := range c.PathPolicies {
		if !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path || forbiddenRoot(p.Path) || p.ProtectSeconds < 0 || p.ProtectSeconds > 86400 || p.MinFileAgeSeconds < 0 || p.MinFileAgeSeconds > 86400 {
			return errors.New("路径角色必须是允许的规范绝对路径，保护期范围0..86400秒")
		}
		if p.Role != "database-source" && p.Role != "backup-output" && p.Role != "excluded" {
			return errors.New("role须为database-source、backup-output或excluded")
		}
		covered := false
		for _, r := range append(append([]string{}, c.Paths...), c.AutoRoots...) {
			if beneath(p.Path, r) {
				covered = true
			}
		}
		if !covered {
			return errors.New("路径角色必须位于显式paths/auto_roots内，不能扩展授权根")
		}
		for _, v := range c.PathPolicies[:i] {
			if p.Path == v.Path || (p.Role != v.Role && p.Role != "excluded" && v.Role != "excluded" && (beneath(p.Path, v.Path) || beneath(v.Path, p.Path))) {
				return errors.New("重叠路径不能指定冲突角色")
			}
		}
		if p.LimitedOutput && (p.Role != "backup-output" || !p.LowReuse || c.profile() != "strict") {
			return errors.New("逐路径limited_output仅允许显式低复用备份输出，整体保持strict")
		}
		if p.LimitedOutput {
			v := c.roleConfig(p)
			if err := v.validateProfile(); err != nil {
				return err
			}
			k := c.KernelReclaim
			if !filepath.IsAbs(k.SignalFile) || !identifier.MatchString(k.SignalSourceID) || !validDeviceList(k.DiskDevices) || !finiteNonnegative(k.MaxDiskAwaitMS) || !finiteNonnegative(k.MaxDiskQueue) || k.MaxDiskAwaitMS <= 0 || k.MaxDiskQueue <= 0 {
				return errors.New("逐路径limited_output还须绑定schema2真实磁盘信号、设备集合及正数await/queue上限")
			}
		}
	}
	if c.TaskSignalDirectory != "" {
		if !filepath.IsAbs(c.TaskSignalDirectory) || filepath.Clean(c.TaskSignalDirectory) != c.TaskSignalDirectory || c.TaskSignalDirectory == "/" || !identifier.MatchString(c.TaskSourceID) || c.InstanceID == "" {
			return errors.New("任务目录须为规范绝对路径并绑定task_source_id/instance_id")
		}
	}
	if c.TaskMaxAgeSeconds < 3 || c.TaskMaxAgeSeconds > 30 || c.MaxActiveTasks < 1 || c.MaxActiveTasks > 128 {
		return errors.New("task_max_age_seconds范围3..30；max_active_tasks范围1..128")
	}
	return nil
}
func (c Config) pathPolicy(path string) PathPolicy {
	p := PathPolicy{Role: "default"}
	for _, v := range c.PathPolicies {
		if beneath(path, v.Path) && (v.Role == "excluded" || len(v.Path) > len(p.Path)) {
			p = v
			if v.Role == "excluded" {
				return p
			}
		}
	}
	return p
}
func (c Config) roleConfig(p PathPolicy) Config {
	v := c
	v.ProtectSeconds = int(max64(int64(c.ProtectSeconds), int64(p.ProtectSeconds)))
	v.MinFileAgeSeconds = int(max64(int64(c.MinFileAgeSeconds), int64(p.MinFileAgeSeconds)))
	if p.Role == "database-source" {
		v.ProtectSeconds = int(max64(60, int64(v.ProtectSeconds)))
		v.MinFileAgeSeconds = int(max64(60, int64(v.MinFileAgeSeconds)))
	}
	if p.LimitedOutput {
		v.ProtectionProfile = "limited-output"
		v.OutputLowReuse = true
		v.Mode = "paths"
		v.Paths = []string{p.Path}
		v.AutoRoots = nil
	}
	return v
}
func (e *engine) roleAllows(path string, now time.Time) bool {
	if e.c.pathPolicy(path).Role == "excluded" {
		return false
	}
	if e.roles == nil {
		return true
	}
	return e.pathAllowed(path, now, e.roles.snapshot.Load())
}
func (e *engine) pathAllowed(path string, now time.Time, s *pathPermissions) bool {
	if s == nil || now.Sub(s.Time) > time.Duration(e.c.IntervalMS*3)*time.Millisecond || now.Before(s.Time.Add(-time.Second)) {
		return false
	}
	if taskProtects(s.Tasks, path) {
		return false
	}
	for _, t := range s.Tasks.Active {
		if !freshKernel(t.Time, now, time.Duration(e.c.TaskMaxAgeSeconds)*time.Second) {
			return false
		}
	}
	p := e.c.pathPolicy(path)
	if p.Role == "excluded" {
		return false
	}
	if len(s.Roles) == 0 {
		return true
	}
	if p.Role == "default" {
		return s.DefaultAllow()
	}
	for _, v := range s.Roles {
		if v.Path == p.Path {
			return v.Decision.Allow && (v.ValidUntil.IsZero() || !now.After(v.ValidUntil))
		}
	}
	return false
}
func (s *pathPermissions) DefaultAllow() bool {
	for _, r := range s.Roles {
		if r.Role == "default" {
			return r.Decision.Allow
		}
	}
	return false
}

func roleDecisions(c Config, m Metrics, started, now time.Time, paused bool, oom int64, base Decision, gates map[string]*recoveryGate) ([]RoleDecision, Decision) {
	if len(c.PathPolicies) == 0 && c.TaskSignalDirectory == "" {
		return nil, base
	}
	policies := append([]PathPolicy{{Role: "default"}}, c.PathPolicies...)
	result := make([]RoleDecision, 0, len(policies))
	aggregate := base
	for _, p := range policies {
		v := c.roleConfig(p)
		d := decide(v, m, started, now, paused, oom)
		if p.Role == "excluded" {
			d.Allow = false
			d.State = "SUSPENDED"
			d.Reason = "excluded_path_role"
		}
		var until time.Time
		if p.LimitedOutput {
			until = roleDiskGuard(c, m, now)
			if until.IsZero() {
				d.Allow = false
				d.State = "SUSPENDED"
				d.Reason = "role_disk_signal_unavailable_or_high"
			}
		}
		key := p.Path
		g := gates[key]
		if g == nil {
			g = &recoveryGate{}
			gates[key] = g
		}
		d, recovery := g.apply(v, d, now)
		result = append(result, RoleDecision{Path: p.Path, Role: p.Role, Decision: d, Recovery: recovery, ValidUntil: until})
		if d.Allow && !aggregate.Allow {
			aggregate = d
			aggregate.Reason = "path_role_subset_allowed"
		}
	}
	return result, aggregate
}

func validDeviceList(devices []string) bool {
	if len(devices) == 0 || len(devices) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if !regexp.MustCompile(`^[1-9][0-9]*:[0-9]+$`).MatchString(d) || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}
func sameDevices(a, b []string) bool {
	if !validDeviceList(a) || !validDeviceList(b) {
		return false
	}
	a = append([]string{}, a...)
	b = append([]string{}, b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}
func roleDiskGuard(c Config, m Metrics, now time.Time) time.Time {
	k := c.KernelReclaim
	var n KernelSignal
	if readTrustedKernelJSON(k.SignalFile, 32768, &n) != nil {
		return time.Time{}
	}
	boot, e := readBootID()
	if n.BusinessSourceID != c.BusinessSourceID || !n.BusinessTime.Equal(m.BusinessTime) {
		return time.Time{}
	}
	if e != nil || n.Schema != 2 || n.NodeID != c.NodeID || n.SourceID != k.SignalSourceID || n.BootID != boot || !freshKernel(n.Time, now, 15*time.Second) || !freshKernel(n.DiskTime, now, 5*time.Second) || !sameDevices(k.DiskDevices, n.DiskDevices) || n.DiskAwaitMS == nil || n.DiskQueue == nil || !finiteNonnegative(*n.DiskAwaitMS) || !finiteNonnegative(*n.DiskQueue) || *n.DiskAwaitMS > k.MaxDiskAwaitMS || *n.DiskQueue > k.MaxDiskQueue || !freshKernel(n.BusinessTime, now, 5*time.Second) || n.BusinessSamples == nil || *n.BusinessSamples < 20 || n.BusinessErrorRate == nil || *n.BusinessErrorRate != 0 {
		return time.Time{}
	}
	until := n.DiskTime.Add(5 * time.Second)
	if t := n.BusinessTime.Add(5 * time.Second); t.Before(until) {
		until = t
	}
	return until
}

func (e *engine) bindRoleRoots() error {
	e.roleRoots = map[string]identity{}
	seen := map[[2]uint64]PathPolicy{}
	for _, p := range e.c.PathPolicies {
		if p.Role == "excluded" {
			continue
		}
		f, err := safeOpen(p.Path)
		if err != nil {
			return fmt.Errorf("role root %s: %w", p.Path, err)
		}
		id, err := statFile(f)
		supported := supportedFS(f)
		f.Close()
		if err != nil || id.Mode&0170000 != 0040000 || !supported {
			return errors.New("路径角色必须绑定可访问的受支持真实目录")
		}
		k := [2]uint64{id.Dev, id.Ino}
		if old, ok := seen[k]; ok && old.Role != p.Role {
			return errors.New("同一根inode的挂载别名不能指定冲突角色")
		}
		seen[k] = p
		e.roleRoots[p.Path] = id
	}
	return nil
}
func (e *engine) roleRootUnchanged(path string) bool {
	p := e.c.pathPolicy(path)
	if p.Role == "default" || e.roleRoots == nil {
		return true
	}
	before, ok := e.roleRoots[p.Path]
	if !ok {
		return false
	}
	f, err := safeOpen(p.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	id, err := statFile(f)
	return err == nil && id.Dev == before.Dev && id.Ino == before.Ino && id.UID == before.UID && id.Mode == before.Mode
}

// A bounded 3:1 output/source scheduler. Every fourth visit retains the old
// round-robin slot; priority probes consume the SAME operation/time budgets.
func (e *engine) preferredCandidate(fallback int) int {
	e.roleVisits++
	if len(e.c.PathPolicies) == 0 || e.roleVisits%4 == 0 {
		return fallback
	}
	now := time.Now()
	for i := 0; i < minInt(16, len(e.queue)); i++ {
		j := (e.roleCursor + i) % len(e.queue)
		r := e.queue[j]
		if e.c.pathPolicy(r.Path).Role == "backup-output" && e.regions[r.Key] == r && !now.Before(r.Due) && !now.Before(r.Until) {
			e.roleCursor = (j + 1) % len(e.queue)
			return j
		}
	}
	return fallback
}

func (e *engine) refreshIndexedIdentity(entry *fileEntry, id identity) bool {
	oldKey := [2]uint64{entry.ID.Dev, entry.ID.Ino}
	newKey := [2]uint64{id.Dev, id.Ino}
	if other := e.inodePaths[newKey]; other != "" && other != entry.Path {
		if e.c.pathPolicy(other).Role != e.c.pathPolicy(entry.Path).Role {
			if len(e.aliasDenied) < e.c.MaxFiles {
				e.aliasDenied[newKey] = true
			} else {
				e.aliasOverflow = true
			}
			e.auditSkip(entry.Path, "inode_role_conflict", &id)
		} else {
			e.auditSkip(entry.Path, "duplicate_inode_alias", &id)
		}
		return false
	}
	if e.inodePaths[oldKey] == entry.Path {
		delete(e.inodePaths, oldKey)
	}
	e.inodePaths[newKey] = entry.Path
	entry.ID = id
	return true
}
