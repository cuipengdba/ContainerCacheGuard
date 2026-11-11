//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportRunIDAndCgroupIdentityBreakCounterContinuity(t *testing.T) {
	a := reportTestAggregator(t)
	at := time.Now()
	runA := strings.Repeat("a", 32)
	runB := strings.Repeat("b", 32)
	for i := 0; i < 4; i++ {
		s := reportTestSample(at.Add(time.Duration(i)*10*time.Second), int64(i)*100)
		s.InstanceID = "dbs"
		s.RunID = runA
		s.Metrics.CgroupDevice = 1
		s.Metrics.CgroupInode = 2
		if i > 0 {
			s.RunID = runB
		}
		if i > 1 {
			s.Metrics.CgroupInode = 3
		}
		if e := a.add(s); e != nil {
			t.Fatal(e)
		}
	}
	r, e := a.finish()
	if e != nil {
		t.Fatal(e)
	}
	var advice int64
	for _, h := range r.Hours {
		advice += h.AdviceBytes
	}
	if advice != 100 {
		t.Fatalf("bridged restart/inode change: %d", advice)
	}
}
func TestMultiReportManifestSeparatesContainersAndRejectsIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	manifest := reportManifest{}
	at := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	for _, id := range []string{"dbs", "hisdbs"} {
		path := filepath.Join(dir, id+".jsonl")
		var lines strings.Builder
		for i := 0; i < 2; i++ {
			s := reportTestSample(at.Add(time.Duration(i)*10*time.Second), int64(i)*100)
			s.InstanceID = id
			s.RunID = strings.Repeat("c", 32)
			b, _ := json.Marshal(s)
			lines.Write(b)
			lines.WriteByte('\n')
		}
		os.WriteFile(path, []byte(lines.String()), 0600)
		manifest.Sources = append(manifest.Sources, reportSource{InstanceID: id, LogFiles: []string{path}})
	}
	path := filepath.Join(dir, "sources.json")
	save := func() { b, _ := json.Marshal(manifest); os.WriteFile(path, b, 0600) }
	save()
	o := reportOptions{location: time.UTC, maxGap: 30 * time.Second}
	r, paths, e := manifestReport(path, o)
	if e != nil || len(r.Hours) != 2 || len(paths) != 2 {
		t.Fatalf("%+v %v", r, e)
	}
	if r.Hours[0].AdviceBytes != 100 || r.Hours[1].AdviceBytes != 100 {
		t.Fatal("cross-container counters merged")
	}
	manifest.Sources[1].InstanceID = "other"
	save()
	if _, _, e = manifestReport(path, o); e == nil {
		t.Fatal("identity mismatch accepted")
	}
	manifest.Sources[1].InstanceID = "dbs"
	save()
	if _, _, e = manifestReport(path, o); e == nil {
		t.Fatal("duplicate id accepted")
	}
	manifest.Sources[1].InstanceID = "hisdbs"
	manifest.Sources[1].LogFiles = manifest.Sources[0].LogFiles
	save()
	if _, _, e = manifestReport(path, o); e == nil {
		t.Fatal("duplicate file accepted")
	}
}
func TestSVGIsStandaloneEscapedAndUnknownIsNotHealthy(t *testing.T) {
	r := OfflineReport{Timezone: "UTC", Hours: []*HourReport{{Hour: "hour<1>", InstanceID: "x<script>", States: map[string]int{"SUSPENDED": 1}, Actions: map[string]int{"enforce": 1}}}}
	svg := svgReport(r)
	if strings.Contains(svg, "<script>") || strings.Contains(svg, "http://") && !strings.Contains(svg, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatal(svg)
	}
	if !strings.Contains(svg, "未知") || !strings.Contains(svg, "SUSPENDED") || !strings.Contains(svg, "未验证SQL") {
		t.Fatal(svg)
	}
	decoder := xml.NewDecoder(strings.NewReader(svg))
	for {
		_, e := decoder.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
}
