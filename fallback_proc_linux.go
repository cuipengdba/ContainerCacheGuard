//go:build linux && (amd64 || arm64)

package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type fallbackProc struct {
	members, fds   *os.File
	scan           *bufio.Scanner
	pidPath        string
	pid            int
	start          string
	entries        int
	next           time.Time
	key            [2]uint64
	epoch          uint64
	lastCleanEpoch uint64
	gapsAtStart    uint64
}

func (p *fallbackProc) closeFDs() {
	if p.fds != nil {
		p.fds.Close()
		p.fds = nil
	}
}

func (p *fallbackProc) close() {
	p.closeFDs()
	if p.members != nil {
		p.members.Close()
		p.members = nil
	}
}

func (p *fallbackProc) step(e *engine, m Metrics, now time.Time) {
	f := e.fallback
	s := &f.current
	if now.Before(p.next) {
		return
	}
	if m.Version != 2 || m.CgroupInode == 0 {
		s.ProcCoverage = "cgroup_v2_membership_unavailable"
		p.next = now.Add(time.Second)
		return
	}
	key := [2]uint64{m.CgroupDevice, m.CgroupInode}
	if p.members != nil && key != p.key {
		p.close()
		s.ProcCoverage = "cgroup_identity_changed"
		s.ProcGaps++
		return
	}
	if p.entries >= f.c.ProcEntriesPerPass {
		s.ProcCoverage = "bounded_pass_resuming"
		p.entries = 0
		p.next = now.Add(time.Second)
		return
	}
	p.entries++
	s.ProcExamined++
	if p.members == nil {
		cg, err := safeOpen(m.Cgroup)
		if err == nil {
			defer cg.Close()
			id, statErr := statFile(cg)
			if statErr == nil && id.Dev == key[0] && id.Ino == key[1] {
				fd, openErr := syscall.Openat(int(cg.Fd()), "cgroup.procs", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
				if openErr == nil {
					p.members = os.NewFile(uintptr(fd), "cgroup.procs")
				}
			}
		}
		if p.members == nil {
			s.ProcCoverage = "cgroup_procs_unavailable"
			s.ProcGaps++
			p.next = now.Add(time.Second)
			return
		}
		p.key = key
		p.epoch++
		p.gapsAtStart = s.ProcGaps
		p.scan = bufio.NewScanner(p.members)
		p.scan.Buffer(make([]byte, 128), 128)
	}
	if p.fds == nil {
		if !p.scan.Scan() {
			if p.scan.Err() != nil {
				s.ProcGaps++
			}
			s.ProcCoverage = "sampled_visible_direct_members"
			if p.gapsAtStart != s.ProcGaps {
				s.ProcCoverage = "partial_or_inaccessible"
			} else {
				p.lastCleanEpoch = p.epoch
			}
			p.close()
			p.next, p.entries = now.Add(time.Second), 0
			return
		}
		pid, err := strconv.Atoi(p.scan.Text())
		if err != nil || pid <= 0 {
			s.ProcGaps++
			return
		}
		if pid == os.Getpid() {
			return
		}
		p.pidPath = "/proc/" + strconv.Itoa(pid)
		if !sameProcObject(p.pidPath+"/ns/mnt", "/proc/self/ns/mnt") ||
			!sameProcObject(p.pidPath+"/root", "/proc/self/root") {
			s.ProcGaps++
			return
		}
		p.start, err = processStart(pid)
		if err == nil {
			p.fds, err = os.Open(p.pidPath + "/fd")
		}
		if err != nil {
			s.ProcGaps++
			return
		}
		p.pid = pid
		return
	}
	names, err := p.fds.Readdirnames(1)
	if err != nil {
		p.closeFDs()
		if err != io.EOF {
			s.ProcGaps++
		}
	}
	if len(names) == 0 {
		return
	}
	p.recordFD(e, names[0], now)
}

func sameProcObject(a, b string) bool {
	var left, right syscall.Stat_t
	return syscall.Stat(a, &left) == nil && syscall.Stat(b, &right) == nil && left.Dev == right.Dev && left.Ino == right.Ino
}

func (p *fallbackProc) recordFD(e *engine, name string, now time.Time) {
	fd := p.pidPath + "/fd/" + name
	path, err := os.Readlink(fd)
	if err != nil {
		e.fallback.current.ProcGaps++
		return
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return
	}
	i, root := e.fallback.scope(path)
	lane := &e.fallback.lanes[i]
	if strings.HasSuffix(path, " (deleted)") {
		lane.stats.Skipped["deleted_open_file"]++
		return
	}
	if e.fallbackExcluded(path) {
		lane.stats.Skipped["path_excluded"]++
		return
	}
	// Examine the FD through procfs first so pipes, devices and sockets are
	// never opened. For regular files also verify the path still names it.
	var st syscall.Stat_t
	if syscall.Stat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return
	}
	if root == nil {
		e.fallback.finding(i, path, "cgroup_open_fd_outside_scope", st.Size, now)
		lane.stats.Skipped["outside_allowed_roots"]++
		return
	}
	flags, err := procFDFlags(p.pidPath + "/fdinfo/" + name)
	if err != nil {
		e.fallback.current.ProcGaps++
	}
	file, openErr := safeOpen(path)
	if openErr != nil {
		lane.stats.Skipped["candidate_open"]++
		return
	}
	defer file.Close()
	id, statErr := statFile(file)
	start, startErr := processStart(p.pid)
	if statErr != nil || startErr != nil || start != p.start || !sameFile(id, identityFromStat(st)) || !sameProcObject(fd, "/proc/self/fd/"+strconv.FormatUint(uint64(file.Fd()), 10)) {
		lane.stats.Skipped["fd_identity_changed"]++
		return
	}
	e.fallbackRecord(path, "cgroup_open_fd", file, id, now, true)
	if entry := lane.byInode[[2]uint64{id.Dev, id.Ino}]; entry != nil && (err != nil || flags&syscall.O_ACCMODE != syscall.O_RDONLY) {
		// A pass containing this writer must not itself clear the protection.
		entry.writerEpoch = p.epoch + 1
	}
}

func procFDFlags(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if flags, ok := strings.CutPrefix(line, "flags:"); ok {
			return strconv.ParseUint(strings.TrimSpace(flags), 8, 64)
		}
	}
	return 0, io.ErrUnexpectedEOF
}
