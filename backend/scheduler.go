package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Schedule is a recurring command that the server runs against an agent
// without a browser client connected. Useful for "ping 8.8.8.8 every minute,
// keep the last 10 results" so the latency-matrix view has data even when
// nobody's looking.
type Schedule struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	NodeName    string    `json:"node_name"` // denormalized for display when the node has been removed
	Command     string    `json:"command"`
	Target      string    `json:"target"`
	Options     string    `json:"options,omitempty"`
	IntervalSec int       `json:"interval_sec"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	LastResult  string    `json:"last_result,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"` // "ok" | "degraded" | "error" | "agent_offline" | "running"
	NextRunAt   time.Time `json:"next_run_at,omitempty"`

	// SchemaVersion is the one-shot migration marker (S10/F30). Absent (0)
	// means "written by a binary that HTML-escaped NodeName at storage time";
	// scheduleSchemaVersion means "already migrated, never unescape again".
	//
	// It is deliberately a **per-schedule** field: schedules.json is a
	// top-level JSON array, so a top-level marker object would make any older
	// binary fail to unmarshal the file, log "starting empty" and rewrite it —
	// losing every schedule. An unknown per-entry field is simply ignored by an
	// older binary, which keeps `git revert` safe for data.
	SchemaVersion int `json:"schema_version,omitempty"`

	// LastSummary is the canonical structured summary of the last *completed*
	// run (S2). It is written only on the `done` path and only when it fits in
	// scheduleMaxSummary; every other terminal path clears it, so a failed run
	// can never leave stale monitoring data behind.
	LastSummary json.RawMessage `json:"last_summary,omitempty"`

	// History is the rolling ring of finalized runs (S3), newest last. It is
	// appended under schedulesMu inside finalizeScheduleWithSummary — the one
	// funnel every finalize path goes through — so failures (agent_offline,
	// timeouts, not_allowed) show up as gaps in the sparkline instead of
	// silently disappearing. Capped at scheduleHistoryMax in memory;
	// schedulePersistHistory points are written to disk.
	History []historyPoint `json:"history,omitempty"`

	// Buffer for the run currently in flight. Populated by handleAgentOutput
	// when output type is "output". Cleared at the start of each run.
	// currentSummary holds the validated summary of the in-flight run between
	// its `summary` and `done` messages; it is cleared alongside currentBuf.
	currentBuf     strings.Builder
	currentSummary []byte
	currentMu      sync.Mutex

	// lifecycleMu serializes claim/registration/dispatch/terminal output and
	// operator deletion. Take it before any server mutex, never while holding
	// one, so a stale run cannot modify the next run's buffer or status.
	lifecycleMu sync.Mutex

	// runningCmdID / runStartedAt describe the run currently in flight. They
	// are unexported (never serialized) and are only ever read or written
	// while holding schedulesMu — the watchdog uses them to tell "this run is
	// stuck" from "this run already finished and a newer one started".
	runningCmdID string
	runStartedAt time.Time

	// runEpoch increments on every claimed run. The watchdog snapshots it
	// alongside the cmdID so the terminal write can be refused if a newer run
	// started in the meantime — the cmdID check alone leaves a window between
	// the re-check and the status write.
	runEpoch uint64
}

// historyPoint is one finalized run. Fields are exported so the persistence and
// API layers can marshal them; the two metrics are pointers so "this probe has
// no RTT" (http, dns, mtr) is distinguishable from "0 ms".
//
// RttMs is taken from the summary's top-level avg_ms and from nothing else:
// coercing http total_ms or dns query_time_ms into it would make one metric
// name mix wall-clock request time with ICMP latency.
type historyPoint struct {
	T       time.Time `json:"t"`
	Status  string    `json:"status"`
	RttMs   *float64  `json:"rtt_ms,omitempty"`
	LossPct *float64  `json:"loss_pct,omitempty"`
}

// scheduleFields is a flat, lock-free copy of a Schedule's exported fields. It
// exists so serialization (file + API) happens on a snapshot taken under
// schedulesMu instead of marshaling the live struct while the ticker mutates
// it. The json tags must stay identical to Schedule's — the persisted file
// shape is a compatibility surface.
//
// LastRunAt / NextRunAt are pointers *here only*: a zero time.Time ignores
// `omitempty` and serializes as "0001-01-01T00:00:00Z". Schedule keeps plain
// time.Time fields so an old file carrying that string still unmarshals; it is
// simply rewritten without it.
type scheduleFields struct {
	ID          string          `json:"id"`
	NodeID      string          `json:"node_id"`
	NodeName    string          `json:"node_name"`
	Command     string          `json:"command"`
	Target      string          `json:"target"`
	Options     string          `json:"options,omitempty"`
	IntervalSec int             `json:"interval_sec"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   time.Time       `json:"created_at"`
	LastRunAt   *time.Time      `json:"last_run_at,omitempty"`
	LastResult  string          `json:"last_result,omitempty"`
	LastStatus  string          `json:"last_status,omitempty"`
	LastSummary json.RawMessage `json:"last_summary,omitempty"`
	NextRunAt   *time.Time      `json:"next_run_at,omitempty"`
	// SchemaVersion must be copied here or the migration marker would be
	// silently dropped by the next save and every restart would re-migrate.
	SchemaVersion int `json:"schema_version,omitempty"`
}

// scheduleView is the full snapshot: every exported field plus the run history.
// It is what the file on disk and the create response carry.
type scheduleView struct {
	scheduleFields
	History []historyPoint `json:"history,omitempty"`
}

// scheduleListView is the same snapshot minus the history. GET /api/schedules
// is polled every 5 s by every open UI, so shipping up to 100 × 288 points on
// each poll would be wasteful; the sparkline fetches
// GET /api/schedules/<id>/history instead. A separate type (rather than a
// `json:"-"` tag on the shared one) keeps the field in the persisted file.
type scheduleListView struct {
	scheduleFields
}

// optionalTime returns nil for the zero time so `omitempty` can drop the field.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t
	return &v
}

// newScheduleFields copies the exported scalar fields of sc. Callers must hold
// schedulesMu (read or write).
func newScheduleFields(sc *Schedule) scheduleFields {
	return scheduleFields{
		ID:          sc.ID,
		NodeID:      sc.NodeID,
		NodeName:    sc.NodeName,
		Command:     sc.Command,
		Target:      sc.Target,
		Options:     sc.Options,
		IntervalSec: sc.IntervalSec,
		Enabled:     sc.Enabled,
		CreatedAt:   sc.CreatedAt,
		LastRunAt:   optionalTime(sc.LastRunAt),
		LastResult:  sc.LastResult,
		LastStatus:  sc.LastStatus,
		// RawMessage is a byte slice: copy it so the snapshot cannot alias a
		// buffer a later finalize replaces while this view is being marshalled.
		LastSummary:   append(json.RawMessage(nil), sc.LastSummary...),
		NextRunAt:     optionalTime(sc.NextRunAt),
		SchemaVersion: sc.SchemaVersion,
	}
}

// newScheduleView copies the exported fields of sc, history included. The
// history slice is copied for the same reason LastSummary is: the view is
// marshalled after schedulesMu has been released, while the ring keeps being
// appended to in place. Callers must hold schedulesMu (read or write).
func newScheduleView(sc *Schedule) scheduleView {
	return scheduleView{
		scheduleFields: newScheduleFields(sc),
		History:        append([]historyPoint(nil), sc.History...),
	}
}

// newScheduleListView copies everything but the history.
func newScheduleListView(sc *Schedule) scheduleListView {
	return scheduleListView{scheduleFields: newScheduleFields(sc)}
}

// scheduleViews snapshots every schedule under schedulesMu.RLock and returns
// value copies (history included) that are safe to marshal after the lock is
// released. This is the persistence view.
func (s *Server) scheduleViews() []scheduleView {
	s.schedulesMu.RLock()
	list := make([]scheduleView, 0, len(s.schedules))
	for _, sc := range s.schedules {
		list = append(list, newScheduleView(sc))
	}
	s.schedulesMu.RUnlock()
	return list
}

// scheduleListViews is scheduleViews without the history — the response shape
// of GET /api/schedules.
func (s *Server) scheduleListViews() []scheduleListView {
	s.schedulesMu.RLock()
	list := make([]scheduleListView, 0, len(s.schedules))
	for _, sc := range s.schedules {
		list = append(list, newScheduleListView(sc))
	}
	s.schedulesMu.RUnlock()
	return list
}

// scheduleHistory returns a copy of one schedule's history ring.
func (s *Server) scheduleHistory(id string) ([]historyPoint, bool) {
	s.schedulesMu.RLock()
	defer s.schedulesMu.RUnlock()
	sc, ok := s.schedules[id]
	if !ok {
		return nil, false
	}
	return append([]historyPoint(nil), sc.History...), true
}

// scheduleMetric is the per-schedule snapshot /metrics renders. Status comes
// from the schedule itself (so a run in flight shows as "running"); rtt and
// loss come from the newest history point, which is the only place they exist
// for a summary too big to persist.
type scheduleMetric struct {
	ID      string
	Node    string
	Command string
	Target  string
	Status  string
	RttMs   *float64
	LossPct *float64
}

// scheduleMetrics snapshots every schedule for the /metrics exposition.
// Cardinality is bounded by scheduleMaxCount.
func (s *Server) scheduleMetrics() []scheduleMetric {
	s.schedulesMu.RLock()
	defer s.schedulesMu.RUnlock()
	out := make([]scheduleMetric, 0, len(s.schedules))
	for _, sc := range s.schedules {
		m := scheduleMetric{
			ID:      sc.ID,
			Node:    sc.NodeName,
			Command: sc.Command,
			Target:  sc.Target,
			Status:  sc.LastStatus,
		}
		if n := len(sc.History); n > 0 {
			last := sc.History[n-1]
			if last.RttMs != nil {
				v := *last.RttMs
				m.RttMs = &v
			}
			if last.LossPct != nil {
				v := *last.LossPct
				m.LossPct = &v
			}
		}
		out = append(out, m)
	}
	return out
}

const (
	scheduleMaxInterval = 365 * 24 * 60 * 60 // one year; avoids duration overflow
	scheduleMinInterval = 60                 // seconds; lower = abuse risk
	scheduleMaxResult   = 4096               // chars retained from last run output
	scheduleMaxCount    = 100
	// scheduleMaxSummary caps the structured summary a schedule persists. A
	// bigger summary (a full 64-hop mtr) is simply not persisted — it is
	// never truncated, because a truncated json.RawMessage would make
	// saveSchedules fail permanently.
	scheduleMaxSummary = 4096

	// scheduleHistoryMax is the in-memory ring size: 288 points is 24 h at one
	// run per 5 min, the interval the UI preset defaults to.
	scheduleHistoryMax = 288
	// schedulePersistHistory is how much of that ring reaches the file. 64
	// points × 100 schedules keeps an atomic rewrite well under a megabyte.
	schedulePersistHistory = 64

	scheduleNotAllowedResult = "command not allowed for schedules"

	// scheduleSchemaVersion is the current per-schedule storage format. 1 means
	// "NodeName is stored raw" — see Schedule.SchemaVersion and loadSchedules.
	scheduleSchemaVersion = 1
	// scheduleNodeNameMax mirrors the 64-rune cap agent_ws.go applies to a
	// node's Name, so the F30 migration cannot lengthen a denormalized copy of
	// it past what the registration path would have stored.
	scheduleNodeNameMax = 64
)

// Tick interval and stuck-run timeout are package vars (not consts) so tests
// can shorten them before starting a ticker. The timeout is deliberately
// longer than the agent's own 10-minute command cap, so the watchdog only
// fires for runs the agent will never report on.
// scheduleSaveInterval bounds how often the file is rewritten: the first save
// after a quiet period is synchronous, and everything inside the window that
// follows collapses into one trailing write. Also a package var so tests need
// no multi-second sleeps.
var (
	scheduleTickInterval = 10 * time.Second
	scheduleRunTimeout   = 11 * time.Minute
	scheduleSaveInterval = 5 * time.Second
)

// staleRun is a snapshot of a run that looked stuck during the watchdog scan.
// The cmdID and the run epoch are copied so the finalize step can verify it is
// still the very same run.
type staleRun struct {
	sc    *Schedule
	cmdID string
	epoch uint64
}

// scheduleAllowedCommands is intentionally narrower than the regular command
// allowlist — iperf3, speedtest, and shell are too expensive or interactive
// to run on a timer, and so is the native `download` probe: up to 100 MB per
// run, on every tick, from a URL nobody re-reviews (F10). tcp/tls/dnsbench are
// cheap and are exactly the things worth watching over time.
var scheduleAllowedCommands = map[string]bool{
	"ping":       true,
	"traceroute": true,
	"mtr":        true,
	"dns":        true,
	"http":       true,
	"tcp":        true,
	"tls":        true,
	"dnsbench":   true,
}

func schedulesFilePath() string {
	if p := os.Getenv("SCHEDULES_FILE"); p != "" {
		return p
	}
	return "schedules.json"
}

// loadSchedules populates the server's schedule map from disk. Errors are
// logged but not fatal — a missing file just means "no schedules yet".
func (s *Server) loadSchedules() {
	path := schedulesFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.scheduleLoadFailed.Store(true)
			log.Printf("Schedules file %s read error: %v (schedules unavailable; file preserved)", path, err)
		}
		return
	}
	var list []*Schedule
	if err := json.Unmarshal(data, &list); err != nil {
		s.scheduleLoadFailed.Store(true)
		log.Printf("Schedules file %s parse error: %v (schedules unavailable; file preserved)", path, err)
		return
	}
	var disallowed []string
	migrated := 0
	s.schedulesMu.Lock()
	for _, sc := range list {
		if sc == nil || sc.ID == "" {
			continue
		}
		// Clamp operator-edited/legacy intervals before duration arithmetic.
		if sc.IntervalSec < scheduleMinInterval {
			sc.IntervalSec = scheduleMinInterval
		}
		if sc.IntervalSec > scheduleMaxInterval {
			sc.IntervalSec = scheduleMaxInterval
		}
		// One-time storage migration (S10/F30). Entries written before
		// sanitizeString stopped HTML-escaping carry an escaped NodeName
		// ("A&amp;T"); unescape it exactly once, gated on the per-schedule
		// marker. Without the gate this is not a migration but a compounding
		// corruption: NodeName is stored raw now, so a node legitimately named
		// "A&amp;T" would lose one entity level on every single restart.
		//
		// The unescaped value is re-stripped and re-truncated because
		// html.UnescapeString can *synthesise* control characters — a
		// hand-edited "&#13;" becomes a carriage return, and the /metrics
		// exposition escaper (ops.go metricsLabelValue) escapes backslash,
		// quote and newline but not CR.
		if sc.SchemaVersion < scheduleSchemaVersion {
			sc.NodeName = truncateRunes(stripControlChars(html.UnescapeString(sc.NodeName)), scheduleNodeNameMax)
			sc.SchemaVersion = scheduleSchemaVersion
			migrated++
		}
		// A "running" status in the file means the process died mid-run;
		// clear it so the ticker will pick the schedule up again.
		if sc.LastStatus == "running" {
			sc.LastStatus = ""
		}
		// A persisted summary is re-validated on load: the file is operator
		// owned, but it was written from agent-supplied data and is served
		// straight back to browsers. Anything that no longer fits the grammar
		// or the size cap is dropped rather than re-published.
		if len(sc.LastSummary) > 0 {
			canonical, ok := validateSummary(string(sc.LastSummary))
			if !ok || len(canonical) > scheduleMaxSummary {
				sc.LastSummary = nil
			} else {
				sc.LastSummary = canonical
			}
		}
		// A hand-edited file could carry an arbitrarily long history; clamp it
		// to the ring size so memory stays bounded by scheduleHistoryMax.
		if len(sc.History) > scheduleHistoryMax {
			sc.History = append([]historyPoint(nil), sc.History[len(sc.History)-scheduleHistoryMax:]...)
		}
		for i := range sc.History {
			sc.History[i].Status = truncateRunes(stripControlChars(sc.History[i].Status), 32)
		}
		// A persisted schedule whose command is no longer allowed is kept
		// (never silently dropped from the user's file) but disabled, so it
		// can never be selected by the ticker or run manually.
		if !scheduleAllowedCommands[sc.Command] {
			sc.Enabled = false
			sc.LastStatus = "error"
			sc.LastResult = scheduleNotAllowedResult
			disallowed = append(disallowed, fmt.Sprintf("%q (%q)", sc.ID, sc.Command))
		}
		s.schedules[sc.ID] = sc
	}
	s.schedulesMu.Unlock()
	// Node identities normally live in memory. Restore the identities used by
	// persisted schedules so reconnecting agents retain their schedule targets.
	// No other lock is held while acquiring nodesMu.
	s.nodesMu.Lock()
	identities := make(map[string]string)
	for id, node := range s.nodes {
		identities[node.Name] = id
	}
	remap := make(map[*Schedule]string)
	for _, sc := range list {
		if sc == nil || sc.ID == "" || sc.NodeID == "" {
			continue
		}
		if id, exists := identities[sc.NodeName]; exists {
			if sc.NodeID != id {
				remap[sc] = id
			}
			continue
		}
		if _, exists := s.nodes[sc.NodeID]; !exists {
			s.nodes[sc.NodeID] = &Node{ID: sc.NodeID, Name: sc.NodeName, mu: &sync.Mutex{}}
			identities[sc.NodeName] = sc.NodeID
		}
	}
	s.nodesMu.Unlock()
	// Files written across earlier restarts can reference multiple UUIDs for
	// the same unique agent name. Reconcile those schedules onto one identity.
	s.schedulesMu.Lock()
	for sc, id := range remap {
		sc.NodeID = id
	}
	s.schedulesMu.Unlock()
	log.Printf("Loaded %d schedule(s) from %s", len(list), path)
	if len(disallowed) > 0 {
		// %q above quotes the command, so a hand-edited file cannot inject
		// control characters into the log.
		log.Printf("Disabled %d schedule(s) with a command not allowed for schedules: %s",
			len(disallowed), strings.Join(disallowed, ", "))
	}
	// Write the migration marker synchronously, before the server starts
	// serving — and only here, after schedulesMu has been released.
	// persistSchedules calls scheduleViews, which takes schedulesMu.RLock;
	// sync.RWMutex is not reentrant, so calling it inside the loop above would
	// hang startup. It also bypasses saveSchedules' coalescer on purpose: a
	// marker that depended on a later finalize or the shutdown flush would let
	// a server restarting before either re-migrate on every start — exactly the
	// compounding defect the gate exists to stop. Written even when no
	// NodeName actually changed, because the marker is the one-time-ness.
	if migrated > 0 {
		log.Printf("Migrated %d schedule(s) to schema_version %d (node_name stored unescaped)", migrated, scheduleSchemaVersion)
	}
	if migrated > 0 || len(remap) > 0 {
		s.persistSchedules(path)
	}
}

// saveSchedules persists all schedules, coalescing bursts (F22): the first
// call after a quiet period writes synchronously — callers (and tests) that
// save and immediately read the file keep working — and every call inside the
// scheduleSaveInterval that follows just marks the state dirty. One trailing
// write, or the shutdown flush, drains it. A run storm across 100 schedules
// therefore costs one rewrite per interval instead of one per finalize.
//
// The coalescing state lives on Server, not in a package var, so a freshly
// constructed test server's first save is never swallowed by a previous one.
func (s *Server) saveSchedules() {
	path := schedulesFilePath()
	now := time.Now()

	s.saveMu.Lock()
	if !s.saveLastAt.IsZero() && now.Sub(s.saveLastAt) < scheduleSaveInterval {
		s.saveDirty = true
		// The path is captured here, not in the trailing writer: a test that
		// restores SCHEDULES_FILE when it ends must not redirect a write it
		// already asked for into another test's file.
		s.savePath = path
		if !s.saveTrailing {
			s.saveTrailing = true
			delay := scheduleSaveInterval - now.Sub(s.saveLastAt)
			go s.trailingSaveSchedules(delay)
		}
		s.saveMu.Unlock()
		return
	}
	s.saveLastAt = now
	s.saveDirty = false
	s.savePath = path
	s.saveMu.Unlock()

	s.persistSchedules(path)
}

// trailingSaveSchedules drains the dirty flag once the current interval is up.
func (s *Server) trailingSaveSchedules(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
	s.flushSchedules()
}

// flushSchedules writes immediately if anything is pending. The trailing
// writer uses it; shutdown persists a final snapshot unconditionally so an
// already-claimed trailing write cannot outlive process exit.
func (s *Server) flushSchedules() {
	s.saveMu.Lock()
	s.saveTrailing = false
	if !s.saveDirty {
		s.saveMu.Unlock()
		return
	}
	s.saveDirty = false
	path := s.savePath
	s.saveLastAt = time.Now()
	s.saveMu.Unlock()
	if path == "" {
		return
	}
	s.persistSchedules(path)
}

// persistSchedules writes the file atomically — write to a temp file in the
// same directory then rename, so an OS crash mid-write can't corrupt the
// existing file. saveWriteMu serializes writers so a slow marshal can never
// land after a newer snapshot.
func (s *Server) persistSchedules(path string) {
	if s.scheduleLoadFailed.Load() {
		return
	}
	s.saveWriteMu.Lock()
	defer s.saveWriteMu.Unlock()
	// Marshal value copies taken under the lock — marshaling the live
	// *Schedule structs would read fields the ticker mutates concurrently.
	list := s.scheduleViews()
	for i := range list {
		if n := len(list[i].History); n > schedulePersistHistory {
			list[i].History = list[i].History[n-schedulePersistHistory:]
		}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Printf("Schedule marshal error: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		log.Printf("Schedule write error: %v", err)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		log.Printf("Schedule write error: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		log.Printf("Schedule close error: %v", err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		log.Printf("Schedule rename error: %v", err)
	}
}

// scheduleTicker wakes up every scheduleTickInterval (10s in production) and
// dispatches any schedule whose NextRunAt is in the past; on the same tick it
// runs the stuck-run watchdog. 10s is fine even for 60s schedules — the worst
// case is a one-tick (10s) jitter, which is irrelevant for monitoring.
func (s *Server) scheduleTicker(done <-chan struct{}) {
	t := time.NewTicker(scheduleTickInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			due := s.collectDueSchedules(now)
			stale := s.collectStaleRuns(now)
			for _, sc := range due {
				cmdID, reason := s.beginRun(sc)
				if reason != "" {
					continue
				}
				go s.dispatchSchedule(sc, cmdID)
			}
			// Watchdog: a run whose agent never reported `done` (and whose
			// disconnect was never observed) would otherwise stay "running"
			// forever, blocking every future run of that schedule.
			for _, st := range stale {
				s.finalizeStaleRunSnapshot(st)
			}
		}
	}
}

// collectDueSchedules snapshots the schedules that should run now. The
// returned slice is used after schedulesMu is released.
func (s *Server) collectDueSchedules(now time.Time) []*Schedule {
	s.schedulesMu.RLock()
	defer s.schedulesMu.RUnlock()
	due := make([]*Schedule, 0)
	for _, sc := range s.schedules {
		if !sc.Enabled || sc.LastStatus == "running" {
			continue
		}
		if !scheduleAllowedCommands[sc.Command] {
			continue
		}
		if sc.NextRunAt.IsZero() || !now.Before(sc.NextRunAt) {
			due = append(due, sc)
		}
	}
	return due
}

// collectStaleRuns snapshots runs that have been "running" for longer than
// scheduleRunTimeout, copying the cmdID so finalizeStaleRun can confirm it is
// still the same run once the lock has been released and re-taken.
func (s *Server) collectStaleRuns(now time.Time) []staleRun {
	s.schedulesMu.RLock()
	defer s.schedulesMu.RUnlock()
	var stale []staleRun
	for _, sc := range s.schedules {
		if sc.LastStatus != "running" || sc.runStartedAt.IsZero() {
			continue
		}
		if now.Sub(sc.runStartedAt) > scheduleRunTimeout {
			stale = append(stale, staleRun{sc: sc, cmdID: sc.runningCmdID, epoch: sc.runEpoch})
		}
	}
	return stale
}

// finalizeStaleRun times out a run that the watchdog flagged, but only if it
// is still the very same run: a run that completed and restarted between the
// scan and this call has a different cmdID and must be left alone.
//
// The signature is fixed (callers outside this file pass sc + cmdID only); the
// run epoch is read here and carried in the snapshot.
func (s *Server) finalizeStaleRun(sc *Schedule, cmdID string) {
	s.schedulesMu.RLock()
	epoch := sc.runEpoch
	s.schedulesMu.RUnlock()
	s.finalizeStaleRunSnapshot(staleRun{sc: sc, cmdID: cmdID, epoch: epoch})
}

// finalizeStaleRunSnapshot is the epoch-aware form the watchdog uses. The
// cheap pre-check avoids touching registrations for a run that already
// finished; the authoritative check happens inside the same schedulesMu
// section as the status write (see finalizeScheduleGuarded), which closes the
// window between "still stuck?" and "mark it failed".
func (s *Server) finalizeStaleRunSnapshot(st staleRun) {
	st.sc.lifecycleMu.Lock()
	defer st.sc.lifecycleMu.Unlock()
	s.schedulesMu.RLock()
	sameRun := st.sc.LastStatus == "running" && st.sc.runningCmdID == st.cmdID && st.sc.runEpoch == st.epoch
	s.schedulesMu.RUnlock()
	if !sameRun {
		return
	}
	// Dropping the registration for this cmdID is safe either way: a newer run
	// has a different cmdID.
	s.clearRunRegistration(st.cmdID)
	s.finalizeScheduleGuarded(st.sc, "error", "run timed out", nil, &st)
}

// tryBeginRun performs the whole "is this schedule runnable, and if so claim
// it" transition atomically under schedulesMu. reason is "" on success, else
// one of "disabled", "running", "not_allowed". It takes no other lock.
func (s *Server) tryBeginRun(sc *Schedule) (cmdID string, reason string) {
	s.schedulesMu.Lock()
	defer s.schedulesMu.Unlock()
	if s.schedules[sc.ID] != sc {
		return "", "deleted"
	}
	if !sc.Enabled {
		return "", "disabled"
	}
	if sc.LastStatus == "running" {
		return "", "running"
	}
	if !scheduleAllowedCommands[sc.Command] {
		return "", "not_allowed"
	}
	now := time.Now()
	cmdID = uuid.New().String()
	sc.LastStatus = "running"
	sc.LastRunAt = now
	sc.runningCmdID = cmdID
	sc.runStartedAt = now
	// Every claim is a new epoch, so a watchdog snapshot taken before this
	// point can no longer finalize the schedule.
	sc.runEpoch++
	return cmdID, ""
}

// beginRun claims the schedule and registers the run *before* returning, so
// that by the time the caller dispatches (or a second caller tries to start a
// run) the cmdID → schedule and cmdID → node mappings already exist and the
// first output line is routed correctly. Each map is locked separately;
// schedulesMu is never held while taking them.
func (s *Server) beginRun(sc *Schedule) (cmdID string, reason string) {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	cmdID, reason = s.tryBeginRun(sc)
	if reason != "" {
		if reason == "not_allowed" {
			// Outside any lock: finalizeSchedule takes schedulesMu itself.
			s.finalizeSchedule(sc, "error", scheduleNotAllowedResult)
		}
		return "", reason
	}

	sc.currentMu.Lock()
	sc.currentBuf.Reset()
	// A new run must never inherit the previous run's summary.
	sc.currentSummary = nil
	sc.currentMu.Unlock()

	// Register the cmdID → schedule mapping so handleAgentOutput knows where
	// output belongs. cmdOwners stays unset (nil entry) since there's no
	// browser to forward output to — sendToCommandOwner short-circuits when
	// the owner is missing.
	s.scheduleRunsMu.Lock()
	s.scheduleRuns[cmdID] = sc
	s.scheduleRunsMu.Unlock()

	s.cmdNodesMu.Lock()
	s.cmdNodes[cmdID] = sc.NodeID
	s.cmdNodesMu.Unlock()

	return cmdID, ""
}

// clearRunRegistration drops every registration for a cmdID — the schedule
// run, the node mapping and the summary dedup entry. The mutexes are taken one
// at a time, never nested.
func (s *Server) clearRunRegistration(cmdID string) {
	s.scheduleRunsMu.Lock()
	delete(s.scheduleRuns, cmdID)
	s.scheduleRunsMu.Unlock()

	s.cmdNodesMu.Lock()
	delete(s.cmdNodes, cmdID)
	s.cmdNodesMu.Unlock()

	s.forgetSummary(cmdID)
}

// dispatchSchedule sends an already-claimed run to its agent:
//  1. validate target node is online
//  2. send the command through the same agent-WS path the browser uses
//  3. let handleAgentOutput funnel output into sc.currentBuf
//  4. on `done`, capture the buffer into LastResult and persist
//
// The run must already have been claimed and registered by beginRun. On any
// failure here the registrations are dropped and the schedule is finalized so
// it can never stay stuck in "running".
func (s *Server) dispatchSchedule(sc *Schedule, cmdID string) {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if !s.scheduleRunCurrent(sc, cmdID) {
		s.clearRunRegistration(cmdID)
		return
	}
	s.nodesMu.RLock()
	node, nodeOK := s.nodes[sc.NodeID]
	// Online must be read while nodesMu is held; the agent read loop writes it.
	online := nodeOK && node.Online
	s.nodesMu.RUnlock()
	if !online {
		s.clearRunRegistration(cmdID)
		s.finalizeSchedule(sc, "agent_offline", "node "+sc.NodeID+" is not online")
		return
	}

	payload, _ := json.Marshal(CommandRequest{ID: cmdID, Type: sc.Command, Target: sc.Target, Options: sc.Options})
	agentMsg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})

	node.mu.Lock()
	var writeErr error
	if node.conn != nil {
		node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		writeErr = node.conn.WriteMessage(websocket.TextMessage, agentMsg)
		node.conn.SetWriteDeadline(time.Time{})
	} else {
		writeErr = fmt.Errorf("agent connection is nil")
	}
	node.mu.Unlock()

	if writeErr != nil {
		s.clearRunRegistration(cmdID)
		s.finalizeSchedule(sc, "error", "failed to send command: "+writeErr.Error())
		return
	}
}

// scheduleRunCurrent is checked with lifecycleMu held. A deleted, disabled
// in-flight, timed-out or replaced registration must not be dispatched or
// allowed to finalize a newer execution.
func (s *Server) scheduleRunCurrent(sc *Schedule, cmdID string) bool {
	s.schedulesMu.RLock()
	current := s.schedules[sc.ID] == sc && sc.LastStatus == "running" && sc.runningCmdID == cmdID
	s.schedulesMu.RUnlock()
	s.scheduleRunsMu.RLock()
	registered := s.scheduleRuns[cmdID] == sc
	s.scheduleRunsMu.RUnlock()
	return current && registered
}

// scheduleAgentDisconnected is the explicit terminal path for a scheduled run
// whose agent connection died mid-run. It is called from the agent disconnect
// loop for every orphaned cmdID and returns true if the cmdID belonged to a
// scheduled run (so the browser-notification path can be skipped). The status
// is set directly — it never goes through scheduleAcceptOutput's output
// inspection, because no `done` message will ever arrive.
func (s *Server) scheduleAgentDisconnected(cmdID string) bool {
	s.scheduleRunsMu.RLock()
	sc, ok := s.scheduleRuns[cmdID]
	s.scheduleRunsMu.RUnlock()
	if !ok {
		return false
	}
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	current := s.scheduleRunCurrent(sc, cmdID)
	s.clearRunRegistration(cmdID)
	if current {
		s.finalizeSchedule(sc, "agent_offline", "agent disconnected mid-run")
	}
	return true
}

// scheduleAcceptOutput is called from handleAgentOutput when the cmd ID
// belongs to a scheduled run. Returns true if the output was consumed (so the
// regular browser-forwarding path can be skipped).
func (s *Server) scheduleAcceptOutput(resp CommandResponse) bool {
	s.scheduleRunsMu.RLock()
	sc, ok := s.scheduleRuns[resp.ID]
	s.scheduleRunsMu.RUnlock()
	if !ok {
		return false
	}
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if !s.scheduleRunCurrent(sc, resp.ID) {
		return true
	}
	switch resp.Type {
	case "output", "error":
		sc.currentMu.Lock()
		// Cap buffer size in-place so a runaway command can't OOM us.
		if sc.currentBuf.Len() < scheduleMaxResult {
			if sc.currentBuf.Len() > 0 {
				sc.currentBuf.WriteByte('\n')
			}
			remain := scheduleMaxResult - sc.currentBuf.Len()
			line := resp.Data
			if len(line) > remain {
				line = line[:remain]
			}
			sc.currentBuf.WriteString(line)
		}
		sc.currentMu.Unlock()
	case "summary":
		// Already validated, sanitized and deduplicated by handleAgentOutput;
		// held until `done` decides the status and whether to persist it.
		sc.currentMu.Lock()
		sc.currentSummary = []byte(resp.Data)
		sc.currentMu.Unlock()
	case "done":
		sc.currentMu.Lock()
		buf := sc.currentBuf.String()
		summary := sc.currentSummary
		sc.currentSummary = nil
		sc.currentMu.Unlock()
		status := scheduleStatusFor(parseExitOK(resp.Data), summary)
		s.finalizeScheduleWithSummary(sc, status, buf, summary)
		// Release the schedule run mapping + node mapping.
		s.clearRunRegistration(resp.ID)
	}
	return true
}

// scheduleStatusFor derives a run's terminal status from the agent's exit
// status and the structured summary (F35 — the old substring match on the
// output buffer is gone).
//
//   - an unsuccessful exit is always error, regardless of partial results.
//   - numeric loss_pct (ping, mtr) after a successful exit: ok when
//     loss < 20 %, degraded from 20 % up to (but not including) 100 %,
//     error otherwise — a fully black-holed target is an outage, not a
//     degradation.
//   - no loss_pct (http, dns) or no summary at all: ok when the command
//     exited cleanly, else error.
func scheduleStatusFor(exitOK bool, summary []byte) string {
	if !exitOK {
		return "error"
	}
	if loss, ok := summaryLossPct(summary); ok {
		switch {
		case loss < 20:
			return "ok"
		case loss >= 20 && loss < 100:
			return "degraded"
		default:
			return "error"
		}
	}
	return "ok"
}

// finalizeSchedule writes the result into the schedule, schedules the next
// run, and persists the file. Safe to call from any goroutine. It always
// clears LastSummary: every caller but the `done` path is a failure path
// (agent_offline, write failure, not_allowed, watchdog timeout) and must not
// leave the previous run's monitoring data in place.
func (s *Server) finalizeSchedule(sc *Schedule, status, result string) {
	s.finalizeScheduleWithSummary(sc, status, result, nil)
}

// finalizeScheduleWithSummary is finalizeSchedule plus the structured summary
// of a completed run. It is the single funnel every finalize path goes
// through, which is why the history point is appended here.
func (s *Server) finalizeScheduleWithSummary(sc *Schedule, status, result string, summary []byte) {
	s.finalizeScheduleGuarded(sc, status, result, summary, nil)
}

// finalizeScheduleGuarded writes the terminal state, appends the history point
// and queues an alert. When guard is non-nil the write happens only if the run
// it describes is still the current one — the watchdog's re-check and its
// status write are then a single critical section.
//
// The summary is persisted only when it fits in scheduleMaxSummary; a larger
// one is reduced (scalars kept, hops dropped) and, if still too big, left out
// entirely. Raw JSON is never truncated — a truncated RawMessage would break
// every later save.
//
// Returns true when the state was written.
func (s *Server) finalizeScheduleGuarded(sc *Schedule, status, result string, summary []byte, guard *staleRun) bool {
	now := time.Now()
	persist := persistableSummary(summary)

	point := historyPoint{T: now, Status: status}
	// rtt/loss come from the summary of *this* run, even when it was too big
	// to persist — the history point is the only record left in that case.
	if len(summary) > 0 {
		if rtt, ok := summaryAvgMs(summary); ok {
			point.RttMs = &rtt
		}
		if loss, ok := summaryLossPct(summary); ok {
			point.LossPct = &loss
		}
	}

	s.schedulesMu.Lock()
	if guard != nil && !(sc.LastStatus == "running" && sc.runningCmdID == guard.cmdID && sc.runEpoch == guard.epoch) {
		s.schedulesMu.Unlock()
		return false
	}
	prevStatus := sc.LastStatus
	sc.LastStatus = status
	sc.LastResult = result
	sc.LastSummary = persist
	sc.NextRunAt = now.Add(time.Duration(sc.IntervalSec) * time.Second)
	sc.History = appendHistoryPoint(sc.History, point)
	ev := alertEvent{
		ScheduleID: sc.ID,
		Node:       sc.NodeName,
		Command:    sc.Command,
		Target:     sc.Target,
		From:       prevStatus,
		To:         status,
		Summary:    append(json.RawMessage(nil), summary...),
		At:         now,
	}
	s.schedulesMu.Unlock()

	s.saveSchedules()
	// Outside schedulesMu: the alert path takes its own mutex and does a
	// non-blocking channel send.
	s.alerts.observe(ev)
	return true
}

// appendHistoryPoint appends p and keeps at most scheduleHistoryMax points,
// newest last.
func appendHistoryPoint(h []historyPoint, p historyPoint) []historyPoint {
	h = append(h, p)
	if len(h) > scheduleHistoryMax {
		h = h[len(h)-scheduleHistoryMax:]
	}
	return h
}

// persistableSummary returns the blob to store in LastSummary: the canonical
// summary when it fits, otherwise a reduced form with the array fields (mtr's
// `hops`) removed so the scalar badges still render, otherwise nothing. A
// 64-hop mtr summary is 8–13 KB, so without this every mtr schedule would show
// no summary at all.
func persistableSummary(summary []byte) json.RawMessage {
	if len(summary) == 0 {
		return nil
	}
	if len(summary) <= scheduleMaxSummary {
		return append(json.RawMessage(nil), summary...)
	}
	reduced, ok := reduceSummary(summary)
	if !ok || len(reduced) > scheduleMaxSummary {
		return nil
	}
	return reduced
}

// reduceSummary re-marshals an already-validated summary keeping only its
// scalar fields. The input came out of validateSummary, so this can only ever
// shrink it; the result is still valid against the same grammar.
func reduceSummary(summary []byte) (json.RawMessage, bool) {
	dec := json.NewDecoder(strings.NewReader(string(summary)))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, false
	}
	out := make(map[string]any, len(top))
	for k, v := range top {
		switch v.(type) {
		case string, json.Number, bool:
			out[k] = v
		default:
			// arrays (hops) and anything else are dropped
		}
	}
	reduced, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return reduced, true
}

// summaryAvgMs extracts the top-level avg_ms of an already-validated summary.
// Deliberately narrow: http's total_ms and dns's query_time_ms are *not*
// accepted, so lookingglass_probe_rtt_ms never mixes probe kinds.
func summaryAvgMs(summary []byte) (float64, bool) {
	if len(summary) == 0 {
		return 0, false
	}
	dec := json.NewDecoder(strings.NewReader(string(summary)))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return 0, false
	}
	num, ok := top["avg_ms"].(json.Number)
	if !ok {
		return 0, false
	}
	f, err := num.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

// ---- HTTP handlers ----

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Schedule data is operator state behind an API key; never let a proxy or
	// browser cache keep a copy of it.
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case "GET":
		s.handleSchedulesList(w, r)
	case "POST":
		s.handleSchedulesCreate(w, r)
	default:
		writeJSONError(w, "method not allowed", 405)
	}
}

func (s *Server) handleSchedulesList(w http.ResponseWriter, r *http.Request) {
	// Schedules are an operator surface: in public mode an anonymous visitor
	// must not even enumerate them. Checked before authClient, which returns
	// true when no key is configured.
	if refusePublic(w, r) {
		return
	}
	if !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return
	}
	if s.scheduleLoadFailed.Load() {
		writeJSONError(w, "schedules unavailable: repair the persistence file and restart", 503)
		return
	}
	// Encode value copies taken under the lock — see scheduleViews. The list
	// response deliberately omits the run history (polled every 5 s); the
	// sparkline fetches /api/schedules/<id>/history instead.
	json.NewEncoder(w).Encode(s.scheduleListViews())
}

func (s *Server) handleSchedulesCreate(w http.ResponseWriter, r *http.Request) {
	// No anonymous visitor may make the deployment run recurring probes.
	if refusePublic(w, r) {
		return
	}
	if !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return
	}
	if s.scheduleLoadFailed.Load() {
		writeJSONError(w, "schedules unavailable: repair the persistence file and restart", 503)
		return
	}
	var req struct {
		NodeID      string `json:"node_id"`
		Command     string `json:"command"`
		Target      string `json:"target"`
		Options     string `json:"options"`
		IntervalSec int    `json:"interval_sec"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "bad request", 400)
		return
	}
	if !scheduleAllowedCommands[req.Command] {
		writeJSONError(w, fmt.Sprintf("command %q not allowed for schedules", req.Command), 400)
		return
	}
	if req.IntervalSec < scheduleMinInterval || req.IntervalSec > scheduleMaxInterval {
		writeJSONError(w, fmt.Sprintf("interval_sec must be between %d and %d", scheduleMinInterval, scheduleMaxInterval), 400)
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		writeJSONError(w, "target is required", 400)
		return
	}
	if len(req.Target) > 1024 || len(req.Options) > 512 {
		writeJSONError(w, "fields too long", 400)
		return
	}
	if strings.ContainsAny(req.Target, "\n\r\t\x00") || strings.ContainsAny(req.Options, "\n\r\t\x00") {
		writeJSONError(w, "fields contain invalid characters", 400)
		return
	}

	// Validate target node exists.
	s.nodesMu.RLock()
	node, nodeOK := s.nodes[req.NodeID]
	var nodeName string
	if nodeOK {
		nodeName = node.Name
	}
	s.nodesMu.RUnlock()
	if !nodeOK {
		writeJSONError(w, "unknown node_id", 400)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	sc := &Schedule{
		ID:          uuid.New().String(),
		NodeID:      req.NodeID,
		NodeName:    nodeName,
		Command:     req.Command,
		Target:      req.Target,
		Options:     req.Options,
		IntervalSec: req.IntervalSec,
		Enabled:     enabled,
		CreatedAt:   time.Now(),
		// Stamped at construction: an unstamped new schedule would be
		// re-classified as legacy on the next restart and have its already-raw
		// NodeName unescaped once more.
		SchemaVersion: scheduleSchemaVersion,
	}
	s.schedulesMu.Lock()
	if len(s.schedules) >= scheduleMaxCount {
		s.schedulesMu.Unlock()
		writeJSONError(w, fmt.Sprintf("schedule cap reached (%d)", scheduleMaxCount), 409)
		return
	}
	s.schedules[sc.ID] = sc
	s.schedulesMu.Unlock()
	s.saveSchedules()

	w.WriteHeader(201)
	// Encode a value snapshot taken under the lock — the ticker may already be
	// mutating this schedule (LastStatus, NextRunAt) from another goroutine.
	s.schedulesMu.RLock()
	view := newScheduleView(sc)
	s.schedulesMu.RUnlock()
	json.NewEncoder(w).Encode(view)
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Covers DELETE, /toggle, /run and /history — all of them expose or mutate
	// operator state, so none is reachable from a public session.
	if refusePublic(w, r) {
		return
	}
	if !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return
	}
	if s.scheduleLoadFailed.Load() {
		writeJSONError(w, "schedules unavailable: repair the persistence file and restart", 503)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/schedules/")
	id = strings.TrimSuffix(id, "/run")
	id = strings.TrimSuffix(id, "/toggle")
	id = strings.TrimSuffix(id, "/history")
	if id == "" || strings.Contains(id, "/") {
		writeJSONError(w, "invalid id", 400)
		return
	}

	// Serialize operator mutations with run registration and output. Acquire
	// the per-schedule lock only after releasing schedulesMu.
	s.schedulesMu.RLock()
	lockedSchedule := s.schedules[id]
	s.schedulesMu.RUnlock()
	if lockedSchedule == nil {
		writeJSONError(w, "not found", 404)
		return
	}
	// /run takes lifecycleMu in beginRun itself; read-only history needs none.
	if r.Method == "DELETE" || (r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/toggle")) {
		lockedSchedule.lifecycleMu.Lock()
		defer lockedSchedule.lifecycleMu.Unlock()
	}

	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/history"):
		history, ok := s.scheduleHistory(id)
		if !ok {
			writeJSONError(w, "not found", 404)
			return
		}
		if history == nil {
			history = []historyPoint{}
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "history": history})
	case r.Method == "DELETE":
		// Snapshot the in-flight run before the entry disappears: the run
		// registration would otherwise survive until the agent's `done` for a
		// schedule that no longer exists.
		s.schedulesMu.Lock()
		sc, ok := s.schedules[id]
		var runningCmdID string
		if ok && sc.LastStatus == "running" {
			runningCmdID = sc.runningCmdID
		}
		delete(s.schedules, id)
		s.schedulesMu.Unlock()
		if !ok {
			writeJSONError(w, "not found", 404)
			return
		}
		if runningCmdID != "" {
			// Outside schedulesMu — clearRunRegistration takes three other
			// mutexes, one at a time.
			s.clearRunRegistration(runningCmdID)
		}
		// Drop the alert bookkeeping too, so an id that comes back later
		// starts from a fresh baseline instead of an old streak.
		s.alerts.forgetSchedule(id)
		s.saveSchedules()
		w.Write([]byte(`{"ok":true}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/toggle"):
		s.schedulesMu.Lock()
		sc, ok := s.schedules[id]
		if !ok {
			s.schedulesMu.Unlock()
			writeJSONError(w, "not found", 404)
			return
		}
		sc.Enabled = !sc.Enabled
		// LastStatus is deliberately left alone. Clearing it on disable used
		// to let an off→on toggle start a second run that shared currentBuf
		// with the first. A schedule disabled mid-run instead stays "running"
		// (Run now returns 409, and the button is disabled) until the
		// watchdog finalizes it — self-healing.
		var runningCmdID string
		if !sc.Enabled && sc.LastStatus == "running" {
			runningCmdID = sc.runningCmdID
		}
		s.schedulesMu.Unlock()
		if runningCmdID != "" {
			s.clearRunRegistration(runningCmdID)
		}
		s.saveSchedules()
		w.Write([]byte(`{"ok":true}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/run"):
		s.schedulesMu.RLock()
		sc, ok := s.schedules[id]
		s.schedulesMu.RUnlock()
		if !ok {
			writeJSONError(w, "not found", 404)
			return
		}
		// The run transition + registration happen synchronously here, so a
		// second concurrent /run for the same schedule always loses the race
		// and gets 409 instead of double-dispatching.
		cmdID, reason := s.beginRun(sc)
		switch reason {
		case "":
			go s.dispatchSchedule(sc, cmdID)
			w.Write([]byte(`{"ok":true}`))
		case "running":
			writeJSONError(w, "already running", 409)
		case "not_allowed":
			writeJSONError(w, scheduleNotAllowedResult, 400)
		case "disabled":
			writeJSONError(w, "disabled", 409)
		default:
			writeJSONError(w, "cannot run", 409)
		}
	default:
		writeJSONError(w, "method not allowed", 405)
	}
}
