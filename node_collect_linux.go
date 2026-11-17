//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type kubeMemory struct {
	Time      time.Time `json:"time"`
	Available *int64    `json:"availableBytes"`
	Working   *int64    `json:"workingSetBytes"`
}
type kubeSummary struct {
	Node struct {
		Name   string     `json:"nodeName"`
		Memory kubeMemory `json:"memory"`
		System []struct {
			Name   string     `json:"name"`
			Memory kubeMemory `json:"memory"`
		} `json:"systemContainers"`
	} `json:"node"`
}
type kubeNode struct {
	Metadata struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Info struct {
			BootID  string `json:"bootID"`
			Version string `json:"kubeletVersion"`
			OS      string `json:"operatingSystem"`
		} `json:"nodeInfo"`
		Conditions []struct {
			Type   string    `json:"type"`
			Status string    `json:"status"`
			Time   time.Time `json:"lastHeartbeatTime"`
		} `json:"conditions"`
	} `json:"status"`
}
type kubeConfig struct {
	Hard    map[string]string `json:"evictionHard"`
	Soft    map[string]string `json:"evictionSoft"`
	Grace   map[string]string `json:"evictionSoftGracePeriod"`
	Minimum map[string]string `json:"evictionMinimumReclaim"`
	Enforce []string          `json:"enforceNodeAllocatable"`
	Cgroups *bool             `json:"cgroupsPerQOS"`
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei|k|K|M|G|T|P|E|m|u|n)?$`)

func memoryQuantity(s string, capacity int64) (int64, error) {
	if len(s) > 96 {
		return 0, errors.New("quantity too long")
	}
	n := new(big.Rat)
	if strings.HasSuffix(s, "%") {
		if capacity <= 0 {
			return 0, errors.New("capacity missing")
		}
		if _, ok := n.SetString(strings.TrimSuffix(s, "%")); !ok || n.Sign() < 0 || n.Cmp(big.NewRat(100, 1)) > 0 {
			return 0, errors.New("percentage invalid")
		}
		n.Mul(n, big.NewRat(capacity, 100))
	} else {
		m := quantityPattern.FindStringSubmatch(s)
		if m == nil {
			return 0, errors.New("unsupported memory quantity")
		}
		n.SetString(m[1])
		suffix := m[2]
		if strings.HasSuffix(suffix, "i") {
			n.Mul(n, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(1024), big.NewInt(int64(strings.Index("KMGTPE", suffix[:1])+1)), nil)))
		} else if strings.Contains("kKMGTPE", suffix) && suffix != "" {
			index := strings.Index("KMGTPE", strings.ToUpper(suffix))
			n.Mul(n, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(1000), big.NewInt(int64(index+1)), nil)))
		} else {
			switch suffix {
			case "m":
				n.Quo(n, big.NewRat(1000, 1))
			case "u":
				n.Quo(n, big.NewRat(1000000, 1))
			case "n":
				n.Quo(n, big.NewRat(1000000000, 1))
			}
		}
	}
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n.Num(), n.Denom(), r)
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Sign() < 0 {
		return 0, errors.New("quantity overflow")
	}
	return q.Int64(), nil
}
func kubeCapacity(m kubeMemory, now time.Time) (int64, error) {
	if m.Available == nil || m.Working == nil || *m.Available < 0 || *m.Working < 0 || *m.Available > 1<<62-*m.Working || !freshKernel(m.Time, now, 15*time.Second) {
		return 0, errors.New("memory sample missing/stale/overflow")
	}
	return *m.Available + *m.Working, nil
}

// This adapter uses the verified Summary/configz shape, not release defaults.
// Unknown kubelet versions fail closed until their eviction semantics are reviewed.
func collectNodeEnvelope(nodeBody, summaryBody, configBody []byte, source, boot string, now time.Time) (KernelSignal, NodeSignal, error) {
	var out KernelSignal
	var legacy NodeSignal
	fail := func(s string) (KernelSignal, NodeSignal, error) { return out, legacy, errors.New(s) }
	var n kubeNode
	var summary kubeSummary
	var config struct {
		Config kubeConfig `json:"kubeletconfig"`
	}
	for _, b := range [][]byte{nodeBody, summaryBody, configBody} {
		if len(b) > 4<<20 || uniqueJSONObject(b) != nil {
			return fail("kubelet JSON invalid/oversized")
		}
	}
	if json.Unmarshal(nodeBody, &n) != nil || json.Unmarshal(summaryBody, &summary) != nil || json.Unmarshal(configBody, &config) != nil {
		return fail("kubelet JSON decode failed")
	}
	match := regexp.MustCompile(`^v1\.(2[4-9]|3[01])\.[0-9]+(?:[-+].*)?$`).MatchString(n.Status.Info.Version)
	if !match || n.Status.Info.OS != "linux" || n.Status.Info.BootID != boot || !identifier.MatchString(n.Metadata.UID) || n.Metadata.Name != summary.Node.Name || !identifier.MatchString(source) {
		return fail("node identity/version/boot not supported")
	}
	cfg := config.Config
	if cfg.Hard == nil || cfg.Enforce == nil || cfg.Cgroups == nil {
		return fail("effective config fields unavailable")
	}
	cap, err := kubeCapacity(summary.Node.Memory, now)
	if err != nil {
		return fail(err.Error())
	}
	threshold := func(m map[string]string, capacity int64) (int64, error) {
		if s, ok := m["memory.available"]; ok {
			return memoryQuantity(s, capacity)
		}
		return 0, nil
	}
	hard, err := threshold(cfg.Hard, cap)
	if err != nil {
		return fail("hard threshold invalid")
	}
	soft, err := threshold(cfg.Soft, cap)
	if err != nil {
		return fail("soft threshold invalid")
	}
	grace := int64(0)
	if _, ok := cfg.Soft["memory.available"]; ok {
		d, e := time.ParseDuration(cfg.Grace["memory.available"])
		if e != nil || d < 0 {
			return fail("soft grace missing/invalid")
		}
		grace = int64((d + time.Second - 1) / time.Second)
	}
	// Including minimum reclaim conservatively avoids re-entering an unresolved
	// eviction episode. Keep the original effective config hash for audit.
	minimum := int64(0)
	if s, ok := cfg.Minimum["memory.available"]; ok {
		minimum, err = memoryQuantity(s, cap)
		if err != nil {
			return fail("minimum reclaim invalid")
		}
	}
	add := func(a, b int64) (int64, error) {
		if a > 1<<62-b {
			return 0, errors.New("threshold overflow")
		}
		if a == 0 {
			return 0, nil
		}
		return a + b, nil
	}
	hard, err = add(hard, minimum)
	if err != nil {
		return fail(err.Error())
	}
	soft, err = add(soft, minimum)
	if err != nil {
		return fail(err.Error())
	}
	pressure := false
	conditions := 0
	for _, v := range n.Status.Conditions {
		if v.Type == "MemoryPressure" {
			conditions++
			if (v.Status != "True" && v.Status != "False") || !freshKernel(v.Time, now, 5*time.Minute) {
				return fail("MemoryPressure condition unknown/stale")
			}
			pressure = v.Status == "True"
		}
	}
	if conditions != 1 {
		return fail("MemoryPressure condition unavailable")
	}
	required := false
	for _, v := range cfg.Enforce {
		if v == "pods" && hard > 0 {
			required = true
		}
	}
	out = KernelSignal{Schema: 2, SourceID: source, NodeID: n.Metadata.UID, BootID: boot, Time: summary.Node.Memory.Time, AllocatableRequired: &required, MemoryPressure: &pressure, KubeletVersion: n.Status.Info.Version, CollectedAt: now}
	// Hash the complete effective config, not only fields this adapter uses.
	var canonical any
	_ = json.Unmarshal(configBody, &canonical)
	body, _ := json.Marshal(canonical)
	hash := sha256.Sum256(body)
	out.ConfigVersion = hex.EncodeToString(hash[:])
	out.Constraints = []KernelConstraint{{Kind: "node", Time: summary.Node.Memory.Time, Available: summary.Node.Memory.Available, Hard: &hard, Soft: &soft}}
	if required {
		found := 0
		for _, v := range summary.Node.System {
			if v.Name == "pods" {
				found++
				ac, e := kubeCapacity(v.Memory, now)
				if e != nil {
					return fail("allocatable Summary unavailable")
				}
				h, e := threshold(cfg.Hard, ac)
				if e != nil {
					return fail("allocatable threshold invalid")
				}
				mn := int64(0)
				if s, ok := cfg.Minimum["memory.available"]; ok {
					mn, e = memoryQuantity(s, ac)
					if e != nil {
						return fail("allocatable min reclaim invalid")
					}
				}
				h, e = add(h, mn)
				if e != nil {
					return fail(e.Error())
				}
				zero := int64(0)
				out.Constraints = append(out.Constraints, KernelConstraint{Kind: "allocatable", Time: v.Memory.Time, Available: v.Memory.Available, Hard: &h, Soft: &zero})
			}
		}
		if found != 1 || !*cfg.Cgroups {
			return fail("allocatable stats/cgroup policy unavailable")
		}
	}
	// Legacy file backend cannot represent two constraints. Use the lowest
	// headroom in bytes, preserving that constraint's original timestamp.
	chosen := out.Constraints[0]
	for _, v := range out.Constraints[1:] {
		if *v.Available-max64(*v.Hard, *v.Soft) < *chosen.Available-max64(*chosen.Hard, *chosen.Soft) {
			chosen = v
		}
	}
	legacy = NodeSignal{SchemaVersion: 1, SourceID: source, NodeID: n.Metadata.UID, Time: chosen.Time, AvailableBytes: chosen.Available, HardBytes: chosen.Hard, SoftBytes: chosen.Soft, SoftGraceSeconds: &grace, MemoryPressure: &pressure}
	return out, legacy, nil
}

type limitedCommandOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedCommandOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("command output limit")
	}
	return b.Buffer.Write(p)
}
func kubeGet(ctx context.Context, node string, kind string) ([]byte, error) {
	args := []string{"--request-timeout=3s", "--insecure-skip-tls-verify=false"}
	if kind == "node" {
		args = append(args, "get", "node", node, "-o", "json")
	} else {
		args = append(args, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/"+kind)
	}
	call, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, "kubectl", args...)
	out := &limitedCommandOutput{limit: 4 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return nil, errors.New("authenticated kubectl read failed; details suppressed")
	}
	return out.Bytes(), nil
}

type diskCounter struct{ Reads, Writes, ReadMS, WriteMS, Weighted, ReadSectors, WriteSectors uint64 }

func parseDisks(body string) map[string]diskCounter {
	out := map[string]diskCounter{}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		n := make([]uint64, 11)
		valid := true
		for i := range n {
			var e error
			n[i], e = strconv.ParseUint(f[i+3], 10, 64)
			if e != nil {
				valid = false
			}
		}
		if valid {
			out[f[0]+":"+f[1]] = diskCounter{n[0], n[4], n[3], n[7], n[10], n[2], n[6]}
		}
	}
	return out
}
func deviceForPath(path, mountinfo string) (string, error) {
	best, dev, fs := "", "", ""
	for _, line := range strings.Split(mountinfo, "\n") {
		p := strings.Split(line, " - ")
		if len(p) != 2 {
			continue
		}
		a, b := strings.Fields(p[0]), strings.Fields(p[1])
		if len(a) < 5 || len(b) < 1 {
			continue
		}
		mount := unescapeMount(a[4])
		if beneath(path, mount) && len(mount) > len(best) {
			best, dev, fs = mount, a[2], b[0]
		}
	}
	if !regexp.MustCompile(`^[1-9][0-9]*:[0-9]+$`).MatchString(dev) || (fs != "xfs" && fs != "ext4" && fs != "ext3") {
		return "", errors.New("path device unknown: overlay/network/multi-device unsupported")
	}
	return dev, nil
}
func expandDevice(dev string, seen map[string]bool) error {
	if seen[dev] {
		return nil
	}
	if len(seen) >= 16 {
		return errors.New("too many block layers")
	}
	seen[dev] = true
	path, e := filepath.EvalSymlinks("/sys/dev/block/" + dev)
	if e != nil {
		return errors.New("block device identity unavailable")
	}
	if _, e := os.Stat(filepath.Join(path, "partition")); e == nil {
		b, e := os.ReadFile(filepath.Join(filepath.Dir(path), "dev"))
		if e != nil {
			return e
		}
		return expandDevice(strings.TrimSpace(string(b)), seen)
	}
	children, e := os.ReadDir(filepath.Join(path, "slaves"))
	if e != nil {
		return e
	}
	for _, ch := range children {
		b, e := os.ReadFile(filepath.Join(path, "slaves", ch.Name(), "dev"))
		if e != nil {
			return e
		}
		if e = expandDevice(strings.TrimSpace(string(b)), seen); e != nil {
			return e
		}
	}
	return nil
}
func diskInterval(previous, current map[string]diskCounter, devices []string, seconds float64) (float64, float64, error) {
	if seconds <= 0 || seconds > 5 {
		return 0, 0, errors.New("disk interval invalid")
	}
	await, queue := 0.0, 0.0
	for _, dev := range devices {
		p, ok := previous[dev]
		n, yes := current[dev]
		if !ok || !yes || n.Reads < p.Reads || n.Writes < p.Writes || n.ReadMS < p.ReadMS || n.WriteMS < p.WriteMS || n.Weighted < p.Weighted || n.ReadSectors < p.ReadSectors || n.WriteSectors < p.WriteSectors {
			return 0, 0, errors.New("disk sample missing/reset")
		}
		ops := float64(n.Reads-p.Reads) + float64(n.Writes-p.Writes)
		ms := float64(n.ReadMS-p.ReadMS) + float64(n.WriteMS-p.WriteMS)
		a := 0.0
		if ops > 0 {
			a = ms / ops
		} else if ms > 0 {
			return 0, 0, errors.New("incomplete disk interval")
		}
		q := float64(n.Weighted-p.Weighted) / (seconds * 1000)
		if a > await {
			await = a
		}
		if q > queue {
			queue = q
		}
	}
	return await, queue, nil
}
func nodeCollectCommand(args []string) error {
	fs := flag.NewFlagSet("node-collect", flag.ContinueOnError)
	node := fs.String("node", "", "本节点Kubernetes名称")
	source := fs.String("source-id", "", "信号来源ID")
	out := fs.String("output", "", "schema2输出")
	legacy := fs.String("legacy-output", "", "可选schema1输出，给文件后端")
	businessFile := fs.String("business-signal", "", "已有可信业务采样文件")
	businessSource := fs.String("business-source-id", "", "业务来源ID")
	once := fs.Bool("once", false, "采样一次")
	interval := fs.Duration("interval", 2*time.Second, "采样间隔1..5s")
	var paths pathsFlag
	fs.Var(&paths, "path", "宿主机实际源/输出目录，可重复；无映射则磁盘指标为unknown")
	if e := fs.Parse(args); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if fs.NArg() != 0 || !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`).MatchString(*node) || !identifier.MatchString(*source) || *out == "" || !filepath.IsAbs(*out) || (*legacy != "" && (!filepath.IsAbs(*legacy) || *legacy == *out)) || *interval < time.Second || *interval > 5*time.Second || len(paths) > 16 {
		return errors.New("node-collect参数无效")
	}
	for _, p := range []string{*out, *legacy} {
		if p != "" {
			if filepath.Clean(p) != p {
				return errors.New("输出须为规范绝对路径")
			}
			f, e := taskDirectory(filepath.Dir(p))
			if e != nil {
				return e
			}
			f.Close()
		}
	}
	if (*businessFile != "" && !filepath.IsAbs(*businessFile)) || (*businessFile != "" && !identifier.MatchString(*businessSource)) {
		return errors.New("业务文件和来源绑定无效")
	}
	boot, e := readBootID()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	var previous map[string]diskCounter
	var diskAt time.Time
	var previousDevices []string
	for {
		err := func() error {
			nb, e := kubeGet(ctx, *node, "node")
			if e != nil {
				return e
			}
			sb, e := kubeGet(ctx, *node, "stats/summary")
			if e != nil {
				return e
			}
			cb, e := kubeGet(ctx, *node, "configz")
			if e != nil {
				return e
			}
			now := time.Now()
			n, l, e := collectNodeEnvelope(nb, sb, cb, *source, boot, now)
			if e != nil {
				return e
			}
			// Read actual host mappings every time; unknown disks remain nil and
			// cannot qualify a no-PSI kernel request. No write calibration occurs.
			mountBytes, _ := os.ReadFile("/proc/self/mountinfo")
			devices := map[string]bool{}
			diskValid := len(paths) > 0
			for _, p := range paths {
				f, err := safeOpen(p)
				if err != nil {
					diskValid = false
					break
				}
				f.Close()
				d, err := deviceForPath(p, string(mountBytes))
				if err != nil || expandDevice(d, devices) != nil {
					diskValid = false
					break
				}
			}
			var list []string
			for d := range devices {
				list = append(list, d)
			}
			sort.Strings(list)
			body, e := os.ReadFile("/proc/diskstats")
			current := parseDisks(string(body))
			if e != nil {
				diskValid = false
			}
			at := time.Now()
			if diskValid && previous != nil && sameDevices(previousDevices, list) {
				a, q, err := diskInterval(previous, current, list, at.Sub(diskAt).Seconds())
				if err == nil {
					n.DiskTime = at
					n.DiskAwaitMS = &a
					n.DiskQueue = &q
					n.DiskDevices = list
				}
			}
			previous = current
			previousDevices = append([]string{}, list...)
			diskAt = at
			if *businessFile != "" {
				var b struct {
					Time    time.Time `json:"time"`
					Source  string    `json:"source_id"`
					Samples *int      `json:"samples"`
					Errors  *float64  `json:"error_rate"`
				}
				if kernelTrustedFile(*businessFile) == nil && readRegularJSON(*businessFile, 16384, &b, false) == nil && b.Source == *businessSource && freshKernel(b.Time, now, 5*time.Second) && b.Samples != nil && *b.Samples >= 0 && b.Errors != nil && finiteNonnegative(*b.Errors) && *b.Errors <= 1 {
					n.BusinessTime = b.Time
					n.BusinessSourceID = b.Source
					n.BusinessSamples = b.Samples
					n.BusinessErrorRate = b.Errors
				}
			}
			if e = atomicJSON(*out, n); e != nil {
				return e
			}
			if *legacy != "" {
				if e = atomicJSON(*legacy, l); e != nil {
					return e
				}
			}
			return output(map[string]any{"event": "node_signal_written", "time": time.Now(), "source_time": n.Time, "node_id": n.NodeID, "disk_available": n.DiskAwaitMS != nil, "business_available": n.BusinessSamples != nil})
		}()
		if err != nil {
			fmt.Fprintln(os.Stderr, "node-collect: signal not renewed:", err)
			if *once {
				return err
			}
		}
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}
