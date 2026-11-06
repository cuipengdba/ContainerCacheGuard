//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type identity struct {
	Dev, Ino    uint64
	Size, Mtime int64
	UID         uint32
	Mode        uint32
}

func statFile(f *os.File) (identity, error) {
	var s syscall.Stat_t
	e := syscall.Fstat(int(f.Fd()), &s)
	return identity{uint64(s.Dev), s.Ino, s.Size, s.Mtim.Sec*1e9 + s.Mtim.Nsec, s.Uid, s.Mode}, e
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
			if e == nil {
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
					if e == nil {
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
	m := Metrics{Time: time.Now(), Cgroup: r.path}
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
		m.OOMKill = values(filepath.Join(r.path, "memory.events"))["oom_kill"]
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
	}
	if e != nil {
		m.Error = e.Error()
		return m
	}
	// Respect visible ancestors' limits; hidden ancestors are not observable.
	if m.Version == 2 {
		for p := filepath.Dir(r.path); p != "/" && p != "."; p = filepath.Dir(p) {
			v, e := number(filepath.Join(p, "memory.max"))
			if e != nil {
				break
			}
			if v > 0 && (m.Limit == 0 || v < m.Limit) {
				m.Limit = v
			}
		}
	}
	b, ioErr := os.ReadFile(filepath.Join(r.path, "io.stat"))
	if ioErr == nil {
		for _, l := range strings.Split(string(b), "\n") {
			for _, v := range strings.Fields(l) {
				if strings.HasPrefix(v, "rbytes=") || strings.HasPrefix(v, "wbytes=") {
					n, _ := strconv.ParseInt(strings.SplitN(v, "=", 2)[1], 10, 64)
					m.IOBytes += n
				}
			}
		}
	} else if c.MaxIOBytesPerSecond > 0 {
		m.Error = "configured IO guard unavailable (io.stat)"
	}
	m.IOPSI = psi(filepath.Join(r.path, "io.pressure"))
	m.MemoryPSI = psi(filepath.Join(r.path, "memory.pressure"))
	m.NodeAvailable = values("/proc/meminfo")["MemAvailable"] * 1024
	self := values("/proc/self/status")
	m.RSS = self["VmRSS"] * 1024
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		m.CPUSeconds = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	}
	if !r.previous.Time.IsZero() {
		dt := m.Time.Sub(r.previous.Time).Seconds()
		m.IOBytesPerSecond = float64(max64(0, m.IOBytes-r.previous.IOBytes)) / dt
		m.CPUPercent = (m.CPUSeconds - r.previous.CPUSeconds) / dt * 100
	}
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
