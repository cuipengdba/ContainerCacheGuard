package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type CgroupBudget struct {
	Path          string `json:"path"`
	MaxWorkingSet uint64 `json:"max_working_set_bytes"`
	MaxActiveFile uint64 `json:"max_active_file_bytes"`
}
type Config struct {
	bundledDir         string
	expectedSystemID   string
	Backend            string         `json:"backend"`
	Basebackup         string         `json:"pg_basebackup"`
	Verifybackup       string         `json:"pg_verifybackup"`
	Combinebackup      string         `json:"pg_combinebackup"`
	BitmapBudget       uint64         `json:"bitmap_budget_bytes"`
	PGData             string         `json:"pgdata"`
	Repository         string         `json:"repository"`
	Stanza             string         `json:"stanza"`
	Engine             string         `json:"pgbackrest"`
	EngineConfig       string         `json:"pgbackrest_config"`
	PSQL               string         `json:"psql"`
	PGHost             string         `json:"pg_host"`
	PGPort             int            `json:"pg_port"`
	PGUser             string         `json:"pg_user"`
	PGCheckUser        string         `json:"pg_check_user"`
	PGDatabase         string         `json:"pg_database"`
	RuntimeDir         string         `json:"runtime_dir"`
	LockFiles          []string       `json:"lock_files"`
	ReadBPS            uint64         `json:"read_bytes_per_second"`
	WriteBPS           uint64         `json:"write_bytes_per_second"`
	FlushBytes         uint64         `json:"flush_bytes"`
	ReadWindowBytes    uint64         `json:"read_window_bytes"`
	RateBurstBytes     uint64         `json:"rate_burst_bytes"`
	CachePolicy        string         `json:"cache_policy"`
	ProcessMax         int            `json:"process_max"`
	MinAvailable       uint64         `json:"min_node_available_bytes"`
	WarnAvailable      uint64         `json:"warn_node_available_bytes"`
	KubeletFile        string         `json:"kubelet_summary_file"`
	KubeletURL         string         `json:"kubelet_summary_url"`
	KubeletCA          string         `json:"kubelet_ca_file"`
	KubeletToken       string         `json:"kubelet_token_file"`
	StatsMaxAgeSeconds int            `json:"stats_max_age_seconds"`
	Cgroups            []CgroupBudget `json:"cgroups"`
	PollMS             int            `json:"poll_ms"`
	DeadlineSeconds    int            `json:"deadline_seconds"`
	GraceSeconds       int            `json:"grace_seconds"`
	LockWaitSeconds    int            `json:"lock_wait_seconds"`
	RequireKubelet     bool           `json:"require_kubelet_stats"`
	RestoreToPGData    bool           `json:"allow_stopped_pgdata_restore"`
}

func loadConfig(path string) (Config, error) {
	c := Config{Backend: "native", BitmapBudget: 256 << 20, PGPort: 5432, PGUser: "postgres", PGDatabase: "postgres", ProcessMax: 1,
		FlushBytes: 8 << 20, ReadWindowBytes: 8 << 20, RateBurstBytes: 1 << 20, CachePolicy: "preserve-hot", PollMS: 1000, DeadlineSeconds: 86400, GraceSeconds: 30, StatsMaxAgeSeconds: 15}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration must contain one JSON object")
	}
	return c, c.validate()
}
func (c *Config) validate() error {
	if c.Backend != "pgbackrest" && c.Backend != "native" {
		return errors.New("backend must be native or pgbackrest")
	}
	paths := map[string]*string{"pgdata": &c.PGData, "repository": &c.Repository, "psql": &c.PSQL, "runtime_dir": &c.RuntimeDir}
	if c.Backend == "native" {
		paths["pg_basebackup"] = &c.Basebackup
		paths["pg_verifybackup"] = &c.Verifybackup
		paths["pg_combinebackup"] = &c.Combinebackup
	} else {
		paths["pgbackrest"] = &c.Engine
		paths["pgbackrest_config"] = &c.EngineConfig
	}
	for name, p := range paths {
		if c.Backend == "native" && *p == "" && (name == "psql" || name == "pg_basebackup" || name == "pg_verifybackup" || name == "pg_combinebackup") {
			continue // populated from the embedded static toolchain at admission
		}
		if !filepath.IsAbs(*p) || strings.ContainsAny(*p, ":\n\r\x00") {
			return fmt.Errorf("%s must be an absolute Linux path without colon or newline", name)
		}
		*p = filepath.Clean(*p)
		if *p == string(filepath.Separator) {
			return fmt.Errorf("%s cannot be filesystem root", name)
		}
	}
	if c.BitmapBudget < 1<<20 || c.BitmapBudget > 1<<30 {
		return errors.New("bitmap_budget_bytes must be 1MiB..1GiB")
	}
	if overlap(c.PGData, c.Repository) || overlap(c.PGData, c.RuntimeDir) || overlap(c.Repository, c.RuntimeDir) {
		return errors.New("PGDATA, repository and runtime directory must be disjoint")
	}
	if c.Backend == "pgbackrest" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`).MatchString(c.Stanza) {
		return errors.New("invalid stanza")
	}
	if c.PGPort < 1 || c.PGPort > 65535 || c.ProcessMax < 1 || c.ProcessMax > 16 {
		return errors.New("invalid port or process_max (1..16)")
	}
	if c.ReadBPS < 32768 || c.WriteBPS < 32768 || c.ReadBPS > 1<<40 || c.WriteBPS > 1<<40 {
		return errors.New("explicit read/write limits between 32KiB/s and 1TiB/s are required")
	}
	if c.FlushBytes < 4096 || c.FlushBytes > 64<<20 {
		return errors.New("flush_bytes must be between 4096 and 64MiB")
	}
	if (c.ReadWindowBytes != 0 && (c.ReadWindowBytes < 4096 || c.ReadWindowBytes > 64<<20)) || c.RateBurstBytes > 64<<20 {
		return errors.New("read_window_bytes must be 0 or 4096..64MiB; rate_burst_bytes must be 0..64MiB")
	}
	if c.CachePolicy != "preserve-hot" && c.CachePolicy != "destination-only" && c.CachePolicy != "off" {
		return errors.New("cache_policy must be preserve-hot, destination-only, or off (rate-limited baseline)")
	}
	if c.PollMS < 100 || c.PollMS > 10000 || c.DeadlineSeconds < 1 || c.DeadlineSeconds > 604800 || c.GraceSeconds < 1 || c.GraceSeconds > 120 || c.LockWaitSeconds < 0 || c.LockWaitSeconds > 86400 {
		return errors.New("invalid polling/deadline/grace/lock wait setting")
	}
	if c.MinAvailable > 0 && c.WarnAvailable < c.MinAvailable {
		return errors.New("warn_node_available_bytes must be >= min_node_available_bytes")
	}
	if c.StatsMaxAgeSeconds < 1 || c.StatsMaxAgeSeconds > 300 {
		return errors.New("stats_max_age_seconds must be 1..300")
	}
	if c.KubeletFile != "" && c.KubeletURL != "" {
		return errors.New("select either kubelet_summary_file or kubelet_summary_url")
	}
	if c.RequireKubelet && c.KubeletFile == "" && c.KubeletURL == "" {
		return errors.New("require_kubelet_stats needs a kubelet summary source")
	}
	if c.MinAvailable == 0 && len(c.Cgroups) == 0 {
		return errors.New("configure at least one memory safety budget")
	}
	if len(c.LockFiles) == 0 {
		return errors.New("at least one shared per-device lock file is required")
	}
	for _, p := range c.LockFiles {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\n\r\x00") || overlap(c.PGData, p) || overlap(c.Repository, p) {
			return errors.New("lock files must be absolute and outside PGDATA/repository")
		}
	}
	for _, b := range c.Cgroups {
		if !filepath.IsAbs(b.Path) || (b.MaxWorkingSet == 0 && b.MaxActiveFile == 0) {
			return errors.New("each cgroup needs an absolute path and a nonzero budget")
		}
	}
	return nil
}
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func overlap(a, b string) bool { return within(a, b) || within(b, a) }

// Resolve existing ancestors too, so a missing restore target below a symlink
// cannot bypass disjointness checks.
func canonical(path string) (string, error) {
	p := filepath.Clean(path)
	var tail []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}
func (c *Config) resolve() error {
	paths := []*string{&c.PGData, &c.Repository, &c.RuntimeDir, &c.PSQL}
	if c.Backend == "native" {
		paths = append(paths, &c.Basebackup, &c.Verifybackup, &c.Combinebackup)
	} else {
		paths = append(paths, &c.Engine, &c.EngineConfig)
	}
	for _, p := range paths {
		if c.Backend == "native" && *p == "" {
			continue
		}
		r, e := canonical(*p)
		if e != nil {
			return e
		}
		*p = r
	}
	return c.validate()
}
func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".pg18guard-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func event(kind string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = kind
	fields["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(fields)
	fmt.Fprintln(os.Stderr, string(b))
}
