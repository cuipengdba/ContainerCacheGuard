//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (c Config) fileSkipReason(path string, id identity) string {
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG || id.Size < int64(os.Getpagesize()) {
		return "not_regular_or_subpage"
	}
	if c.Mode == "auto" && id.Mode&0111 != 0 && !c.executableData(path) {
		return "execute_bit_auto"
	}
	b := filepath.Base(path)
	if strings.Contains(b, ".so") || strings.HasSuffix(b, ".a") {
		return "library_name"
	}
	return ""
}

func (c Config) executableData(path string) bool {
	for _, p := range c.ExecutableDataPaths {
		if beneath(path, p) {
			return true
		}
	}
	return false
}
func (c Config) validateReclaimOptions() error {
	if c.MaxAuditGroups < 2 || c.MaxAuditGroups > 256 || c.MaxScanHistory < 1 || c.MaxScanHistory > 65536 || c.MaxCandidateEvents < 1 || c.MaxCandidateEvents > 256 {
		return errors.New("max_audit_groups范围2..256；max_scan_history范围1..65536；max_candidate_events范围1..256")
	}
	if len(c.ExecutableDataPaths) > 64 {
		return errors.New("executable_data_paths最多64项")
	}
	for _, p := range c.ExecutableDataPaths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || forbiddenRoot(p) || c.excluded(p) {
			return errors.New("executable_data_paths须为允许且未排除的规范绝对数据路径")
		}
		// Exceptions do not introduce roots; explicit existing roots are required.
		found := false
		for _, root := range append(append([]string{}, c.Paths...), c.AutoRoots...) {
			if beneath(p, root) {
				found = true
			}
		}
		if !found {
			return errors.New("executable_data_paths必须位于显式paths或auto_roots内；不自动增加扫描根")
		}
	}
	last := 0
	if len(c.ExtendedRecheckSeconds) > 8 {
		return errors.New("extended_recheck_seconds最多8个时点")
	}
	for _, v := range c.ExtendedRecheckSeconds {
		if v <= last || v > 3600 {
			return errors.New("额外复查时点为严格递增的1..3600秒，均相对于最近一次建议")
		}
		last = v
	}
	if c.PressureBoostNodeHeadroomBytes < 0 || c.PressureBoostNodeHeadroomBytes > 1<<50 {
		return errors.New("pressure_boost_node_headroom_bytes范围0..1PiB")
	}
	if c.PressureBoost && (c.profile() != "strict" || c.CriticalMode != "bounded" || c.NodeSignal == "" || c.NodeSafetyMarginBytes < 256*MiB || c.BusinessSignal == "" || c.BusinessSourceID == "" || c.MaxBusinessP99MS <= 0 || !finiteNonnegative(c.MaxBusinessP99MS) || c.MaxIOBytesPerSecond <= 0 || c.MaxFileRefaultsPerSecond <= 0 || !finiteNonnegative(c.MaxFileRefaultsPerSecond) || c.MaxIOPSIAvg10 <= 0 || !finiteNonnegative(c.MaxIOPSIAvg10)) {
		return errors.New("pressure_boost须strict、critical_mode=bounded，以及来源绑定的节点/业务、至少256MiB节点余量、正数p99/IO/refault/PSI保护；不能绕过缺项保护")
	}
	return nil
}
func openSkipReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.ELOOP):
		return "symlink_rejected"
	default:
		return "open_or_metadata_error"
	}
}
func (e *engine) scheduleExtendedRecheck(r *region, now time.Time) bool {
	if r.AdvisedAt.IsZero() {
		return false
	}
	// r.Due is the last scheduled horizon. If a worker is late, retain every
	// remaining horizon as an honestly late sample rather than invent a sample.
	last := r.Due.Sub(r.AdvisedAt)
	for _, seconds := range e.c.ExtendedRecheckSeconds {
		delay := time.Duration(seconds) * time.Second
		if delay > last {
			r.Due = r.AdvisedAt.Add(delay)
			r.Until = time.Time{}
			return true
		}
	}
	return false
}
func pressureBoostWanted(c Config, m Metrics) bool {
	if !c.PressureBoost || max64(0, m.File-m.Shmem) == 0 {
		return false
	}
	if memoryAtRatio(m, c.MemoryHighRatio) {
		return true
	}
	return c.NodeSignal != "" && validNodeSignal(c, m.Node, m.Time) && !*m.Node.MemoryPressure &&
		*m.Node.AvailableBytes > nodeStopThreshold(c, m.Node) && *m.Node.AvailableBytes-nodeStopThreshold(c, m.Node) <= c.PressureBoostNodeHeadroomBytes
}
