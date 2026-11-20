//go:build linux && (amd64 || arm64)

// Package vmtouch contains the Linux eviction operation used by vmtouch -e.
// It is compiled into the Go executable. Traversal, eligibility and reporting
// belong to CCG. See https://github.com/hoytech/vmtouch/blob/v1.3.1/vmtouch.c.
package vmtouch

import "syscall"

// Evict applies POSIX_FADV_DONTNEED to an already validated descriptor and
// nonempty page-aligned range. Success reports acceptance of the advice.
func Evict(fd uintptr, offset, length int64) error {
	const posixFadvDontNeed = 4
	_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, fd, uintptr(offset), uintptr(length), posixFadvDontNeed, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
