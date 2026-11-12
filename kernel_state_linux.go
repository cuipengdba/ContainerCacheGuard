//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/hex"
	"errors"
	"io"
	"syscall"
	"time"
)

func validateKernelJournal(j kernelJournal) error {
	fail := func() error {
		return errors.New("kernel journal invalid; preserve state and investigate, no automatic reset")
	}
	if j.Schema != 1 || j.Targets == nil || len(j.Targets) > 128 || len(j.Budget) > 64 {
		return fail()
	}
	var last time.Time
	for _, b := range j.Budget {
		if b.Time.IsZero() || b.Time.Before(last) || b.Bytes < 64*MiB || b.Bytes > 1<<30 || b.Bytes%4096 != 0 {
			return fail()
		}
		last = b.Time
	}
	for key, s := range j.Targets {
		if _, e := hex.DecodeString(key); e != nil || len(key) != 64 {
			return fail()
		}
		if s.TargetIdentity != key || !s.Attempted || s.Episode == "" || len(s.Episode) > 64 || s.Started.IsZero() || s.Request < 64*MiB || s.Request > 1<<30 || len(s.Observations) > 8 || s.Before == nil {
			return fail()
		}
		switch s.Phase {
		case "RESERVED", "INFLIGHT", "UNKNOWN", "WAIT_RECOVERY":
		default:
			return fail()
		}
		if s.Phase == "WAIT_RECOVERY" && (s.Finished.IsZero() || s.Finished.Before(s.Started)) {
			return fail()
		}
	}
	return nil
}

func kernelSignalRegressed(n, p KernelSignal) bool {
	if p.Time.IsZero() {
		return false
	}
	if n.Time.Before(p.Time) || n.ConfigVersion != p.ConfigVersion {
		return true
	}
	for _, q := range n.Constraints {
		for _, v := range p.Constraints {
			if q.Kind == v.Kind && q.Time.Before(v.Time) {
				return true
			}
		}
	}
	return n.DiskTime.Before(p.DiskTime) || n.BusinessTime.Before(p.BusinessTime)
}

func restoreKernelStatus(s KernelStatus) KernelStatus {
	if s.Phase == "RESERVED" || s.Phase == "INFLIGHT" {
		s.Phase = "UNKNOWN"
		s.Reason = "previous_write_outcome_unknown"
		s.HoldFile = true
	}
	return s
}

func chooseKernelTarget(targets []*kernelTarget) *kernelTarget {
	var chosen *kernelTarget
	for _, t := range targets {
		s := t.machine.Status
		if s.Phase != "PREPARING" || s.Request <= 0 || s.Episode == "" || t.heartbeat.AckEpisode != s.Episode || !t.heartbeat.FileIdle {
			continue
		}
		if chosen == nil || t.registration.Priority < chosen.registration.Priority {
			chosen = t
		}
	}
	return chosen
}

func (a *kernelAgent) supervise(now time.Time) error {
	if a.busy == nil {
		return nil
	}
	s := &a.busy.machine.Status
	if now.Sub(s.Started) > time.Duration(a.busy.c.KernelReclaim.CallWatchdogMS)*time.Millisecond && !s.Watchdog {
		s.Watchdog = true
		s.Reason = "watchdog_expired_inflight_unknown"
		// Do not clear busy here. The kernel may not have returned yet.
		_ = a.child.Process.Kill()
		return a.save()
	}
	return nil
}

// Unlike os.File.Write, this adapter cannot retry a kernel control write on
// EINTR. EAGAIN can already include partial reclaim, so it is terminal too.
func kernelWriteOnce(fd int, body []byte, write func(int, []byte) (int, error)) error {
	n, e := write(fd, body)
	if e != nil {
		return e
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	return nil
}

func kernelNativeWrite(fd int, body []byte) error { return kernelWriteOnce(fd, body, syscall.Write) }
