//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This is bounded observation, not a complete file inventory or proof of heat,
// clean pages, cgroup ownership, or causal reclaim benefit.
type AuditGroup struct {
	Root               string            `json:"root"`
	Kind               string            `json:"file_type"`
	OpenOperations     uint64            `json:"open_operations"`
	MetadataOperations uint64            `json:"discovery_metadata_operations"`
	MetadataBytes      int64             `json:"discovery_logical_bytes_cumulative"`
	OpenedLogicalBytes int64             `json:"opened_logical_bytes_cumulative"`
	Skipped            uint64            `json:"skipped_operations"`
	SkippedKnownBytes  int64             `json:"skipped_known_logical_bytes_cumulative"`
	SkippedUnknownSize uint64            `json:"skipped_unknown_size_operations"`
	SkipReasons        map[string]uint64 `json:"skip_reasons"`
	InitialBytes       int64             `json:"initial_query_bytes_cumulative"`
	UniqueBytes        int64             `json:"window_new_query_bytes_cumulative"`
	RepeatBytes        int64             `json:"window_repeat_query_bytes_cumulative"`
	RecheckBytes       int64             `json:"recheck_query_bytes_cumulative"`
	AfterBytes         int64             `json:"immediate_after_query_bytes_cumulative"`
	ResidentBytes      int64             `json:"observed_resident_bytes_cumulative"`
	ResidentPages      uint64            `json:"observed_resident_pages_cumulative"`
	EligibleBytes      int64             `json:"policy_eligible_resident_bytes_cumulative"`
	EligiblePages      uint64            `json:"policy_eligible_resident_pages_cumulative"`
	AdviceBytes        int64             `json:"advice_range_bytes"`
	ReductionBytes     int64             `json:"immediate_region_decrease_bytes"`
	ReappearedBytes    int64             `json:"delayed_region_increase_bytes"`
}
type RootScope struct {
	Path       string `json:"path"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	UID        uint32 `json:"uid"`
	Mode       uint32 `json:"mode"`
	MountPath  string `json:"mount_path,omitempty"`
	MountRoot  string `json:"mount_root,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
}
type CandidateEvent struct {
	Sequence           uint64    `json:"sequence"`
	CandidateID        uint64    `json:"candidate_id"`
	Time               time.Time `json:"time"`
	Outcome            string    `json:"outcome"`
	Path               string    `json:"path"`
	Device             uint64    `json:"device"`
	Inode              uint64    `json:"inode"`
	Offset             int64     `json:"offset"`
	Length             int64     `json:"length"`
	Attempts           int       `json:"attempts"`
	Recheck            int       `json:"recheck_stage"`
	ResidentBytes      *int64    `json:"resident_bytes"`
	SinceAdviceSeconds *float64  `json:"since_advice_seconds"`
}
type AuditSnapshot struct {
	RequestedRoots    []RequestedRoot   `json:"requested_roots"`
	Groups            []AuditGroup      `json:"groups"`
	Roots             []RootScope       `json:"roots"`
	PGDATAHint        string            `json:"pgdata_environment_hint,omitempty"`
	HistoryEntries    int               `json:"scan_history_entries"`
	HistoryEvictions  uint64            `json:"scan_history_evictions"`
	MergedGroups      uint64            `json:"group_overflow_operations"`
	RootOverflow      uint64            `json:"root_metadata_overflow"`
	CandidateOutcomes map[string]uint64 `json:"candidate_outcomes"`
	Events            []CandidateEvent  `json:"recent_candidate_events"`
	EventsOverwritten uint64            `json:"candidate_events_overwritten"`
	Accounting        string            `json:"accounting_scope"`
}
type RequestedRoot struct {
	Path   string `json:"path"`
	Result string `json:"result"`
}

func (e *engine) auditRootRequests() {
	requested := append(append([]string{}, e.c.Paths...), e.c.AutoRoots...)
	if e.c.Mode == "auto" && e.audit.snapshot.PGDATAHint != "" {
		requested = append(requested, e.audit.snapshot.PGDATAHint)
	}
	seen := map[string]bool{}
	for _, path := range requested {
		if seen[path] {
			continue
		}
		seen[path] = true
		if len(e.audit.snapshot.RequestedRoots) >= e.c.MaxAuditGroups {
			e.audit.snapshot.RootOverflow++
			continue
		}
		result := "not_in_normalized_scope"
		if e.c.excluded(path) {
			result = "excluded"
		} else {
			for _, p := range e.roots {
				if beneath(path, p) {
					result = "inside_detected_scope"
					break
				}
			}
			if result != "inside_detected_scope" {
				f, err := safeOpen(path)
				if err != nil {
					result = openSkipReason(err)
				} else {
					if !supportedFS(f) {
						result = "unsupported_filesystem"
					}
					f.Close()
				}
			}
		}
		e.audit.snapshot.RequestedRoots = append(e.audit.snapshot.RequestedRoots, RequestedRoot{Path: path, Result: result})
	}
}

type queryIdentity struct {
	Dev, Ino                    uint64
	Mtime, Size, Offset, Length int64
}
type scanAudit struct {
	groups                map[string]*AuditGroup
	roots                 map[string]RootScope
	mounts                []mount
	seen                  map[queryIdentity]bool
	history               []queryIdentity
	historyCursor         int
	snapshot              AuditSnapshot
	events                []CandidateEvent
	eventCursor           int
	sequence, candidateID uint64
}

var relationName = regexp.MustCompile(`^[0-9]+(_(fsm|vm|init))?(\.[0-9]+)?$`)

func fileKind(path string) string {
	b := filepath.Base(path)
	switch {
	case relationName.MatchString(b):
		return "pg_relation_name"
	case strings.HasSuffix(b, ".log") || strings.HasSuffix(b, ".csv") || strings.HasSuffix(b, ".jsonl"):
		return "log"
	case strings.HasSuffix(b, ".tar") || strings.HasSuffix(b, ".gz") || strings.HasSuffix(b, ".zst") || strings.HasSuffix(b, ".zip"):
		return "archive"
	case strings.Contains(b, ".so") || strings.HasSuffix(b, ".a"):
		return "library_name"
	default:
		return "other"
	}
}
func newScanAudit() *scanAudit {
	p := os.Getenv("PGDATA")
	if !filepath.IsAbs(p) || len(p) > 4096 || filepath.Clean(p) != p {
		p = ""
	}
	return &scanAudit{groups: map[string]*AuditGroup{}, roots: map[string]RootScope{}, mounts: mounts(), seen: map[queryIdentity]bool{}, snapshot: AuditSnapshot{PGDATAHint: p, CandidateOutcomes: map[string]uint64{}, Accounting: "window uniqueness is bounded and versioned; repeated operations are cumulative; resident/policy-eligible is not proven cold or clean; inode cache is shared, cgroup charge and global benefit are unknown"}}
}
func (e *engine) auditGroup(path string) *AuditGroup {
	root := "outside_detected_roots"
	for _, p := range e.roots {
		if beneath(path, p) && len(p) > len(rootMatch(root)) {
			root = p
		}
	}
	kind := fileKind(path)
	key := root + "\x00" + kind
	if g := e.audit.groups[key]; g != nil {
		return g
	}
	if len(e.audit.groups) >= e.c.MaxAuditGroups-1 {
		e.audit.snapshot.MergedGroups++
		root, kind, key = "overflow", "mixed", "overflow"
		if g := e.audit.groups[key]; g != nil {
			return g
		}
	}
	g := &AuditGroup{Root: root, Kind: kind, SkipReasons: map[string]uint64{}}
	e.audit.groups[key] = g
	return g
}
func rootMatch(s string) string {
	if s == "outside_detected_roots" {
		return ""
	}
	return s
}
func (e *engine) auditOpen(path string, id identity) {
	g := e.auditGroup(path)
	g.OpenOperations++
	if id.Mode&0170000 == 0100000 {
		g.OpenedLogicalBytes += max64(0, id.Size)
	}
	for _, p := range e.roots {
		if path == p {
			if _, ok := e.audit.roots[p]; !ok && len(e.audit.roots) >= e.c.MaxAuditGroups {
				e.audit.snapshot.RootOverflow++
				return
			}
			r := RootScope{Path: p, Device: id.Dev, Inode: id.Ino, UID: id.UID, Mode: id.Mode}
			for _, m := range e.audit.mounts {
				if (m.Path == "/" || beneath(p, m.Path)) && len(m.Path) > len(r.MountPath) {
					r.MountPath, r.MountRoot, r.Filesystem = m.Path, m.Root, m.FSType
				}
			}
			e.audit.roots[p] = r
			return
		}
	}
}
func (e *engine) auditMetadata(path string, id identity) {
	g := e.auditGroup(path)
	g.MetadataOperations++
	if id.Mode&0170000 == 0100000 {
		g.MetadataBytes += max64(0, id.Size)
	}
}
func (e *engine) auditSkip(path, reason string, id *identity) {
	g := e.auditGroup(path)
	g.Skipped++
	g.SkipReasons[reason]++
	if id == nil {
		g.SkippedUnknownSize++
	} else if id.Mode&0170000 == 0100000 {
		g.SkippedKnownBytes += max64(0, id.Size)
	}
}
func (e *engine) auditQuery(path string, id identity, off, length, res int64, stage string) {
	g := e.auditGroup(path)
	g.ResidentBytes += res
	g.ResidentPages += uint64(res / int64(os.Getpagesize()))
	switch stage {
	case "after":
		g.AfterBytes += length
	case "recheck":
		g.RecheckBytes += length
	default:
		g.InitialBytes += length
		k := queryIdentity{id.Dev, id.Ino, id.Mtime, id.Size, off, length}
		if e.audit.seen[k] {
			g.RepeatBytes += length
			return
		}
		g.UniqueBytes += length
		if len(e.audit.history) >= e.c.MaxScanHistory {
			delete(e.audit.seen, e.audit.history[e.audit.historyCursor])
			e.audit.history[e.audit.historyCursor] = k
			e.audit.historyCursor = (e.audit.historyCursor + 1) % len(e.audit.history)
			e.audit.snapshot.HistoryEvictions++
		} else {
			e.audit.history = append(e.audit.history, k)
		}
		e.audit.seen[k] = true
	}
}
func (e *engine) candidateEvent(r *region, outcome string, res int64, now time.Time) {
	e.audit.snapshot.CandidateOutcomes[outcome]++
	e.audit.sequence++
	var since *float64
	if !r.AdvisedAt.IsZero() {
		n := maxFloat(0, now.Sub(r.AdvisedAt).Seconds())
		since = &n
	}
	var observed *int64
	if res >= 0 {
		n := res
		observed = &n
	}
	v := CandidateEvent{Sequence: e.audit.sequence, CandidateID: r.CandidateID, Time: now, Outcome: outcome, Path: r.Path, Device: r.Key.Dev, Inode: r.Key.Ino, Offset: r.Key.Offset, Length: r.Length, Attempts: r.Attempts, Recheck: r.RecheckStage, ResidentBytes: observed, SinceAdviceSeconds: since}
	if len(e.audit.events) >= e.c.MaxCandidateEvents {
		e.audit.events[e.audit.eventCursor] = v
		e.audit.eventCursor = (e.audit.eventCursor + 1) % len(e.audit.events)
		e.audit.snapshot.EventsOverwritten++
	} else {
		e.audit.events = append(e.audit.events, v)
	}
}
func (e *engine) auditSnapshot() AuditSnapshot {
	s := e.audit.snapshot
	s.Groups = make([]AuditGroup, 0, len(e.audit.groups))
	s.Roots = make([]RootScope, 0, len(e.audit.roots))
	for _, g := range e.audit.groups {
		n := *g
		n.SkipReasons = map[string]uint64{}
		for k, v := range g.SkipReasons {
			n.SkipReasons[k] = v
		}
		s.Groups = append(s.Groups, n)
	}
	sort.Slice(s.Groups, func(i, j int) bool {
		if s.Groups[i].Root == s.Groups[j].Root {
			return s.Groups[i].Kind < s.Groups[j].Kind
		}
		return s.Groups[i].Root < s.Groups[j].Root
	})
	for _, r := range e.audit.roots {
		s.Roots = append(s.Roots, r)
	}
	sort.Slice(s.Roots, func(i, j int) bool { return s.Roots[i].Path < s.Roots[j].Path })
	s.HistoryEntries = len(e.audit.seen)
	s.CandidateOutcomes = map[string]uint64{}
	for k, v := range e.audit.snapshot.CandidateOutcomes {
		s.CandidateOutcomes[k] = v
	}
	s.Events = append([]CandidateEvent{}, e.audit.events...)
	sort.Slice(s.Events, func(i, j int) bool { return s.Events[i].Sequence < s.Events[j].Sequence })
	return s
}
