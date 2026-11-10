package main

import (
	"path/filepath"
	"testing"
)

func TestLoggingConfigAndAutomaticExclusion(t *testing.T) {
	c := defaultConfig()
	if c.LogKeepDays != 30 || c.LogMaxTotalBytes != 1000000000 || c.validateLogging() != nil {
		t.Fatal("logging defaults do not match retention policy")
	}
	c.LogTimezone = "Asia/Shanghai"
	if err := c.validateLogging(); err != nil {
		t.Fatal("named zone is not embedded", err)
	}
	if !c.excluded(filepath.Join(c.logDirectory(), "containercacheguard-2026-11-10.log")) {
		t.Fatal("daemon can scan its own active logs")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.LogKeepDays = 0 },
		func(c *Config) { c.LogMaxTotalBytes = MiB - 1 },
		func(c *Config) { c.LogTimezone = "not/a/zone" },
		func(c *Config) { c.LogDirectory = "/" },
		func(c *Config) { c.LogDirectory = "/tmp/x/../logs" },
	} {
		x := c
		mutate(&x)
		if x.validateLogging() == nil {
			t.Fatal("invalid logging configuration accepted", x)
		}
	}
}
