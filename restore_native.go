package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func requireEmptyStoppedPGData(c Config) error {
	if _, err := os.Lstat(filepath.Join(c.PGData, "postmaster.pid")); !os.IsNotExist(err) {
		return errors.New("configured PGDATA has a postmaster.pid or cannot be checked; stop and inspect the database before restoring")
	}
	socket := filepath.Join(c.PGHost, ".s.PGSQL."+strconv.Itoa(c.PGPort))
	conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return errors.New("database socket is accepting connections; refuse PGDATA restore")
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot establish that database socket is stopped: %w", err)
	}
	entries, err := os.ReadDir(c.PGData)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("configured PGDATA is nonempty; it will never be overwritten")
	}
	return nil
}

// Existing DBS asks pg_combinebackup to output directly to PGDATA. With this
// explicit opt-in, reconstruct and verify in a sibling first, then publish to
// an empty/stopped target. Failures retain the staging directory and backups.
func (r *Runner) combineOutput(path string, dry bool) (target, final string, publish func() error, err error) {
	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return
		}
	}
	final, err = canonical(path)
	if err != nil {
		return
	}
	if final == r.c.PGData {
		if !r.c.RestoreToPGData {
			err = errors.New("restoring configured PGDATA requires allow_stopped_pgdata_restore=true; otherwise use a separate staging target")
			return
		}
		if err = requireEmptyStoppedPGData(r.c); err != nil {
			return
		}
		if dry {
			target = final
			return
		}
		if err = os.MkdirAll(filepath.Dir(final), 0700); err != nil {
			return
		}
		target, err = os.MkdirTemp(filepath.Dir(final), ".pgcacheguard-restore-")
		if err != nil {
			return
		}
		publish = func() error {
			if err := requireEmptyStoppedPGData(r.c); err != nil {
				return err
			}
			if _, err := os.Lstat(final); err == nil {
				// Remove only an empty directory. A mount point or concurrent file
				// creation makes this fail, with the verified staging copy intact.
				if err = os.Remove(final); err != nil {
					return fmt.Errorf("cannot publish over target directory/mount point; verified staging retained at %s: %w", target, err)
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := renameNoReplace(target, final); err != nil {
				return fmt.Errorf("restore publish refused; verified staging retained at %s: %w", target, err)
			}
			return syncDir(filepath.Dir(final))
		}
		return
	}
	if within(r.c.Repository, final) {
		target, err = r.nativeOutput(final)
	} else {
		target, err = restoreTarget(r.c, final)
	}
	return
}
