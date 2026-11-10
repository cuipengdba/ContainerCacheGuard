//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func logFixture(t *testing.T) (Config, time.Time) {
	t.Helper()
	c := defaultConfig()
	c.LogDirectory = filepath.Join(t.TempDir(), "logs")
	c.LogTimezone = "UTC"
	if err := os.Mkdir(c.LogDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	return c, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
}

func seedLog(t *testing.T, c Config, day time.Time, size int64) string {
	t.Helper()
	p := filepath.Join(c.LogDirectory, "containercacheguard-"+day.Format("2006-01-02")+".log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func dailyFiles(t *testing.T, c Config) []string {
	t.Helper()
	entries, err := os.ReadDir(c.LogDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if _, ok := logDay(e.Name(), time.UTC); ok {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestDailyLogsKeepThirtyCalendarDays(t *testing.T) {
	c, now := logFixture(t)
	for day := -30; day <= 0; day++ {
		seedLog(t, c, now.AddDate(0, 0, day), 0)
	}
	sentinel := filepath.Join(c.LogDirectory, "business-notes.txt")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	l, err := newDailyLogger(c, &console, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if names := dailyFiles(t, c); len(names) != 30 || names[0] != "containercacheguard-2026-10-12.log" {
		t.Fatal("wrong calendar retention boundary", names)
	}
	if l.Status().DeletedFiles != 1 || !strings.Contains(console.String(), "log_cleanup") {
		t.Fatal("cleanup was not recorded")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "untouched" {
		t.Fatal("unrelated file was changed", err)
	}
	// A quiet daemon also cleans on its maintenance tick, without a status log.
	now = now.AddDate(0, 0, 1)
	if err = l.Maintain(); err != nil {
		t.Fatal(err)
	}
	if names := dailyFiles(t, c); len(names) != 30 || names[0] != "containercacheguard-2026-10-13.log" {
		t.Fatal("quiet-day maintenance did not retain 30 days", names)
	}
}

func TestDailyLogsRotateByConfiguredTimezoneAndAppendAfterRestart(t *testing.T) {
	c, _ := logFixture(t)
	c.LogTimezone = "Asia/Shanghai"
	now := time.Date(2026, 11, 9, 15, 59, 59, 0, time.UTC)
	var console bytes.Buffer
	l, err := newDailyLogger(c, &console, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.WriteJSON(map[string]string{"message": "before midnight"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err = l.WriteJSON(map[string]string{"message": "after midnight"}); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = newDailyLogger(c, &console, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.WriteJSON(map[string]string{"message": "after restart"}); err != nil {
		t.Fatal(err)
	}
	if names := dailyFiles(t, c); len(names) != 2 {
		t.Fatal("more than one file per date", names)
	}
	body, err := os.ReadFile(filepath.Join(c.LogDirectory, "containercacheguard-2026-11-10.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "\n") != 2 || !strings.Contains(string(body), "after restart") {
		t.Fatal("restart truncated daily log", string(body))
	}
	if !strings.Contains(console.String(), "before midnight") || !strings.Contains(console.String(), "after restart") {
		t.Fatal("console output missing")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatal("invalid JSON log", line)
		}
	}
}

func TestDailyLogsEnforceOneGBWithoutCopyingSparseContents(t *testing.T) {
	c, now := logFixture(t)
	old := seedLog(t, c, now.AddDate(0, 0, -2), 600000000)
	newer := seedLog(t, c, now.AddDate(0, 0, -1), 600000000)
	l, err := newDailyLogger(c, io.Discard, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("oldest log was not removed", err)
	}
	if _, err = os.Stat(newer); err != nil {
		t.Fatal("newer log was needlessly removed", err)
	}
	if l.total > c.LogMaxTotalBytes || l.deleted != 1 {
		t.Fatal("total log cap failed", l.Status())
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	// Sparse fixture exercises the real 1GB boundary without writing 1GB of data.
	if err = os.Remove(newer); err != nil {
		t.Fatal(err)
	}
	active := seedLog(t, c, now, 1000000000)
	var console bytes.Buffer
	l, err = newDailyLogger(c, &console, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.truncations != 1 || !strings.Contains(console.String(), `"current_file_truncated":true`) {
		t.Fatal("active-file truncation not visible", l.Status())
	}
	if err = l.WriteJSON(map[string]string{"message": "new content"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(active)
	if err != nil || info.Size() > c.LogMaxTotalBytes || info.Size() > 1024 {
		t.Fatal("active cap failed", err)
	}
	if len(dailyFiles(t, c)) != 1 {
		t.Fatal("daily truncation created numbered parts")
	}
}

func TestDailyLogsApplyCapBeforeAppendAndRefuseSharedWriter(t *testing.T) {
	c, now := logFixture(t)
	c.LogMaxTotalBytes = 2048
	l, err := newDailyLogger(c, io.Discard, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 20; i++ {
		if err = l.WriteJSON(map[string]string{"message": strings.Repeat("x", 400)}); err != nil {
			t.Fatal(err)
		}
		if l.total > 2048 {
			t.Fatal("append exceeded hard byte budget")
		}
	}
	if l.truncations == 0 {
		t.Fatal("within-day cap was not exercised")
	}
	if other, err := newDailyLogger(c, io.Discard, func() time.Time { return now }); err == nil {
		other.Close()
		t.Fatal("two writers accepted the same dedicated log directory")
	}
}

func TestDailyLogsRejectUnsafeFilesAndDirectories(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "public_directory"} {
		t.Run(kind, func(t *testing.T) {
			c, now := logFixture(t)
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(c.LogDirectory, "containercacheguard-2026-11-09.log")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(sentinel, name)
			case "hardlink":
				err = os.Link(sentinel, name)
			case "fifo":
				err = syscall.Mkfifo(name, 0600)
			case "public_directory":
				err = os.Chmod(c.LogDirectory, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				l, e := newDailyLogger(c, io.Discard, func() time.Time { return now })
				if e == nil {
					l.Close()
				}
				done <- e
			}()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("unsafe log accepted")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("unsafe log blocked startup")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "untouched" {
				t.Fatal("sentinel was modified", err)
			}
		})
	}
}

func TestDailyLogsReportRealWriteFailure(t *testing.T) {
	c, now := logFixture(t)
	l, err := newDailyLogger(c, io.Discard, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.file.Close()
	l.file, err = os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.WriteJSON(map[string]string{"message": "must fail"}); err == nil {
		t.Fatal("ENOSPC silently ignored")
	}
}
