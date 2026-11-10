package main

import (
	"math"
	"testing"
	"time"
)

func TestIOCounterParsingDistinguishesZeroAndCorruptData(t *testing.T) {
	for _, tc := range []struct {
		data  string
		bytes int64
		valid bool
	}{
		{"", 0, true}, {"8:0 rbytes=0 wbytes=0 rios=1", 0, true},
		{"8:0 rbytes=10 wbytes=20\n8:1 rbytes=1 wbytes=2", 33, true},
		{"8:0 rbytes=bad wbytes=0", 0, false}, {"8:0 rbytes=-1 wbytes=0", 0, false},
		{"8:0 rbytes=2", 0, false}, {"8:0 rbytes=2 rbytes=3 wbytes=0", 0, false},
		{"bad rbytes=0 wbytes=0", 0, false}, {"8:0 rbytes=9223372036854775807 wbytes=1", 0, false},
	} {
		n, ok := parseIOBytes(tc.data)
		if ok != tc.valid || n != tc.bytes {
			t.Fatal("bad counter validity", tc, n, ok)
		}
	}
}

func TestRatesInvalidateAcrossSamplingGapsAndCounterReset(t *testing.T) {
	c := defaultConfig()
	for _, name := range []string{"healthy", "gap", "reset", "first", "backward", "failed", "changed_scope"} {
		t.Run(name, func(t *testing.T) {
			p := healthyProtectionMetrics()
			p.Cgroup = "/test"
			p.IOBytes = 10
			p.CPUSeconds = 1
			p.FileRefaults = 10
			p.Available["io_bytes"], p.Available["self_cpu_seconds"] = true, true
			m := healthyProtectionMetrics()
			m.Cgroup = p.Cgroup
			m.Time = p.Time.Add(time.Second)
			m.IOBytes = 20
			m.CPUSeconds = 1.01
			m.FileRefaults = 20
			m.Available["io_bytes"], m.Available["self_cpu_seconds"] = true, true
			switch name {
			case "gap":
				m.Time = p.Time.Add(4 * time.Second)
			case "reset":
				m.IOBytes = 0
				m.CPUSeconds = 0
				m.FileRefaults = 0
			case "first":
				p = Metrics{}
			case "backward":
				m.Time = p.Time.Add(-time.Second)
			case "failed":
				p.Error = "read failed"
			case "changed_scope":
				m.Cgroup = "/different"
			}
			setSampleRates(c, &m, p)
			valid := name == "healthy"
			if m.Available["io_bytes_per_second"] != valid || m.Available["self_cpu_percent"] != valid || m.FileRefaultRateValid != valid {
				t.Fatal("rates accepted a gap or reset", name, m)
			}
		})
	}
}

func TestProtectionUnknownAndDisabledAreDifferent(t *testing.T) {
	c := defaultConfig()
	c.Action = "enforce"
	c.Trigger = "always"
	c.WarmupSeconds = 0
	m := healthyProtectionMetrics()
	if !assessProtection(c, m).Ready {
		t.Fatal("valid zero unavailable")
	}
	m.Available = nil
	if assessProtection(c, m).Ready || decide(c, m, m.Time, m.Time, false, 0).Allow {
		t.Fatal("absent metadata accepted")
	}
	c.Action = "observe"
	if decide(c, m, m.Time, m.Time, false, 0).State != "OBSERVE" {
		t.Fatal("missing optional guards stopped read-only observation")
	}
	c.Action = "enforce"
	c.MaxIOPSIAvg10 = 0
	m = healthyProtectionMetrics()
	m.IOPSI = nil
	delete(m.Available, "io_psi_avg10")
	if !assessProtection(c, m).Ready || !decide(c, m, m.Time, m.Time, false, 0).Allow {
		t.Fatal("explicitly disabled PSI still required")
	}
	c.MaxIOPSIAvg10 = 10
	bad := math.NaN()
	m.IOPSI = &bad
	m.Available["io_psi_avg10"] = true
	if assessProtection(c, m).Ready {
		t.Fatal("non-finite PSI accepted")
	}
	c.MaxBusinessP99MS = 100
	if c.validate() == nil {
		t.Fatal("latency guard without a signal path accepted")
	}
}
