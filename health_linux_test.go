//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthRoutesAndAuthentication(t *testing.T) {
	c := defaultConfig()
	c.WebListen = "127.0.0.1:0"
	c.WebTokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(c.WebTokenFile, []byte("test-monitoring-token-1234"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := startWeb(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	base := "http://" + w.listen.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	request := func(route, method string, authorized bool) (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.SetBasicAuth("ccg", "test-monitoring-token-1234")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body), response.Header.Get("Content-Type")
	}
	for _, route := range []string{"/healthz", "/readyz", "/metrics"} {
		if code, _, _ := request(route, "GET", false); code != http.StatusUnauthorized {
			t.Fatal("new route bypassed configured authentication", route, code)
		}
		if code, _, _ := request(route, "POST", true); code != http.StatusMethodNotAllowed {
			t.Fatal("new route accepted a modifying method", route, code)
		}
	}
	if code, _, _ := request("/healthz", "GET", true); code != http.StatusOK {
		t.Fatal("HTTP process is not alive before its first sample")
	}
	if code, _, _ := request("/readyz", "GET", true); code != http.StatusServiceUnavailable {
		t.Fatal("monitoring ready before first sample")
	}
	if code, body, contentType := request("/metrics", "GET", true); code != 200 ||
		!strings.Contains(body, "ccg_monitoring_ready 0\n") || !strings.Contains(contentType, "version=0.0.4") {
		t.Fatal("first scrape lacked explicit unavailable data")
	}
	for _, state := range []string{"OBSERVE", "SUSPENDED", "BACKOFF"} {
		s := metricsFixture(time.Now())
		s.Decision = Decision{State: state}
		w.publish(s)
		if code, _, _ := request("/readyz", "GET", true); code != http.StatusOK {
			t.Fatal("monitoring readiness incorrectly required reclaim permission", state)
		}
	}
	for _, problem := range []string{"stale", "error"} {
		s := metricsFixture(time.Now())
		if problem == "stale" {
			s.Time = s.Time.Add(-10 * time.Second)
		} else {
			s.Metrics.Error = "private-marker"
		}
		w.publish(s)
		if code, body, _ := request("/readyz", "GET", true); code != http.StatusServiceUnavailable || strings.Contains(body, "private-marker") {
			t.Fatal("invalid snapshot was ready or leaked error details", problem)
		}
		if code, _, _ := request("/healthz", "GET", true); code != http.StatusOK {
			t.Fatal("metric failure was mistaken for HTTP process death")
		}
	}
	s := metricsFixture(time.Now())
	w.publish(s)
	for i := 0; i < 20; i++ {
		if code, _, _ := request("/metrics", "GET", true); code != 200 {
			t.Fatal("scrape failed", code)
		}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.latest.Counters.AdviceBytes != s.Counters.AdviceBytes || w.latest.Counters.ScannedBytes != 0 || w.latest.Counters.Discovered != 0 {
		t.Fatal("reading monitoring endpoints changed work counters")
	}
}
