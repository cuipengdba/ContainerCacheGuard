package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCgroupWorkingSetDoesNotSubtractShmem(t *testing.T) {
	p := t.TempDir()
	files := map[string]string{"memory.current": "1000\n", "memory.stat": "anon 300\nfile 600\nshmem 400\nactive_file 100\ninactive_file 200\nfile_dirty 20\nfile_writeback 10\n", "memory.events": "max 15\noom 0\noom_kill 0\n"}
	for n, b := range files {
		if e := os.WriteFile(filepath.Join(p, n), []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	s, e := readCgroup(p)
	if e != nil {
		t.Fatal(e)
	}
	if s.WorkingSet != 800 || s.Shmem != 400 || s.File != 600 {
		t.Fatalf("incorrect cache accounting: %+v", s)
	}
	if s.Events["max"] != 15 || s.Events["oom_kill"] != 0 {
		t.Fatal("max events must not become OOM kills")
	}
}
func TestKubeletFreshnessAndMissingAvailable(t *testing.T) {
	now := time.Now().UTC()
	body := func(stamp time.Time) []byte {
		return []byte(fmt.Sprintf(`{"node":{"memory":{"time":%q,"availableBytes":1234}}}`, stamp.Format(time.RFC3339Nano)))
	}
	n, e := summaryAvailable(body(now), 15*time.Second, now)
	if e != nil || n != 1234 {
		t.Fatal(n, e)
	}
	if _, e = summaryAvailable(body(now.Add(-time.Minute)), 15*time.Second, now); e == nil {
		t.Fatal("stale stats accepted")
	}
	if _, e = summaryAvailable([]byte(`{"node":{"memory":{}}}`), 15*time.Second, now); e == nil {
		t.Fatal("missing available accepted")
	}
}
func TestPolicyUsesActiveFileAndNodeHeadroom(t *testing.T) {
	c := Config{MinAvailable: 100, WarnAvailable: 200, Cgroups: []CgroupBudget{{Path: "db", MaxActiveFile: 50}}}
	if critical, _, _ := c.policy(Sample{NodeAvailable: 99}); !critical {
		t.Fatal("headroom breach admitted")
	}
	if critical, warn, _ := c.policy(Sample{NodeAvailable: 150}); critical || !warn {
		t.Fatal("warning policy")
	}
	if critical, _, _ := c.policy(Sample{NodeAvailable: 1000, Cgroups: []CgroupSample{{Path: "db", ActiveFile: 50}}}); !critical {
		t.Fatal("active_file breach admitted")
	}
}
func TestSelectBackupRejectsWrongVersionAndError(t *testing.T) {
	data := `[{"name":"demo","db":[{"id":1,"version":"18","repo-key":1}],"backup":[{"label":"20261105-030000F","error":false,"database":{"id":1,"repo-key":1}}]}]`
	label, e := selectBackup([]byte(data), "")
	if e != nil || label != "20261105-030000F" {
		t.Fatal(label, e)
	}
	for _, bad := range []string{strings.Replace(data, `"version":"18"`, `"version":"14"`, 1), strings.Replace(data, `"error":false`, `"error":true`, 1)} {
		if _, e = selectBackup([]byte(bad), ""); e == nil {
			t.Fatal("unsafe backup selected")
		}
	}
	if _, e = selectBackup([]byte(data), "missing"); e == nil {
		t.Fatal("nonexistent label accepted")
	}
}
func TestRestoreTargetPreservesExistingDataAndRejectsSymlinkOverlap(t *testing.T) {
	base := t.TempDir()
	c := Config{PGData: filepath.Join(base, "data"), Repository: filepath.Join(base, "repo"), RuntimeDir: filepath.Join(base, "run")}
	for _, p := range []string{c.PGData, c.Repository, c.RuntimeDir} {
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := restoreTarget(c, filepath.Join(c.Repository, "child")); e == nil {
		t.Fatal("repository overlap accepted")
	}
	target := filepath.Join(base, "restore")
	os.Mkdir(target, 0700)
	os.WriteFile(filepath.Join(target, "keep"), []byte("valuable data"), 0600)
	if _, e := restoreTarget(c, target); e == nil {
		t.Fatal("nonempty restore target accepted")
	}
	b, _ := os.ReadFile(filepath.Join(target, "keep"))
	if string(b) != "valuable data" {
		t.Fatal("existing data modified")
	}
	link := filepath.Join(base, "link")
	if e := os.Symlink(c.PGData, link); e == nil {
		if _, e := restoreTarget(c, filepath.Join(link, "new")); e == nil {
			t.Fatal("symlink bypass accepted")
		}
	}
}
func TestRemoteEngineConfigRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engine.conf")
	for _, key := range []string{"pg1-host=remote", "repo1-host=remote", "pg2-path=/data", "config-include-path=/other"} {
		os.WriteFile(p, []byte("[global]\n"+key+"\n"), 0600)
		if e := validateEngineConfig(p); e == nil {
			t.Fatal("uncontrolled remote config accepted", key)
		}
	}
}

func TestVerifyResultIsCheckedIndependentlyOfProcessExit(t *testing.T) {
	good := "stanza: demo\nstatus: ok\n  backup: 20261105-030000F, status: valid, total files checked: 100, total valid files: 100\n"
	if e := validVerification(good); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"", "stanza: demo\nstatus: error\n", strings.Replace(good, "status: valid", "status: invalid", 1), "stanza: demo\nstatus: ok\n  backup: none found\n"} {
		if e := validVerification(bad); e == nil {
			t.Fatal("unsafe verification accepted", bad)
		}
	}
}
