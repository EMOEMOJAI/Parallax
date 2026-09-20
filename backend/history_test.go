package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// S3 — run history, the history endpoint, coalesced saves, /metrics gauges and
// the deferred scheduler items folded into this slice.
//
// The scheduler_test.go harness (newSchedulerTestServer, dialFakeAgent,
// addSchedule, waitFor, scheduleState, …) and the summary_test.go helpers
// (agentReply, runScheduleWithFrames, mtrSummaryJSON, summarySeenCount) are
// reused unchanged; only the history/metrics specific helpers live here.

// histServer is newSchedulerTestServer plus /metrics and an explicit
// environment, so the auth matrix can be driven per test. Every variable the
// handlers read is pinned, including the ones a previous test may have set.
func histServer(t *testing.T, env map[string]string) (*Server, *httptest.Server, string) {
	t.Helper()
	pinned := map[string]string{
		"AGENT_API_KEY":     "",
		"CLIENT_API_KEY":    "",
		"ALLOWED_ORIGINS":   "",
		"PUBLIC_MODE":       "",
		"METRICS_TOKEN":     "",
		"ALERT_WEBHOOK_URL": "",
	}
	for k, v := range env {
		pinned[k] = v
	}
	for k, v := range pinned {
		t.Setenv(k, v)
	}
	path := testSchedulesPath(t)
	t.Setenv("SCHEDULES_FILE", path)

	srv := NewServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/api/schedules", srv.handleSchedules)
	mux.HandleFunc("/api/schedules/", srv.handleSchedule)
	mux.HandleFunc("/metrics", srv.handleMetrics)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts, path
}

// histGet performs a GET with an optional bearer key and returns status,
// body and the response headers.
func histGet(t *testing.T, ts *httptest.Server, path, key string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// histPoints reads a schedule's history under the lock.
func histPoints(srv *Server, sc *Schedule) []historyPoint {
	srv.schedulesMu.RLock()
	defer srv.schedulesMu.RUnlock()
	return append([]historyPoint(nil), sc.History...)
}

// histStatuses is the status sequence of a schedule's history.
func histStatuses(srv *Server, sc *Schedule) []string {
	out := []string{}
	for _, p := range histPoints(srv, sc) {
		out = append(out, p.Status)
	}
	return out
}

// setSaveInterval shortens the coalescing window for a test.
func setSaveInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := scheduleSaveInterval
	scheduleSaveInterval = d
	t.Cleanup(func() { scheduleSaveInterval = old })
}

// histSummary is a tiny helper for "a summary carrying exactly these fields".
func histSummary(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	blob, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	canonical, ok := validateSummary(string(blob))
	if !ok {
		t.Fatalf("fixture summary is not valid: %s", blob)
	}
	return canonical
}

// ---- ring behaviour ---------------------------------------------------------

func TestHistoryRingKeepsTheNewest288Points(t *testing.T) {
	srv, _, _ := histServer(t, nil)
	sc := addSchedule(srv, "node-h", "ping", "1.1.1.1")

	const runs = 300
	for i := 0; i < runs; i++ {
		srv.finalizeScheduleWithSummary(sc, "ok", "run", histSummary(t, map[string]any{"avg_ms": i}))
	}

	points := histPoints(srv, sc)
	if len(points) != scheduleHistoryMax {
		t.Fatalf("history holds %d points, want %d", len(points), scheduleHistoryMax)
	}
	if points[0].RttMs == nil || *points[0].RttMs != float64(runs-scheduleHistoryMax) {
		t.Errorf("oldest kept point = %v, want avg_ms %d", points[0].RttMs, runs-scheduleHistoryMax)
	}
	if points[len(points)-1].RttMs == nil || *points[len(points)-1].RttMs != float64(runs-1) {
		t.Errorf("newest point = %v, want avg_ms %d", points[len(points)-1].RttMs, runs-1)
	}
}

// Every terminal path funnels through finalizeScheduleWithSummary, so a failed
// or lost run shows up as a point instead of a silent gap.
func TestHistoryRecordsEveryFinalizePath(t *testing.T) {
	srv, ts, _ := histServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-paths")
	nodeID := waitForNodeID(t, srv, "node-paths")
	sc := addSchedule(srv, nodeID, "ping", "1.1.1.1")

	// 1. clean run → ok
	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"summary", `{"sent":5,"received":5,"loss_pct":0,"avg_ms":10}`},
		{"done", `{"exit_ok":true}`},
	})

	// 2. agent dies mid-run → agent_offline
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the second run: %q", reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	fa.awaitCommand(t)
	fa.close()
	waitFor(t, "agent_offline finalize", 5*time.Second, func() bool {
		status, _, _ := scheduleState(srv, sc)
		return status == "agent_offline"
	})

	// 3. node gone → dispatch failure path
	srv.finalizeSchedule(sc, "error", "failed to send command")

	// 4. watchdog timeout
	cmdID, reason = srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the fourth run: %q", reason)
	}
	srv.finalizeStaleRun(sc, cmdID)

	// 5. a disallowed command is finalized by beginRun itself
	bad := addSchedule(srv, nodeID, "iperf3", "1.1.1.1")
	if _, reason := srv.beginRun(bad); reason != "not_allowed" {
		t.Fatalf("beginRun on a disallowed command = %q, want not_allowed", reason)
	}

	if got, want := histStatuses(srv, sc), []string{"ok", "agent_offline", "error", "error"}; !equalStrings(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if got, want := histStatuses(srv, bad), []string{"error"}; !equalStrings(got, want) {
		t.Errorf("not_allowed history = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rtt_ms comes from a top-level avg_ms and from nothing else: http's total_ms,
// dns's query_time_ms and mtr's per-hop avg must never be coerced into it, or
// one metric name would mix wall-clock time with ICMP latency.
func TestHistoryRttOnlyComesFromAvgMs(t *testing.T) {
	// S12: the per-schedule gauges are served only to a METRICS_TOKEN scrape.
	srv, ts, _ := histServer(t, map[string]string{"METRICS_TOKEN": "hist-tok"})

	cases := []struct {
		name    string
		command string
		summary map[string]any
		wantRtt *float64
		wantLos *float64
	}{
		{"ping", "ping", map[string]any{"loss_pct": 0, "avg_ms": 12.5}, f64(12.5), f64(0)},
		{"http", "http", map[string]any{"http_code": 200, "total_ms": 61.2}, nil, nil},
		{"dns", "dns", map[string]any{"status": "NOERROR", "query_time_ms": 23}, nil, nil},
		{"mtr", "mtr", map[string]any{"hop_count": 3, "loss_pct": 40}, nil, f64(40)},
	}
	made := make([]*Schedule, 0, len(cases))
	for _, c := range cases {
		sc := addSchedule(srv, "node-r", c.command, c.name+".example.net")
		srv.finalizeScheduleWithSummary(sc, "ok", "run", histSummary(t, c.summary))
		made = append(made, sc)

		points := histPoints(srv, sc)
		if len(points) != 1 {
			t.Fatalf("%s: %d points", c.name, len(points))
		}
		if !eqFloatPtr(points[0].RttMs, c.wantRtt) {
			t.Errorf("%s: rtt_ms = %v, want %v", c.name, deref(points[0].RttMs), deref(c.wantRtt))
		}
		if !eqFloatPtr(points[0].LossPct, c.wantLos) {
			t.Errorf("%s: loss_pct = %v, want %v", c.name, deref(points[0].LossPct), deref(c.wantLos))
		}
	}

	// The gauge follows the same rule.
	code, body, _ := histGet(t, ts, "/metrics", "hist-tok")
	if code != 200 {
		t.Fatalf("/metrics status %d", code)
	}
	for i, c := range cases {
		want := c.wantRtt != nil
		got := strings.Contains(body, fmt.Sprintf(`lookingglass_probe_rtt_ms{schedule_id="%s"`, made[i].ID))
		if got != want {
			t.Errorf("%s: _rtt_ms sample present = %v, want %v", c.name, got, want)
		}
	}
}

func f64(v float64) *float64 { return &v }

func eqFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// ---- the history endpoint ---------------------------------------------------

func TestHistoryEndpointRequiresAuthAndRefusesPublic(t *testing.T) {
	t.Run("key set, no credential", func(t *testing.T) {
		srv, ts, _ := histServer(t, map[string]string{"CLIENT_API_KEY": "hist-key"})
		sc := addSchedule(srv, "n", "ping", "1.1.1.1")
		if code, _, _ := histGet(t, ts, "/api/schedules/"+sc.ID+"/history", ""); code != 401 {
			t.Errorf("status = %d, want 401", code)
		}
		if code, _, _ := histGet(t, ts, "/api/schedules/"+sc.ID+"/history", "hist-key"); code != 200 {
			t.Errorf("status with key = %d, want 200", code)
		}
	})

	t.Run("public mode", func(t *testing.T) {
		srv, ts, _ := histServer(t, map[string]string{"PUBLIC_MODE": "1"})
		sc := addSchedule(srv, "n", "ping", "1.1.1.1")
		code, body, _ := histGet(t, ts, "/api/schedules/"+sc.ID+"/history", "")
		if code != 403 || !strings.Contains(body, "not available in public mode") {
			t.Errorf("status/body = %d/%s, want 403", code, body)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		_, ts, _ := histServer(t, nil)
		if code, _, _ := histGet(t, ts, "/api/schedules/does-not-exist/history", ""); code != 404 {
			t.Errorf("status = %d, want 404", code)
		}
	})
}

func TestScheduleListOmitsHistoryButTheEndpointServesIt(t *testing.T) {
	srv, ts, _ := histServer(t, nil)
	sc := addSchedule(srv, "node-l", "ping", "1.1.1.1")
	srv.finalizeScheduleWithSummary(sc, "ok", "run", histSummary(t, map[string]any{"avg_ms": 7, "loss_pct": 0}))

	code, body, hdr := histGet(t, ts, "/api/schedules", "")
	if code != 200 {
		t.Fatalf("list status %d", code)
	}
	if strings.Contains(body, `"history"`) {
		t.Errorf("GET /api/schedules leaked the history: %s", body)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("list Cache-Control = %q, want no-store", hdr.Get("Cache-Control"))
	}
	// The list still carries everything a card needs.
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("list is not JSON: %v", err)
	}
	if len(list) != 1 || list[0]["last_status"] != "ok" {
		t.Fatalf("list = %s", body)
	}

	code, body, hdr = histGet(t, ts, "/api/schedules/"+sc.ID+"/history", "")
	if code != 200 {
		t.Fatalf("history status %d", code)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("history Cache-Control = %q, want no-store", hdr.Get("Cache-Control"))
	}
	var resp struct {
		ID      string         `json:"id"`
		History []historyPoint `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("history is not JSON: %v", err)
	}
	if resp.ID != sc.ID || len(resp.History) != 1 {
		t.Fatalf("history response = %s", body)
	}
	if resp.History[0].RttMs == nil || *resp.History[0].RttMs != 7 {
		t.Errorf("history rtt = %v, want 7", resp.History[0].RttMs)
	}
}

// Schedule data is operator state behind an API key: no response on any of
// these routes may be cached (S8 advisory).
func TestScheduleRoutesAlwaysSetNoStore(t *testing.T) {
	srv, ts, _ := histServer(t, nil)
	sc := addSchedule(srv, "node-ns", "ping", "1.1.1.1")

	requests := []struct {
		method string
		path   string
		body   string
	}{
		{"GET", "/api/schedules", ""},
		{"GET", "/api/schedules/" + sc.ID + "/history", ""},
		{"POST", "/api/schedules/" + sc.ID + "/run", ""},
		{"POST", "/api/schedules/" + sc.ID + "/toggle", ""},
		{"DELETE", "/api/schedules/" + sc.ID, ""},
		{"POST", "/api/schedules", `{"node_id":"nope","command":"ping","target":"1.1.1.1","interval_sec":60}`},
		{"GET", "/api/schedules/does-not-exist/history", ""},
		{"PUT", "/api/schedules/" + sc.ID, ""},
	}
	for _, rq := range requests {
		var body io.Reader
		if rq.body != "" {
			body = strings.NewReader(rq.body)
		}
		req, err := http.NewRequest(rq.method, ts.URL+rq.path, body)
		if err != nil {
			t.Fatalf("build %s %s: %v", rq.method, rq.path, err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rq.method, rq.path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s (status %d) Cache-Control = %q, want no-store", rq.method, rq.path, resp.StatusCode, got)
		}
	}
}

// The view is marshalled after schedulesMu is released, so it must own its
// copy of the ring.
func TestScheduleViewsRaceWithConcurrentFinalizes(t *testing.T) {
	srv, _, _ := histServer(t, nil)
	sc := addSchedule(srv, "node-race", "ping", "1.1.1.1")
	summary := histSummary(t, map[string]any{"avg_ms": 1.5, "loss_pct": 0})

	for iter := 0; iter < 50; iter++ {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, v := range srv.scheduleViews() {
					_ = len(v.History)
				}
			}
		}()
		for i := 0; i < 300; i++ {
			srv.finalizeScheduleWithSummary(sc, "ok", "run", summary)
		}
		close(stop)
		wg.Wait()
	}
	if n := len(histPoints(srv, sc)); n != scheduleHistoryMax {
		t.Errorf("history = %d points, want %d", n, scheduleHistoryMax)
	}
}

// ---- coalesced saves --------------------------------------------------------

func TestSaveSchedulesLeadingEdgeAndTrailingWrite(t *testing.T) {
	setSaveInterval(t, 150*time.Millisecond)
	srv, _, path := histServer(t, nil)
	addSchedule(srv, "node-s", "ping", "1.1.1.1")

	// Leading edge: the first save after a quiet period is synchronous, which
	// is what every test that saves and immediately reads the file relies on.
	srv.saveSchedules()
	if n := countPersistedSchedules(t, path); n != 1 {
		t.Fatalf("after the first save the file holds %d schedules, want 1", n)
	}

	// A burst inside the window collapses: nothing is written yet …
	for i := 0; i < 5; i++ {
		addSchedule(srv, "node-s", "ping", fmt.Sprintf("10.0.0.%d", i))
		srv.saveSchedules()
	}
	if n := countPersistedSchedules(t, path); n != 1 {
		t.Fatalf("a coalesced burst wrote the file %d schedules early", n)
	}
	// … and one trailing write lands after the interval.
	waitFor(t, "the trailing write", 3*time.Second, func() bool {
		return countPersistedSchedules(t, path) == 6
	})
}

func TestShutdownFlushWritesThePendingSave(t *testing.T) {
	setSaveInterval(t, time.Hour) // nothing trailing can fire during the test
	srv, _, path := histServer(t, nil)
	addSchedule(srv, "node-f", "ping", "1.1.1.1")

	srv.saveSchedules() // leading edge, synchronous
	if n := countPersistedSchedules(t, path); n != 1 {
		t.Fatalf("first save wrote %d schedules, want 1", n)
	}
	addSchedule(srv, "node-f", "ping", "1.1.1.2")
	srv.saveSchedules() // coalesced away
	if n := countPersistedSchedules(t, path); n != 1 {
		t.Fatalf("the second save was not coalesced (%d schedules on disk)", n)
	}

	srv.flushSchedules() // what the signal handler calls before Shutdown
	if n := countPersistedSchedules(t, path); n != 2 {
		t.Fatalf("the shutdown flush wrote %d schedules, want 2", n)
	}
	// A flush with nothing pending is a no-op, not a rewrite loop.
	srv.flushSchedules()
	if n := countPersistedSchedules(t, path); n != 2 {
		t.Fatalf("flush with nothing pending changed the file (%d schedules)", n)
	}
}

func countPersistedSchedules(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read schedules file: %v", err)
	}
	var list []scheduleView
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("schedules file is not valid JSON: %v", err)
	}
	return len(list)
}

// Only the tail of the ring reaches the file, so an atomic rewrite stays small
// even with 100 schedules.
func TestOnlyTheLastPointsArePersisted(t *testing.T) {
	srv, _, path := histServer(t, nil)
	sc := addSchedule(srv, "node-p", "ping", "1.1.1.1")
	for i := 0; i < schedulePersistHistory+20; i++ {
		srv.finalizeScheduleWithSummary(sc, "ok", "run", histSummary(t, map[string]any{"avg_ms": i}))
	}
	srv.flushSchedules()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schedules file: %v", err)
	}
	var list []scheduleView
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("schedules file is not valid JSON: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("file holds %d schedules", len(list))
	}
	if len(list[0].History) != schedulePersistHistory {
		t.Fatalf("persisted %d points, want %d", len(list[0].History), schedulePersistHistory)
	}
	last := list[0].History[len(list[0].History)-1]
	if last.RttMs == nil || *last.RttMs != float64(schedulePersistHistory+19) {
		t.Errorf("persisted tail ends at %v, want the newest point", last.RttMs)
	}

	// …and the in-memory ring survives a reload.
	reloaded := NewServer()
	reloaded.loadSchedules()
	reloaded.schedulesMu.RLock()
	got := reloaded.schedules[sc.ID]
	reloaded.schedulesMu.RUnlock()
	if got == nil || len(got.History) != schedulePersistHistory {
		t.Fatalf("reloaded history = %d points", len(got.History))
	}
}

// ---- deferred items (a)–(d) -------------------------------------------------

// DELETE and toggle-off must reap the run registration of an in-flight run;
// toggle-off must leave LastStatus alone so an off→on flip cannot start a
// second run that shares the first one's buffer.
func TestDeleteAndToggleOffReapAnInFlightRun(t *testing.T) {
	for _, op := range []string{"delete", "toggle"} {
		t.Run(op, func(t *testing.T) {
			srv, ts, _ := histServer(t, nil)
			fa := dialFakeAgent(t, ts, "node-"+op)
			nodeID := waitForNodeID(t, srv, "node-"+op)
			sc := addSchedule(srv, nodeID, "ping", "1.1.1.1")

			cmdID, reason := srv.beginRun(sc)
			if reason != "" {
				t.Fatalf("beginRun refused: %q", reason)
			}
			srv.dispatchSchedule(sc, cmdID)
			fa.awaitCommand(t)
			// Give the run a summary so the dedup entry exists too.
			agentReply(t, fa, cmdID, "summary", `{"sent":5,"received":5,"loss_pct":0,"avg_ms":9}`)
			waitFor(t, "the summary to be accepted", 2*time.Second, func() bool {
				return summarySeenCount(srv) == 1
			})

			var code int
			if op == "delete" {
				req, _ := http.NewRequest("DELETE", ts.URL+"/api/schedules/"+sc.ID, nil)
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatalf("DELETE: %v", err)
				}
				resp.Body.Close()
				code = resp.StatusCode
			} else {
				resp, err := ts.Client().Post(ts.URL+"/api/schedules/"+sc.ID+"/toggle", "", nil)
				if err != nil {
					t.Fatalf("toggle: %v", err)
				}
				resp.Body.Close()
				code = resp.StatusCode
			}
			if code != 200 {
				t.Fatalf("%s returned %d", op, code)
			}

			if n := scheduleRunCount(srv); n != 0 {
				t.Errorf("scheduleRuns leaked %d entries", n)
			}
			if cmdNodeExists(srv, cmdID) {
				t.Error("cmdNodes still holds the reaped run")
			}
			if n := summarySeenCount(srv); n != 0 {
				t.Errorf("summarySeen leaked %d entries", n)
			}
			if op == "toggle" {
				// Left running on purpose: the watchdog finalizes it, and
				// until then "Run now" answers 409 (and the button is off).
				status, _, _ := scheduleState(srv, sc)
				if status != "running" {
					t.Errorf("toggle-off changed LastStatus to %q, want it left as running", status)
				}
				code, body := postRun(t, ts, sc.ID)
				if code != 409 {
					t.Errorf("/run after toggle-off = %d (%s), want 409", code, body)
				}
			}
		})
	}
}

// The epoch closes the window the cmdID check alone leaves open: a snapshot
// taken before a newer run started can never write the schedule's status, even
// if it somehow carries the current cmdID.
func TestWatchdogEpochGuardRejectsAStaleSnapshot(t *testing.T) {
	srv, _, _ := histServer(t, nil)
	sc := addSchedule(srv, "node-e", "ping", "1.1.1.1")

	firstCmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the first run: %q", reason)
	}
	srv.schedulesMu.RLock()
	firstEpoch := sc.runEpoch
	srv.schedulesMu.RUnlock()

	// The run completes and a new one starts: same schedule, new epoch.
	srv.clearRunRegistration(firstCmdID)
	srv.finalizeSchedule(sc, "ok", "finished normally")
	secondCmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the second run: %q", reason)
	}

	// A snapshot from the first run — even one holding the *current* cmdID —
	// must not finalize the schedule.
	srv.finalizeStaleRunSnapshot(staleRun{sc: sc, cmdID: secondCmdID, epoch: firstEpoch})
	status, _, running := scheduleState(srv, sc)
	if status != "running" || running != secondCmdID {
		t.Fatalf("stale snapshot finalized the new run: status=%q running=%q", status, running)
	}

	// A current snapshot still works.
	srv.schedulesMu.RLock()
	epoch := sc.runEpoch
	srv.schedulesMu.RUnlock()
	srv.finalizeStaleRunSnapshot(staleRun{sc: sc, cmdID: secondCmdID, epoch: epoch})
	status, result, _ := scheduleState(srv, sc)
	if status != "error" || result != "run timed out" {
		t.Fatalf("current snapshot did not time the run out: %q/%q", status, result)
	}
}

// A file written by an older build contains zero times spelled out in full;
// it must load and be rewritten without them.
func TestZeroTimesAreLoadedAndRewrittenAway(t *testing.T) {
	srv, _, path := histServer(t, nil)
	writeSchedulesFile(t, path, []map[string]any{{
		"id": "sched-zero", "node_id": "n1", "node_name": "n", "command": "ping",
		"target": "1.1.1.1", "interval_sec": 60, "enabled": true,
		"created_at":   time.Now().Add(-time.Hour),
		"last_run_at":  "0001-01-01T00:00:00Z",
		"next_run_at":  "0001-01-01T00:00:00Z",
		"last_status":  "",
		"last_summary": nil,
	}})
	srv.loadSchedules()
	srv.schedulesMu.RLock()
	_, ok := srv.schedules["sched-zero"]
	srv.schedulesMu.RUnlock()
	if !ok {
		t.Fatal("a schedule with zero times did not load")
	}

	srv.saveSchedules()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	if strings.Contains(string(data), "0001-01-01") {
		t.Errorf("rewritten file still serializes zero times: %s", data)
	}
	if strings.Contains(string(data), "last_run_at") || strings.Contains(string(data), "next_run_at") {
		t.Errorf("rewritten file carries empty time fields: %s", data)
	}
}

// ---- /metrics ---------------------------------------------------------------

func TestMetricsScheduleGaugeAuthMatrix(t *testing.T) {
	const key = "metrics-client-key"
	cases := []struct {
		name       string
		env        map[string]string
		credential string
		wantStatus int
		wantProbe  bool
	}{
		{
			name:       "token set, no credential",
			env:        map[string]string{"METRICS_TOKEN": "tok"},
			wantStatus: 401,
		},
		{
			name:       "token set, token supplied",
			env:        map[string]string{"METRICS_TOKEN": "tok"},
			credential: "tok",
			wantStatus: 200,
			wantProbe:  true,
		},
		{
			name:       "token empty, client key set, no credential",
			env:        map[string]string{"CLIENT_API_KEY": key},
			wantStatus: 200,
			wantProbe:  false,
		},
		{
			// S12: the client key is no longer a credential for the schedule
			// inventory — only METRICS_TOKEN is.
			name:       "token empty, client key set, key supplied",
			env:        map[string]string{"CLIENT_API_KEY": key},
			credential: key,
			wantStatus: 200,
			wantProbe:  false,
		},
		{
			name:       "token empty, public mode with no key",
			env:        map[string]string{"PUBLIC_MODE": "1"},
			wantStatus: 200,
			wantProbe:  false,
		},
		{
			// S12: the keyless deployment — this fleet's default — no longer
			// hands the schedule inventory to an unauthenticated scrape.
			name:       "token empty, no key, not public",
			env:        nil,
			wantStatus: 200,
			wantProbe:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, ts, _ := histServer(t, c.env)
			sc := addSchedule(srv, "node-m", "ping", "1.1.1.1")
			srv.finalizeScheduleWithSummary(sc, "ok", "run", histSummary(t, map[string]any{"avg_ms": 4, "loss_pct": 0}))

			code, body, headers := histGet(t, ts, "/metrics", c.credential)
			if headers.Get("Cache-Control") != "no-store" {
				t.Fatal("metrics responses must not be cached")
			}
			if code != c.wantStatus {
				t.Fatalf("status = %d, want %d", code, c.wantStatus)
			}
			if code != 200 {
				return
			}
			// The aggregates are always there.
			if !strings.Contains(body, "lookingglass_uptime_seconds") {
				t.Error("aggregate metrics are missing")
			}
			if got := strings.Contains(body, "lookingglass_probe_"); got != c.wantProbe {
				t.Errorf("per-schedule samples present = %v, want %v", got, c.wantProbe)
			}
			if !c.wantProbe && strings.Contains(body, sc.ID) {
				t.Errorf("the schedule inventory leaked into /metrics: %s", body)
			}
		})
	}
}

func TestMetricsStatusEncodingAndEmptyStatus(t *testing.T) {
	// S12: the per-schedule gauges are served only to a METRICS_TOKEN scrape.
	srv, ts, _ := histServer(t, map[string]string{"METRICS_TOKEN": "status-tok"})
	want := map[string]int{"ok": 0, "degraded": 1, "error": 2, "agent_offline": 3}
	ids := map[string]string{}
	for status := range want {
		sc := addSchedule(srv, "node-st", "ping", "1.1.1.1")
		ids[status] = sc.ID
		srv.finalizeSchedule(sc, status, "run")
	}
	// running: claimed but not finalized.
	runningSc := addSchedule(srv, "node-st", "ping", "1.1.1.1")
	if _, reason := srv.beginRun(runningSc); reason != "" {
		t.Fatalf("beginRun refused: %q", reason)
	}
	// never run: no sample at all.
	neverSc := addSchedule(srv, "node-st", "ping", "1.1.1.1")

	code, body, _ := histGet(t, ts, "/metrics", "status-tok")
	if code != 200 {
		t.Fatalf("/metrics status %d", code)
	}
	for status, code := range want {
		line := fmt.Sprintf(`lookingglass_probe_status{schedule_id="%s",node="Testville",command="ping",target="1.1.1.1"} %d`, ids[status], code)
		if !strings.Contains(body, line) {
			t.Errorf("missing %q\n%s", line, body)
		}
	}
	if !strings.Contains(body, fmt.Sprintf(`lookingglass_probe_status{schedule_id="%s",node="Testville",command="ping",target="1.1.1.1"} 4`, runningSc.ID)) {
		t.Errorf("running schedule is not encoded as 4:\n%s", body)
	}
	if strings.Contains(body, neverSc.ID) {
		t.Errorf("a never-run schedule produced a sample:\n%s", body)
	}
}

// Truncation must happen before escaping: escaping first can cut a backslash
// escape in half at the 64-rune boundary and corrupt the whole page.
func TestMetricsLabelsAreTruncatedThenEscaped(t *testing.T) {
	// S12: the per-schedule gauges are served only to a METRICS_TOKEN scrape.
	srv, ts, _ := histServer(t, map[string]string{"METRICS_TOKEN": "label-tok"})
	target := strings.Repeat("a", 60) + "\"x\ny" + strings.Repeat("b", 30)
	sc := addSchedule(srv, "node-lbl", "ping", target)
	srv.finalizeSchedule(sc, "ok", "run")

	code, body, _ := histGet(t, ts, "/metrics", "label-tok")
	if code != 200 {
		t.Fatalf("/metrics status %d", code)
	}
	wantLabel := `target="` + strings.Repeat("a", 60) + `\"x\ny"`
	if !strings.Contains(body, wantLabel) {
		t.Fatalf("label was not truncated-then-escaped, want %q in:\n%s", wantLabel, body)
	}
	if strings.Contains(body, target[:70]) {
		t.Error("the untruncated target reached the exposition")
	}
	// No raw newline may survive inside a label: every probe line must still
	// be one line starting with the metric name.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "schedule_id=") && !strings.HasPrefix(line, "lookingglass_probe_") {
			t.Fatalf("a label broke the exposition format: %q", line)
		}
	}
	// A trailing metric still parses, i.e. the page was not corrupted.
	if !strings.Contains(body, "lookingglass_alerts_dropped_total") {
		t.Error("metrics after the labelled samples are missing")
	}
}

func TestMetricsLabelValueEscaping(t *testing.T) {
	long := strings.Repeat("é", 100)
	if got := metricsLabelValue(long); len([]rune(got)) != 64 {
		t.Errorf("truncation is not rune-based: %d runes", len([]rune(got)))
	}
	if got, want := metricsLabelValue(`a\b"c`+"\n"), `a\\b\"c\n`; got != want {
		t.Errorf("escaping = %q, want %q", got, want)
	}
}
