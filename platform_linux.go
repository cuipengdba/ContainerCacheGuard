//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"
	"unsafe"
)

const controlMagic uint64 = 0x5047313847554152

func renameNoReplace(old, new string) error {
	a, err := syscall.BytePtrFromString(old)
	if err != nil {
		return err
	}
	b, err := syscall.BytePtrFromString(new)
	if err != nil {
		return err
	}
	number := uintptr(316) // Linux x86_64 __NR_renameat2
	if runtime.GOARCH == "arm64" {
		number = 276
	}
	atFDCWD := ^uintptr(99)
	_, _, errno := syscall.Syscall6(number, atFDCWD, uintptr(unsafe.Pointer(a)), atFDCWD, uintptr(unsafe.Pointer(b)), 1, 0) // RENAME_NOREPLACE
	if errno != 0 {
		return os.NewSyscallError("renameat2", errno)
	}
	return nil
}

type Control struct{ f *os.File }

func newControl(path string, r, w uint64) (*Control, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	b := make([]byte, 128)
	binary.LittleEndian.PutUint64(b, controlMagic)
	binary.LittleEndian.PutUint64(b[8:], 1)
	binary.LittleEndian.PutUint64(b[16:], r)
	binary.LittleEndian.PutUint64(b[24:], w)
	_, e = f.Write(b)
	if e != nil {
		f.Close()
		return nil, e
	}
	return &Control{f}, nil
}
func (c *Control) access(fn func([]byte)) error {
	l := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
	for {
		e := syscall.FcntlFlock(c.f.Fd(), syscall.F_SETLKW, &l)
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			return e
		}
		break
	}
	defer func() { l.Type = syscall.F_UNLCK; _ = syscall.FcntlFlock(c.f.Fd(), syscall.F_SETLK, &l) }()
	b := make([]byte, 128)
	n, e := c.f.ReadAt(b, 0)
	if e != nil || n != 128 {
		return errors.New("invalid control file")
	}
	fn(b)
	_, e = c.f.WriteAt(b, 0)
	return e
}
func (c *Control) rates(r, w uint64) error {
	return c.access(func(b []byte) { binary.LittleEndian.PutUint64(b[16:], r); binary.LittleEndian.PutUint64(b[24:], w) })
}
func (c *Control) abort() error {
	return c.access(func(b []byte) { binary.LittleEndian.PutUint64(b[13*8:], 1) })
}
func (c *Control) stats() (map[string]uint64, error) {
	m := map[string]uint64{}
	e := c.access(func(b []byte) {
		names := map[int]string{2: "read_limit_bps", 3: "write_limit_bps", 6: "read_bytes", 7: "write_bytes", 8: "read_advice_requested_bytes", 9: "write_advice_requested_bytes", 10: "mincore_failures", 11: "advice_failures", 12: "helper_loads", 14: "preserved_resident_bytes", 15: "sync_failures"}
		for i, n := range names {
			m[n] = binary.LittleEndian.Uint64(b[i*8:])
		}
	})
	return m, e
}
func (c *Control) close() { _ = c.f.Close() }
func syncDir(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func acquireLocks(ctx context.Context, paths []string, wait time.Duration) (func(), error) {
	paths = append([]string{}, paths...)
	sort.Strings(paths)
	var held []*os.File
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(held[i].Fd()), syscall.LOCK_UN)
			_ = held[i].Close()
		}
	}
	deadline := time.Now().Add(wait)
	for i, p := range paths {
		if i > 0 && p == paths[i-1] {
			continue
		}
		if e := os.MkdirAll(filepath.Dir(p), 0750); e != nil {
			release()
			return nil, e
		}
		fd, e := syscall.Open(p, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			release()
			return nil, e
		}
		f := os.NewFile(uintptr(fd), p)
		for {
			e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
			if e == nil {
				held = append(held, f)
				break
			}
			if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
				f.Close()
				release()
				return nil, e
			}
			if !time.Now().Before(deadline) {
				f.Close()
				release()
				return nil, fmt.Errorf("resource lock busy: %s", p)
			}
			select {
			case <-ctx.Done():
				f.Close()
				release()
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return release, nil
}
func groupCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
func terminateGroup(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case e := <-done:
		return e
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return <-done
	}
}
