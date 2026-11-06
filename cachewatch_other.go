//go:build !linux

package main

import (
	"context"
	"errors"
)

type cacheWatchStats struct{}
type cacheWatch struct {
	done chan struct{}
	err  error
}

func newCacheWatch(context.Context, Config, string, bool, func() error) (*cacheWatch, error) {
	return nil, errors.New("Linux required")
}
func (w *cacheWatch) finish() error             { return errors.New("Linux required") }
func (w *cacheWatch) snapshot() cacheWatchStats { return cacheWatchStats{} }
