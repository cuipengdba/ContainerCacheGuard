package main

import "time"

type RecoveryStatus struct {
	Pending         bool    `json:"pending"`
	HealthySeconds  float64 `json:"healthy_seconds"`
	RequiredSeconds int     `json:"required_seconds"`
	LastStopReason  string  `json:"last_stop_reason,omitempty"`
	GuardStops      uint64  `json:"guard_stop_transitions_total"`
	Resumes         uint64  `json:"recovery_resumes_total"`
}

// A recovery gate can only remove permission granted by the current policy.
// It never authorizes advice, modifies limits, or treats a sampling gap as
// evidence of health. Only the main sampling loop owns this state.
type recoveryGate struct {
	armed, blocked bool
	healthy, last  time.Time
	reason         string
	stops, resumes uint64
}

func (g *recoveryGate) apply(c Config, d Decision, now time.Time) (Decision, RecoveryStatus) {
	status := func() RecoveryStatus {
		healthy := 0.0
		if !g.healthy.IsZero() {
			healthy = now.Sub(g.healthy).Seconds()
			if healthy < 0 {
				healthy = 0
			}
		}
		return RecoveryStatus{g.armed, healthy, c.RecoveryHealthySeconds, g.reason, g.stops, g.resumes}
	}
	if c.Action != "enforce" || c.RecoveryHealthySeconds == 0 {
		g.armed, g.blocked = false, false
		g.healthy, g.last = time.Time{}, time.Time{}
		return d, status()
	}
	risk := d.State == "BACKOFF" || d.State == "CRITICAL" ||
		(d.State == "SUSPENDED" && d.Reason != "user_paused")
	if risk {
		if !g.blocked {
			g.stops++
		}
		g.armed, g.blocked = true, true
		g.healthy, g.last = time.Time{}, now
		g.reason = d.Reason
		return d, status()
	}
	g.blocked = false
	if d.Reason == "user_paused" {
		g.healthy, g.last = time.Time{}, time.Time{}
		return d, status()
	}
	if !g.armed {
		return d, status()
	}
	gap := now.Sub(g.last)
	if g.healthy.IsZero() || g.last.IsZero() || gap <= 0 || gap > time.Duration(c.IntervalMS)*3*time.Millisecond {
		g.healthy = now
	}
	g.last = now
	if now.Sub(g.healthy) < time.Duration(c.RecoveryHealthySeconds)*time.Second {
		if d.Allow {
			d.Allow, d.State, d.Reason = false, "SUSPENDED", "waiting_healthy_recovery"
		}
		return d, status()
	}
	g.armed = false
	g.resumes++
	result := status()
	g.healthy = time.Time{}
	return d, result
}
