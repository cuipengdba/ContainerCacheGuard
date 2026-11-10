package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

func parseIOBytes(data string) (int64, bool) {
	var total int64
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		device := strings.Split(fields[0], ":")
		if len(device) != 2 {
			return 0, false
		}
		for _, part := range device {
			if _, err := strconv.ParseUint(part, 10, 32); err != nil {
				return 0, false
			}
		}
		r, w := false, false
		for _, field := range fields[1:] {
			key, value, _ := strings.Cut(field, "=")
			if key != "rbytes" && key != "wbytes" {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || (key == "rbytes" && r) || (key == "wbytes" && w) || total > math.MaxInt64-n {
				return 0, false
			}
			if key == "rbytes" {
				r = true
			} else {
				w = true
			}
			total += n
		}
		if !r || !w {
			return 0, false
		}
	}
	return total, true
}

func setSampleRates(c Config, m *Metrics, previous Metrics) {
	m.IOBytesPerSecond, m.CPUPercent = 0, 0
	m.FileRefaultsPerSecond, m.FileRefaultRateValid = 0, false
	m.Available["io_bytes_per_second"], m.Available["self_cpu_percent"] = false, false
	dt := m.Time.Sub(previous.Time)
	if previous.Time.IsZero() || dt <= 0 || dt > time.Duration(c.IntervalMS)*3*time.Millisecond ||
		previous.Error != "" || m.Error != "" || previous.Cgroup != m.Cgroup || previous.Version != m.Version {
		return
	}
	if m.Available["io_bytes"] && previous.Available["io_bytes"] && m.IOBytes >= previous.IOBytes {
		m.IOBytesPerSecond = float64(m.IOBytes-previous.IOBytes) / dt.Seconds()
		m.Available["io_bytes_per_second"] = true
	}
	if m.Available["self_cpu_seconds"] && previous.Available["self_cpu_seconds"] && m.CPUSeconds >= previous.CPUSeconds {
		m.CPUPercent = (m.CPUSeconds - previous.CPUSeconds) / dt.Seconds() * 100
		m.Available["self_cpu_percent"] = finiteNonnegative(m.CPUPercent)
	}
	setRefaultRate(m, previous)
}

func setRefaultRate(m *Metrics, previous Metrics) {
	m.FileRefaultsPerSecond = 0
	m.FileRefaultRateValid = false
	dt := m.Time.Sub(previous.Time).Seconds()
	if !m.FileRefaultsAvailable || !previous.FileRefaultsAvailable ||
		m.Version != 2 || previous.Version != 2 || m.Cgroup != previous.Cgroup ||
		previous.Time.IsZero() || dt <= 0 || previous.Error != "" || m.Error != "" ||
		m.FileRefaults < previous.FileRefaults {
		return
	}
	m.FileRefaultsPerSecond = float64(m.FileRefaults-previous.FileRefaults) / dt
	m.FileRefaultRateValid = true
}
