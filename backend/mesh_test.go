package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- harness ----------------------------------------------------------------
//
// Same shape as the scheduler/summary harnesses: a real Server behind an
// httptest.Server, with fake agents on real gorilla WebSockets so node.conn /
// node.mu and the disconnect path are genuine. Everything here is prefixed
// "mesh" to stay clear of the helpers in scheduler_test.go, summary_test.go,
// public_test.go, auth_test.go, history_test.go and alerts_test.go, whose
// generic helpers (waitFor, waitForNodeID, summarySeenCount) are reused.

const meshTestKey = "mesh-secret-key"

// meshOKSummary is what a healthy 3-ping probe reports; meshLossSummary is a
// fully black-holed target (the agent omits avg_ms at 100 % loss).
const meshLossSummary = `{"sent":3,"received":0,"loss_pct":100}`

func meshOKSummary(avgMs float64) string {
	b, _ := json.Marshal(map[string]any{
		"sent": 3, "received": 3, "loss_pct": 0, "avg_ms": avgMs,
	})
	return string(b)
}

func newMeshTestServer(t *testing.T, env map[string]string) (*Server, *httptest.Server) {
	t.Helper()
	settings := map[string]string{
		"AGENT_API_KEY":     "",
		"CLIENT_API_KEY":    "",
		"ALLOWED_ORIGINS":   "",
		"PUBLIC_MODE":       "",
		"STRICT_ORIGIN":     "",
		"TRUST_PROXY":       "",
		"MESH_INTERVAL_SEC": "",
	}
	for k, v := range env {
		settings[k] = v
	}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	t.Setenv("SCHEDULES_FILE", testSchedulesPath(t))

	srv := NewServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/api/nodes", srv.handleNodes)
	mux.HandleFunc("/api/latency-matrix", srv.handleLatencyMatrix)
	mux.HandleFunc("/api/latency-matrix/measure", srv.handleLatencyMatrixMeasure)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

// setMeshTimings shortens the probe deadline and the ticker interval for one
// test. The cleanup waits for any in-flight run before restoring the package
// vars — a surviving run goroutine reading them while the restore writes them
// is a data race the -race build would (correctly) report.
func setMeshTimings(t *testing.T, srv *Server, probeTimeout, tick time.Duration) {
	t.Helper()
	oldProbe, oldTick := meshProbeTimeout, meshTickInterval
	meshProbeTimeout, meshTickInterval = probeTimeout, tick
	t.Cleanup(func() {
		deadline := time.Now().Add(15 * time.Second)
		for srv.meshInProgress() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		meshProbeTimeout, meshTickInterval = oldProbe, oldTick
	})
}

// meshAgent is a fake agent that can answer probes, stay silent, or record how
// many commands were in flight at once.
type meshAgent struct {
	conn *websocket.Conn
	name string
	id   string

	// auto replies to every command with summary+done after replyDelay.
	auto       bool
	avgMs      float64
	summary    string // overrides the avgMs summary when non-empty
	replyDelay time.Duration

	writeMu sync.Mutex

	mu          sync.Mutex
	received    []CommandRequest
	cancels     []string
	inflight    int
	maxInflight int

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func dialMeshAgent(t *testing.T, srv *Server, ts *httptest.Server, name, ipv4, ipv6 string) *meshAgent {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial /ws/agent: %v", err)
	}
	reg, _ := json.Marshal(map[string]any{
		"name": name, "location": "Testville", "flag": "🏁",
		"ipv4": ipv4, "ipv6": ipv6, "provider": "TestNet",
	})
	env, _ := json.Marshal(AgentMessage{Action: "register", Payload: reg})
	if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
		t.Fatalf("send register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read register ack: %v", err)
	} else if !strings.Contains(string(msg), `"registered"`) {
		t.Fatalf("unexpected register ack: %s", msg)
	}
	conn.SetReadDeadline(time.Time{})

	a := &meshAgent{conn: conn, name: name, avgMs: 1}
	a.id = waitForNodeID(t, srv, name)
	go a.readLoop()
	t.Cleanup(a.close)
	return a
}

func (a *meshAgent) readLoop() {
	for {
		_, msg, err := a.conn.ReadMessage()
		if err != nil {
			return
		}
		var env AgentMessage
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		switch env.Action {
		case "command":
			var req CommandRequest
			if json.Unmarshal(env.Payload, &req) != nil {
				continue
			}
			a.mu.Lock()
			a.received = append(a.received, req)
			a.inflight++
			if a.inflight > a.maxInflight {
				a.maxInflight = a.inflight
			}
			auto, delay := a.auto, a.replyDelay
			a.mu.Unlock()
			if auto {
				a.wg.Add(1)
				go func(id string) {
					defer a.wg.Done()
					if delay > 0 {
						time.Sleep(delay)
					}
					a.answer(id)
				}(req.ID)
			}
		case "cancel":
			var p struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			a.mu.Lock()
			a.cancels = append(a.cancels, p.ID)
			a.mu.Unlock()
		}
	}
}

// answer sends the configured summary followed by a clean `done`, then marks
// the command as no longer in flight.
func (a *meshAgent) answer(cmdID string) {
	a.mu.Lock()
	summary, avg := a.summary, a.avgMs
	a.mu.Unlock()
	if summary == "" {
		summary = meshOKSummary(avg)
	}
	a.send(cmdID, "summary", summary)
	a.send(cmdID, "done", `{"exit_ok":true}`)
	a.mu.Lock()
	a.inflight--
	a.mu.Unlock()
}

func (a *meshAgent) send(cmdID, typ, data string) {
	payload, _ := json.Marshal(CommandResponse{ID: cmdID, Type: typ, Data: data})
	env, _ := json.Marshal(AgentMessage{Action: "output", Payload: payload})
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	// A closed connection here is expected in the disconnect tests.
	_ = a.conn.WriteMessage(websocket.TextMessage, env)
	a.conn.SetWriteDeadline(time.Time{})
}

// close drops the TCP connection without a close handshake — the "agent died
// mid-run" case.
func (a *meshAgent) close() {
	a.closeOnce.Do(func() {
		a.conn.Close()
		a.wg.Wait()
	})
}

func (a *meshAgent) commandCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.received)
}

func (a *meshAgent) commands() []CommandRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]CommandRequest(nil), a.received...)
}

func (a *meshAgent) peakInflight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxInflight
}

func (a *meshAgent) cancelled() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.cancels...)
}

func (a *meshAgent) enableAuto(avgMs float64, delay time.Duration) {
	a.mu.Lock()
	a.auto, a.avgMs, a.replyDelay = true, avgMs, delay
	a.mu.Unlock()
}

// ---- state helpers ----------------------------------------------------------

func meshRunsCount(srv *Server) int {
	srv.meshRunsMu.RLock()
	defer srv.meshRunsMu.RUnlock()
	return len(srv.meshRuns)
}

func meshCmdNodesCount(srv *Server) int {
	srv.cmdNodesMu.RLock()
	defer srv.cmdNodesMu.RUnlock()
	return len(srv.cmdNodes)
}

func meshCellCount(srv *Server) int {
	n := 0
	for _, row := range srv.snapshotMatrix() {
		n += len(row)
	}
	return n
}

func meshAwaitIdle(t *testing.T, srv *Server) {
	t.Helper()
	waitFor(t, "mesh run to finish", 20*time.Second, func() bool { return !srv.meshInProgress() })
}

// meshMeasure POSTs (or uses another method) to the measure endpoint.
func meshMeasure(t *testing.T, ts *httptest.Server, method, key string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+"/api/latency-matrix/measure", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("measure request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// meshGetMatrix fetches GET /api/latency-matrix and returns the decoded cells.
func meshGetMatrix(t *testing.T, ts *httptest.Server) map[string]map[string]map[string]any {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/latency-matrix")
	if err != nil {
		t.Fatalf("get matrix: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/latency-matrix = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Nodes   []map[string]any                     `json:"nodes"`
		Latency map[string]map[string]map[string]any `json:"latency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	return body.Latency
}

// ---- target validation ------------------------------------------------------

func TestMeshNodeAddressAcceptsOnlyIPLiterals(t *testing.T) {
	cases := []struct {
		name       string
		ipv4, ipv6 string
		want       string
		ok         bool
	}{
		{"ipv4 preferred", "192.0.2.10", "2001:db8::1", "192.0.2.10", true},
		{"ipv6 fallback", "", "2001:db8::1", "2001:db8::1", true},
		{"no address", "", "", "", false},
		{"hostname", "evil.example.com", "", "", false},
		{"hostname in v6 slot", "", "evil.example.com", "", false},
		{"html escaped", "&lt;script&gt;", "&amp;", "", false},
		{"truncated literal", "192.0.2.1 ; ping evil.example.com", "", "", false},
		{"cidr", "192.0.2.0/24", "", "", false},
		{"port suffix", "192.0.2.10:53", "", "", false},
		{"canonicalised", "::ffff:192.0.2.7", "", "192.0.2.7", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := meshNodeAddress(tc.ipv4, tc.ipv6)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("meshNodeAddress(%q,%q) = (%q,%v), want (%q,%v)", tc.ipv4, tc.ipv6, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMeshHostileAddressProducesNoProbe(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 2*time.Second, 0)

	good := dialMeshAgent(t, srv, ts, "mesh-good-a", "192.0.2.11", "")
	good.enableAuto(11, 0)
	good2 := dialMeshAgent(t, srv, ts, "mesh-good-b", "192.0.2.12", "")
	good2.enableAuto(12, 0)
	// A hostile agent registers a hostname and an HTML-escaped value. Without
	// the net.ParseIP gate the server would ping evil.example.com from every
	// node in the fleet, on a timer.
	evil := dialMeshAgent(t, srv, ts, "mesh-evil", "evil.example.com", "&lt;script&gt;")
	evil.enableAuto(99, 0)

	pairs, result := srv.startMeshRun()
	if result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	if pairs != 2 {
		t.Fatalf("pairs = %d, want 2 (the hostile node must not be in the mesh)", pairs)
	}
	meshAwaitIdle(t, srv)

	if n := evil.commandCount(); n != 0 {
		t.Fatalf("hostile node received %d commands, want 0", n)
	}
	for _, a := range []*meshAgent{good, good2} {
		for _, cmd := range a.commands() {
			if cmd.Target == "evil.example.com" || strings.Contains(cmd.Target, "&") {
				t.Fatalf("agent %s was asked to ping %q", a.name, cmd.Target)
			}
		}
	}
	// The two well-formed nodes still measured each other.
	if n := meshCellCount(srv); n != 2 {
		t.Fatalf("matrix has %d cells, want 2", n)
	}
	if row, ok := srv.snapshotMatrix()[evil.id]; ok && len(row) > 0 {
		t.Fatalf("hostile node has a matrix row: %v", row)
	}
}

func TestMeshNodeWithoutAddressIsSkipped(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 2*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-addr-a", "192.0.2.21", "")
	a.enableAuto(21, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-addr-b", "192.0.2.22", "")
	b.enableAuto(22, 0)
	none := dialMeshAgent(t, srv, ts, "mesh-addr-none", "", "")
	none.enableAuto(23, 0)

	pairs, result := srv.startMeshRun()
	if result != meshStarted || pairs != 2 {
		t.Fatalf("startMeshRun = (%d,%v), want (2, meshStarted)", pairs, result)
	}
	meshAwaitIdle(t, srv)

	if n := none.commandCount(); n != 0 {
		t.Fatalf("addressless node received %d commands, want 0", n)
	}
	if n := meshCellCount(srv); n != 2 {
		t.Fatalf("matrix has %d cells, want 2", n)
	}
}

func TestMeshFewerThanTwoUsableNodesIsANoOp(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 2*time.Second, 0)

	lonely := dialMeshAgent(t, srv, ts, "mesh-lonely", "192.0.2.31", "")
	lonely.enableAuto(31, 0)
	noAddr := dialMeshAgent(t, srv, ts, "mesh-lonely-noaddr", "", "")
	noAddr.enableAuto(32, 0)

	pairs, result := srv.startMeshRun()
	if result != meshNotEnoughNodes || pairs != 0 {
		t.Fatalf("startMeshRun = (%d,%v), want (0, meshNotEnoughNodes)", pairs, result)
	}
	if srv.meshInProgress() {
		t.Fatal("the run slot was not released after a no-op run")
	}
	if n := lonely.commandCount() + noAddr.commandCount(); n != 0 {
		t.Fatalf("%d commands dispatched for a no-op run, want 0", n)
	}
	if n := meshCellCount(srv); n != 0 {
		t.Fatalf("matrix has %d cells, want 0", n)
	}
}

// ---- a full run -------------------------------------------------------------

func TestMeshThreeNodesProduceSixProbesAndAFullMatrix(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 5*time.Second, 0)

	agents := []*meshAgent{
		dialMeshAgent(t, srv, ts, "mesh-full-a", "192.0.2.41", ""),
		dialMeshAgent(t, srv, ts, "mesh-full-b", "192.0.2.42", ""),
		// IPv6-only: unreachable for the old browser loop, fine for the mesh.
		dialMeshAgent(t, srv, ts, "mesh-full-c", "", "2001:db8::43"),
	}
	want := map[string]float64{}
	for i, a := range agents {
		avg := float64(10 * (i + 1))
		a.enableAuto(avg, 0)
		want[a.id] = avg
	}

	pairs, result := srv.startMeshRun()
	if result != meshStarted || pairs != 6 {
		t.Fatalf("startMeshRun = (%d,%v), want (6, meshStarted)", pairs, result)
	}
	meshAwaitIdle(t, srv)

	total := 0
	for _, a := range agents {
		if n := a.commandCount(); n != 2 {
			t.Fatalf("agent %s ran %d probes, want 2", a.name, n)
		}
		total += a.commandCount()
		for _, cmd := range a.commands() {
			if cmd.Type != "ping" || cmd.Options != "count=3" {
				t.Fatalf("agent %s got %+v, want a ping with count=3", a.name, cmd)
			}
		}
	}
	if total != 6 {
		t.Fatalf("%d probes dispatched, want exactly 6", total)
	}

	matrix := srv.snapshotMatrix()
	for _, from := range agents {
		for _, to := range agents {
			if from.id == to.id {
				if _, exists := matrix[from.id][to.id]; exists {
					t.Fatalf("self-cell %s→%s exists", from.name, to.name)
				}
				continue
			}
			cell, ok := matrix[from.id][to.id]
			if !ok {
				t.Fatalf("missing matrix cell %s→%s", from.name, to.name)
			}
			if cell.LatencyMs != want[from.id] {
				t.Fatalf("cell %s→%s = %v ms, want %v (the source agent's avg_ms)", from.name, to.name, cell.LatencyMs, want[from.id])
			}
			if cell.Source != "mesh" {
				t.Fatalf("cell %s→%s source = %q, want \"mesh\"", from.name, to.name, cell.Source)
			}
			if cell.MeasuredAt.IsZero() {
				t.Fatalf("cell %s→%s has no measured_at", from.name, to.name)
			}
		}
	}

	// Every registration is gone once the run is over.
	if n := meshRunsCount(srv); n != 0 {
		t.Fatalf("meshRuns has %d entries after the run, want 0", n)
	}
	if n := meshCmdNodesCount(srv); n != 0 {
		t.Fatalf("cmdNodes has %d entries after the run, want 0", n)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Fatalf("summarySeen has %d entries after the run, want 0", n)
	}
}

func TestMeshFullLossRecordsNoValue(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 5*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-loss-a", "192.0.2.51", "")
	b := dialMeshAgent(t, srv, ts, "mesh-loss-b", "192.0.2.52", "")
	// A black-holed target: the agent reports 100 % loss and no avg_ms.
	a.mu.Lock()
	a.summary = meshLossSummary
	a.mu.Unlock()
	a.enableAuto(0, 0)
	b.enableAuto(52, 0)

	if _, result := srv.startMeshRun(); result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	meshAwaitIdle(t, srv)

	matrix := srv.snapshotMatrix()
	if cell, ok := matrix[a.id][b.id]; ok {
		t.Fatalf("100%% loss recorded a matrix value: %+v — a dead link must not look like 0 ms", cell)
	}
	if cell, ok := matrix[b.id][a.id]; !ok || cell.LatencyMs != 52 {
		t.Fatalf("the healthy direction is missing or wrong: %+v (ok=%v)", cell, ok)
	}
	if n := meshRunsCount(srv); n != 0 {
		t.Fatalf("meshRuns has %d entries, want 0", n)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Fatalf("summarySeen has %d entries, want 0", n)
	}
}

func TestMeshPerSourceConcurrencyCapHolds(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 10*time.Second, 0)

	const nodes = 6
	agents := make([]*meshAgent, 0, nodes)
	for i := 0; i < nodes; i++ {
		a := dialMeshAgent(t, srv, ts, "mesh-cap-"+string(rune('a'+i)), "192.0.2.6"+string(rune('0'+i)), "")
		// A deliberate delay so probes from one source genuinely overlap.
		a.enableAuto(float64(60+i), 40*time.Millisecond)
		agents = append(agents, a)
	}

	pairs, result := srv.startMeshRun()
	if result != meshStarted || pairs != nodes*(nodes-1) {
		t.Fatalf("startMeshRun = (%d,%v), want (%d, meshStarted)", pairs, result, nodes*(nodes-1))
	}
	meshAwaitIdle(t, srv)

	peak := 0
	for _, a := range agents {
		if n := a.commandCount(); n != nodes-1 {
			t.Fatalf("agent %s ran %d probes, want %d — waiting pairs must never be dropped", a.name, n, nodes-1)
		}
		if p := a.peakInflight(); p > meshPerNodeConcurrency {
			t.Fatalf("agent %s saw %d concurrent probes, cap is %d", a.name, p, meshPerNodeConcurrency)
		} else if p > peak {
			peak = p
		}
	}
	if peak < 2 {
		t.Fatalf("peak concurrency was %d — the run was serial, so the cap was never exercised", peak)
	}
	if n := meshCellCount(srv); n != nodes*(nodes-1) {
		t.Fatalf("matrix has %d cells, want %d", n, nodes*(nodes-1))
	}
}

// ---- teardown ---------------------------------------------------------------

func TestMeshProbeWithoutDoneIsDroppedAtItsDeadline(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 300*time.Millisecond, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-stuck-a", "192.0.2.71", "")
	b := dialMeshAgent(t, srv, ts, "mesh-stuck-b", "192.0.2.72", "")
	// Neither agent answers `done`; both send a summary first so the dedup
	// entry exists and has to be reaped with the rest.
	if _, result := srv.startMeshRun(); result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	waitFor(t, "both probes to be dispatched", 5*time.Second, func() bool {
		return a.commandCount() == 1 && b.commandCount() == 1
	})
	a.send(a.commands()[0].ID, "summary", meshOKSummary(71))
	b.send(b.commands()[0].ID, "summary", meshOKSummary(72))
	waitFor(t, "summaries to be accepted", 5*time.Second, func() bool { return summarySeenCount(srv) == 2 })

	meshAwaitIdle(t, srv)

	if n := meshRunsCount(srv); n != 0 {
		t.Fatalf("meshRuns has %d entries after the deadline, want 0", n)
	}
	if n := meshCmdNodesCount(srv); n != 0 {
		t.Fatalf("cmdNodes has %d entries after the deadline, want 0", n)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Fatalf("summarySeen has %d entries after the deadline, want 0", n)
	}
	if n := meshCellCount(srv); n != 0 {
		t.Fatalf("a probe that never finished wrote %d matrix cells, want 0", n)
	}
	// The agent is told to stop pinging rather than being left running.
	waitFor(t, "cancel to reach both agents", 5*time.Second, func() bool {
		return len(a.cancelled()) == 1 && len(b.cancelled()) == 1
	})
	if got := a.cancelled()[0]; got != a.commands()[0].ID {
		t.Fatalf("cancelled %q, want the probe's command id %q", got, a.commands()[0].ID)
	}
}

func TestMeshAgentDisconnectReapsItsProbes(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	// A deadline far longer than the test: only the disconnect path can end
	// these probes in time.
	setMeshTimings(t, srv, 30*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-drop-a", "192.0.2.81", "")
	b := dialMeshAgent(t, srv, ts, "mesh-drop-b", "192.0.2.82", "")

	if _, result := srv.startMeshRun(); result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	waitFor(t, "both probes to be dispatched", 5*time.Second, func() bool {
		return meshRunsCount(srv) == 2
	})
	a.send(a.commands()[0].ID, "summary", meshOKSummary(81))
	waitFor(t, "the summary to be accepted", 5*time.Second, func() bool { return summarySeenCount(srv) == 1 })

	a.close()
	b.close()

	waitFor(t, "the run to end on disconnect", 10*time.Second, func() bool { return !srv.meshInProgress() })
	if n := meshRunsCount(srv); n != 0 {
		t.Fatalf("meshRuns has %d entries after the agents died, want 0", n)
	}
	if n := meshCmdNodesCount(srv); n != 0 {
		t.Fatalf("cmdNodes has %d entries after the agents died, want 0", n)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Fatalf("summarySeen has %d entries after the agents died, want 0", n)
	}
	if n := meshCellCount(srv); n != 0 {
		t.Fatalf("an abandoned probe wrote %d matrix cells, want 0", n)
	}
}

// ---- ticker -----------------------------------------------------------------

func TestMeshIntervalZeroStartsNoTicker(t *testing.T) {
	srv, ts := newMeshTestServer(t, map[string]string{"MESH_INTERVAL_SEC": "0"})
	setMeshTimings(t, srv, time.Second, 20*time.Millisecond)

	if _, enabled := meshConfig(); enabled {
		t.Fatal("meshConfig reports enabled with MESH_INTERVAL_SEC=0")
	}

	a := dialMeshAgent(t, srv, ts, "mesh-off-a", "192.0.2.91", "")
	a.enableAuto(91, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-off-b", "192.0.2.92", "")
	b.enableAuto(92, 0)

	done := make(chan struct{})
	defer close(done)
	returned := make(chan struct{})
	go func() {
		srv.meshTicker(done)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("meshTicker did not return immediately with MESH_INTERVAL_SEC=0")
	}
	// Well past several would-be ticks.
	time.Sleep(150 * time.Millisecond)
	if n := a.commandCount() + b.commandCount(); n != 0 {
		t.Fatalf("a disabled mesh dispatched %d probes", n)
	}
}

func TestMeshTickerMeasuresOnTheInterval(t *testing.T) {
	srv, ts := newMeshTestServer(t, map[string]string{"MESH_INTERVAL_SEC": "300"})
	setMeshTimings(t, srv, 5*time.Second, 25*time.Millisecond)

	if interval, enabled := meshConfig(); !enabled || interval != 25*time.Millisecond {
		t.Fatalf("meshConfig = (%v,%v), want (25ms,true)", interval, enabled)
	}

	a := dialMeshAgent(t, srv, ts, "mesh-tick-a", "192.0.2.101", "")
	a.enableAuto(101, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-tick-b", "192.0.2.102", "")
	b.enableAuto(102, 0)

	done := make(chan struct{})
	go srv.meshTicker(done)
	t.Cleanup(func() { close(done) })

	waitFor(t, "the ticker to fill the matrix", 10*time.Second, func() bool { return meshCellCount(srv) == 2 })
	// And again on a later tick: a run is repeated, not one-shot.
	waitFor(t, "a second measurement round", 10*time.Second, func() bool { return a.commandCount() >= 2 })
}

func TestMeshTickDuringARunIsSkippedAndLoggedOnce(t *testing.T) {
	srv, _ := newMeshTestServer(t, nil)

	if !srv.tryClaimMesh() {
		t.Fatal("the first claim was refused")
	}
	if _, result := srv.startMeshRun(); result != meshBusy {
		t.Fatalf("startMeshRun during a run = %v, want meshBusy", result)
	}
	if !srv.noteMeshSkip() {
		t.Fatal("the first skipped tick of a run must be logged")
	}
	if srv.noteMeshSkip() {
		t.Fatal("the second skipped tick of the same run must not log again")
	}
	srv.releaseMesh()
	if !srv.tryClaimMesh() {
		t.Fatal("the slot was not released")
	}
	if !srv.noteMeshSkip() {
		t.Fatal("a new run must be allowed to log its first skipped tick")
	}
	srv.releaseMesh()
}

// ---- HTTP -------------------------------------------------------------------

func TestMeshMeasureEndpointStartsARunAndReportsPairs(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 5*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-http-a", "192.0.2.111", "")
	a.enableAuto(111, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-http-b", "192.0.2.112", "")
	b.enableAuto(112, 0)

	code, body, hdr := meshMeasure(t, ts, "POST", "")
	if code != 202 {
		t.Fatalf("POST measure = %d %s, want 202", code, body)
	}
	var started struct {
		Started bool `json:"started"`
		Pairs   int  `json:"pairs"`
	}
	if err := json.Unmarshal([]byte(body), &started); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if !started.Started || started.Pairs != 2 {
		t.Fatalf("body = %s, want started=true pairs=2", body)
	}
	if got := hdr.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	meshAwaitIdle(t, srv)
	if n := meshCellCount(srv); n != 2 {
		t.Fatalf("matrix has %d cells after the run, want 2", n)
	}
}

func TestMeshMeasureWhileRunningReturns409(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	// Long deadline: the first run is still in flight when the second call
	// arrives, because neither agent answers.
	setMeshTimings(t, srv, 30*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-busy-a", "192.0.2.121", "")
	b := dialMeshAgent(t, srv, ts, "mesh-busy-b", "192.0.2.122", "")

	if code, body, _ := meshMeasure(t, ts, "POST", ""); code != 202 {
		t.Fatalf("first POST = %d %s, want 202", code, body)
	}
	waitFor(t, "the first run to dispatch", 5*time.Second, func() bool { return meshRunsCount(srv) == 2 })

	code, body, hdr := meshMeasure(t, ts, "POST", "")
	if code != 409 {
		t.Fatalf("second POST = %d %s, want 409", code, body)
	}
	if !strings.Contains(body, "a measurement is already running") {
		t.Fatalf("409 body = %s", body)
	}
	if got := hdr.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	// Let the run end through the disconnect path instead of the 30 s deadline.
	a.close()
	b.close()
	meshAwaitIdle(t, srv)
	if code, _, _ := meshMeasure(t, ts, "POST", ""); code == 409 {
		t.Fatal("the run slot was never released")
	}
	meshAwaitIdle(t, srv)
}

func TestMeshMeasureWithoutEnoughNodes(t *testing.T) {
	_, ts := newMeshTestServer(t, nil)
	code, body, _ := meshMeasure(t, ts, "POST", "")
	if code != 200 {
		t.Fatalf("POST measure with no nodes = %d %s, want 200", code, body)
	}
	var out struct {
		Started bool   `json:"started"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if out.Started || out.Reason != "need at least two online nodes with an address" {
		t.Fatalf("body = %s, want started=false with the node-count reason", body)
	}
}

func TestMeshMeasureRejectsNonPOST(t *testing.T) {
	_, ts := newMeshTestServer(t, nil)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		code, body, hdr := meshMeasure(t, ts, method, "")
		if code != 405 {
			t.Fatalf("%s measure = %d %s, want 405", method, code, body)
		}
		if got := hdr.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", method, got)
		}
	}
}

func TestMeshMeasureAuthMatrix(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		key  string
		want int
	}{
		{
			name: "public mode, no credential",
			env:  map[string]string{"PUBLIC_MODE": "1", "CLIENT_API_KEY": meshTestKey},
			want: 403,
		},
		{
			name: "public mode with an empty key is public for everyone",
			env:  map[string]string{"PUBLIC_MODE": "1"},
			want: 403,
		},
		{
			name: "public mode, correct key",
			env:  map[string]string{"PUBLIC_MODE": "1", "CLIENT_API_KEY": meshTestKey},
			key:  meshTestKey,
			want: 200, // no nodes → started:false, but the request got through
		},
		{
			name: "key set, no credential",
			env:  map[string]string{"CLIENT_API_KEY": meshTestKey},
			want: 401,
		},
		{
			name: "key set, wrong credential",
			env:  map[string]string{"CLIENT_API_KEY": meshTestKey},
			key:  "wrong-key",
			want: 401,
		},
		{
			name: "key set, correct credential",
			env:  map[string]string{"CLIENT_API_KEY": meshTestKey},
			key:  meshTestKey,
			want: 200,
		},
		{
			name: "homelab: no key, no public mode",
			env:  nil,
			want: 200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newMeshTestServer(t, tc.env)
			code, body, hdr := meshMeasure(t, ts, "POST", tc.key)
			if code != tc.want {
				t.Fatalf("POST measure = %d %s, want %d", code, body, tc.want)
			}
			if got := hdr.Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestMeshMatrixCellsCarryLatencyMeasuredAtAndSource(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 5*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-cell-a", "192.0.2.131", "")
	a.enableAuto(13.5, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-cell-b", "192.0.2.132", "")
	b.enableAuto(14.5, 0)

	if _, result := srv.startMeshRun(); result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	meshAwaitIdle(t, srv)

	cell := meshGetMatrix(t, ts)[a.id][b.id]
	if cell == nil {
		t.Fatal("GET /api/latency-matrix has no cell for the measured pair")
	}
	if v, ok := cell["latency_ms"].(float64); !ok || v != 13.5 {
		t.Fatalf("latency_ms = %v (%T), want 13.5", cell["latency_ms"], cell["latency_ms"])
	}
	if src, _ := cell["source"].(string); src != "mesh" {
		t.Fatalf("source = %v, want \"mesh\"", cell["source"])
	}
	measured, _ := cell["measured_at"].(string)
	ts2, err := time.Parse(time.RFC3339Nano, measured)
	if err != nil {
		t.Fatalf("measured_at %q is not RFC3339: %v", measured, err)
	}
	if time.Since(ts2) > time.Minute {
		t.Fatalf("measured_at %v is not recent", ts2)
	}
}

func TestLegacyLatencyPostStillWritesAClientCell(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)

	a := dialMeshAgent(t, srv, ts, "mesh-legacy-a", "192.0.2.141", "")
	b := dialMeshAgent(t, srv, ts, "mesh-legacy-b", "192.0.2.142", "")

	body := `{"from_id":"` + a.id + `","to_id":"` + b.id + `","latency_ms":7.25}`
	resp, err := http.Post(ts.URL+"/api/latency-matrix", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post latency: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/latency-matrix = %d, want 200", resp.StatusCode)
	}

	cell, ok := srv.snapshotMatrix()[a.id][b.id]
	if !ok {
		t.Fatal("the legacy POST wrote no cell")
	}
	if cell.LatencyMs != 7.25 || cell.Source != "client" || cell.MeasuredAt.IsZero() {
		t.Fatalf("cell = %+v, want 7.25 ms from source \"client\" with a timestamp", cell)
	}

	// And an invalid value is still refused.
	bad := `{"from_id":"` + a.id + `","to_id":"` + b.id + `","latency_ms":-1}`
	resp2, err := http.Post(ts.URL+"/api/latency-matrix", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatalf("post bad latency: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("POST with latency -1 = %d, want 400", resp2.StatusCode)
	}
}

func TestMeshValidLatencyMs(t *testing.T) {
	ok := []float64{0, 0.5, 12.5, 60000}
	bad := []float64{-0.1, 60001, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, v := range ok {
		if !validLatencyMs(v) {
			t.Fatalf("validLatencyMs(%v) = false, want true", v)
		}
	}
	for _, v := range bad {
		if validLatencyMs(v) {
			t.Fatalf("validLatencyMs(%v) = true, want false", v)
		}
	}
}

// Node deletion must still purge the row and the column, now that cells are
// structs rather than floats.
func TestMeshNodeDeletePurgesRowAndColumn(t *testing.T) {
	srv, ts := newMeshTestServer(t, nil)
	setMeshTimings(t, srv, 5*time.Second, 0)

	a := dialMeshAgent(t, srv, ts, "mesh-del-a", "192.0.2.151", "")
	a.enableAuto(151, 0)
	b := dialMeshAgent(t, srv, ts, "mesh-del-b", "192.0.2.152", "")
	b.enableAuto(152, 0)

	if _, result := srv.startMeshRun(); result != meshStarted {
		t.Fatalf("startMeshRun = %v, want meshStarted", result)
	}
	meshAwaitIdle(t, srv)
	if n := meshCellCount(srv); n != 2 {
		t.Fatalf("matrix has %d cells before the delete, want 2", n)
	}

	// The node must be offline before it can be deleted.
	a.close()
	waitFor(t, "node a to go offline", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return !srv.nodes[a.id].Online
	})

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/nodes?id="+a.id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete node: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE /api/nodes = %d %s, want 200", resp.StatusCode, body)
	}

	if n := meshCellCount(srv); n != 0 {
		t.Fatalf("matrix still has %d cells after purging the node, want 0", n)
	}
}
