//go:build linux

package main

// A native basebackup reads PGDATA in the server, outside the client process.
// This observer uses the same shared-volume inodes and completed destination
// ranges. It never writes, syncs, truncates, or unlinks a source file.
import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const incrementalMagic = 0xd3ae1f0d // PostgreSQL 18 basebackup_incremental.h
const pgBlockSize = 8192

type pageBaseline struct {
	dev, ino uint64
	size     int64
	bits     []byte
}

var mincoreRootCheck sync.Once
var mincoreRootCapable bool

func rootMincoreCapability() bool {
	if os.Geteuid() != 0 {
		return false
	}
	mincoreRootCheck.Do(func() {
		// This conservative root exception covers an ordinary root DBS process
		// without assuming a rootless/user-namespace UID has host capabilities.
		for _, path := range []string{"/proc/self/uid_map", "/proc/self/gid_map"} {
			b, err := os.ReadFile(path)
			if err != nil {
				return
			}
			v := strings.Fields(string(b))
			if len(v) != 3 || v[0] != "0" || v[1] != "0" || v[2] != "4294967295" {
				return
			}
		}
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			v := strings.Fields(line)
			if len(v) == 2 && v[0] == "CapEff:" {
				caps, err := strconv.ParseUint(v[1], 16, 64)
				mincoreRootCapable = err == nil && caps&(1<<3) != 0 // CAP_FOWNER
			}
		}
	})
	return mincoreRootCapable
}

type cacheWatchStats struct {
	Files, BaselineBytes, BaselineResidentBytes uint64
	SourceAdviceBytes, DestinationAdviceBytes   uint64 // requests, not measured eviction
	UnknownSourceFiles, ChangedSourceFiles      uint64
	IncrementalFiles, Syncs                     uint64
}
type cacheProgress struct {
	source, destination int64
	incremental         bool
	closed              bool
}
type cacheWatch struct {
	ctx                 context.Context
	source, destination string
	window              int64
	fd                  int
	watches             map[int]string
	baseline            map[string]pageBaseline
	progress            map[string]cacheProgress
	pending             map[string]bool
	stats               cacheWatchStats
	mu                  sync.Mutex
	stop                chan struct{}
	done                chan struct{}
	err                 error
}

func fileOpenNoFollow(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("guarded file path must be absolute")
	}
	// O_NOFOLLOW on the final component alone does not stop a parent being
	// replaced with a symlink. Walk using directory descriptors instead.
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	for i, part := range parts {
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, err := syscall.Openat(fd, part, flags, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func fileIdentity(f *os.File) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	e := syscall.Fstat(int(f.Fd()), &st)
	return st, e
}
func residentBitmap(f *os.File, size int64) ([]byte, uint64, error) {
	st, err := fileIdentity(f)
	if err != nil {
		return nil, 0, err
	}
	// Linux can deliberately return an all-resident vector to callers that
	// lack ownership/write capability. Require ownership rather than silently
	// interpreting that privacy mask as a measured cache baseline. Running the
	// backup container with the database's UID also works on a read-only mount.
	if st.Uid != uint32(os.Geteuid()) && !rootMincoreCapability() {
		return nil, 0, fmt.Errorf("accurate mincore requires the file owner's UID: file uid=%d, process uid=%d; run the backup worker with the database UID", st.Uid, os.Geteuid())
	}
	page := int64(os.Getpagesize())
	pages := (size + page - 1) / page
	if pages == 0 {
		return []byte{}, 0, nil
	}
	if size < 0 || pages > int64(int(^uint(0)>>1))/page {
		return nil, 0, errors.New("file too large to map")
	}
	// Read-only mmap does not populate file pages (no MAP_POPULATE and no
	// loads from the mapping). It also works with old qemu-user versions
	// which incorrectly require PROT_READ for a valid mincore query.
	m, e := syscall.Mmap(int(f.Fd()), 0, int(pages*page), syscall.PROT_READ, syscall.MAP_SHARED)
	if e != nil {
		return nil, 0, e
	}
	defer syscall.Munmap(m)
	bits := make([]byte, (pages+7)/8)
	var resident uint64
	vec := make([]byte, 4096)
	for p := int64(0); p < pages; p += int64(len(vec)) {
		n := pages - p
		if n > int64(len(vec)) {
			n = int64(len(vec))
		}
		_, _, errno := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m[p*page])), uintptr(n*page), uintptr(unsafe.Pointer(&vec[0])))
		if errno != 0 {
			return nil, 0, errno
		}
		for i := int64(0); i < n; i++ {
			if vec[i]&1 != 0 {
				bits[(p+i)/8] |= 1 << uint((p+i)%8)
				resident += uint64(page)
			}
		}
	}
	return bits, resident, nil
}
func adviseDiscard(f *os.File, begin, end int64) error {
	if end <= begin {
		return nil
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(begin), uintptr(end-begin), 4, 0, 0) // POSIX_FADV_DONTNEED
	if errno != 0 {
		return errno
	}
	return nil
}
func newCacheWatch(ctx context.Context, c Config, destination string, source bool, preflight func() error) (*cacheWatch, error) {
	w := &cacheWatch{ctx: ctx, destination: destination, window: int64(c.FlushBytes), fd: -1, watches: map[int]string{}, baseline: map[string]pageBaseline{}, progress: map[string]cacheProgress{}, pending: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	if source {
		w.source = c.PGData
	}
	if e := os.Mkdir(destination, 0700); e != nil {
		if !os.IsExist(e) {
			return nil, e
		}
		entries, err := os.ReadDir(destination)
		if err != nil {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, errors.New("cache observer requires an empty output directory")
		}
	}
	if source {
		// External tablespaces need explicit, identity-checked mounts and mappings.
		entries, e := os.ReadDir(filepath.Join(c.PGData, "pg_tblspc"))
		if e != nil {
			return nil, e
		}
		if len(entries) != 0 {
			return nil, errors.New("native cache control currently requires default tablespaces")
		}
		var memory uint64
		e = filepath.WalkDir(c.PGData, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if e := ctx.Err(); e != nil {
				return e
			}
			rel, e := filepath.Rel(c.PGData, path)
			if e != nil {
				return e
			}
			if d.IsDir() {
				if rel == "pg_wal" || rel == "pg_stat_tmp" || rel == "pg_replslot" || rel == "log" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			f, e := fileOpenNoFollow(path)
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			defer f.Close()
			st, e := fileIdentity(f)
			if e != nil {
				return e
			}
			// Charge metadata too. Do not let an inventory become a new OOM source.
			cost := uint64((st.Size+int64(os.Getpagesize())-1)/int64(os.Getpagesize())+7)/8 + uint64(len(rel)) + 256
			if memory+cost > c.BitmapBudget {
				return errors.New("source cache inventory exceeds bitmap_budget_bytes")
			}
			bits, resident, e := residentBitmap(f, st.Size)
			if e != nil {
				return fmt.Errorf("mincore %s: %w", rel, e)
			}
			w.baseline[rel] = pageBaseline{st.Dev, st.Ino, st.Size, bits}
			memory += cost
			w.stats.Files++
			w.stats.BaselineBytes += uint64(st.Size)
			w.stats.BaselineResidentBytes += resident
			if w.stats.Files%500 == 0 && preflight != nil {
				return preflight()
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	fd, e := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if e != nil {
		return nil, e
	}
	w.fd = fd
	if e = w.addDirectory(destination); e != nil {
		syscall.Close(fd)
		return nil, e
	}
	go func() { defer syscall.Close(w.fd); w.err = w.loop(); close(w.done) }()
	return w, nil
}
func (w *cacheWatch) addDirectory(path string) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in guarded backup")
		}
		if d.IsDir() {
			wd, e := syscall.InotifyAddWatch(w.fd, p, syscall.IN_CREATE|syscall.IN_MODIFY|syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF)
			if e != nil {
				return e
			}
			w.watches[wd] = p
		} else if d.Type().IsRegular() {
			w.pending[p] = false
		}
		return nil
	})
}
func (w *cacheWatch) drain() error {
	buf := make([]byte, 65536)
	for {
		n, e := syscall.Read(w.fd, buf)
		if e == syscall.EAGAIN {
			return nil
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return errors.New("inotify closed")
		}
		for pos := 0; pos+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[pos]))
			end := pos + syscall.SizeofInotifyEvent + int(ev.Len)
			if end > n {
				return errors.New("truncated inotify event")
			}
			if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
				return errors.New("cache observer inotify queue overflow; backup must stop")
			}
			root, ok := w.watches[int(ev.Wd)]
			if !ok {
				return errors.New("unknown inotify watch")
			}
			name := strings.TrimRight(string(buf[pos+syscall.SizeofInotifyEvent:end]), "\x00")
			p := filepath.Join(root, name)
			if ev.Mask&(syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF) != 0 {
				return errors.New("guarded directory moved or removed")
			}
			if ev.Mask&syscall.IN_ISDIR != 0 {
				if ev.Mask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0 {
					if e = w.addDirectory(p); e != nil {
						return e
					}
				}
			} else {
				w.pending[p] = w.pending[p] || ev.Mask&syscall.IN_CLOSE_WRITE != 0
			}
			pos = end
		}
	}
}
func (w *cacheWatch) loop() error {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		final := false
		select {
		case <-w.ctx.Done():
			return w.ctx.Err()
		case <-w.stop:
			final = true
		case <-t.C:
		}
		if e := w.drain(); e != nil {
			return e
		}
		if final {
			if e := w.addDirectory(w.destination); e != nil {
				return e
			}
			for p := range w.pending {
				w.pending[p] = true
			}
		}
		w.mu.Lock()
		var err error
		for p, closed := range w.pending {
			if e := w.process(p, closed); e != nil {
				err = e
				break
			}
			if closed {
				delete(w.pending, p)
			}
		}
		w.mu.Unlock()
		if err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}
func (w *cacheWatch) process(path string, closed bool) error {
	f, e := fileOpenNoFollow(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := fileIdentity(f)
	if e != nil {
		return e
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return errors.New("unexpected backup file type")
	}
	p := w.progress[path]
	if p.closed && st.Size == p.destination {
		return nil
	}
	if st.Size < p.destination {
		return errors.New("backup output truncated after progress")
	}
	if !closed && st.Size-p.destination < w.window {
		return nil
	}
	if e = syscall.Fdatasync(int(f.Fd())); e != nil {
		return fmt.Errorf("backup fdatasync: %w", e)
	}
	w.stats.Syncs++
	page := int64(os.Getpagesize())
	end := st.Size / page * page
	if closed {
		end = (st.Size + page - 1) / page * page
	}
	if e = adviseDiscard(f, p.destination/page*page, end); e != nil {
		return e
	}
	if end > p.destination {
		w.stats.DestinationAdviceBytes += uint64(end - p.destination)
	}
	p.destination = st.Size
	p.closed = closed
	if w.source != "" {
		if e = w.releaseSource(path, f, st.Size, closed, &p); e != nil {
			return e
		}
	}
	w.progress[path] = p
	return nil
}
func (w *cacheWatch) releaseSource(path string, dest *os.File, size int64, closed bool, p *cacheProgress) error {
	rel, e := filepath.Rel(w.destination, path)
	if e != nil {
		return e
	}
	if strings.HasPrefix(rel, "pg_wal/") || rel == "backup_manifest" || rel == "backup_manifest.tmp" {
		return nil
	}
	incremental := strings.HasPrefix(filepath.Base(rel), "INCREMENTAL.")
	sourceRel := rel
	if incremental {
		sourceRel = filepath.Join(filepath.Dir(rel), strings.TrimPrefix(filepath.Base(rel), "INCREMENTAL."))
	}
	b, ok := w.baseline[sourceRel]
	if !ok {
		if p.source == 0 {
			w.stats.UnknownSourceFiles++
			p.source = -1
		}
		return nil
	}
	f, e := fileOpenNoFollow(filepath.Join(w.source, sourceRel))
	if os.IsNotExist(e) {
		w.stats.ChangedSourceFiles++
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := fileIdentity(f)
	if e != nil {
		return e
	}
	if st.Dev != b.dev || st.Ino != b.ino || st.Size < b.size {
		w.stats.ChangedSourceFiles++
		return nil
	}
	if incremental {
		blocks, header, e := incrementalHeader(dest, size, closed)
		if e != nil {
			return e
		}
		if blocks == nil {
			return nil
		}
		n := (size - header) / pgBlockSize
		if n < 0 {
			return nil
		}
		if n > int64(len(blocks)) {
			return errors.New("incremental output longer than header declares")
		}
		if !p.incremental {
			p.incremental = true
			w.stats.IncrementalFiles++
		}
		ranges, consumed := completedIncrementalRanges(blocks, p.source, n, int64(os.Getpagesize()))
		for _, v := range ranges {
			if e = w.dropCold(f, b, v[0], v[1]); e != nil {
				return e
			}
		}
		p.source = consumed
	} else {
		end := size
		if end > b.size {
			end = b.size
		}
		if closed && end == b.size {
			page := int64(os.Getpagesize())
			end = (end + page - 1) / page * page
		}
		if end > p.source {
			if e = w.dropCold(f, b, p.source, end); e != nil {
				return e
			}
			p.source = end
		}
	}
	return nil
}

// A 64KiB Linux page contains eight standard PostgreSQL blocks. Wait until
// every changed block for a page has arrived, then advise that whole page.
// Initial residency is tracked at the Linux page size, so a previously hot
// subrange protects the entire page. This also works for 4KiB/16KiB kernels.
func completedIncrementalRanges(blocks []uint32, start, available, page int64) ([][2]int64, int64) {
	var ranges [][2]int64
	i := start
	for i < available {
		begin := int64(blocks[i]) * pgBlockSize / page * page
		end := (int64(blocks[i]+1)*pgBlockSize + page - 1) / page * page
		j := i + 1
		for j < int64(len(blocks)) && int64(blocks[j])*pgBlockSize < end {
			j++
		}
		if j > available {
			break
		}
		ranges = append(ranges, [2]int64{begin, end})
		i = j
	}
	return ranges, i
}
func incrementalHeader(f *os.File, size int64, closed bool) ([]uint32, int64, error) {
	if size < 12 {
		if closed {
			return nil, 0, errors.New("truncated incremental header")
		}
		return nil, 0, nil
	}
	h := make([]byte, 12)
	if _, e := f.ReadAt(h, 0); e != nil {
		return nil, 0, e
	}
	if binary.LittleEndian.Uint32(h) != incrementalMagic {
		return nil, 0, errors.New("invalid PostgreSQL 18 incremental magic")
	}
	n := binary.LittleEndian.Uint32(h[4:])
	if n > 131072 {
		return nil, 0, errors.New("incremental block count exceeds standard 1GiB relation segment")
	}
	header := int64(12 + 4*n)
	if n > 0 {
		header = (header + pgBlockSize - 1) / pgBlockSize * pgBlockSize
	}
	if size < header {
		if closed {
			return nil, 0, errors.New("truncated incremental block map")
		}
		return nil, 0, nil
	}
	raw := make([]byte, int(n)*4)
	if _, e := f.ReadAt(raw, 12); e != nil && e != io.EOF {
		return nil, 0, e
	}
	blocks := make([]uint32, n)
	for i := range blocks {
		blocks[i] = binary.LittleEndian.Uint32(raw[i*4:])
		if blocks[i] >= 131072 || (i > 0 && blocks[i] <= blocks[i-1]) {
			return nil, 0, errors.New("invalid incremental block ordering")
		}
	}
	if closed && size != header+int64(n)*pgBlockSize {
		return nil, 0, errors.New("incremental file length mismatch")
	}
	return blocks, header, nil
}
func (w *cacheWatch) dropCold(f *os.File, b pageBaseline, begin, end int64) error {
	page := int64(os.Getpagesize())
	first := begin / page
	last := end / page
	if last > (b.size+page-1)/page {
		last = (b.size + page - 1) / page
	}
	for p := first; p < last; {
		if b.bits[p/8]&(1<<uint(p%8)) != 0 {
			p++
			continue
		}
		start := p
		for p < last && b.bits[p/8]&(1<<uint(p%8)) == 0 {
			p++
		}
		if e := adviseDiscard(f, start*page, p*page); e != nil {
			return e
		}
		w.stats.SourceAdviceBytes += uint64((p - start) * page)
	}
	return nil
}
func (w *cacheWatch) finish() error             { close(w.stop); <-w.done; return w.err }
func (w *cacheWatch) snapshot() cacheWatchStats { w.mu.Lock(); defer w.mu.Unlock(); return w.stats }
