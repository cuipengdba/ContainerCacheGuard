//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"math"
)

func ordinarySample(s Status) bool {
	m := s.Metrics
	return logSampleUsable(s) && m.Available["file_bytes"] && m.Available["shmem_bytes"] && m.File >= 0 && m.Shmem >= 0
}
func businessSample(s Status) bool {
	m := s.Metrics
	return logSampleUsable(s) && s.Signals.Business.State == "ready" && m.BusinessSourceID != "" && !m.BusinessTime.IsZero() && m.BusinessSignalError == "" && m.BusinessP99 != nil && finiteNonnegative(*m.BusinessP99)
}
func nodeSample(s Status) bool {
	n := s.Metrics.Node
	return logSampleUsable(s) && s.Signals.Node.State == "ready" && s.Metrics.NodeSignalError == "" && n != nil && n.NodeID != "" && n.SourceID != "" && n.AvailableBytes != nil && *n.AvailableBytes >= 0
}
func coverageInterval(h *HourReport, a, b Status, seconds float64) error {
	if ordinarySample(a) && ordinarySample(b) {
		// Left sample held over the interval; not a continuous kernel measurement.
		h.FileByteSeconds += float64(max64(0, a.Metrics.File-a.Metrics.Shmem)) * seconds
		h.FileCoveredSeconds += seconds
	}
	if logSampleUsable(a) && logSampleUsable(b) && a.Metrics.Available["self_cpu_percent"] && b.Metrics.Available["self_cpu_percent"] && finiteNonnegative(a.Metrics.CPUPercent) && finiteNonnegative(b.Metrics.CPUPercent) {
		h.CPUPercentSeconds += a.Metrics.CPUPercent * seconds
		h.CPUCoveredSeconds += seconds
	}
	if businessSample(a) && businessSample(b) {
		h.BusinessCoveredSeconds += seconds
	}
	if nodeSample(a) && nodeSample(b) && a.Metrics.Node.NodeID == b.Metrics.Node.NodeID {
		h.NodeCoveredSeconds += seconds
	}
	if math.IsInf(h.FileByteSeconds, 0) || math.IsInf(h.CPUPercentSeconds, 0) {
		return errors.New("覆盖统计溢出")
	}
	return nil
}
