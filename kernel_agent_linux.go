//go:build linux && (amd64 || arm64)

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type kernelRegistration struct {
	Name     string `json:"name"`
	Cgroup   string `json:"cgroup"`
	Config   string `json:"config"`
	PeerUID  uint32 `json:"peer_uid"`
	Priority int    `json:"priority"`
}
type kernelAgentConfig struct {
	Socket         string               `json:"socket"`
	SocketGID      *int                 `json:"socket_gid,omitempty"`
	StateDirectory string               `json:"state_directory"`
	LogDirectory   string               `json:"log_directory"`
	Targets        []kernelRegistration `json:"targets"`
}
type kernelHeartbeat struct {
	Target     string `json:"target"`
	Action     string `json:"action"`
	Paused     bool   `json:"paused"`
	AckEpisode string `json:"ack_episode"`
	FileIdle   bool   `json:"file_idle"`
}
type kernelBudgetEntry struct {
	Time  time.Time `json:"time"`
	Bytes int64     `json:"bytes"`
}
type kernelJournal struct {
	Schema  int                     `json:"schema"`
	Targets map[string]KernelStatus `json:"targets"`
	Budget  []kernelBudgetEntry     `json:"budget"`
}
type kernelTarget struct {
	registration  kernelRegistration
	c             Config
	r             *metricReader
	machine       kernelMachine
	sample        KernelSample
	heartbeat     kernelHeartbeat
	heartbeatTime time.Time
	external      externalGuard
	prepared      time.Time
}
type kernelMessage struct {
	request kernelHeartbeat
	peer    *syscall.Ucred
	reply   chan KernelStatus
}
type kernelAgent struct {
	config      kernelAgentConfig
	boot        string
	targets     []*kernelTarget
	journal     kernelJournal
	journalPath string
	lock        *os.File
	logs        *dailyLogger
	busy        *kernelTarget
	child       *exec.Cmd
	done        chan error
	started     time.Time
	lastEvent   map[string]string
	childOutput cappedKernelOutput
	readSample  func(*kernelTarget) KernelSample // internal test seam; not configurable
}

func (a *kernelAgent) read(t *kernelTarget) KernelSample {
	if a.readSample != nil {
		return a.readSample(t)
	}
	return kernelRead(t.c, t.r, a.boot)
}

// exec.Wait synchronizes completion before the sampler reads this buffer.
type cappedKernelOutput struct{ bytes.Buffer }

func (b *cappedKernelOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		b.Buffer.Write(p[:minInt(len(p), 4096-b.Len())])
	}
	return n, nil
}

func durableKernelJSON(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > 2<<20 {
		return errors.New("kernel journal capacity exceeded")
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".reclaim-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := safeOpen(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func kernelTrustedFile(path string) error {
	f, e := safeOpen(path)
	if e != nil {
		return e
	}
	id, e := statFile(f)
	f.Close()
	if e != nil || id.UID != uint32(os.Geteuid()) || id.Mode&0022 != 0 {
		return errors.New("kernel input must be owned by agent UID and not group/world writable")
	}
	return nil
}
func trustedKernelJSON(path string, v any) error {
	return readTrustedKernelJSON(path, 2<<20, v)
}
func readTrustedKernelJSON(path string, limit int64, v any) error {
	if e := kernelTrustedFile(path); e != nil {
		return e
	}
	return readRegularJSON(path, limit, v, true)
}
func kernelKey(boot string, r *metricReader) string {
	b := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", boot, r.id.Dev, r.id.Ino)))
	return hex.EncodeToString(b[:])
}
func (a *kernelAgent) save() error {
	for _, t := range a.targets {
		if t.machine.Status.Attempted {
			a.journal.Targets[kernelKey(a.boot, t.r)] = t.machine.Status
		}
	}
	if len(a.journal.Targets) > 128 {
		return errors.New("kernel journal target history full; unresolved history is never evicted")
	}
	return durableKernelJSON(a.journalPath, a.journal)
}
func (a *kernelAgent) remaining(now time.Time) int64 {
	total := int64(0)
	fresh := a.journal.Budget[:0]
	for _, b := range a.journal.Budget {
		if b.Time.After(now) {
			return 0
		}
		if now.Sub(b.Time) < 300*time.Second {
			fresh = append(fresh, b)
			total += b.Bytes
		}
	}
	a.journal.Budget = fresh
	if len(fresh) > 0 && now.Sub(fresh[len(fresh)-1].Time) < 30*time.Second {
		return 0
	}
	return max64(0, (1<<30)-total)
}
func kernelLeaf(path string) error {
	if filepath.Clean(path) != path || !beneath(path, "/sys/fs/cgroup") || path == "/sys/fs/cgroup" {
		return errors.New("kernel target must be an explicit cgroup2 descendant")
	}
	f, e := safeOpen(path)
	if e != nil {
		return e
	}
	defer f.Close()
	var fs syscall.Statfs_t
	if e = syscall.Fstatfs(int(f.Fd()), &fs); e != nil || uint64(fs.Type) != 0x63677270 {
		return errors.New("kernel target is not cgroup2")
	}
	for {
		entries, err := f.Readdir(64)
		for _, x := range entries {
			if x.IsDir() {
				return errors.New("kernel target must be a leaf cgroup")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func newKernelAgent(ac kernelAgentConfig) (a *kernelAgent, err error) {
	if len(ac.Targets) < 1 || len(ac.Targets) > 32 || !filepath.IsAbs(ac.Socket) || filepath.Clean(ac.Socket) != ac.Socket || !filepath.IsAbs(ac.StateDirectory) || filepath.Clean(ac.StateDirectory) != ac.StateDirectory || ac.StateDirectory == "/" || !filepath.IsAbs(ac.LogDirectory) || filepath.Clean(ac.LogDirectory) != ac.LogDirectory || ac.LogDirectory == "/" || (ac.SocketGID != nil && (*ac.SocketGID < 0 || *ac.SocketGID > 1<<31-1)) {
		return nil, errors.New("agent requires bounded targets and absolute socket/state/log paths")
	}
	boot, err := readBootID()
	if err != nil {
		return nil, err
	}
	// A fixed node-wide lock prevents a second differently-configured executor.
	lock, err := lockState("/run/containercacheguard-node/agent")
	if err != nil {
		return nil, err
	}
	// Bind the state path to this boot under the same fixed node lock. Changing
	// config paths must not silently discard an already-reserved episode.
	binding := struct {
		Boot      string `json:"boot"`
		Directory string `json:"directory"`
	}{boot, ac.StateDirectory}
	const bindPath = "/run/containercacheguard-node/state-binding.json"
	if _, e := os.Lstat(bindPath); e == nil {
		var old struct {
			Boot      string `json:"boot"`
			Directory string `json:"directory"`
		}
		if e = trustedKernelJSON(bindPath, &old); e != nil || (old.Boot == boot && old.Directory != ac.StateDirectory) {
			lock.Close()
			return nil, errors.New("node state directory binding changed or invalid; preserve original journal")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		lock.Close()
		return nil, e
	}
	if e := durableKernelJSON(bindPath, binding); e != nil {
		lock.Close()
		return nil, e
	}
	if err = os.MkdirAll(ac.StateDirectory, 0700); err != nil {
		lock.Close()
		return nil, err
	}
	dir, e := safeOpen(ac.StateDirectory)
	if e != nil {
		lock.Close()
		return nil, e
	}
	did, e := statFile(dir)
	dir.Close()
	if e != nil || did.UID != uint32(os.Geteuid()) || did.Mode&0077 != 0 {
		lock.Close()
		return nil, errors.New("journal directory must be agent-owned mode0700")
	}
	a = &kernelAgent{config: ac, boot: boot, lock: lock, journalPath: filepath.Join(ac.StateDirectory, "journal.json"), journal: kernelJournal{Schema: 1, Targets: map[string]KernelStatus{}}, started: time.Now(), lastEvent: map[string]string{}}
	allocated := a
	defer func() {
		if err != nil {
			if allocated.logs != nil {
				allocated.logs.Close()
			}
			lock.Close()
		}
	}()
	if _, e := os.Lstat(a.journalPath); e == nil {
		if err = trustedKernelJSON(a.journalPath, &a.journal); err != nil {
			return nil, err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if err = validateKernelJournal(a.journal); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, reg := range ac.Targets {
		if reg.Name == "" || !identifier.MatchString(reg.Name) || seen[reg.Name] || seen[reg.Cgroup] || reg.Priority < 0 || reg.Priority > 100 {
			return nil, errors.New("duplicate/invalid registered target")
		}
		seen[reg.Name] = true
		seen[reg.Cgroup] = true
		var c Config = defaultConfig()
		if err = trustedKernelJSON(reg.Config, &c); err != nil {
			return nil, err
		}
		if err = c.validate(); err != nil {
			return nil, err
		}
		if !c.KernelReclaim.Enabled || c.KernelReclaim.Target != reg.Name || c.KernelReclaim.Socket != ac.Socket || c.KernelReclaim.StateDirectory != ac.StateDirectory {
			return nil, errors.New("agent target/config binding mismatch")
		}
		if err = kernelLeaf(reg.Cgroup); err != nil {
			return nil, err
		}
		r, e := newMetricReader(reg.Cgroup)
		if e != nil {
			return nil, e
		}
		t := &kernelTarget{registration: reg, c: c, r: r}
		t.machine.Status = KernelStatus{Phase: "ARMED", Reason: "waiting_heartbeat", Time: time.Now(), TargetIdentity: kernelKey(boot, r)}
		if saved, ok := a.journal.Targets[kernelKey(boot, r)]; ok {
			t.machine.Status = restoreKernelStatus(saved)
		}
		a.targets = append(a.targets, t)
	}
	lc := defaultConfig()
	lc.StateFile = filepath.Join(ac.StateDirectory, "agent-status.json")
	lc.LogDirectory = ac.LogDirectory
	a.logs, err = newDailyLogger(lc, os.Stdout, time.Now)
	if err != nil {
		return nil, err
	}
	return a, nil
}
func (a *kernelAgent) log(t *kernelTarget, force bool) error {
	s := t.machine.Status
	sig := s.Phase + "/" + s.Reason + "/" + s.Return
	if !force && a.lastEvent[t.registration.Name] == sig {
		return nil
	}
	a.lastEvent[t.registration.Name] = sig
	return a.logs.WriteJSON(map[string]any{"time": time.Now(), "event": "kernel_reclaim", "target": t.registration.Name, "version": version, "status": s, "message": "内核主动回收事件；请求量、容器下降和驱逐余量分别核对，不保证阻止驱逐。"})
}
func (a *kernelAgent) authenticated(msg kernelMessage) (*kernelTarget, error) {
	if msg.peer == nil {
		return nil, errors.New("missing peer credentials")
	}
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", msg.peer.Pid))
	if e != nil {
		return nil, e
	}
	path := ""
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "0::") {
			path = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(l, "0::"))
		}
	}
	for _, t := range a.targets {
		if t.registration.Name != msg.request.Target || t.registration.PeerUID != msg.peer.Uid {
			continue
		}
		f, e := safeOpen(path)
		if e != nil {
			return nil, e
		}
		id, e := statFile(f)
		f.Close()
		if e == nil && id.Dev == t.r.id.Dev && id.Ino == t.r.id.Ino {
			return t, nil
		}
	}
	return nil, errors.New("peer cgroup/UID/target mismatch")
}
func (a *kernelAgent) reserve(t *kernelTarget, now time.Time) error {
	s := &t.machine.Status
	s.Phase = "RESERVED"
	s.Reason = "single_write_reserved"
	s.Attempted = true
	before := t.sample
	s.Before = &before
	s.Started = now
	s.HoldFile = true
	a.journal.Budget = append(a.journal.Budget, kernelBudgetEntry{now, s.Request})
	if err := a.save(); err != nil {
		s.Phase = "UNKNOWN"
		s.Reason = "journal_commit_failed_no_write"
		return err
	}
	if err := a.log(t, true); err != nil {
		return err
	}
	cg, err := safeOpen(t.registration.Cgroup)
	if err != nil {
		return err
	}
	defer cg.Close()
	id, err := statFile(cg)
	if err != nil || id.Dev != t.r.id.Dev || id.Ino != t.r.id.Ino {
		return errors.New("target changed before spawn")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "internal-kernel-write", strconv.FormatInt(s.Request, 10), strconv.FormatUint(id.Dev, 10), strconv.FormatUint(id.Ino, 10))
	// FD4 retains the global flock even if the parent dies in a blocked syscall.
	cmd.ExtraFiles = []*os.File{cg, a.lock}
	a.childOutput.Reset()
	cmd.Stdout = &a.childOutput
	cmd.Stderr = &a.childOutput
	if err = cmd.Start(); err != nil {
		return err
	}
	a.busy = t
	a.child = cmd
	a.done = make(chan error, 1)
	s.Phase = "INFLIGHT"
	s.Reason = "kernel_write_inflight"
	go func() { a.done <- cmd.Wait() }()
	return a.save()
}
func (a *kernelAgent) tick(now time.Time) error {
	for _, t := range a.targets {
		old := t.sample
		t.sample = a.read(t)
		if kernelSignalRegressed(t.sample.Signal, old.Signal) {
			t.sample.Error = "kernel_signal_clock_or_version_regression"
		}
		m := &t.machine
		s := &m.Status
		s.Time = now
		t.external.check(&t.sample.Metrics)
		reason := kernelGuard(t.c, t.sample, a.boot, now)
		risk, healthy, d := kernelRisk(t.c, t.sample, old)
		heartbeatOK := freshKernel(t.heartbeatTime, now, 3*time.Second) && t.heartbeat.Action == "enforce" && !t.heartbeat.Paused && t.c.Action == "enforce"
		if s.Attempted {
			if a.busy == t {
				if now.Sub(s.Started) > time.Duration(t.c.KernelReclaim.CallWatchdogMS)*time.Millisecond && !s.Watchdog {
					s.Watchdog = true
					s.Reason = "watchdog_expired_inflight_unknown"
					_ = a.child.Process.Kill()
					if err := a.save(); err != nil {
						return err
					}
				}
				continue
			}
			if s.Phase == "UNKNOWN" {
				continue
			}
			if !s.Finished.IsZero() {
				for _, sec := range t.c.KernelReclaim.RecheckSeconds {
					found := false
					for _, o := range s.Observations {
						if o.Seconds == sec {
							found = true
						}
					}
					if !found && now.Sub(s.Finished) >= time.Duration(sec)*time.Second {
						s.Observations = append(s.Observations, KernelObservation{Seconds: sec, Time: now, Current: t.sample.Metrics.Current, OrdinaryFile: max64(0, t.sample.Metrics.File-t.sample.Metrics.Shmem), Anon: t.sample.Metrics.Anon, Shmem: t.sample.Metrics.Shmem, Slab: t.sample.Metrics.Slab, Signal: t.sample.Signal, Error: reason, Metrics: t.sample.Metrics})
						if err := a.log(t, true); err != nil {
							return err
						}
						if err := a.save(); err != nil {
							return err
						}
					}
				}
			}
			s.HoldFile = s.Finished.IsZero() || now.Sub(s.Finished) < 30*time.Second
			if healthy && reason == "" {
				if m.healthy.IsZero() {
					m.healthy = now
				}
			} else {
				m.healthy = time.Time{}
			}
			if !m.healthy.IsZero() && now.Sub(m.healthy) >= time.Duration(t.c.KernelReclaim.RearmHealthySeconds)*time.Second && now.Sub(s.Started) >= time.Duration(t.c.KernelReclaim.MinEpisodeIntervalSeconds)*time.Second {
				id := s.TargetIdentity
				*s = KernelStatus{Phase: "ARMED", Reason: "rearmed_after_sustained_recovery", Time: now, TargetIdentity: id}
				delete(a.journal.Targets, kernelKey(a.boot, t.r))
				m.confirmations = 0
				if err := a.save(); err != nil {
					return err
				}
			}
			continue
		}
		if reason != "" || !heartbeatOK || now.Sub(a.started) < time.Duration(t.c.WarmupSeconds)*time.Second {
			m.confirmations = 0
			s.HoldFile = false
			s.Phase = "UNAVAILABLE"
			s.Reason = reason
			if reason == "" {
				s.Reason = "observe_paused_or_waiting_fresh_heartbeat"
			}
			continue
		}
		if !risk {
			m.confirmations = 0
			s.HoldFile = false
			s.Phase = "ARMED"
			s.Reason = "below_preemptive_trigger"
			continue
		}
		m.confirmations++
		if m.confirmations < t.c.KernelReclaim.ConfirmSamples {
			s.Phase = "CONFIRMING"
			s.Reason = "confirming_pressure"
			continue
		}
		if s.Phase != "PREPARING" {
			id, e := newRunID()
			if e != nil {
				return e
			}
			s.Episode = id
			s.Phase = "PREPARING"
			t.prepared = now
		}
		s.HoldFile = true
		s.Reason = "waiting_file_worker_ack"
		s.Deficit = d
		s.Request, s.EstimatedFile = kernelRequest(t.c, t.sample, d, a.remaining(now))
		if s.Request == 0 {
			s.Reason = "insufficient_file_or_node_budget"
			continue
		}
	}
	if a.busy == nil {
		var chosen *kernelTarget
		for _, t := range a.targets {
			s := t.machine.Status
			if s.Phase != "PREPARING" || s.Request == 0 || t.heartbeat.AckEpisode != s.Episode || !t.heartbeat.FileIdle {
				continue
			}
			if chosen == nil || t.registration.Priority < chosen.registration.Priority {
				chosen = t
			}
		}
		if chosen != nil {
			// Fresh authorization after waiting: leaf identity and all gates rechecked.
			if err := kernelLeaf(chosen.registration.Cgroup); err != nil {
				return err
			}
			old := chosen.sample
			clone := *chosen.r
			chosen.sample = kernelRead(chosen.c, &clone, a.boot)
			// Spot-read current values without manufacturing new rate baselines.
			chosen.sample.Metrics.Time = old.Metrics.Time
			chosen.sample.Metrics.IOBytesPerSecond = old.Metrics.IOBytesPerSecond
			chosen.sample.Metrics.CPUPercent = old.Metrics.CPUPercent
			chosen.sample.Metrics.FileRefaultsPerSecond = old.Metrics.FileRefaultsPerSecond
			chosen.sample.Metrics.FileRefaultRateValid = old.Metrics.FileRefaultRateValid
			for _, key := range []string{"self_cpu_percent", "io_bytes_per_second", "file_refaults_per_second"} {
				chosen.sample.Metrics.Available[key] = old.Metrics.Available[key]
			}
			if reason := kernelGuard(chosen.c, chosen.sample, a.boot, time.Now()); reason != "" {
				chosen.machine.Status.Reason = reason
				return nil
			}
			risk, _, d := kernelRisk(chosen.c, chosen.sample, old)
			if !risk {
				chosen.machine.Status = KernelStatus{Phase: "ARMED", Reason: "pressure_cleared_before_write", Time: now, TargetIdentity: kernelKey(a.boot, chosen.r)}
				return nil
			}
			chosen.machine.Status.Request, chosen.machine.Status.EstimatedFile = kernelRequest(chosen.c, chosen.sample, d, a.remaining(now))
			if chosen.machine.Status.Request == 0 {
				return nil
			}
			return a.reserve(chosen, now)
		}
	}
	for _, t := range a.targets {
		if err := a.log(t, false); err != nil {
			return err
		}
	}
	return nil
}
func kernelPeer(conn *net.UnixConn) (*syscall.Ucred, error) {
	raw, e := conn.SyscallConn()
	if e != nil {
		return nil, e
	}
	var cred *syscall.Ucred
	var err error
	e = raw.Control(func(fd uintptr) {
		cred, err = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if e != nil {
		return nil, e
	}
	return cred, err
}
func serveKernelConn(c *net.UnixConn, messages chan<- kernelMessage, ctx context.Context) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	cred, e := kernelPeer(c)
	if e != nil {
		return
	}
	b, e := bufio.NewReaderSize(io.LimitReader(c, 4097), 4097).ReadBytes('\n')
	if e != nil || len(b) > 4096 {
		return
	}
	body := bytes.TrimSpace(b)
	if uniqueJSONObject(body) != nil {
		return
	}
	var h kernelHeartbeat
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&h) != nil || len(h.Target) > 128 || len(h.AckEpisode) > 64 {
		return
	}
	msg := kernelMessage{h, cred, make(chan KernelStatus, 1)}
	select {
	case messages <- msg:
	case <-ctx.Done():
		return
	default:
		return
	}
	select {
	case s := <-msg.reply:
		_ = json.NewEncoder(c).Encode(s)
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
	}
}
func kernelAgentCommand(args []string) error {
	fs := flag.NewFlagSet("reclaim-agent", flag.ContinueOnError)
	config := fs.String("config", "", "受限节点执行器配置")
	duration := fs.Duration("duration", 0, "运行时长，0常驻")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *config == "" || *duration < 0 {
		return errors.New("reclaim-agent --config agent.json [--duration 5m]")
	}
	var ac kernelAgentConfig
	if err := trustedKernelJSON(*config, &ac); err != nil {
		return err
	}
	a, err := newKernelAgent(ac)
	if err != nil {
		return err
	}
	defer a.lock.Close()
	defer a.logs.Close()
	socketDir, e := safeOpen(filepath.Dir(ac.Socket))
	if e != nil {
		return e
	}
	id, e := statFile(socketDir)
	socketDir.Close()
	if e != nil || id.UID != uint32(os.Geteuid()) || id.Mode&0022 != 0 {
		return errors.New("socket parent must be agent-owned and not writable by others")
	}
	// Never unlink an occupied/stale socket automatically; operator verifies it.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: ac.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	mode := os.FileMode(0600)
	if ac.SocketGID != nil {
		if err = os.Chown(ac.Socket, -1, *ac.SocketGID); err != nil {
			return err
		}
		mode = 0660
	}
	if err = os.Chmod(ac.Socket, mode); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}
	messages := make(chan kernelMessage, 8)
	slots := make(chan struct{}, 8)
	go func() {
		for {
			listener.SetDeadline(time.Now().Add(time.Second))
			conn, e := listener.AcceptUnix()
			if e != nil {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			select {
			case slots <- struct{}{}:
				go func() { defer func() { <-slots }(); serveKernelConn(conn, messages, ctx) }()
			default:
				conn.Close()
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	stopping := false
	ctxDone := ctx.Done()
	for {
		if stopping && a.busy == nil {
			return nil
		}
		select {
		case <-ctxDone:
			stopping = true
			ctxDone = nil
			listener.Close()
			if a.busy != nil {
				_ = a.child.Process.Kill()
			}
		case msg := <-messages:
			t, e := a.authenticated(msg)
			if e != nil {
				msg.reply <- KernelStatus{Phase: "REJECTED", Reason: "peer_identity_rejected", Time: time.Now(), HoldFile: true}
				continue
			}
			if msg.request.Action != "observe" && msg.request.Action != "enforce" {
				msg.reply <- KernelStatus{Phase: "REJECTED", Reason: "invalid_action", HoldFile: true}
				continue
			}
			t.heartbeat = msg.request
			t.heartbeatTime = time.Now()
			msg.reply <- t.machine.Status
		case err := <-a.done:
			t := a.busy
			s := &t.machine.Status
			s.Finished = time.Now()
			s.Phase = "WAIT_RECOVERY"
			s.Reason = "single_attempt_finished_no_retry"
			s.Return = "accepted_request_not_measured_bytes"
			if err != nil {
				s.Return = err.Error() + ": " + strings.TrimSpace(a.childOutput.String())
				s.Reason = "write_failed_or_partial_no_retry"
			}
			if s.Watchdog {
				s.Return = "unknown_after_watchdog"
				s.Phase = "UNKNOWN"
			}
			a.busy = nil
			a.child = nil
			a.done = nil
			if err = a.save(); err != nil {
				return err
			}
			if err = a.log(t, true); err != nil {
				return err
			}
		case now := <-ticker.C:
			if !stopping {
				if err := a.tick(now); err != nil {
					return err
				}
			}
			if err := a.logs.Maintain(); err != nil {
				return err
			}
		}
	}
}

// Internal, not a bypass: FD3 must already be a real non-root leaf cgroup2.
// There is exactly one syscall.Write, including on EINTR/EAGAIN/short write.
func kernelWriteCommand(args []string) error {
	if len(args) != 3 {
		return errors.New("internal kernel helper requires inherited validated descriptors")
	}
	amount, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil || amount < 64*MiB || amount > 1<<30 || amount%int64(os.Getpagesize()) != 0 {
		return errors.New("invalid kernel request")
	}
	dev, e := strconv.ParseUint(args[1], 10, 64)
	if e != nil {
		return e
	}
	ino, e := strconv.ParseUint(args[2], 10, 64)
	if e != nil {
		return e
	}
	cg := os.NewFile(3, "cgroup")
	defer cg.Close()
	lock := os.NewFile(4, "node-lock")
	defer lock.Close()
	id, e := statFile(cg)
	var fs syscall.Statfs_t
	if e != nil || id.Dev != dev || id.Ino != ino || syscall.Fstatfs(3, &fs) != nil || uint64(fs.Type) != 0x63677270 {
		return errors.New("invalid cgroup descriptor")
	}
	// Root cgroups lack memory.current; the fixed open verifies non-root scope.
	fd, e := syscall.Openat(3, "memory.current", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	syscall.Close(fd)
	fd, e = syscall.Openat(3, "memory.reclaim", syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	body := []byte(strconv.FormatInt(amount, 10))
	return kernelNativeWrite(fd, body)
}
