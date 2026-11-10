//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const logEventReserve = 512

type LoggingStatus struct {
	Enabled       bool   `json:"enabled"`
	Directory     string `json:"directory"`
	CurrentFile   string `json:"current_file"`
	Timezone      string `json:"timezone"`
	KeepDays      int    `json:"keep_days"`
	MaxTotalBytes int64  `json:"max_total_bytes"`
	ManagedBytes  int64  `json:"managed_bytes"`
	DeletedFiles  uint64 `json:"deleted_files_total"`
	Truncations   uint64 `json:"current_file_truncations_total"`
}

type dailyLogEntry struct {
	day  time.Time
	size int64
}

// The sampling goroutine is the sole owner. No unbounded queue, background
// writer, directory recursion, compression, tail-copy, or per-record fsync.
type dailyLogger struct {
	config      Config
	directory   *os.File
	lock        *os.File
	file        *os.File
	name        string
	location    *time.Location
	now         func() time.Time
	console     io.Writer
	files       map[string]dailyLogEntry
	total       int64
	lastScan    time.Time
	deleted     uint64
	truncations uint64
}

func newDailyLogger(c Config, console io.Writer, now func() time.Time) (*dailyLogger, error) {
	location, err := time.LoadLocation(c.LogTimezone)
	if err != nil {
		return nil, err
	}
	lock, err := lockState(filepath.Join(c.logDirectory(), ".writer"))
	if err != nil {
		return nil, fmt.Errorf("daily log directory: %w", err)
	}
	directory, err := safeOpen(c.logDirectory())
	if err != nil {
		lock.Close()
		return nil, err
	}
	l := &dailyLogger{config: c, directory: directory, lock: lock, location: location,
		console: console, now: now, files: make(map[string]dailyLogEntry)}
	if err = l.Maintain(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *dailyLogger) openManaged(name string, create bool) (*os.File, error) {
	flags := syscall.O_WRONLY | syscall.O_APPEND | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := syscall.Openat(int(l.directory.Fd()), name, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var stat syscall.Stat_t
	err = syscall.Fstat(fd, &stat)
	if err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != uint32(os.Geteuid()) ||
		stat.Nlink != 1 || stat.Mode&0077 != 0 {
		f.Close()
		return nil, errors.New("managed log must be a private, single-link regular file owned by this UID")
	}
	return f, nil
}

func (l *dailyLogger) ensureDay(now time.Time) (bool, error) {
	name := "containercacheguard-" + now.In(l.location).Format("2006-01-02") + ".log"
	if name == l.name {
		return false, nil
	}
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return false, err
		}
		l.file = nil
	}
	f, err := l.openManaged(name, true)
	if err != nil {
		return false, err
	}
	l.file, l.name = f, name
	return true, nil
}

func logDay(name string, location *time.Location) (time.Time, bool) {
	const prefix, suffix = "containercacheguard-", ".log"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return time.Time{}, false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	d, err := time.ParseInLocation("2006-01-02", date, location)
	return d, err == nil && len(date) == 10 && d.Format("2006-01-02") == date
}

func (l *dailyLogger) scan(now time.Time) error {
	if _, err := l.directory.Seek(0, io.SeekStart); err != nil {
		return err
	}
	files := make(map[string]dailyLogEntry)
	var total int64
	entries := 0
	for {
		batch, err := l.directory.ReadDir(256)
		for _, entry := range batch {
			entries++
			if entries > 8192 {
				return errors.New("daily log directory exceeds 8192 entries; use a dedicated directory")
			}
			day, managed := logDay(entry.Name(), l.location)
			if !managed {
				continue
			}
			f, openErr := l.openManaged(entry.Name(), false)
			if openErr != nil {
				return fmt.Errorf("unsafe or inaccessible daily log: %w", openErr)
			}
			info, statErr := f.Stat()
			if entry.Name() == l.name {
				id, idErr := statFile(f)
				active, activeErr := statFile(l.file)
				if idErr != nil || activeErr != nil || id.Dev != active.Dev || id.Ino != active.Ino {
					f.Close()
					return errors.New("active daily log identity changed")
				}
			}
			f.Close()
			if statErr != nil {
				return statErr
			}
			if info.Size() < 0 || total > math.MaxInt64-info.Size() {
				return errors.New("daily log size overflow")
			}
			files[entry.Name()] = dailyLogEntry{day, info.Size()}
			total += info.Size()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if _, ok := files[l.name]; !ok {
		return errors.New("active daily log disappeared")
	}
	l.files, l.total, l.lastScan = files, total, now
	return nil
}

func (l *dailyLogger) remove(name string) error {
	// Revalidate before unlink, relative to our locked private directory FD.
	f, err := l.openManaged(name, false)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Unlinkat(int(l.directory.Fd()), name); err != nil {
		return err
	}
	l.total -= l.files[name].size
	delete(l.files, name)
	l.deleted++
	return nil
}

func (l *dailyLogger) prepare(now time.Time, reserve int64) error {
	changed, err := l.ensureDay(now)
	if err != nil {
		return err
	}
	due := changed || l.lastScan.IsZero() || now.Before(l.lastScan) || now.Sub(l.lastScan) >= time.Minute
	if due {
		if err = l.scan(now); err != nil {
			return err
		}
	}
	if !due && l.total+reserve <= l.config.LogMaxTotalBytes {
		return nil
	}
	beforeDeleted, beforeTruncations := l.deleted, l.truncations
	day, _ := logDay(l.name, l.location)
	cutoff := day.AddDate(0, 0, -(l.config.LogKeepDays - 1))
	names := make([]string, 0, len(l.files))
	for name := range l.files {
		if name != l.name {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if l.files[name].day.Before(cutoff) || l.total+reserve > l.config.LogMaxTotalBytes {
			if err = l.remove(name); err != nil {
				return err
			}
		}
	}
	if l.total+reserve > l.config.LogMaxTotalBytes {
		// Keep one file per day: discard this day's old content in-place. Never
		// copy a GB tail, create numbered parts, or touch an unrelated file.
		if err = l.file.Truncate(0); err != nil {
			return err
		}
		e := l.files[l.name]
		l.total -= e.size
		e.size = 0
		l.files[l.name] = e
		l.truncations++
	}
	if l.deleted != beforeDeleted || l.truncations != beforeTruncations {
		event, _ := json.Marshal(map[string]any{"time": now.In(l.location), "event": "log_cleanup",
			"deleted_files": l.deleted - beforeDeleted, "current_file_truncated": l.truncations != beforeTruncations})
		event = append(event, '\n')
		if len(event) > logEventReserve {
			return errors.New("cleanup event exceeded reserved space")
		}
		return l.append(event)
	}
	return nil
}

func (l *dailyLogger) append(line []byte) error {
	n, err := l.file.Write(line)
	e := l.files[l.name]
	e.size += int64(n)
	l.files[l.name] = e
	l.total += int64(n)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	n, err = l.console.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	return err
}

func (l *dailyLogger) WriteJSON(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if int64(len(b)+logEventReserve) > l.config.LogMaxTotalBytes {
		return errors.New("one log record exceeds the configured total log budget")
	}
	if err = l.prepare(l.now(), int64(len(b)+logEventReserve)); err != nil {
		return err
	}
	return l.append(b)
}

func (l *dailyLogger) Maintain() error {
	now := l.now()
	if l.name != "containercacheguard-"+now.In(l.location).Format("2006-01-02")+".log" ||
		l.lastScan.IsZero() || now.Before(l.lastScan) || now.Sub(l.lastScan) >= time.Minute {
		return l.prepare(now, logEventReserve)
	}
	return nil
}

func (l *dailyLogger) Status() LoggingStatus {
	return LoggingStatus{true, l.config.logDirectory(), l.name, l.config.LogTimezone,
		l.config.LogKeepDays, l.config.LogMaxTotalBytes, l.total, l.deleted, l.truncations}
}

func (l *dailyLogger) Close() error {
	var result error
	for _, f := range []*os.File{l.file, l.directory, l.lock} {
		if f != nil {
			if err := f.Close(); result == nil {
				result = err
			}
		}
	}
	l.file, l.directory, l.lock = nil, nil, nil
	return result
}
