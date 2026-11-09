//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWebScopeAndHistory(t *testing.T) {
	if validateWebListen("0.0.0.0:9898", "") == nil {
		t.Fatal("public listen without token accepted")
	}
	if err := validateWebListen("127.0.0.1:9898", ""); err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.WebListen = "127.0.0.1:0"
	c.WebHistorySamples = 60
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := startWeb(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	base := "http://" + w.listen.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(base + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first sample status %d", response.StatusCode)
	}
	for i := 0; i < 65; i++ {
		w.publish(Status{Time: time.Now(), Metrics: Metrics{File: int64(i)}})
	}
	w.mu.RLock()
	points := w.pointsLocked()
	w.mu.RUnlock()
	if len(points) != 60 || points[0].File != 5 || points[59].File != 64 {
		t.Fatal("history ring did not cap oldest entries")
	}
	for _, route := range []string{"/", "/api/v1/status", "/api/v1/history"} {
		response, err = client.Get(base + route)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d", route, response.StatusCode)
		}
		if _, err := time.Parse(time.RFC3339Nano, response.Header.Get("X-CCG-Server-Time")); err != nil {
			t.Fatalf("%s missing server time: %v", route, err)
		}
	}
}

func TestWebBasicAuthentication(t *testing.T) {
	c := defaultConfig()
	c.WebListen = "127.0.0.1:0"
	c.WebTokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(c.WebTokenFile, []byte("test-token-123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := startWeb(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	w.publish(Status{Time: time.Now()})
	url := "http://" + w.listen.Addr().String() + "/api/v1/status"
	client := &http.Client{Timeout: 2 * time.Second}
	for _, password := range []string{"", "wrong", "test-token-123456789"} {
		req, _ := http.NewRequest("GET", url, nil)
		if password != "" {
			req.SetBasicAuth("ccg", password)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		expect := http.StatusUnauthorized
		if password == "test-token-123456789" {
			expect = http.StatusOK
		}
		if response.StatusCode != expect {
			t.Fatalf("password=%q status=%d", password, response.StatusCode)
		}
	}
}

func TestWebHistoryMarksInvalidSamples(t *testing.T) {
	w := &webState{limit: 60}
	w.publish(Status{Time: time.Now(), IntervalMS: 1000, Metrics: Metrics{File: 123}})
	w.publish(Status{Time: time.Now(), IntervalMS: 1000, Metrics: Metrics{Error: "read failed"}})
	w.mu.RLock()
	defer w.mu.RUnlock()
	p := w.pointsLocked()
	if len(p) != 2 || !p[0].Valid || p[1].Valid || p[0].IntervalMS != 1000 {
		t.Fatalf("failed metrics sample was not marked for a chart gap: %+v", p)
	}
}
