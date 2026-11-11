//go:build linux && (amd64 || arm64)

package main

import "time"

type SignalHealth struct {
	State            string   `json:"state"`
	AgeSeconds       *float64 `json:"age_seconds"`
	RemainingSeconds *float64 `json:"remaining_seconds"`
	Error            string   `json:"error,omitempty"`
}
type SignalStatus struct {
	Node              SignalHealth `json:"node"`
	Business          SignalHealth `json:"business"`
	NodeStopBytes     *int64       `json:"node_stop_bytes"`
	NodeHeadroomBytes *int64       `json:"node_headroom_above_stop_bytes"`
}

func signalHealth(configured bool, at, now time.Time, maxAge int, issue string) SignalHealth {
	h := SignalHealth{State: "disabled", Error: issue}
	if !configured {
		return h
	}
	h.State = "unavailable"
	if at.IsZero() {
		return h
	}
	age, remaining := now.Sub(at).Seconds(), float64(maxAge)-now.Sub(at).Seconds()
	h.AgeSeconds = &age
	h.RemainingSeconds = &remaining
	h.State = "ready"
	if age > float64(maxAge) {
		h.State = "stale"
	} else if age < -1 {
		h.State = "future"
	} else if issue != "" {
		h.State = "invalid"
	}
	return h
}
func signals(c Config, m Metrics, now time.Time) SignalStatus {
	s := SignalStatus{Node: signalHealth(c.NodeSignal != "", nodeTime(m.Node), now, c.SignalMaxAgeSeconds, m.NodeSignalError), Business: signalHealth(c.BusinessSignal != "", m.BusinessTime, now, c.SignalMaxAgeSeconds, m.BusinessSignalError)}
	if c.NodeSignal != "" && validNodeSignal(c, m.Node, now) && m.NodeSignalError == "" {
		stop := nodeStopThreshold(c, m.Node)
		headroom := *m.Node.AvailableBytes - stop
		s.NodeStopBytes = &stop
		s.NodeHeadroomBytes = &headroom
	}
	return s
}

// Permission expires at the earliest deadline, not merely at the next sample.
func adviceFresh(c Config, m Metrics, now time.Time) bool {
	if m.Error != "" || m.Time.IsZero() || now.Before(m.Time) || now.Sub(m.Time) > time.Duration(c.IntervalMS)*3*time.Millisecond {
		return false
	}
	if c.NodeSignal != "" && !validNodeSignal(c, m.Node, now) {
		return false
	}
	if c.BusinessSignal != "" && (m.BusinessTime.IsZero() || now.Sub(m.BusinessTime) > time.Duration(c.SignalMaxAgeSeconds)*time.Second || m.BusinessTime.Sub(now) > time.Second) {
		return false
	}
	return true
}
