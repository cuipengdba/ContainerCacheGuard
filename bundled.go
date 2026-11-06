package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"pgcacheguard/internal/native"
)

var clientNames = []string{"pg_basebackup", "pg_verifybackup", "pg_combinebackup", "pg_waldump", "psql"}

func configurationEnvironment() string {
	if value := os.Getenv("PGCACHEGUARD_CONFIG"); value != "" {
		return value
	}
	return os.Getenv("PG18GUARD_CONFIG") // development-version compatibility
}

// Each invocation uses a fresh owner-only directory. No PATH lookup, shared
// cache of executables, or pre-installed PostgreSQL client is required.
func unpackClients(parent string) (string, func(), error) {
	name := "assets/tools-linux-" + runtime.GOARCH + ".tar.gz"
	f, err := native.Assets.Open(name)
	if err != nil {
		return "", nil, fmt.Errorf("embedded PG18 clients missing for %s: %w", runtime.GOARCH, err)
	}
	defer f.Close()
	if parent != "" {
		if err = os.MkdirAll(parent, 0700); err != nil {
			return "", nil, err
		}
	}
	dir, err := os.MkdirTemp(parent, "pgcacheguard-clients-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	z, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, err
	}
	defer z.Close()
	t := tar.NewReader(z)
	seen := map[string]bool{}
	allowed := map[string]bool{}
	for _, v := range clientNames {
		allowed[v] = true
	}
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", nil, e
		}
		if !allowed[h.Name] || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > 64<<20 {
			return "", nil, errors.New("invalid embedded client archive member")
		}
		p := filepath.Join(dir, h.Name)
		out, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
		if e != nil {
			return "", nil, e
		}
		_, e = io.CopyN(out, t, h.Size)
		closeErr := out.Close()
		if e != nil {
			return "", nil, e
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		seen[h.Name] = true
	}
	if len(seen) != len(clientNames) {
		return "", nil, errors.New("incomplete embedded PG18 toolchain")
	}
	ok = true
	return dir, cleanup, nil
}

func (c *Config) prepareClients() (func(), error) {
	needed := c.Basebackup == "" || c.Verifybackup == "" || c.Combinebackup == "" || c.PSQL == ""
	if !needed {
		return func() {}, nil
	}
	dir, cleanup, err := unpackClients(c.RuntimeDir)
	if err != nil {
		return nil, err
	}
	c.bundledDir = dir
	for _, p := range []struct {
		field *string
		name  string
	}{
		{&c.Basebackup, "pg_basebackup"}, {&c.Verifybackup, "pg_verifybackup"},
		{&c.Combinebackup, "pg_combinebackup"}, {&c.PSQL, "psql"},
	} {
		if *p.field == "" {
			*p.field = filepath.Join(dir, p.name)
		}
	}
	return cleanup, nil
}

func nativeCommandName(command string) string {
	switch command {
	case "basebackup":
		return "pg_basebackup"
	case "combinebackup":
		return "pg_combinebackup"
	case "verifybackup":
		return "pg_verifybackup"
	default:
		return command
	}
}

func clientInformation(command string, args []string) (bool, error) {
	command = nativeCommandName(command)
	allowed := false
	for _, v := range clientNames {
		if command == v {
			allowed = true
		}
	}
	if !allowed || len(args) != 1 {
		return false, nil
	}
	if args[0] != "--help" && args[0] != "-?" && args[0] != "--version" && args[0] != "-V" {
		return false, nil
	}
	dir, cleanup, err := unpackClients("")
	if err != nil {
		return true, err
	}
	defer cleanup()
	cmd := exec.Command(filepath.Join(dir, command), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return true, cmd.Run()
}

// Used in receipts and release checks, without printing executable contents.
func clientDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cleanGuardEnvironment(env []string) []string {
	var out []string
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if key != "LD_PRELOAD" && !strings.HasPrefix(key, "PG18GUARD_") && !strings.HasPrefix(key, "PGCACHEGUARD_") && !strings.HasPrefix(key, "PGBACKREST_") {
			out = append(out, value)
		}
	}
	return out
}
