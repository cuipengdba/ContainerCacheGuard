//go:build !linux

package main

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

type Control struct{}

func renameNoReplace(string, string) error { return errors.New("Linux is required") }

func newControl(string, uint64, uint64) (*Control, error) {
	return nil, errors.New("Linux is required")
}
func (c *Control) rates(uint64, uint64) error        { return nil }
func (c *Control) abort() error                      { return nil }
func (c *Control) stats() (map[string]uint64, error) { return nil, nil }
func (c *Control) close()                            {}
func syncDir(string) error                           { return nil }
func acquireLocks(context.Context, []string, time.Duration) (func(), error) {
	return nil, errors.New("Linux is required")
}
func groupCommand(*exec.Cmd) {}
func terminateGroup(*exec.Cmd, <-chan error, time.Duration) error {
	return errors.New("Linux is required")
}
