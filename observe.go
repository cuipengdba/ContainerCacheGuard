package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type CgroupSample struct {
	Path         string            `json:"path"`
	Current      uint64            `json:"current_bytes"`
	WorkingSet   uint64            `json:"working_set_bytes"`
	Anon         uint64            `json:"anon_bytes"`
	File         uint64            `json:"file_bytes"`
	Shmem        uint64            `json:"shmem_bytes"`
	ActiveFile   uint64            `json:"active_file_bytes"`
	InactiveFile uint64            `json:"inactive_file_bytes"`
	Dirty        uint64            `json:"dirty_bytes"`
	Writeback    uint64            `json:"writeback_bytes"`
	Events       map[string]uint64 `json:"events"`
}
type Sample struct {
	NodeAvailable uint64         `json:"node_available_bytes"`
	Source        string         `json:"node_available_source"`
	Cgroups       []CgroupSample `json:"cgroups"`
}
type Observer struct {
	config Config
	client *http.Client
}

func newObserver(c Config) (*Observer, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if c.KubeletCA != "" {
		b, e := os.ReadFile(c.KubeletCA)
		if e != nil {
			return nil, e
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid kubelet CA")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	if c.KubeletURL != "" && !strings.HasPrefix(c.KubeletURL, "https://") {
		return nil, errors.New("kubelet_summary_url must use verified HTTPS")
	}
	return &Observer{c, &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func parseCounters(b []byte) map[string]uint64 {
	m := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, e := strconv.ParseUint(f[1], 10, 64)
			if e == nil {
				m[f[0]] = n
			}
		}
	}
	return m
}
func readCgroup(path string) (CgroupSample, error) {
	s := CgroupSample{Path: path}
	b, e := os.ReadFile(path + "/memory.current")
	if e != nil {
		return s, e
	}
	s.Current, e = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if e != nil {
		return s, e
	}
	b, e = os.ReadFile(path + "/memory.stat")
	if e != nil {
		return s, e
	}
	m := parseCounters(b)
	for _, key := range []string{"anon", "file", "shmem", "active_file", "inactive_file"} {
		if _, ok := m[key]; !ok {
			return s, fmt.Errorf("%s missing %s", path, key)
		}
	}
	s.Anon = m["anon"]
	s.File = m["file"]
	s.Shmem = m["shmem"]
	s.ActiveFile = m["active_file"]
	s.InactiveFile = m["inactive_file"]
	s.Dirty = m["file_dirty"]
	s.Writeback = m["file_writeback"]
	if s.Current > s.InactiveFile {
		s.WorkingSet = s.Current - s.InactiveFile
	}
	b, e = os.ReadFile(path + "/memory.events")
	if e != nil {
		return s, e
	}
	s.Events = parseCounters(b)
	return s, nil
}
func summaryAvailable(b []byte, maxAge time.Duration, now time.Time) (uint64, error) {
	var v struct {
		Node struct {
			Memory struct {
				Time      time.Time `json:"time"`
				Available *uint64   `json:"availableBytes"`
			} `json:"memory"`
		} `json:"node"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return 0, e
	}
	m := v.Node.Memory
	if m.Available == nil || m.Time.IsZero() {
		return 0, errors.New("kubelet summary is missing memory.availableBytes/time")
	}
	if age := now.Sub(m.Time); age > maxAge || age < -5*time.Second {
		return 0, errors.New("stale or future kubelet memory sample")
	}
	return *m.Available, nil
}
func (o *Observer) Sample() (Sample, error) {
	c := o.config
	s := Sample{}
	var b []byte
	var e error
	switch {
	case c.KubeletFile != "":
		b, e = os.ReadFile(c.KubeletFile)
		s.Source = "kubelet_summary_file"
	case c.KubeletURL != "":
		var req *http.Request
		req, e = http.NewRequest("GET", c.KubeletURL, nil)
		if e == nil && c.KubeletToken != "" {
			var tok []byte
			tok, e = os.ReadFile(c.KubeletToken)
			if e == nil {
				req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
			}
		}
		if e == nil {
			var r *http.Response
			r, e = o.client.Do(req)
			if e == nil {
				defer r.Body.Close()
				if r.StatusCode != 200 {
					e = fmt.Errorf("kubelet HTTP status %d", r.StatusCode)
				} else {
					b, e = io.ReadAll(io.LimitReader(r.Body, 8<<20))
				}
			}
		}
		s.Source = "kubelet_summary_https"
	default:
		b, e = os.ReadFile("/proc/meminfo")
		s.Source = "linux_MemAvailable_NOT_kubelet"
		if e == nil {
			m := parseCounters(b)
			v, ok := m["MemAvailable:"]
			if !ok {
				e = errors.New("MemAvailable missing")
			} else {
				s.NodeAvailable = v * 1024
			}
		}
	}
	if e != nil {
		return s, e
	}
	if c.KubeletFile != "" || c.KubeletURL != "" {
		s.NodeAvailable, e = summaryAvailable(b, time.Duration(c.StatsMaxAgeSeconds)*time.Second, time.Now())
		if e != nil {
			return s, e
		}
	}
	for _, budget := range c.Cgroups {
		v, err := readCgroup(budget.Path)
		if err != nil {
			return s, err
		}
		s.Cgroups = append(s.Cgroups, v)
	}
	return s, nil
}

// Limits apply immediately; no waiting for kubelet's eviction threshold.
func (c Config) policy(s Sample) (critical, warning bool, reason string) {
	if c.MinAvailable > 0 && s.NodeAvailable < c.MinAvailable {
		return true, false, "node memory headroom below minimum"
	}
	if c.WarnAvailable > 0 && s.NodeAvailable < c.WarnAvailable {
		warning = true
		reason = "node memory headroom below warning"
	}
	for i, v := range s.Cgroups {
		b := c.Cgroups[i]
		if b.MaxWorkingSet > 0 && v.WorkingSet >= b.MaxWorkingSet {
			return true, false, "cgroup working set budget exceeded: " + v.Path
		}
		if b.MaxActiveFile > 0 && v.ActiveFile >= b.MaxActiveFile {
			return true, false, "cgroup active_file budget exceeded: " + v.Path
		}
	}
	return false, warning, reason
}
