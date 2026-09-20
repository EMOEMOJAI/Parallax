package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ---- harness ----------------------------------------------------------------
//
// These helpers build a real Server plus an httptest server carrying the three
// routes the scheduler needs (/ws/agent, /api/schedules, /api/schedules/), and
// connect a fake agent over a real gorilla WebSocket so node.conn / node.mu are
// genuine and the disconnect path is exercised for real. Later slices reuse
// this harness.

// testTempRoot holds every schedules file the tests write. It is deliberately
// package-scoped rather than per-test t.TempDir: the server finalizes runs from
// goroutines (dispatch, agent-disconnect) that can still call saveSchedules a
// moment after the test that started them returned. A per-test t.TempDir would
// then race its own RemoveAll ("directory not empty"), and the restored
// SCHEDULES_FILE would send late writes into the source tree. One root removed
// after all tests have finished avoids both.
var testTempRoot string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lg-scheduler-test")
	if err != nil {
		panic(err)
	}
	testTempRoot = dir
	os.Setenv("SCHEDULES_FILE", filepath.Join(dir, "fallback-schedules.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testSchedulesPath returns a unique, still-absent schedules file path inside
// testTempRoot.
func testSchedulesPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(testTempRoot, "sched")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	return filepath.Join(dir, "schedules.json")
}

// newSchedulerTestServer returns a fresh Server, an httptest.Server exposing the
// scheduler routes, and the path of the (initially absent) schedules file.
func newSchedulerTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	// Keep every test hermetic: no API keys, no origin allowlist, and a
	// schedules file inside the test's own temp dir.
	t.Setenv("AGENT_API_KEY", "")
	t.Setenv("CLIENT_API_KEY", "")
	t.Setenv("ALLOWED_ORIGINS", "")
	path := testSchedulesPath(t)
	t.Setenv("SCHEDULES_FILE", path)

	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/api/schedules", srv.handleSchedules)
	mux.HandleFunc("/api/schedules/", srv.handleSchedule)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return srv, ts, path
}

// setSchedTimings shortens the ticker interval / stuck-run timeout for a test
// and restores the production values afterwards. Must be called before any
// ticker is started.
func setSchedTimings(t *testing.T, tick, timeout time.Duration) {
	t.Helper()
	oldTick, oldTimeout := scheduleTickInterval, scheduleRunTimeout
	scheduleTickInterval, scheduleRunTimeout = tick, timeout
	t.Cleanup(func() {
		scheduleTickInterval, scheduleRunTimeout = oldTick, oldTimeout
	})
}

// startTicker runs the schedule ticker and stops it via its done channel when
// the test ends.
func startTicker(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go srv.scheduleTicker(done)
	t.Cleanup(func() { close(done) })
}

// fakeAgent is a minimal agent: it registers, then reports every command the
// server sends on cmds. It never answers with output/done unless the test
// explicitly does so, which keeps runs "in flight".
type fakeAgent struct {
	conn      *websocket.Conn
	cmds      chan CommandRequest
	closeOnce sync.Once
}

func dialFakeAgent(t *testing.T, ts *httptest.Server, name string) *fakeAgent {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial agent ws: %v", err)
	}
	reg, _ := json.Marshal(map[string]any{
		"name": name, "location": "Testville", "flag": "🏁",
		"ipv4": "192.0.2.10", "provider": "TestNet",
	})
	env, _ := json.Marshal(AgentMessage{Action: "register", Payload: reg})
	if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
		t.Fatalf("send register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read register ack: %v", err)
	}
	if !strings.Contains(string(msg), `"registered"`) {
		t.Fatalf("unexpected register ack: %s", msg)
	}
	conn.SetReadDeadline(time.Time{})

	fa := &fakeAgent{conn: conn, cmds: make(chan CommandRequest, 16)}
	go fa.readLoop()
	t.Cleanup(fa.close)
	return fa
}

func (fa *fakeAgent) readLoop() {
	for {
		_, msg, err := fa.conn.ReadMessage()
		if err != nil {
			return
		}
		var env AgentMessage
		if json.Unmarshal(msg, &env) != nil || env.Action != "command" {
			continue
		}
		var req CommandRequest
		if json.Unmarshal(env.Payload, &req) != nil {
			continue
		}
		select {
		case fa.cmds <- req:
		default:
		}
	}
}

// close drops the TCP connection without a close handshake — the "agent died
// mid-run" case.
func (fa *fakeAgent) close() { fa.closeOnce.Do(func() { fa.conn.Close() }) }

func (fa *fakeAgent) awaitCommand(t *testing.T) CommandRequest {
	t.Helper()
	select {
	case req := <-fa.cmds:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("agent never received a command")
		return CommandRequest{}
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForNodeID(t *testing.T, srv *Server, name string) string {
	t.Helper()
	var id string
	waitFor(t, "node "+name+" to register", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		for _, n := range srv.nodes {
			if n.Name == name && n.Online {
				id = n.ID
				return true
			}
		}
		return false
	})
	return id
}

// addSchedule inserts a schedule directly, bypassing the create handler so a
// test can build states the API would reject (e.g. a disallowed command).
func addSchedule(srv *Server, nodeID, command, target string) *Schedule {
	sc := &Schedule{
		ID:          uuid.New().String(),
		NodeID:      nodeID,
		NodeName:    "Testville",
		Command:     command,
		Target:      target,
		IntervalSec: scheduleMinInterval,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	srv.schedulesMu.Lock()
	srv.schedules[sc.ID] = sc
	srv.schedulesMu.Unlock()
	return sc
}

func scheduleState(srv *Server, sc *Schedule) (status, result, runningCmdID string) {
	srv.schedulesMu.RLock()
	defer srv.schedulesMu.RUnlock()
	return sc.LastStatus, sc.LastResult, sc.runningCmdID
}

func scheduleRunCount(srv *Server) int {
	srv.scheduleRunsMu.RLock()
	defer srv.scheduleRunsMu.RUnlock()
	return len(srv.scheduleRuns)
}

func cmdNodeExists(srv *Server, cmdID string) bool {
	srv.cmdNodesMu.RLock()
	defer srv.cmdNodesMu.RUnlock()
	_, ok := srv.cmdNodes[cmdID]
	return ok
}

func postRun(t *testing.T, ts *httptest.Server, id string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/api/schedules/"+id+"/run", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /run: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func writeSchedulesFile(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// ---- (a) agent disconnect mid-run ------------------------------------------

func TestScheduleAgentDisconnectMidRun(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-a")
	nodeID := waitForNodeID(t, srv, "node-a")

	sc := addSchedule(srv, nodeID, "ping", "192.0.2.1")
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the run: %q", reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	fa.awaitCommand(t)

	// Kill the agent connection while the run is in flight.
	fa.close()

	waitFor(t, "schedule to be finalized as agent_offline", 5*time.Second, func() bool {
		status, _, _ := scheduleState(srv, sc)
		return status == "agent_offline"
	})
	status, result, _ := scheduleState(srv, sc)
	if status != "agent_offline" {
		t.Fatalf("status = %q, want agent_offline", status)
	}
	if result != "agent disconnected mid-run" {
		t.Errorf("result = %q, want %q", result, "agent disconnected mid-run")
	}
	if n := scheduleRunCount(srv); n != 0 {
		t.Errorf("scheduleRuns has %d entries, want 0", n)
	}
	if cmdNodeExists(srv, cmdID) {
		t.Errorf("cmdNodes still holds %s", cmdID)
	}
}

// ---- (b) a persisted "running" status re-runs on the first tick -------------

func TestLoadSchedulesResetsRunningAndDispatches(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	setSchedTimings(t, 20*time.Millisecond, 11*time.Minute)
	fa := dialFakeAgent(t, ts, "node-b")
	nodeID := waitForNodeID(t, srv, "node-b")

	writeSchedulesFile(t, path, []map[string]any{{
		"id":           "sched-running",
		"node_id":      nodeID,
		"node_name":    "Testville",
		"command":      "ping",
		"target":       "192.0.2.1",
		"interval_sec": 60,
		"enabled":      true,
		"created_at":   time.Now().Add(-time.Hour),
		"last_status":  "running",
	}})
	srv.loadSchedules()

	srv.schedulesMu.RLock()
	sc, ok := srv.schedules["sched-running"]
	srv.schedulesMu.RUnlock()
	if !ok {
		t.Fatal("schedule was not loaded")
	}
	if status, _, _ := scheduleState(srv, sc); status != "" {
		t.Fatalf("LastStatus after load = %q, want empty", status)
	}

	startTicker(t, srv)
	req := fa.awaitCommand(t)
	if req.Type != "ping" || req.Target != "192.0.2.1" {
		t.Errorf("dispatched command = %+v, want ping 192.0.2.1", req)
	}
	if status, _, _ := scheduleState(srv, sc); status != "running" {
		t.Errorf("status during dispatch = %q, want running", status)
	}
}

// ---- (c) watchdog finalizes a stuck run ------------------------------------

func TestWatchdogFinalizesStaleRun(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	setSchedTimings(t, 10*time.Millisecond, 20*time.Millisecond)

	sc := addSchedule(srv, "node-that-never-answers", "ping", "192.0.2.1")
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the run: %q", reason)
	}

	startTicker(t, srv)

	waitFor(t, "watchdog to time the run out", 5*time.Second, func() bool {
		status, _, _ := scheduleState(srv, sc)
		return status == "error"
	})
	status, result, _ := scheduleState(srv, sc)
	if status != "error" || result != "run timed out" {
		t.Fatalf("status/result = %q/%q, want error/run timed out", status, result)
	}
	if n := scheduleRunCount(srv); n != 0 {
		t.Errorf("scheduleRuns has %d entries, want 0", n)
	}
	if cmdNodeExists(srv, cmdID) {
		t.Errorf("cmdNodes still holds %s", cmdID)
	}
}

// ---- (c2) a run that restarted between scan and finalize is left alone ------

func TestWatchdogSkipsRestartedRun(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)

	sc := addSchedule(srv, "node-x", "ping", "192.0.2.1")
	firstCmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the first run: %q", reason)
	}
	// Make the first run look stuck.
	srv.schedulesMu.Lock()
	sc.runStartedAt = time.Now().Add(-time.Hour)
	srv.schedulesMu.Unlock()

	stale := srv.collectStaleRuns(time.Now())
	if len(stale) != 1 || stale[0].cmdID != firstCmdID {
		t.Fatalf("collectStaleRuns = %+v, want one entry for %s", stale, firstCmdID)
	}

	// Between the scan and the finalize step the run completes and a new one
	// starts with a fresh cmdID.
	srv.clearRunRegistration(firstCmdID)
	srv.finalizeSchedule(sc, "ok", "finished normally")
	secondCmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the second run: %q", reason)
	}

	srv.finalizeStaleRun(stale[0].sc, stale[0].cmdID)

	status, _, running := scheduleState(srv, sc)
	if status != "running" {
		t.Fatalf("status = %q, want running (new run must survive the watchdog)", status)
	}
	if running != secondCmdID {
		t.Fatalf("runningCmdID = %q, want %q", running, secondCmdID)
	}
	srv.scheduleRunsMu.RLock()
	_, stillRegistered := srv.scheduleRuns[secondCmdID]
	srv.scheduleRunsMu.RUnlock()
	if !stillRegistered {
		t.Error("new run was unregistered by the watchdog")
	}
	if !cmdNodeExists(srv, secondCmdID) {
		t.Error("cmdNodes entry for the new run was deleted by the watchdog")
	}
}

// ---- (d) manual /run cannot double-dispatch --------------------------------

func TestManualRunRejectsSecondConcurrentRun(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-d")
	nodeID := waitForNodeID(t, srv, "node-d")
	sc := addSchedule(srv, nodeID, "ping", "192.0.2.1")

	code, body := postRun(t, ts, sc.ID)
	if code != 200 {
		t.Fatalf("first /run: status %d body %s", code, body)
	}
	// Registration is synchronous, so this is true the instant /run returns.
	if n := scheduleRunCount(srv); n != 1 {
		t.Fatalf("scheduleRuns = %d immediately after first /run, want 1", n)
	}

	code, body = postRun(t, ts, sc.ID)
	if code != 409 {
		t.Fatalf("second /run: status %d body %s, want 409", code, body)
	}
	if !strings.Contains(body, "already running") {
		t.Errorf("second /run body = %s, want 'already running'", body)
	}
	if n := scheduleRunCount(srv); n != 1 {
		t.Errorf("scheduleRuns = %d after rejected /run, want 1", n)
	}
	fa.awaitCommand(t)
	select {
	case req := <-fa.cmds:
		t.Fatalf("agent received a second command %+v", req)
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- (d2) /run on a disallowed command -------------------------------------

func TestManualRunRejectsDisallowedCommand(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	sc := addSchedule(srv, "node-y", "iperf3", "192.0.2.1")

	code, body := postRun(t, ts, sc.ID)
	if code != 400 {
		t.Fatalf("/run status %d body %s, want 400", code, body)
	}
	if !strings.Contains(body, scheduleNotAllowedResult) {
		t.Errorf("body = %s, want %q", body, scheduleNotAllowedResult)
	}
	status, result, _ := scheduleState(srv, sc)
	if status != "error" {
		t.Errorf("status = %q, want error", status)
	}
	if result != scheduleNotAllowedResult {
		t.Errorf("result = %q, want %q", result, scheduleNotAllowedResult)
	}
	if n := scheduleRunCount(srv); n != 0 {
		t.Errorf("scheduleRuns = %d, want 0", n)
	}
}

// ---- (e) disallowed persisted entries are kept, disabled, never due --------

func TestLoadKeepsDisallowedScheduleDisabled(t *testing.T) {
	srv, _, path := newSchedulerTestServer(t)
	writeSchedulesFile(t, path, []map[string]any{
		{
			"id": "sched-iperf", "node_id": "node-z", "node_name": "Testville",
			"command": "iperf3", "target": "192.0.2.1", "interval_sec": 60,
			"enabled": true, "created_at": time.Now().Add(-time.Hour),
			"last_status": "running",
		},
		{
			"id": "sched-ping", "node_id": "node-z", "node_name": "Testville",
			"command": "ping", "target": "192.0.2.1", "interval_sec": 60,
			"enabled": true, "created_at": time.Now().Add(-time.Hour),
		},
	})
	srv.loadSchedules()

	srv.schedulesMu.RLock()
	bad, okBad := srv.schedules["sched-iperf"]
	_, okGood := srv.schedules["sched-ping"]
	srv.schedulesMu.RUnlock()
	if !okBad || !okGood {
		t.Fatalf("schedules missing after load: iperf=%v ping=%v", okBad, okGood)
	}
	srv.schedulesMu.RLock()
	enabled, status, result := bad.Enabled, bad.LastStatus, bad.LastResult
	srv.schedulesMu.RUnlock()
	if enabled {
		t.Error("disallowed schedule is still enabled")
	}
	if status != "error" || result != scheduleNotAllowedResult {
		t.Errorf("disallowed schedule status/result = %q/%q, want error/%q", status, result, scheduleNotAllowedResult)
	}

	srv.saveSchedules()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	var rewritten []scheduleView
	if err := json.Unmarshal(data, &rewritten); err != nil {
		t.Fatalf("parse rewritten file: %v", err)
	}
	var found *scheduleView
	for i := range rewritten {
		if rewritten[i].ID == "sched-iperf" {
			found = &rewritten[i]
		}
	}
	if found == nil {
		t.Fatalf("disallowed schedule was deleted from %s: %s", path, data)
	}
	if found.Enabled {
		t.Error("rewritten file has enabled:true for the disallowed schedule")
	}
	if found.Command != "iperf3" {
		t.Errorf("rewritten command = %q, want iperf3", found.Command)
	}

	due := srv.collectDueSchedules(time.Now())
	if len(due) != 1 || due[0].ID != "sched-ping" {
		ids := make([]string, 0, len(due))
		for _, sc := range due {
			ids = append(ids, sc.ID)
		}
		t.Fatalf("due set = %v, want [sched-ping] only", ids)
	}
}

// ---- (f) finalize concurrent with beginRun ---------------------------------

func TestFinalizeConcurrentWithBeginRun(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	sc := addSchedule(srv, "node-f", "ping", "192.0.2.1")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			cmdID, reason := srv.beginRun(sc)
			if reason == "" {
				srv.clearRunRegistration(cmdID)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			srv.finalizeSchedule(sc, "ok", "concurrent finalize")
		}
	}()
	wg.Wait()

	if n := scheduleRunCount(srv); n != 0 {
		t.Errorf("scheduleRuns leaked %d entries", n)
	}
}
