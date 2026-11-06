package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"pgcacheguard/internal/native"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func nativeRate(text string) (uint64, error) {
	m := float64(1024)
	if strings.HasSuffix(text, "M") {
		m *= 1024
		text = strings.TrimSuffix(text, "M")
	} else {
		text = strings.TrimSuffix(text, "k")
	}
	n, e := strconv.ParseFloat(text, 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n*m < 32768 || n*m > 1073741824 {
		return 0, errors.New("max-rate must be PostgreSQL's 32KiB/s..1GiB/s value")
	}
	return uint64(n * m), nil
}
func nativeEntry(configPath, command string, args []string) error {
	if handled, err := clientInformation(command, args); handled {
		return err
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return errors.New("native backup guard requires Linux amd64 or arm64")
	}
	if configPath == "" {
		return errors.New("set PGCACHEGUARD_CONFIG or pass -config before the command")
	}
	c, e := loadConfig(configPath)
	if e != nil {
		return e
	}
	if c.Backend != "native" {
		return errors.New("utility compatibility mode requires backend=native")
	}
	if e = c.resolve(); e != nil {
		return e
	}
	switch command {
	case "basebackup":
		command = "pg_basebackup"
	case "combinebackup":
		command = "pg_combinebackup"
	case "verifybackup":
		command = "pg_verifybackup"
	}
	if !map[string]bool{"pg_basebackup": true, "pg_combinebackup": true, "pg_verifybackup": true, "doctor": true}[command] {
		return errors.New("native commands: basebackup, combinebackup, verifybackup, doctor")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.DeadlineSeconds)*time.Second)
	defer cancel()
	observer, e := newObserver(c)
	if e != nil {
		return e
	}
	preflight := func() error {
		s, e := observer.Sample()
		if e != nil {
			return e
		}
		bad, _, why := c.policy(s)
		if bad {
			return fmt.Errorf("admission refused: %s", why)
		}
		return ctx.Err()
	}
	if e = preflight(); e != nil {
		return e
	}
	cleanupClients, e := c.prepareClients()
	if e != nil {
		return e
	}
	defer cleanupClients()
	if command == "doctor" {
		if e = checkPG18(ctx, c); e != nil {
			return e
		}
		s, e := observer.Sample()
		event("doctor", map[string]any{"backend": "native", "sample": s})
		return e
	}
	if command == "pg_basebackup" {
		if c.bundledDir != "" && within(c.bundledDir, c.Basebackup) {
			v, err := os.ReadFile(filepath.Join(c.PGData, "PG_VERSION"))
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(v)) != "18" {
				return errors.New("mounted PGDATA must be PostgreSQL 18")
			}
			f, err := os.Open(filepath.Join(c.PGData, "global", "pg_control"))
			if err != nil {
				return err
			}
			var b [8]byte
			_, err = io.ReadFull(f, b[:])
			_ = f.Close()
			if err != nil {
				return err
			}
			c.expectedSystemID = strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10)
		} else if e = checkPG18(ctx, c); e != nil {
			return e
		}
	}
	// Check the actual tool major versions, and reject a PATH wrapper recursion.
	self, _ := os.Executable()
	for _, p := range []string{c.Basebackup, c.Verifybackup, c.Combinebackup} {
		p0, _ := canonical(p)
		self0, _ := canonical(self)
		if p0 == self0 {
			return errors.New("real PostgreSQL utility path resolves to guard itself")
		}
		out, e := exec.CommandContext(ctx, p, "--version").Output()
		if e != nil || !strings.Contains(string(out), "(PostgreSQL) 18.") {
			return fmt.Errorf("expected PostgreSQL 18 utility: %s", p)
		}
	}
	release, e := acquireLocks(ctx, c.LockFiles, time.Duration(c.LockWaitSeconds)*time.Second)
	if e != nil {
		return e
	}
	defer release()
	if e = preflight(); e != nil {
		return e
	}
	if e = os.MkdirAll(c.RuntimeDir, 0750); e != nil {
		return e
	}
	if e = os.MkdirAll(c.Repository, 0700); e != nil {
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
	hook := ""
	for _, client := range []string{c.Basebackup, c.Verifybackup, c.Combinebackup} {
		if c.bundledDir != "" && within(c.bundledDir, client) {
			continue
		}
		b, err := native.Assets.ReadFile("assets/libpg18guard.so")
		if err != nil {
			return errors.New("this architecture only supports the bundled static clients")
		}
		hook = filepath.Join(dir, "libpg18guard.so")
		if e = os.WriteFile(hook, b, 0500); e != nil {
			return e
		}
		break
	}
	r := Runner{c, observer, dir, control, hook, ctx}
	result := map[string]any{"version": version, "backend": "native", "command": command, "started_at": time.Now().UTC(), "verified": false, "cache_policy": c.CachePolicy}
	defer func() {
		result["finished_at"] = time.Now().UTC()
		stats, se := control.stats()
		if se == nil {
			result["io"] = stats
		}
		if e = atomicJSON(filepath.Join(dir, "result.json"), result); e != nil {
			event("receipt_error", map[string]any{"error": e.Error()})
		}
	}()
	event("admitted", map[string]any{"backend": "native", "command": command, "run_directory": dir})
	var runErr error
	switch command {
	case "pg_basebackup":
		runErr = r.nativeBackup(args, result, preflight)
	case "pg_combinebackup":
		runErr = r.nativeCombine(args, result, preflight)
	case "pg_verifybackup":
		// Existing DBS invokes these two weak flags. Accept the spelling but
		// strengthen the operation to complete checksums and WAL validation.
		var paths []string
		for _, a := range args {
			if a == "--skip-checksums" || a == "--no-parse-wal" || a == "-s" || a == "-n" {
				event("verification_strengthened", map[string]any{"ignored_weak_option": a})
				continue
			}
			paths = append(paths, a)
		}
		if len(paths) != 1 || strings.HasPrefix(paths[0], "-") {
			runErr = errors.New("verifybackup requires exactly one backup directory")
		} else {
			var p string
			p, runErr = r.nativeInput(paths[0])
			if runErr == nil {
				runErr = r.nativeVerify(p)
				result["verified"] = runErr == nil
			}
		}
	}
	result["success"] = runErr == nil
	if runErr != nil {
		result["error"] = runErr.Error()
		return runErr
	}
	event("completed", map[string]any{"run_directory": dir, "verified": result["verified"]})
	return nil
}
func (r *Runner) nativeInput(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("backup paths must be absolute")
	}
	p, e := canonical(path)
	if e != nil {
		return "", e
	}
	if p == r.c.Repository || !within(r.c.Repository, p) {
		return "", errors.New("backup input must be below configured repository")
	}
	if _, e = os.Stat(filepath.Join(p, "backup_manifest")); e != nil {
		return "", e
	}
	return p, nil
}
func (r *Runner) nativeOutput(path string) (string, error) {
	if !filepath.IsAbs(path) {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
	}
	p, e := canonical(path)
	if e != nil {
		return "", e
	}
	if p == r.c.Repository || !within(r.c.Repository, p) {
		return "", errors.New("backup output must be below configured repository")
	}
	if _, e = os.Lstat(p); e == nil {
		entries, err := os.ReadDir(p)
		if err != nil {
			return "", err
		}
		if len(entries) != 0 {
			return "", errors.New("backup output must be absent or empty")
		}
	} else if !os.IsNotExist(e) {
		return "", e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return "", e
	}
	return p, nil
}
func (r *Runner) nativeVerify(path string) error {
	return r.execute("verify", r.c.Verifybackup, []string{path}, nil, []string{path}, nil, false, nil)
}
func (r *Runner) nativeBackup(args []string, result map[string]any, preflight func() error) error {
	o, e := parseBaseOptions(args)
	if e != nil {
		return e
	}
	if r.c.expectedSystemID == "" {
		if o.connstr != "" {
			return errors.New("connection strings require the bundled identity-checking PG18 client")
		}
		for _, v := range []struct{ got, want string }{{o.host, r.c.PGHost}, {o.user, r.c.PGUser}, {o.port, strconv.Itoa(r.c.PGPort)}} {
			if v.got != "" && v.got != v.want {
				return errors.New("connection options must match the identity-checked configuration")
			}
		}
	}
	plain := strings.EqualFold(o.format, "p") || strings.EqualFold(o.format, "plain")
	client := o.target == "client"
	localOutput := client && o.destination != "-"
	if r.c.CachePolicy == "preserve-hot" && (!plain || !localOutput || o.waldir != "") {
		return errors.New("source progress cache protection requires plain client output with default WAL placement; explicitly select cache_policy=destination-only or off for other native modes")
	}
	dest := ""
	var writeRoots []string
	if localOutput {
		dest, e = r.nativeOutput(o.destination)
		if e != nil {
			return e
		}
		writeRoots = append(writeRoots, dest)
	}
	if o.waldir != "" {
		p, err := r.auxiliaryBackupOutput(o.waldir)
		if err != nil {
			return fmt.Errorf("WAL output: %w", err)
		}
		writeRoots = append(writeRoots, p)
	}
	for _, mapping := range o.tablespaces {
		_, target, err := parseTablespaceMapping(mapping)
		if err != nil {
			return err
		}
		p, err := r.auxiliaryBackupOutput(target)
		if err != nil {
			return fmt.Errorf("tablespace output: %w", err)
		}
		writeRoots = append(writeRoots, p)
	}
	limit := r.c.ReadBPS
	if r.c.WriteBPS < limit {
		limit = r.c.WriteBPS
	}
	if limit > 1<<30 {
		limit = 1 << 30
	}
	if o.rate != "" {
		n, e := nativeRate(o.rate)
		if e != nil {
			return e
		}
		if n < limit {
			limit = n
		}
	}
	r.c.ReadBPS = limit // recv pacing includes the WAL receiver; write cap is bounded by the receive stream.
	// Keep checkpoint, WAL mode, manifest algorithm, cleanup, compression and
	// all other PostgreSQL semantics. Only the configured resource ceiling is
	// applied explicitly; record its effective value in the receipt.
	a := []string{"-h", r.c.PGHost, "-p", strconv.Itoa(r.c.PGPort), "-U", r.c.PGUser}
	raw := append([]string{}, args...)
	terminated := len(raw) > 0 && raw[len(raw)-1] == "--"
	if terminated {
		raw = raw[:len(raw)-1]
	}
	a = append(a, raw...)
	a = append(a, "--max-rate="+strconv.FormatUint(limit/1024, 10))
	if terminated {
		a = append(a, "--")
	}
	result["effective_max_rate_bytes_per_second"] = limit
	result["source_cache_protected"] = r.c.CachePolicy == "preserve-hot"
	result["format"] = o.format
	result["target"] = o.target
	result["upstream_no_sync"] = o.noSync
	if o.manifest != "" {
		p, err := filepath.Abs(o.manifest)
		if err != nil {
			return err
		}
		p, err = canonical(p)
		if err != nil {
			return err
		}
		if !within(r.c.Repository, p) {
			return errors.New("parent manifest must be inside the configured repository")
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			return errors.New("parent manifest must be a readable regular file")
		}
		result["parent_manifest"] = p
	}
	var w *cacheWatch
	if r.c.CachePolicy == "preserve-hot" {
		w, e = newCacheWatch(r.ctx, r.c, dest, true, preflight)
		if e != nil {
			return e
		}
	}
	e = r.execute("backup", r.c.Basebackup, a, nil, []string{r.c.Repository}, writeRoots, client, w)
	if w != nil {
		we := w.finish()
		result["cache_observer"] = w.snapshot()
		if e == nil {
			e = we
		}
	}
	if e != nil {
		return e
	}
	if localOutput && plain && !o.noManifest {
		verifyArgs := []string{dest}
		wal := strings.ToLower(o.walMethod)
		walIncluded := wal != "n" && wal != "none"
		if !walIncluded {
			verifyArgs = append([]string{"--no-parse-wal"}, verifyArgs...)
		}
		if e = r.execute("verify", r.c.Verifybackup, verifyArgs, nil, []string{dest}, nil, false, nil); e != nil {
			return e
		}
		checksums := !strings.EqualFold(o.manifestChecksums, "NONE")
		result["file_checksums_verified"] = checksums
		result["wal_verified"] = walIncluded
		result["verified"] = walIncluded && checksums
	} else {
		result["verification_note"] = "This native mode has no automatic complete directory/WAL verification; perform the documented extraction/archive verification before recovery."
		event("verification_incomplete", map[string]any{"format": o.format, "target": o.target, "no_manifest": o.noManifest})
	}
	result["backup_directory"] = dest
	return nil
}

func (r *Runner) auxiliaryBackupOutput(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("auxiliary backup output must be absolute")
	}
	p, err := canonical(path)
	if err != nil {
		return "", err
	}
	if within(r.c.Repository, p) {
		return r.nativeOutput(p)
	}
	return restoreTarget(r.c, p)
}

func parseTablespaceMapping(text string) (string, string, error) {
	var old, new strings.Builder
	target := &old
	separator := false
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && text[i+1] == '=' {
			target.WriteByte('=')
			i++
			continue
		}
		if text[i] == '=' {
			if separator {
				return "", "", errors.New("invalid tablespace mapping")
			}
			separator = true
			target = &new
			continue
		}
		target.WriteByte(text[i])
	}
	if !separator || !filepath.IsAbs(old.String()) || !filepath.IsAbs(new.String()) {
		return "", "", errors.New("tablespace mapping requires absolute old=new paths")
	}
	return old.String(), new.String(), nil
}
func (r *Runner) nativeCombine(args []string, result map[string]any, preflight func() error) error {
	var output string
	var inputs []string
	clone, dry := false, false
	for i := 0; i < len(args); i++ {
		key, val, has := strings.Cut(args[i], "=")
		switch key {
		case "-o", "--output":
			if !has {
				i++
				if i == len(args) {
					return errors.New("missing combine output")
				}
				val = args[i]
			}
			output = val
		case "--clone":
			clone = true
		case "-n", "--dry-run":
			dry = true
		default:
			if strings.HasPrefix(key, "-") {
				return fmt.Errorf("unsupported combine option %s", key)
			}
			p, e := r.nativeInput(args[i])
			if e != nil {
				return e
			}
			inputs = append(inputs, p)
		}
	}
	if output == "" || len(inputs) == 0 {
		return errors.New("combinebackup requires -o NEW_TARGET and an ordered full/incremental chain")
	}
	target, finalTarget, publish, e := r.combineOutput(output, dry)
	if e != nil {
		return e
	}
	for _, p := range inputs {
		if overlap(p, target) || overlap(p, finalTarget) {
			return errors.New("combine output must not overlap an input backup")
		}
	}
	result["restore_work_directory"] = target
	result["restore_target"] = finalTarget
	for _, p := range inputs {
		if dry {
			break
		} // upstream dry-run checks chain structure, not every data byte
		if e = r.nativeVerify(p); e != nil {
			return e
		}
	}
	a := []string{"--output=" + target, "--manifest-checksums=SHA256"}
	if clone {
		a = append(a, "--clone")
	}
	if dry {
		a = append(a, "--dry-run")
	}
	a = append(a, inputs...)
	var w *cacheWatch
	if !dry && r.c.CachePolicy != "off" {
		w, e = newCacheWatch(r.ctx, r.c, target, false, preflight)
		if e != nil {
			return e
		}
	}
	e = r.execute("combine", r.c.Combinebackup, a, nil, inputs, []string{target}, false, w)
	if w != nil {
		we := w.finish()
		result["cache_observer"] = w.snapshot()
		if e == nil {
			e = we
		}
	}
	if e != nil {
		return e
	}
	if !dry {
		if e = r.nativeVerify(target); e != nil {
			return e
		}
		result["verified"] = true
		if publish != nil {
			if e = publish(); e != nil {
				return e
			}
			result["published_to_configured_pgdata"] = true
		}
	}
	result["restore_target"] = finalTarget
	result["dry_run"] = dry
	if dry {
		result["verification_note"] = "native structural dry run only; no complete data checksum/WAL verification was performed"
	}
	return nil
}
