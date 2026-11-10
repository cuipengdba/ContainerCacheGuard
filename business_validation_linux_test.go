//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBusinessSignalFIFOIsRejectedWithoutBlocking(t *testing.T) {
	c := defaultConfig()
	c.BusinessSignal = filepath.Join(t.TempDir(), "signal.fifo")
	if err := syscall.Mkfifo(c.BusinessSignal, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Metrics, 1)
	go func() {
		m := Metrics{}
		business(c, &m)
		done <- m
	}()
	select {
	case m := <-done:
		if m.Error == "" {
			t.Fatal("FIFO accepted as a regular signal file")
		}
	case <-time.After(time.Second):
		t.Fatal("opening a signal FIFO blocked the sampling loop")
	}
}

func TestBusinessSignalRejectsInvalidOrOversizedInput(t *testing.T) {
	c := defaultConfig()
	c.BusinessSignal = filepath.Join(t.TempDir(), "signal.json")
	for _, tc := range []struct {
		name, suffix string
		p99          float64
		valid        bool
	}{
		{"valid_zero", "", 0, true},
		{"valid_positive", "  \n", 12.5, true},
		{"negative_latency", "", -1, false},
		{"trailing_object", "{}", 1, false},
		{"oversized_whitespace", strings.Repeat(" ", 17000), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"time": time.Now(), "p99_ms": tc.p99})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(c.BusinessSignal, append(body, []byte(tc.suffix)...), 0600); err != nil {
				t.Fatal(err)
			}
			m := Metrics{}
			business(c, &m)
			if (m.Error == "") != tc.valid {
				t.Fatalf("valid=%v error=%q", tc.valid, m.Error)
			}
		})
	}
}
