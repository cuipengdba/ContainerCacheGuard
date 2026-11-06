package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pgcacheguard/internal/native"
)

const version = "0.1.0-dev"

type Runner struct {
	c        Config
	observer *Observer
	dir      string
	control  *Control
	hook     string
	ctx      context.Context
}
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("command output exceeds safety limit")
	}
	return b.Buffer.Write(p)
}
func main() {
	if err := mainRun(); err != nil {
		event("failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}
func mainRun() error {
	// A deployment can place this binary in a dedicated PATH directory under
	// PostgreSQL utility names. The real executables stay at configured paths.
	if map[string]bool{"pg_basebackup": true, "pg_combinebackup": true, "pg_verifybackup": true}[filepath.Base(os.Args[0])] {
		return nativeEntry(configurationEnvironment(), filepath.Base(os.Args[0]), os.Args[1:])
	}
	// With a configured environment the program also accepts pg_basebackup's
	// argv directly. Explicit subcommands keep the guard's own flags unambiguous.
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "-") && !map[string]bool{"-config": true, "--config": true, "-version": true, "--version": true, "-help": true, "--help": true}[os.Args[1]] && !strings.HasPrefix(os.Args[1], "-config=") && !strings.HasPrefix(os.Args[1], "--config=") {
		return nativeEntry(configurationEnvironment(), "pg_basebackup", os.Args[1:])
	}
	f := flag.NewFlagSet("pgcacheguard", flag.ContinueOnError)
	configPath := f.String("config", configurationEnvironment(), "configuration JSON file (or PGCACHEGUARD_CONFIG)")
	showVersion := f.Bool("version", false, "print version")
	f.Usage = func() {
		fmt.Fprintln(os.Stderr, "pgcacheguard -config /path/config.json COMMAND [POSTGRESQL OPTIONS]\nCommands: doctor, basebackup, verifybackup, combinebackup\nAliases: pg_basebackup, pg_verifybackup, pg_combinebackup\nExample: pgcacheguard -config /etc/pgcacheguard.json basebackup -D /backup/full -Fp -Xstream -r20M\nNative help: pgcacheguard basebackup --help\nDirect compatibility: PGCACHEGUARD_CONFIG=/etc/pgcacheguard.json pgcacheguard -D /backup/full -Fp -Xstream\nThe native PostgreSQL 18 toolchain is embedded. See README for cache-policy coverage.")
		f.PrintDefaults()
	}
	if e := f.Parse(os.Args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if *showVersion {
		fmt.Println("pgcacheguard", version)
		return nil
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return errors.New("this build requires Linux amd64 or arm64; Windows supports development tests only")
	}
	if len(f.Args()) > 0 {
		if handled, err := clientInformation(f.Args()[0], f.Args()[1:]); handled {
			return err
		}
	}
	if *configPath == "" || len(f.Args()) == 0 {
		f.Usage()
		return errors.New("config and command are required")
	}
	c, e := loadConfig(*configPath)
	if e != nil {
		return e
	}
	if e = c.resolve(); e != nil {
		return e
	}
	if c.Backend == "native" {
		return nativeEntry(*configPath, f.Args()[0], f.Args()[1:])
	}
	if e = validateEngineConfig(c.EngineConfig); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.DeadlineSeconds)*time.Second)
	defer cancel()
	observer, e := newObserver(c)
	if e != nil {
		return e
	}
	command := f.Args()[0]
	args := f.Args()[1:]
	if command == "doctor" {
		if len(args) > 0 {
			return errors.New("doctor takes no arguments")
		}
		s, e := observer.Sample()
		if e != nil {
			return e
		}
		bad, warning, reason := c.policy(s)
		event("doctor", map[string]any{"version": version, "sample": s, "critical": bad, "warning": warning, "reason": reason, "pgdata": c.PGData, "repository": c.Repository})
		if e = checkPG18(ctx, c); e != nil {
			return e
		}
		if bad {
			return errors.New(reason)
		}
		return nil
	}
	if !map[string]bool{"init": true, "backup": true, "verify": true, "restore": true, "expire": true}[command] {
		return fmt.Errorf("unknown command %q", command)
	}
	// Parse command-specific flags before touching repository state.
	cf := flag.NewFlagSet(command, flag.ContinueOnError)
	kind := cf.String("type", "full", "backup type")
	set := cf.String("set", "", "backup label")
	target := cf.String("target", "", "new restore directory")
	confirm := cf.Bool("confirm-verified", false, "confirm restore/verification policy before explicit expiration")
	if e = cf.Parse(args); e != nil {
		return e
	}
	if len(cf.Args()) != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !map[string]bool{"full": true, "diff": true, "incr": true}[*kind] {
		return errors.New("backup type must be full, diff or incr")
	}
	if *set != "" && !regexp.MustCompile(`^[0-9]{8}-[0-9]{6}F(_[0-9]{8}-[0-9]{6}[DI])?$`).MatchString(*set) {
		return errors.New("invalid pgBackRest backup label")
	}
	if command != "backup" && *kind != "full" {
		return errors.New("--type is only valid for backup")
	}
	if command != "restore" && *target != "" {
		return errors.New("--target is only valid for restore")
	}
	if (command == "backup" || command == "init" || command == "expire") && *set != "" {
		return errors.New("--set is only valid for verify/restore")
	}
	if command == "restore" && (*set == "" || *target == "") {
		return errors.New("restore requires an explicit --set and --target")
	}
	if command == "expire" && !*confirm {
		return errors.New("expire requires --confirm-verified; last-known-good retention must be reviewed")
	}
	if command != "expire" && *confirm {
		return errors.New("--confirm-verified is only valid for expire")
	}
	if command == "backup" || command == "init" {
		if e = checkPG18(ctx, c); e != nil {
			return e
		}
	}
	if command == "restore" {
		*target, e = restoreTarget(c, *target)
		if e != nil {
			return e
		}
	}
	release, e := acquireLocks(ctx, c.LockFiles, time.Duration(c.LockWaitSeconds)*time.Second)
	if e != nil {
		return e
	}
	defer release()
	s, e := observer.Sample()
	if e != nil {
		return e
	}
	if bad, _, why := c.policy(s); bad {
		return fmt.Errorf("admission refused: %s", why)
	}
	if e = os.MkdirAll(c.RuntimeDir, 0750); e != nil {
		return e
	}
	dir, e := os.MkdirTemp(c.RuntimeDir, command+"-")
	if e != nil {
		return e
	}
	control, e := newControl(filepath.Join(dir, "control.bin"), c.ReadBPS, c.WriteBPS)
	if e != nil {
		return e
	}
	defer control.close()
	b, e := native.Assets.ReadFile("assets/libpg18guard.so")
	if e != nil {
		return errors.New("native helper not embedded; use the documented Linux build")
	}
	hook := filepath.Join(dir, "libpg18guard.so")
	if e = os.WriteFile(hook, b, 0500); e != nil {
		return e
	}
	if e = os.Mkdir(filepath.Join(dir, "empty-config"), 0700); e != nil {
		return e
	}
	r := Runner{c, observer, dir, control, hook, ctx}
	event("admitted", map[string]any{"command": command, "run_directory": dir, "sample": s, "cache_policy": c.CachePolicy})
	result := map[string]any{"version": version, "command": command, "started_at": time.Now().UTC(), "verified": false, "cache_policy": c.CachePolicy}
	defer func() {
		result["finished_at"] = time.Now().UTC()
		stats, err := control.stats()
		if err == nil {
			result["io"] = stats
		}
		if err = atomicJSON(filepath.Join(dir, "result.json"), result); err != nil {
			event("receipt_error", map[string]any{"error": err.Error()})
		}
	}()
	var runErr error
	switch command {
	case "init":
		runErr = r.engine("stanza-create", nil, nil, []string{c.Repository})
	case "backup":
		runErr = r.engine("backup", []string{"--type=" + *kind, "--no-expire-auto", "--archive-copy=y"}, nil, []string{c.Repository})
		if runErr == nil {
			var label string
			label, runErr = r.backupLabel("")
			if runErr == nil {
				result["backup_label"] = label
				runErr = r.verify(label)
				if runErr == nil {
					result["verified"] = true
				}
			}
		}
	case "verify":
		if *set != "" {
			_, runErr = r.backupLabel(*set)
		} else {
			_, runErr = r.backupLabel("")
		}
		if runErr == nil {
			runErr = r.verify(*set)
			if runErr == nil {
				result["verified"] = true
			}
		}
	case "restore":
		_, runErr = r.backupLabel(*set)
		if runErr == nil {
			runErr = r.verify(*set)
		}
		if runErr == nil {
			runErr = r.engine("restore", []string{"--set=" + *set, "--pg1-path=" + *target, "--type=immediate"}, nil, []string{*target})
			if runErr == nil {
				result["verified"] = true
				result["restore_target"] = *target
				result["backup_label"] = *set
			}
		}
	case "expire":
		// A successful repository verification is required before any retention deletion.
		_, runErr = r.backupLabel("")
		if runErr == nil {
			runErr = r.verify("")
		}
		if runErr == nil {
			runErr = r.engine("expire", nil, nil, []string{c.Repository})
		}
	}
	result["success"] = runErr == nil
	if runErr != nil {
		result["error"] = runErr.Error()
		return runErr
	}
	event("completed", map[string]any{"command": command, "run_directory": dir})
	return nil
}
func checkPG18(ctx context.Context, c Config) error {
	if !filepath.IsAbs(c.PGHost) {
		return errors.New("this release requires a local Unix socket; remote workers need their own guard")
	}
	b, e := os.ReadFile(filepath.Join(c.PGData, "PG_VERSION"))
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != "18" {
		return errors.New("source PG_VERSION must be 18; the current PG14 containers are deliberately refused")
	}
	checkUser := c.PGCheckUser
	if checkUser == "" {
		checkUser = c.PGUser
	}
	cmd := exec.CommandContext(ctx, c.PSQL, "-XAtw", "-h", c.PGHost, "-p", strconv.Itoa(c.PGPort), "-U", checkUser, "-d", c.PGDatabase, "-c", "SELECT current_setting('server_version_num') || E'\\n' || current_setting('data_directory');")
	var out cappedBuffer
	out.limit = 4096
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("PostgreSQL preflight failed: %w", e)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		return errors.New("unexpected PostgreSQL identity response")
	}
	n, e := strconv.Atoi(strings.TrimSpace(lines[0]))
	if e != nil || n < 180000 || n >= 190000 {
		return errors.New("server must be PostgreSQL 18")
	}
	p, e := canonical(lines[1])
	if e != nil || p != c.PGData {
		return errors.New("SQL data_directory must resolve to configured PGDATA; mount it at the same path in the worker")
	}
	return nil
}
func validateEngineConfig(path string) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return errors.New("engine config too large")
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("pgBackRest config requires key=value syntax")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if strings.HasPrefix(key, "config") || regexp.MustCompile(`^(pg[0-9]+-host|repo[0-9]+-host)`).MatchString(key) || regexp.MustCompile(`^(pg[2-9][0-9]*-|repo[2-9][0-9]*-)`).MatchString(key) {
			return fmt.Errorf("unsupported remote/multiple-host/config-include option %s; source and repository must be local in this release", key)
		}
		if key == "repo1-type" && value != "posix" {
			return errors.New("this release requires repo1-type=posix")
		}
		if key == "backup-standby" && value != "n" {
			return errors.New("backup-standby is unsupported in local-worker mode")
		}
	}
	return nil
}
func restoreTarget(c Config, target string) (string, error) {
	if !filepath.IsAbs(target) {
		return "", errors.New("restore target must be absolute")
	}
	p, e := canonical(target)
	if e != nil {
		return "", e
	}
	if p == "/" || strings.ContainsAny(p, ":\n\r\x00") || overlap(p, c.PGData) || overlap(p, c.Repository) || overlap(p, c.RuntimeDir) {
		return "", errors.New("restore target overlaps PGDATA, repository, runtime or filesystem root")
	}
	entries, e := os.ReadDir(p)
	if e != nil && !os.IsNotExist(e) {
		return "", e
	}
	if len(entries) > 0 {
		return "", errors.New("restore target must be absent or empty; existing data is never overwritten")
	}
	return p, nil
}
func (r *Runner) baseArgs(phase string) []string {
	a := []string{"--config=" + r.c.EngineConfig, "--config-include-path=" + filepath.Join(r.dir, "empty-config"), "--stanza=" + r.c.Stanza, "--repo1-path=" + r.c.Repository, "--log-path=" + r.dir, "--log-level-console=info"}
	if phase != "verify" {
		a = append(a, "--lock-path="+filepath.Join(r.c.RuntimeDir, "engine-lock"))
	}
	if phase != "stanza-create" && phase != "verify" {
		a = append(a, "--repo=1")
	}
	if phase == "backup" || phase == "stanza-create" {
		a = append(a, "--pg1-path="+r.c.PGData, "--pg1-port="+strconv.Itoa(r.c.PGPort), "--pg1-socket-path="+r.c.PGHost, "--pg1-user="+r.c.PGUser)
	}
	if phase == "restore" {
		a = append(a, "--pg1-path="+r.c.PGData)
	}
	if phase == "backup" {
		a = append(a, "--start-fast=n", "--checksum-page-error=y")
	}
	if phase == "backup" || phase == "restore" || phase == "verify" {
		a = append(a, "--process-max="+strconv.Itoa(r.c.ProcessMax), "--buffer-size=1MiB")
	}
	return a
}
func (r *Runner) engine(phase string, extra []string, capture io.Writer, writeRoots []string) error {
	args := r.baseArgs(phase) // Replace overrides rather than emitting duplicate options.
	for _, v := range extra {
		key, _, _ := strings.Cut(v, "=")
		for i := 0; i < len(args); i++ {
			k, _, _ := strings.Cut(args[i], "=")
			if k == key {
				args = append(args[:i], args[i+1:]...)
				break
			}
		}
		args = append(args, v)
	}
	args = append(args, phase)
	readRoots := []string{r.c.PGData, r.c.Repository}
	return r.execute(phase, r.c.Engine, args, capture, readRoots, writeRoots, false, nil)
}
func (r *Runner) execute(phase, executable string, args []string, capture io.Writer, readRoots, writeRoots []string, socketReads bool, watch *cacheWatch) error {
	roots := strings.Join(writeRoots, ":")
	cmd := exec.Command(executable, args...)
	groupCommand(cmd)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	if capture != nil {
		cmd.Stdout = capture
	}
	cmd.Stderr = os.Stderr
	cmd.Env = cleanGuardEnvironment(os.Environ())
	if r.c.bundledDir == "" || !within(r.c.bundledDir, executable) {
		cmd.Env = append(cmd.Env, "LD_PRELOAD="+r.hook)
	}
	cmd.Env = append(cmd.Env, "PG18GUARD_CONTROL="+filepath.Join(r.dir, "control.bin"), "PG18GUARD_READ_ROOTS="+strings.Join(readRoots, ":"), "PG18GUARD_WRITE_ROOTS="+roots, "PG18GUARD_FLUSH_BYTES="+strconv.FormatUint(r.c.FlushBytes, 10), "PG18GUARD_SUPERVISOR_PID="+strconv.Itoa(os.Getpid()))
	cmd.Env = append(cmd.Env, "PG18GUARD_READ_WINDOW_BYTES="+strconv.FormatUint(r.c.ReadWindowBytes, 10), "PG18GUARD_RATE_BURST_BYTES="+strconv.FormatUint(r.c.RateBurstBytes, 10))
	cmd.Env = append(cmd.Env, "PG18GUARD_CACHE_POLICY="+r.c.CachePolicy)
	if phase == "backup" && r.c.expectedSystemID != "" {
		cmd.Env = append(cmd.Env, "PGCACHEGUARD_EXPECT_SYSTEM_ID="+r.c.expectedSystemID, "PGCACHEGUARD_EXPECT_HOST="+r.c.PGHost, "PGCACHEGUARD_EXPECT_PORT="+strconv.Itoa(r.c.PGPort))
	}
	if socketReads {
		cmd.Env = append(cmd.Env, "PG18GUARD_SOCKET_READS=1")
	}
	before, e := r.control.stats()
	if e != nil {
		return e
	}
	if e = r.control.rates(r.c.ReadBPS, r.c.WriteBPS); e != nil {
		return e
	}
	initial, e := r.observer.Sample()
	if e != nil {
		return fmt.Errorf("phase admission monitoring failed: %w", e)
	}
	if critical, _, why := r.c.policy(initial); critical {
		return errors.New(why)
	}
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(time.Duration(r.c.PollMS) * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	event("phase_started", map[string]any{"phase": phase, "pid": cmd.Process.Pid})
	baseline := initial
	lastSampleLog := time.Time{}
	lastWarning := false
	stop := func(reason error) error {
		_ = r.control.abort()
		_ = terminateGroup(cmd, done, time.Duration(r.c.GraceSeconds)*time.Second)
		return reason
	}
	var watchDone <-chan struct{}
	if watch != nil {
		watchDone = watch.done
	}
	for {
		select {
		case <-watchDone:
			return stop(fmt.Errorf("cache observer stopped: %v", watch.err))
		case err := <-done:
			stats, se := r.control.stats()
			if se != nil {
				return se
			}
			event("phase_finished", map[string]any{"phase": phase, "io": stats})
			if err != nil {
				if b, ok := capture.(*cappedBuffer); ok {
					s := b.String()
					if len(s) > 4096 {
						s = s[len(s)-4096:]
					}
					return fmt.Errorf("%s failed: %w; output: %s", phase, err, s)
				}
				return fmt.Errorf("%s failed: %w", phase, err)
			}
			if stats["helper_loads"] <= before["helper_loads"] {
				return errors.New("I/O guard handshake missing; executable is not instrumented or preload is unavailable")
			}
			if stats["sync_failures"] > before["sync_failures"] {
				return errors.New("output durability flush failed")
			}
			if r.c.CachePolicy != "off" && (stats["mincore_failures"] > before["mincore_failures"] || stats["advice_failures"] > before["advice_failures"]) {
				return errors.New("cache observation or advice failed; inspect the receipt before accepting this backup")
			}
			return nil
		case <-r.ctx.Done():
			return stop(fmt.Errorf("%s canceled: %w", phase, r.ctx.Err()))
		case <-ticker.C:
			s, se := r.observer.Sample()
			if se != nil {
				return stop(fmt.Errorf("monitoring failed closed: %w", se))
			}
			for i, v := range s.Cgroups {
				if v.Events["oom_kill"] > baseline.Cgroups[i].Events["oom_kill"] {
					return stop(errors.New("observed cgroup OOM kill event increased"))
				}
			}
			critical, warning, why := r.c.policy(s)
			if critical {
				return stop(errors.New(why))
			}
			rb, wb := r.c.ReadBPS, r.c.WriteBPS
			if warning {
				rb /= 2
				wb /= 2
			}
			if se = r.control.rates(rb, wb); se != nil {
				return stop(se)
			}
			stats, se := r.control.stats()
			if se != nil {
				return stop(se)
			}
			if r.c.CachePolicy != "off" && (stats["mincore_failures"] > before["mincore_failures"] || stats["advice_failures"] > before["advice_failures"]) {
				return stop(errors.New("cache observation or advice failed"))
			}
			if time.Since(started) > 2*time.Second && stats["helper_loads"] <= before["helper_loads"] {
				return stop(errors.New("native helper load handshake failed"))
			}
			// Poll safety at full frequency, but bound routine output for DBS
			// subprocess callers which collect stdout/stderr in memory.
			if lastSampleLog.IsZero() || time.Since(lastSampleLog) >= 10*time.Second || warning != lastWarning {
				event("sample", map[string]any{"phase": phase, "sample": s, "io": stats, "throttled": warning, "reason": why})
				lastSampleLog, lastWarning = time.Now(), warning
			}
		}
	}
}
