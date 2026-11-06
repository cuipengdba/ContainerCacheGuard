//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Directory-level access hints improve discovery latency during large scans.
// They contain neither byte offsets nor heat information and are never direct
// authorization to evict. Kernel coalescing, dropped events and mmap accesses
// mean hints are incomplete; the budgeted ordinary sweep remains necessary.
type fileHint struct {
	path        string
	offset      int64
	until, next time.Time
}
type accessHints struct {
	fd                int
	watches           map[int]dirEntry
	paths             map[string]int
	files             map[string]*fileHint
	queue             []*fileHint
	cursor            int
	buffer            []byte
	events, overflows uint64
}

func newAccessHints() *accessHints {
	fd, e := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if e != nil {
		fd = -1
	}
	return &accessHints{fd: fd, watches: map[int]dirEntry{}, paths: map[string]int{}, files: map[string]*fileHint{}, buffer: make([]byte, 16384)}
}
func (h *accessHints) close() {
	if h.fd >= 0 {
		syscall.Close(h.fd)
	}
}
func (h *accessHints) watch(path string, dev uint64, limit int) {
	if h.fd < 0 {
		return
	}
	if _, ok := h.paths[path]; ok {
		return
	}
	if len(h.watches) >= limit {
		h.overflows++
		return
	}
	wd, e := syscall.InotifyAddWatch(h.fd, path, syscall.IN_ACCESS|syscall.IN_CLOSE_WRITE|syscall.IN_CREATE|syscall.IN_MOVED_TO|syscall.IN_ONLYDIR|syscall.IN_DONT_FOLLOW)
	if e != nil {
		h.overflows++
		return
	}
	h.watches[wd] = dirEntry{path, dev}
	h.paths[path] = wd
}
func (h *accessHints) poll(e *engine, now time.Time) {
	if h.fd < 0 {
		return
	}
	n, err := syscall.Read(h.fd, h.buffer)
	if err != nil {
		return
	}
	for pos := 0; pos+16 <= n; {
		wd := int(int32(binary.NativeEndian.Uint32(h.buffer[pos:])))
		mask := binary.NativeEndian.Uint32(h.buffer[pos+4:])
		length := int(binary.NativeEndian.Uint32(h.buffer[pos+12:]))
		end := pos + 16 + length
		if end > n {
			h.overflows++
			break
		}
		name := strings.TrimRight(string(h.buffer[pos+16:end]), "\x00")
		pos = end
		if mask&syscall.IN_Q_OVERFLOW != 0 {
			h.overflows++
			continue
		}
		dir, ok := h.watches[wd]
		if !ok {
			continue
		}
		if mask&syscall.IN_IGNORED != 0 {
			delete(h.watches, wd)
			delete(h.paths, dir.Path)
			continue
		}
		if name == "" || mask&syscall.IN_ISDIR != 0 || strings.Contains(name, "/") {
			continue
		}
		h.events++
		path := filepath.Join(dir.Path, name)
		if e.c.excluded(path) {
			continue
		}
		entry := e.known[path]
		if entry == nil && e.discovery.take(1) {
			e.add(path, dir.Dev)
			entry = e.known[path]
		}
		if entry == nil {
			continue
		}
		if old := h.files[path]; old != nil {
			old.until = now.Add(time.Duration(e.c.ProtectSeconds+e.c.RecheckSeconds+10) * time.Second)
			continue
		}
		if len(h.files) >= e.c.MaxHintFiles {
			h.overflows++
			continue
		}
		f := &fileHint{path: path, until: now.Add(time.Duration(e.c.ProtectSeconds+e.c.RecheckSeconds+10) * time.Second)}
		h.files[path] = f
		h.queue = append(h.queue, f)
	}
}
func (h *accessHints) sample(e *engine, now, deadline time.Time) {
	visits := 0
	for len(h.queue) > 0 && time.Now().Before(deadline) {
		if h.cursor >= len(h.queue) {
			h.cursor = 0
		}
		hint := h.queue[h.cursor]
		if now.After(hint.until) || e.known[hint.path] == nil {
			delete(h.files, hint.path)
			last := len(h.queue) - 1
			h.queue[h.cursor] = h.queue[last]
			h.queue[last] = nil
			h.queue = h.queue[:last]
			continue
		}
		if now.Before(hint.next) {
			h.cursor++
			visits++
			if visits >= len(h.queue) {
				return
			}
			continue
		}
		if !e.ops.take(1) {
			return
		}
		entry := e.known[hint.path]
		length := min64(e.c.RegionBytes, entry.ID.Size-hint.offset)
		length = length / int64(os.Getpagesize()) * int64(os.Getpagesize())
		if length <= 0 {
			hint.offset = 0
			hint.next = now.Add(time.Second)
			h.cursor++
			continue
		}
		if !e.scan.take(float64(length)) {
			return
		}
		f, err := safeOpen(hint.path)
		if err != nil {
			hint.until = now
			h.cursor++
			continue
		}
		id, err := statFile(f)
		if err != nil || !sameFile(id, entry.ID) {
			f.Close()
			if err == nil && id.Dev == entry.ID.Dev && id.Ino == entry.ID.Ino {
				entry.ID = id
				entry.Offset = 0
			}
			hint.offset = 0
			hint.next = now.Add(time.Second)
			h.cursor++
			continue
		}
		res, err := resident(f, hint.offset, length)
		f.Close()
		if err != nil {
			e.failure(err)
			hint.until = now
			h.cursor++
			continue
		}
		e.stats.ScannedBytes += length
		e.stats.ObservedResidentBytes += res
		e.track(entry, hint.offset, length, res, now)
		hint.offset += length
		// Round-robin among currently touched files, not all indexed files.
		h.cursor++
	}
}
