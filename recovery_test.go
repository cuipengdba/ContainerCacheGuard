package main

import (
	"testing"
	"time"
)

func recoveryFixture() (Config, Decision, Decision, time.Time) {
	c := defaultConfig()
	c.Action, c.RecoveryHealthySeconds = "enforce", 2
	return c, Decision{State: "CONTROL", Allow: true, CacheTarget: 123},
		Decision{State: "BACKOFF", Reason: "io_bandwidth", CacheTarget: 123}, time.Now()
}

func TestRecoveryGateRequiresHealthySequence(t *testing.T) {
	c, healthy, risk, now := recoveryFixture()
	var g recoveryGate
	if d, s := g.apply(c, healthy, now); !d.Allow || s.Pending {
		t.Fatal("healthy first start was delayed", d, s)
	}
	if d, s := g.apply(c, risk, now); d.Allow || s.GuardStops != 1 || !s.Pending {
		t.Fatal("risk did not immediately stop", d, s)
	}
	for second := 1; second <= 2; second++ {
		d, s := g.apply(c, healthy, now.Add(time.Duration(second)*time.Second))
		if d.Allow || d.Reason != "waiting_healthy_recovery" || s.HealthySeconds != float64(second-1) || d.CacheTarget != 123 {
			t.Fatal("resumed before continuous healthy interval", d, s)
		}
	}
	d, s := g.apply(c, risk, now.Add(3*time.Second))
	if d.Allow || s.GuardStops != 2 || s.HealthySeconds != 0 {
		t.Fatal("recurrent risk did not reset healthy interval", d, s)
	}
	for second := 4; second <= 6; second++ {
		d, s = g.apply(c, healthy, now.Add(time.Duration(second)*time.Second))
		if d.Allow != (second == 6) {
			t.Fatal("wrong recovery boundary", second, d, s)
		}
	}
	if s.Pending || s.Resumes != 1 {
		t.Fatal("successful recovery not recorded", s)
	}
}

func TestRecoverySamplingGapPauseAndClockRegression(t *testing.T) {
	c, healthy, risk, now := recoveryFixture()
	var g recoveryGate
	g.apply(c, risk, now)
	g.apply(c, healthy, now.Add(time.Second))
	g.apply(c, healthy, now.Add(2*time.Second))
	_, s := g.apply(c, healthy, now.Add(10*time.Second))
	if s.HealthySeconds != 0 || !s.Pending {
		t.Fatal("sample gap was counted as healthy", s)
	}
	g.apply(c, Decision{State: "SUSPENDED", Reason: "user_paused"}, now.Add(11*time.Second))
	d, s := g.apply(c, healthy, now.Add(30*time.Second))
	if d.Allow || s.HealthySeconds != 0 {
		t.Fatal("manual pause satisfied recovery wait", d, s)
	}
	d, s = g.apply(c, healthy, now.Add(29*time.Second))
	if d.Allow || s.HealthySeconds != 0 {
		t.Fatal("clock regression satisfied recovery wait", d, s)
	}
}

func TestRecoveryNeverEnablesRejectedPolicy(t *testing.T) {
	c, _, risk, now := recoveryFixture()
	var g recoveryGate
	g.apply(c, risk, now)
	idle := Decision{State: "IDLE", Reason: "below_cache_target"}
	for second := 1; second <= 4; second++ {
		d, _ := g.apply(c, idle, now.Add(time.Duration(second)*time.Second))
		if d.Allow || d.State != "IDLE" {
			t.Fatal("health recovery forced advice below target", d)
		}
	}
	for _, state := range []string{"BACKOFF", "CRITICAL", "SUSPENDED"} {
		d, _ := g.apply(c, Decision{State: state, Reason: "guard"}, now.Add(5*time.Second))
		if d.Allow || d.State != state {
			t.Fatal("original safety decision overridden", d)
		}
	}
}

func TestRecoveryCanBeDisabledAndValidated(t *testing.T) {
	c, healthy, risk, now := recoveryFixture()
	for _, action := range []string{"observe", "enforce"} {
		c.Action, c.RecoveryHealthySeconds = action, 0
		var g recoveryGate
		g.apply(c, risk, now)
		if d, s := g.apply(c, healthy, now.Add(time.Second)); !d.Allow || s.Pending {
			t.Fatal("disabled gate modified policy", d, s)
		}
	}
	for _, value := range []int{-1, 3601} {
		c.RecoveryHealthySeconds = value
		if c.validate() == nil {
			t.Fatal("invalid recovery duration accepted", value)
		}
	}
}
