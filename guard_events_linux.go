//go:build linux && (amd64 || arm64)

package main

// Emitted on every sampled policy transition, including sub-second samples.
// This does not fill unsampled time or claim that permission was continuously
// armed during sampler/log IO. The external guard/recovery remains unchanged.
func guardTransitionLog(s Status, previous *Decision) map[string]any {
	return map[string]any{"event": "guard_transition", "time": s.Time, "instance_id": s.InstanceID, "run_id": s.RunID,
		"previous_decision": previous, "decision": s.Decision, "recovery": s.Recovery, "metrics": s.Metrics, "signals": s.Signals,
		"message": explainDecision(s.Decision), "sampling_interval_ms": s.IntervalMS,
		"scope": "sampled policy transition; gaps and between-sample transients remain unknown; OOM history retained"}
}
