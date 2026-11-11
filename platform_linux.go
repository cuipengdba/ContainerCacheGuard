//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const linuxOPath = 0x200000

type identity struct {
	Dev, Ino    uint64
	Size, Mtime int64
	UID         uint32
	Mode        uint32
}

func statFile(f *os.File) (identity, error) {
	var s syscall.Stat_t
	e := syscall.Fstat(int(f.Fd()), &s)
	return identityFromStat(s), e
}
func identityFromStat(s syscall.Stat_t) identity {
	return identity{uint64(s.Dev), s.Ino, s.Size, s.Mtim.Sec*1e9 + s.Mtim.Nsec, s.Uid, s.Mode}
}

// Discovery already holds a safely opened parent directory. Inspect the child
// relative to that FD, without following links or reopening every ancestor.
// The eventual advice path is independently reopened and identity checked.
func statChild(parent *os.File, name string) (identity, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return identity{}, errors.New("invalid directory entry")
	}
	fd, err := syscall.Openat(int(parent.Fd()), name,
		linuxOPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return identity{}, err
	}
	defer syscall.Close(fd)
	var s syscall.Stat_t
	if err := syscall.Fstat(fd, &s); err != nil {
		return identity{}, err
	}
	return identityFromStat(s), nil
}
func sameFile(a, b identity) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.Mtime == b.Mtime
}

// Every component is opened beneath an already open directory without following
// symlinks. O_NONBLOCK prevents a malicious replacement FIFO from blocking open.
func safeOpen(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("absolute clean path required")
	}
	if openat2Capability.Load() >= 0 {
		p, err := syscall.BytePtrFromString(path)
		if err != nil {
			return nil, err
		}
		how := struct{ Flags, Mode, Resolve uint64 }{Flags: uint64(syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK), Resolve: 0x02 | 0x04}
		fd, _, errno := syscall.Syscall6(437, ^uintptr(99), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		if errno == 0 {
			openat2Capability.Store(1)
			return os.NewFile(fd, path), nil
		}
		if errno == syscall.ENOSYS || errno == syscall.EINVAL || errno == syscall.E2BIG || (errno == syscall.EPERM && openat2Capability.Load() == 0) {
			openat2Capability.Store(-1)
		} else {
			return nil, errno
		}
	}
	return safeOpenWalk(path)
}

var openat2Capability atomic.Int32

func safeOpenWalk(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("absolute clean path required")
	}
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	if path == "/" {
		return os.NewFile(uintptr(fd), path), nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, err := syscall.Openat(fd, p, flags, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func supportedFS(f *os.File) bool {
	var s syscall.Statfs_t
	if syscall.Fstatfs(int(f.Fd()), &s) != nil {
		return false
	}
	switch uint64(s.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0x794c7630, 0xf2f52010:
		return true
	}
	return false
} // ext4,xfs,btrfs,overlay,f2fs
func canMincore(id identity) bool {
	if id.UID == uint32(os.Geteuid()) {
		return true
	}
	if os.Geteuid() != 0 {
		return false
	}
	b, e := os.ReadFile("/proc/self/uid_map")
	if e != nil {
		return false
	}
	v := strings.Fields(string(b))
	if len(v) != 3 || v[0] != "0" || v[1] != "0" || v[2] != "4294967295" {
		return false
	}
	b, e = os.ReadFile("/proc/self/status")
	if e != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		v = strings.Fields(l)
		if len(v) == 2 && v[0] == "CapEff:" {
			n, _ := strconv.ParseUint(v[1], 16, 64)
			return n&(1<<3) != 0
		}
	}
	return false
}
func resident(f *os.File, offset, length int64) (int64, error) {
	if length <= 0 {
		return 0, nil
	}
	page := int64(os.Getpagesize())
	if offset%page != 0 {
		return 0, errors.New("unaligned offset")
	}
	id, e := statFile(f)
	if e != nil {
		return 0, e
	}
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG || !canMincore(id) {
		return 0, errors.New("not regular or mincore ownership unavailable")
	}
	length = min64(length, id.Size-offset)
	if length <= 0 {
		return 0, nil
	}
	size := (length + page - 1) / page * page
	m, e := syscall.Mmap(int(f.Fd()), offset, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if e != nil {
		return 0, e
	}
	defer syscall.Munmap(m)
	v := make([]byte, size/page)
	_, _, errno := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(size), uintptr(unsafe.Pointer(&v[0])))
	if errno != 0 {
		return 0, errno
	}
	var n int64
	for _, b := range v {
		if b&1 != 0 {
			n += page
		}
	}
	return min64(n, length), nil
}
func advise(f *os.File, offset, length int64) error {
	page := int64(os.Getpagesize())
	length = length / page * page
	if offset%page != 0 || length <= 0 {
		return errors.New("empty or unaligned advice")
	}
	_, _, e := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(offset), uintptr(length), 4, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
func values(path string) map[string]int64 {
	b, e := os.ReadFile(path)
	out := map[string]int64{}
	if e != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		v := strings.Fields(l)
		if len(v) >= 2 {
			n, e := strconv.ParseInt(v[1], 10, 64)
			if e == nil && n >= 0 {
				out[strings.TrimSuffix(v[0], ":")] = n
			}
		}
	}
	return out
}
func number(path string) (int64, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return 0, e
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, nil
	}
	v, e := strconv.ParseInt(s, 10, 64)
	if v > 1<<60 {
		v = 0
	}
	return v, e
}
func psi(path string) *float64 {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && f[0] == "some" {
			for _, s := range f[1:] {
				if strings.HasPrefix(s, "avg10=") {
					v, e := strconv.ParseFloat(strings.TrimPrefix(s, "avg10="), 64)
					if e == nil && finiteNonnegative(v) && v <= 100 {
						return &v
					}
				}
			}
		}
	}
	return nil
}

type mount struct{ Root, Path, FSType string }

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
func mounts() []mount {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	var out []mount
	for _, l := range strings.Split(string(b), "\n") {
		parts := strings.Split(l, " - ")
		if len(parts) != 2 {
			continue
		}
		a, b := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(a) > 4 && len(b) > 0 {
			out = append(out, mount{unescapeMount(a[3]), unescapeMount(a[4]), b[0]})
		}
	}
	return out
}
func discoverCgroup() (string, error) {
	b, e := os.ReadFile("/proc/self/cgroup")
	if e != nil {
		return "", e
	}
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.SplitN(line, ":", 3)
		if len(s) != 3 {
			continue
		}
		v2 := s[0] == "0" && s[1] == ""
		v1 := strings.Contains(","+s[1]+",", ",memory,")
		if !v2 && !v1 {
			continue
		}
		for _, m := range mounts() {
			if (v2 && m.FSType != "cgroup2") || (v1 && m.FSType != "cgroup") {
				continue
			}
			rel := ""
			if s[2] != "/" {
				if m.Root != "/" && !beneath(s[2], m.Root) {
					continue
				}
				rel = strings.TrimPrefix(s[2], strings.TrimSuffix(m.Root, "/"))
			}
			p := filepath.Join(m.Path, rel)
			test := "memory.current"
			if v1 {
				test = "memory.usage_in_bytes"
			}
			if _, e := os.Stat(filepath.Join(p, test)); e == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("no readable memory cgroup; set cgroup_path for sidecar")
}
func discoverRoots(c Config) []string {
	r := append([]string{}, c.AutoRoots...)
	for _, m := range mounts() {
		if m.FSType == "ext4" || m.FSType == "xfs" || m.FSType == "btrfs" || m.FSType == "overlay" || m.FSType == "f2fs" {
			if !forbiddenRoot(m.Path) {
				r = append(r, m.Path)
			}
		}
	}
	r = append(r, "/data", "/backup", "/backups", "/pgdata", "/var/lib/postgresql", "/var/lib/mysql", "/app/data", "/opt/data")
	if p := os.Getenv("PGDATA"); filepath.IsAbs(p) {
		r = append(r, filepath.Clean(p))
	}
	return normalizeRoots(c, r)
}
func normalizeRoots(c Config, roots []string) []string {
	var out []string
	devices := map[string]uint64{}
	for _, p := range roots {
		if c.excluded(p) || len(p) > 4096 {
			continue
		}
		f, e := safeOpen(p)
		if e != nil {
			continue
		}
		id, e := statFile(f)
		ok := supportedFS(f)
		f.Close()
		if e != nil || !ok || (id.Mode&syscall.S_IFMT != syscall.S_IFDIR && id.Mode&syscall.S_IFMT != syscall.S_IFREG) {
			continue
		}
		duplicate := false
		for _, q := range out {
			if beneath(p, q) && devices[q] == id.Dev {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		kept := out[:0]
		for _, q := range out {
			if !beneath(q, p) || devices[q] != id.Dev {
				kept = append(kept, q)
			}
		}
		out = append(kept, p)
		devices[p] = id.Dev
	}
	return out
}

type metricReader struct {
	path        string
	id          identity
	previous    Metrics
	oomBaseline int64
}

func newMetricReader(path string) (*metricReader, error) {
	if path == "" {
		var e error
		path, e = discoverCgroup()
		if e != nil {
			return nil, e
		}
	}
	f, e := safeOpen(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var fs syscall.Statfs_t
	if e := syscall.Fstatfs(int(f.Fd()), &fs); e != nil || (uint64(fs.Type) != 0x63677270 && uint64(fs.Type) != 0x27e0eb) {
		return nil, errors.New("cgroup_path must refer to a real cgroup filesystem")
	}
	id, e := statFile(f)
	if e != nil {
		return nil, e
	}
	return &metricReader{path: path, id: id}, nil
}
func (r *metricReader) sample(c Config) Metrics {
	m := Metrics{Time: time.Now(), Cgroup: r.path, CgroupDevice: r.id.Dev, CgroupInode: r.id.Ino}
	defer func() {
		if m.Error != "" {
			// Do not compute a subsequent rate across a collection gap.
			r.previous = Metrics{}
		}
	}()
	f, e := safeOpen(r.path)
	if e != nil {
		m.Error = e.Error()
		return m
	}
	id, e := statFile(f)
	f.Close()
	if e != nil || id.Dev != r.id.Dev || id.Ino != r.id.Ino {
		m.Error = "cgroup identity changed; restart required"
		return m
	}
	s := values(filepath.Join(r.path, "memory.stat"))
	if len(s) == 0 {
		m.Error = "memory.stat missing or empty"
		return m
	}
	m.Current, e = number(filepath.Join(r.path, "memory.current"))
	if e == nil {
		m.Version = 2
		m.Limit, e = number(filepath.Join(r.path, "memory.max"))
		m.File = s["file"]
		m.Anon = s["anon"]
		m.Slab = s["slab"]
		m.Shmem = s["shmem"]
		m.Dirty = s["file_dirty"]
		m.Writeback = s["file_writeback"]
		m.ActiveFile = s["active_file"]
		m.InactiveFile = s["inactive_file"]
		m.FileRefaults = s["workingset_refault_file"]
		_, m.FileRefaultsAvailable = s["workingset_refault_file"]
		events := values(filepath.Join(r.path, "memory.events"))
		m.OOMKill = events["oom_kill"]
		m.Available = memoryAvailability(2, s)
		_, m.Available["oom_kill_total"] = events["oom_kill"]
	} else {
		m.Version = 1
		m.Current, e = number(filepath.Join(r.path, "memory.usage_in_bytes"))
		m.Limit, _ = number(filepath.Join(r.path, "memory.limit_in_bytes"))
		get := func(k string) int64 {
			if n, ok := s["total_"+k]; ok {
				return n
			}
			return s[k]
		}
		m.File = get("cache")
		m.Anon = get("rss")
		m.Shmem = get("shmem")
		m.Dirty = get("dirty")
		m.Writeback = get("writeback")
		m.ActiveFile = get("active_file")
		m.InactiveFile = get("inactive_file")
		m.Available = memoryAvailability(1, s)
	}
	if e != nil {
		m.Error = e.Error()
		return m
	}
	m.Available["current_bytes"] = true
	// Respect visible ancestors' limits; hidden ancestors are not observable.
	if m.Version == 2 {
		var ancestorRatio float64
		for p := filepath.Dir(r.path); p != "/" && p != "."; p = filepath.Dir(p) {
			v, e := number(filepath.Join(p, "memory.max"))
			if e != nil {
				break
			}
			if v > 0 && (m.Limit == 0 || v < m.Limit) {
				m.Limit = v
			}
			if v > 0 {
				current, currentError := number(filepath.Join(p, "memory.current"))
				if currentError == nil && float64(current)/float64(v) > ancestorRatio {
					ancestorRatio = float64(current) / float64(v)
					m.AncestorCgroup = p
					m.AncestorCurrent = current
					m.AncestorLimit = v
				}
			}
		}
	}
	b, ioErr := os.ReadFile(filepath.Join(r.path, "io.stat"))
	m.Available["io_bytes"] = ioErr == nil
	if ioErr == nil {
		m.IOBytes, m.Available["io_bytes"] = parseIOBytes(string(b))
	}
	m.IOPSI = psi(filepath.Join(r.path, "io.pressure"))
	if m.IOPSI == nil {
		_, psiErr := os.Stat(filepath.Join(r.path, "io.pressure"))
		m.Available["io_psi_interface_absent"] = errors.Is(psiErr, os.ErrNotExist)
	}
	m.MemoryPSI = psi(filepath.Join(r.path, "memory.pressure"))
	node := values("/proc/meminfo")
	m.NodeAvailable = node["MemAvailable"] * 1024
	_, m.Available["host_memavailable_bytes"] = node["MemAvailable"]
	self := values("/proc/self/status")
	m.RSS = self["VmRSS"] * 1024
	_, m.Available["self_rss_bytes"] = self["VmRSS"]
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		m.Available["self_cpu_seconds"] = true
		m.CPUSeconds = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	}
	setSampleRates(c, &m, r.previous)
	m.Available["limit_bytes"] = m.Limit > 0
	m.Available["file_refaults"] = m.FileRefaultsAvailable
	m.Available["file_refaults_per_second"] = m.FileRefaultRateValid
	m.Available["io_psi_avg10"] = m.IOPSI != nil
	m.Available["memory_psi_avg10"] = m.MemoryPSI != nil
	r.previous = m
	return m
}

func lockState(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	f, e := safeOpen(dir)
	if e != nil {
		return nil, e
	}
	id, e := statFile(f)
	f.Close()
	if e != nil {
		return nil, e
	}
	if id.UID != uint32(os.Geteuid()) || id.Mode&0077 != 0 {
		return nil, fmt.Errorf("state directory must be owned by UID %d and mode0700: %s", os.Geteuid(), dir)
	}
	fd, e := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	f = os.NewFile(uintptr(fd), path+".lock")
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another daemon holds this state_file lock")
	}
	return f, nil
}
