package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditAgentReconnectPreservesConnectionIdentity(t *testing.T) {
	srv, ts := s5NewServer(t)
	agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Reconnect"))
	srv.nodesMu.RLock()
	old := srv.nodes[id]
	oldConn, oldMu := old.conn, old.mu
	srv.nodesMu.RUnlock()
	agent.Close()
	waitFor(t, "offline before reconnect", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return !old.Online && !old.disconnecting
	})
	_, sameID := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Reconnect"))
	srv.nodesMu.RLock()
	replacement := srv.nodes[sameID]
	srv.nodesMu.RUnlock()
	if sameID != id || replacement == old {
		t.Fatal("reconnect must retain UUID with a fresh per-connection Node")
	}
	if old.conn != oldConn || old.mu != oldMu {
		t.Fatal("reconnect mutated connection state retained by old dispatchers")
	}
}

func TestAuditRepeatedRegistrationCannotRenameNode(t *testing.T) {
	srv, ts := s5NewServer(t)
	agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Original"))
	s5Send(t, agent, "register", s5RegisterPayload("Renamed"))
	s5Send(t, agent, "health", map[string]any{"uptime": "barrier"})
	waitFor(t, "health after repeated registration", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return srv.nodes[id].Health != nil && srv.nodes[id].Health.Uptime == "barrier"
	})
	srv.nodesMu.RLock()
	defer srv.nodesMu.RUnlock()
	if len(srv.nodes) != 1 || srv.nodes[id].Name != "Original" {
		t.Fatal("repeated registration changed the connection's identity")
	}
}

func TestAuditPersistedScheduleSurvivesServerRestart(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	writeSchedulesFile(t, path, []map[string]any{{"id": "saved", "node_id": "stable-node", "node_name": "Restored", "command": "ping", "target": "192.0.2.1", "interval_sec": 60, "enabled": true, "schema_version": 1}})
	srv.loadSchedules()
	fa := dialFakeAgent(t, ts, "Restored")
	id := waitForNodeID(t, srv, "Restored")
	if id != "stable-node" {
		t.Fatalf("node ID after restart = %s", id)
	}
	sc := srv.schedules["saved"]
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("begin restored run: %s", reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	if got := fa.awaitCommand(t); got.ID != cmdID {
		t.Fatalf("wrong command: %+v", got)
	}
	agentReply(t, fa, cmdID, "done", `{"exit_ok":true}`)
	waitFor(t, "restored schedule completes", 5*time.Second, func() bool {
		state, _, _ := scheduleState(srv, sc)
		return state == "ok"
	})
}

func TestAuditScheduleLoadNullAndUnsafeIntervals(t *testing.T) {
	srv, _, path := newSchedulerTestServer(t)
	if err := os.WriteFile(path, []byte(`[null,{"id":"low","command":"ping","interval_sec":-1,"schema_version":1},{"id":"high","command":"ping","interval_sec":9223372036854775807,"schema_version":1}]`), 0600); err != nil {
		t.Fatal(err)
	}
	srv.loadSchedules()
	if len(srv.schedules) != 2 {
		t.Fatalf("loaded %d schedules", len(srv.schedules))
	}
	if srv.schedules["low"].IntervalSec != scheduleMinInterval || srv.schedules["high"].IntervalSec != scheduleMaxInterval {
		t.Fatal("persisted intervals were not bounded before duration conversion")
	}
}

func TestAuditConcurrentScheduleCreateHonorsCap(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	srv.nodes["node"] = &Node{ID: "node", Name: "Node"}
	// Avoid making disk I/O a serialization point for concurrent API calls.
	srv.saveLastAt = time.Now()
	for i := 0; i < scheduleMaxCount-1; i++ {
		addSchedule(srv, "node", "ping", "192.0.2.1")
	}
	var wg sync.WaitGroup
	var created atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/schedules", strings.NewReader(`{"node_id":"node","command":"ping","target":"192.0.2.1","interval_sec":60}`))
			rec := httptest.NewRecorder()
			srv.handleSchedules(rec, req)
			if rec.Code == http.StatusCreated {
				created.Add(1)
			} else if rec.Code != http.StatusConflict {
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	if created.Load() != 1 || len(srv.schedules) != scheduleMaxCount {
		t.Fatalf("created=%d total=%d", created.Load(), len(srv.schedules))
	}
	srv.flushSchedules()
}

func TestAuditRejectOverflowingScheduleInterval(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	srv.nodes["node"] = &Node{ID: "node", Name: "Node"}
	for _, interval := range []int64{int64(scheduleMaxInterval) + 1, 9223372036854775807} {
		req := httptest.NewRequest("POST", "/api/schedules", strings.NewReader(fmt.Sprintf(`{"node_id":"node","command":"ping","target":"192.0.2.1","interval_sec":%d}`, interval)))
		rec := httptest.NewRecorder()
		srv.handleSchedules(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("interval %d accepted: %d", interval, rec.Code)
		}
	}
}

func TestAuditOldScheduleOutputCannotFinalizeNewRun(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	sc := addSchedule(srv, "node", "ping", "192.0.2.1")
	oldID, _ := srv.beginRun(sc)
	srv.finalizeStaleRun(sc, oldID)
	newID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatal(reason)
	}
	// Model a read loop which already obtained the old registration when the
	// watchdog removed it, then resumes after the next run has been claimed.
	srv.scheduleRunsMu.Lock()
	srv.scheduleRuns[oldID] = sc
	srv.scheduleRunsMu.Unlock()
	srv.scheduleAcceptOutput(CommandResponse{ID: oldID, Type: "output", Data: "stale"})
	srv.scheduleAcceptOutput(CommandResponse{ID: oldID, Type: "done", Data: `{"exit_ok":true}`})
	state, _, _ := scheduleState(srv, sc)
	if state != "running" || sc.runningCmdID != newID {
		t.Fatal("stale output finalized newer run")
	}
	sc.currentMu.Lock()
	buf := sc.currentBuf.String()
	sc.currentMu.Unlock()
	if buf != "" {
		t.Fatalf("stale output polluted newer run: %q", buf)
	}
	srv.clearRunRegistration(oldID)
	srv.clearRunRegistration(newID)
}

func TestAuditDeletedScheduleCannotDispatch(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "Delete")
	id := waitForNodeID(t, srv, "Delete")
	sc := addSchedule(srv, id, "ping", "192.0.2.1")
	cmdID, _ := srv.beginRun(sc)
	req := httptest.NewRequest("DELETE", "/api/schedules/"+sc.ID, nil)
	rec := httptest.NewRecorder()
	srv.handleSchedule(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	srv.dispatchSchedule(sc, cmdID)
	s12ExpectNoFurtherCommand(t, fa, 100*time.Millisecond)
	if _, reason := srv.beginRun(sc); reason != "deleted" {
		t.Fatalf("deleted schedule claim: %q", reason)
	}
}

func TestAuditRunCreateAliasEnforcesBodyLimit(t *testing.T) {
	t.Setenv("CLIENT_API_KEY", "")
	t.Setenv("PUBLIC_MODE", "")
	srv := NewServer()
	data, _ := json.Marshal(map[string]any{"command": "ping", "lines": []runLine{{Type: "output", Text: strings.Repeat("x", maxRequestBodySize+1)}}})
	req := httptest.NewRequest("POST", "/api/runs/alias", strings.NewReader(string(data)))
	rec := httptest.NewRecorder()
	srv.handleRuns(rec, req)
	if rec.Code != 400 || len(srv.runStore) != 0 {
		t.Fatalf("oversized alias accepted: %d", rec.Code)
	}
}

func TestAuditGeoRateBucketsHaveHardCap(t *testing.T) {
	srv := NewServer()
	for i := 0; i < rateEntryMaxCount; i++ {
		srv.geoRateBuckets[strconv.Itoa(i)] = &ipRateBucket{tokens: geoRateBurst, lastFill: time.Now()}
	}
	if srv.allowGeoIP("new-source") {
		t.Fatal("source admitted beyond hard cap")
	}
	if !srv.allowGeoIP("0") {
		t.Fatal("existing source refused at capacity")
	}
	if len(srv.geoRateBuckets) != rateEntryMaxCount {
		t.Fatal("rate bucket count exceeded cap")
	}
}

func TestAuditMeshIntervalCannotOverflow(t *testing.T) {
	t.Setenv("MESH_INTERVAL_SEC", "9223372036854775807")
	interval, enabled := meshConfig()
	if !enabled || interval <= 0 || interval > time.Duration(scheduleMaxInterval)*time.Second {
		t.Fatalf("invalid interval %v", interval)
	}
}

func TestAuditDoneRetiresCommandBeforeFinalFrame(t *testing.T) {
	srv, ts := s11NewServer(t)
	client := s11DialClient(t, ts)
	serverConn := s12OnlyClientConn(t, srv)
	const id = "reusable-command"
	srv.cmdOwnersMu.Lock()
	srv.cmdOwners[id] = serverConn
	srv.cmdOwnersMu.Unlock()
	srv.cmdNodesMu.Lock()
	srv.cmdNodes[id] = "node"
	srv.cmdNodesMu.Unlock()
	srv.clientsMu.RLock()
	mu := srv.clients[serverConn]
	srv.clientsMu.RUnlock()
	mu.Lock()
	done := make(chan struct{})
	go func() { srv.sendToCommandOwner(CommandResponse{ID: id, Type: "done"}); close(done) }()
	// The final write is blocked, making the cleanup ordering observable.
	retired := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s12CmdOwner(srv, id) == nil && s12CmdNode(srv, id) == "" {
			retired = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Unlock()
	<-done
	if !retired {
		t.Fatal("command remained registered until after its done frame")
	}
	frame, ok := s11ReadFrame(t, client, time.Second)
	if !ok || frame.ID != id || frame.Type != "done" {
		t.Fatalf("final frame lost: %+v", frame)
	}
}

func TestAuditScheduleRestoreReconcilesOldNodeIdentities(t *testing.T) {
	srv, _, path := newSchedulerTestServer(t)
	writeSchedulesFile(t, path, []map[string]any{
		{"id": "first", "node_id": "old-id", "node_name": "Same agent", "command": "ping", "interval_sec": 60, "schema_version": 1},
		{"id": "second", "node_id": "new-id", "node_name": "Same agent", "command": "ping", "interval_sec": 60, "schema_version": 1},
	})
	srv.loadSchedules()
	if len(srv.nodes) != 1 || srv.schedules["first"].NodeID != srv.schedules["second"].NodeID {
		t.Fatal("schedules from multiple restarts were not reconciled onto one agent identity")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved []scheduleView
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].NodeID != saved[1].NodeID {
		t.Fatal("reconciled identity not persisted")
	}
}
