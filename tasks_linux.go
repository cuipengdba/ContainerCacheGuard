//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type TaskRecord struct {
	Schema       int       `json:"schema_version"`
	SourceID     string    `json:"source_id"`
	InstanceID   string    `json:"instance_id"`
	BootID       string    `json:"boot_id"`
	ID           string    `json:"task_id"`
	Time         time.Time `json:"time"`
	Sequence     uint64    `json:"sequence"`
	Stage        string    `json:"stage"`
	Roots        []string  `json:"roots"`
	PID          int       `json:"pid"`
	ProcessStart string    `json:"process_start"`
}
type TaskStatus struct {
	State  string       `json:"state"`
	Error  string       `json:"error,omitempty"`
	Time   time.Time    `json:"checked_at"`
	Active []TaskRecord `json:"active"`
}
type taskMonitor struct{ previous map[string]TaskRecord }

func processStart(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid PID")
	}
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return "", e
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return "", errors.New("invalid proc stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", errors.New("short proc stat")
	}
	if _, e := strconv.ParseUint(f[19], 10, 64); e != nil {
		return "", e
	}
	return f[19], nil
}
func taskStage(s string) bool {
	switch s {
	case "backup", "verify", "compress", "transfer", "sync", "restore":
		return true
	}
	return false
}
func taskDirectory(path string) (*os.File, error) {
	f, e := safeOpen(path)
	if e != nil {
		return nil, e
	}
	id, e := statFile(f)
	if e != nil || id.Mode&syscall.S_IFMT != syscall.S_IFDIR || id.Mode&0077 != 0 || id.UID != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New("任务目录必须为当前UID所有的0700真实目录")
	}
	return f, nil
}
func (g *taskMonitor) read(c Config, now time.Time) TaskStatus {
	s := TaskStatus{State: "disabled", Time: now, Active: []TaskRecord{}}
	if c.TaskSignalDirectory == "" {
		return s
	}
	fail := func(reason string) TaskStatus { s.State = "unknown"; s.Error = reason; s.Active = nil; return s }
	dir, err := taskDirectory(c.TaskSignalDirectory)
	if err != nil {
		return fail("task_directory_unavailable")
	}
	defer dir.Close()
	// Bound even unrelated files. Overflow is NOT evidence that no task exists.
	names, err := dir.Readdirnames(c.MaxActiveTasks + 1)
	if err != nil && len(names) == 0 && err.Error() != "EOF" {
		return fail("task_directory_read_failed")
	}
	if len(names) > c.MaxActiveTasks {
		return fail("task_directory_overflow")
	}
	boot, err := readBootID()
	if err != nil {
		return fail("task_boot_unknown")
	}
	next := map[string]TaskRecord{}
	for _, name := range names {
		// Atomic-write temporary files are ignored, but count toward the bound.
		if strings.HasPrefix(name, ".ccg-") {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			return fail("unexpected_task_file")
		}
		path := filepath.Join(c.TaskSignalDirectory, name)
		if kernelTrustedFile(path) != nil {
			return fail("task_file_permissions")
		}
		var r TaskRecord
		if readRegularJSON(path, 16384, &r, true) != nil {
			return fail("task_file_invalid")
		}
		if r.Schema != 1 || r.SourceID != c.TaskSourceID || r.InstanceID != c.InstanceID || r.BootID != boot || !runIdentifier.MatchString(r.ID) || name != r.ID+".json" || r.Sequence == 0 || !taskStage(r.Stage) || len(r.Roots) == 0 || len(r.Roots) > 64 || !freshKernel(r.Time, now, time.Duration(c.TaskMaxAgeSeconds)*time.Second) {
			return fail("task_identity_or_age_invalid")
		}
		start, e := processStart(r.PID)
		if e != nil || start != r.ProcessStart {
			return fail("task_process_identity_invalid")
		}
		for _, p := range r.Roots {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p {
				return fail("task_root_invalid")
			}
		}
		if old, ok := g.previous[r.ID]; ok && (r.Sequence < old.Sequence || r.Time.Before(old.Time) || r.Stage != old.Stage || r.ProcessStart != old.ProcessStart || r.PID != old.PID || strings.Join(r.Roots, "\x00") != strings.Join(old.Roots, "\x00")) {
			return fail("task_regressed_or_changed")
		}
		next[r.ID] = r
		s.Active = append(s.Active, r)
	}
	g.previous = next
	s.State = "ready"
	return s
}
func taskProtects(s TaskStatus, path string) bool {
	if s.State == "disabled" {
		return false
	}
	if s.State != "ready" {
		return true
	}
	for _, t := range s.Active {
		if t.Stage == "verify" || t.Stage == "compress" || t.Stage == "transfer" || t.Stage == "restore" {
			for _, root := range t.Roots {
				if beneath(path, root) {
					return true
				}
			}
		}
	}
	return false
}

type taskExit struct{ code int }

func (e taskExit) Error() string { return fmt.Sprintf("wrapped task exited %d", e.code) }
func taskRunCommand(args []string) error {
	fs := flag.NewFlagSet("task-run", flag.ContinueOnError)
	directory := fs.String("directory", "", "预先创建的私有0700任务目录")
	source := fs.String("source-id", "", "与CCG绑定的任务来源")
	instance := fs.String("instance-id", "", "CCG实例")
	stage := fs.String("stage", "backup", "backup|verify|compress|transfer|sync|restore")
	var roots pathsFlag
	fs.Var(&roots, "root", "本任务会访问的规范绝对路径，可重复")
	if e := fs.Parse(args); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if !identifier.MatchString(*source) || !identifier.MatchString(*instance) || !taskStage(*stage) || len(roots) == 0 || len(roots) > 64 || fs.NArg() == 0 {
		return errors.New("task-run参数不完整；用 -- 分隔原命令与全部原参数")
	}
	for _, p := range roots {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("任务根须为规范绝对路径")
		}
	}
	dir, e := taskDirectory(*directory)
	if e != nil {
		return e
	}
	defer dir.Close()
	// Refuse a changed/replaced directory during every heartbeat.
	identityBefore, e := statFile(dir)
	if e != nil {
		return e
	}
	id, e := newRunID()
	if e != nil {
		return e
	}
	boot, e := readBootID()
	if e != nil {
		return e
	}
	start, e := processStart(os.Getpid())
	if e != nil {
		return e
	}
	record := TaskRecord{Schema: 1, SourceID: *source, InstanceID: *instance, BootID: boot, ID: id, Stage: *stage, Roots: roots, PID: os.Getpid(), ProcessStart: start}
	path := filepath.Join(*directory, id+".json")
	publish := func() error {
		check, e := taskDirectory(*directory)
		if e != nil {
			return e
		}
		defer check.Close()
		nowID, e := statFile(check)
		if e != nil || nowID.Dev != identityBefore.Dev || nowID.Ino != identityBefore.Ino {
			return errors.New("task directory identity changed")
		}
		record.Sequence++
		record.Time = time.Now()
		return writePrivateJSON(path, record)
	}
	if e = publish(); e != nil {
		return e
	}
	// All arguments and streams pass directly to exec, without shell quoting.
	child := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if e = child.Start(); e != nil {
		_ = os.Remove(path)
		return e
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var signalErr error
	for {
		select {
		case err := <-done:
			// Keep a stale marker on heartbeat/storage failure, so controllers deny.
			if signalErr == nil {
				_ = os.Remove(path)
			}
			if err != nil {
				if ex, ok := err.(*exec.ExitError); ok {
					code := ex.ExitCode()
					if ws, ok := ex.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
						code = 128 + int(ws.Signal())
					}
					return taskExit{code}
				}
				return err
			}
			if signalErr != nil {
				fmt.Fprintln(os.Stderr, "task-run: heartbeat failed; original task succeeded; stale marker retained")
			}
			return nil
		case sig := <-sigs:
			_ = syscall.Kill(-child.Process.Pid, sig.(syscall.Signal))
		case <-tick.C:
			if signalErr == nil {
				signalErr = publish()
			}
		}
	}
}
func writePrivateJSON(path string, value any) error {
	// Provisioning and ownership checks precede this atomic writer. It never
	// follows a destination symlink, and new data is private by default.
	return atomicJSON(path, value)
}
