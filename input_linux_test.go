//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalJSONRejectsAliasesDepthAndOversizedInput(t *testing.T) {
	for _, body := range []string{`{"time":1,"TIME":2}`, `{"s":1,"ſ":2}`, `{"nested":{"a":1,"A":2}}`, `[]`, `null`, `{} {}`, `{"nested":` + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + "}"} {
		if uniqueJSONObject([]byte(body)) == nil {
			t.Fatal("ambiguous JSON accepted", body[:minInt(len(body), 50)])
		}
	}
	if e := uniqueJSONObject([]byte(`{"nested":[{"a":1},{"a":2}],"other":true}`)); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(t.TempDir(), "signal")
	os.WriteFile(p, []byte("{}"+strings.Repeat(" ", 16383)), 0600)
	var v any
	if readRegularJSON(p, 16384, &v, false) == nil {
		t.Fatal("oversized tail accepted")
	}
}
func TestBusinessSignalRejectsSymlinkComponentsAndPreservesExtraFields(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0700)
	file := filepath.Join(real, "business")
	os.WriteFile(file, []byte(fmt.Sprintf(`{"time":%q,"p99_ms":1,"source_id":"probe","extra":{"count":10}}`, time.Now().Format(time.RFC3339Nano))), 0600)
	c := defaultConfig()
	c.BusinessSignal = file
	c.BusinessSourceID = "probe"
	m := Metrics{}
	business(c, &m)
	if m.Error != "" {
		t.Fatal(m.Error)
	}
	link := filepath.Join(dir, "link")
	if e := os.Symlink(real, link); e != nil {
		t.Fatal(e)
	}
	c.BusinessSignal = filepath.Join(link, "business")
	m = Metrics{}
	business(c, &m)
	if m.Error == "" {
		t.Fatal("symlink component accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	c.BusinessSignal = fifo
	m = Metrics{}
	business(c, &m)
	if m.Error == "" {
		t.Fatal("FIFO accepted")
	}
	m = Metrics{Error: "cgroup unavailable"}
	c.BusinessSignal = file
	business(c, &m)
	if m.BusinessSignalError != "" {
		t.Fatal("sampler failure misattributed to business")
	}
}
func TestAdvicePermissionUsesEarliestSignalDeadline(t *testing.T) {
	c := limitedTestConfig()
	m := healthyLimitedMetrics(c)
	now := m.Time
	m.Node.Time = now.Add(-time.Duration(c.SignalMaxAgeSeconds)*time.Second + 50*time.Millisecond)
	if !adviceFresh(c, m, now) {
		t.Fatal("fresh sample denied")
	}
	if adviceFresh(c, m, now.Add(60*time.Millisecond)) {
		t.Fatal("expired node authorized until next sample")
	}
	m = healthyLimitedMetrics(c)
	now = m.Time
	m.BusinessTime = now.Add(-time.Duration(c.SignalMaxAgeSeconds)*time.Second + 50*time.Millisecond)
	if adviceFresh(c, m, now.Add(60*time.Millisecond)) {
		t.Fatal("expired business authorized")
	}
	m = healthyLimitedMetrics(c)
	if adviceFresh(c, m, m.Time.Add(time.Duration(c.IntervalMS*3+1)*time.Millisecond)) {
		t.Fatal("expired metrics authorized")
	}
}
func TestSignalDiagnosticsKeepUnknownSeparateFromZeroHeadroom(t *testing.T) {
	c := limitedTestConfig()
	m := healthyLimitedMetrics(c)
	s := signals(c, m, m.Time)
	if s.Node.State != "ready" || s.NodeHeadroomBytes == nil || s.Node.AgeSeconds == nil {
		t.Fatal(s)
	}
	*m.Node.AvailableBytes = nodeStopThreshold(c, m.Node)
	s = signals(c, m, m.Time)
	if s.NodeHeadroomBytes == nil || *s.NodeHeadroomBytes != 0 {
		t.Fatal("zero headroom became unknown")
	}
	m.Node.Time = m.Time.Add(-time.Minute)
	m.NodeSignalError = "stale"
	s = signals(c, m, m.Time)
	if s.Node.State != "stale" || s.NodeHeadroomBytes != nil || s.Node.RemainingSeconds == nil || *s.Node.RemainingSeconds >= 0 {
		t.Fatal(s)
	}
	m.Node = nil
	s = signals(c, m, m.Time)
	if s.Node.AgeSeconds != nil || s.NodeStopBytes != nil {
		t.Fatal("unknown rendered zero")
	}
}
