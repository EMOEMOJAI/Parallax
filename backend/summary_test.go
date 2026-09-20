package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- helpers ----------------------------------------------------------------
//
// The harnesses from scheduler_test.go (newSchedulerTestServer, dialFakeAgent,
// waitFor, addSchedule, …) and public_test.go (newPublicTestServer,
// mustDialPublicClient, sendClientCommand, readCommandResponse) are reused
// unchanged; only the summary-specific helpers live here.

// agentReply writes one CommandResponse frame on the fake agent's connection,
// exactly as a real agent does.
func agentReply(t *testing.T, fa *fakeAgent, cmdID, typ, data string) {
	t.Helper()
	payload, _ := json.Marshal(CommandResponse{ID: cmdID, Type: typ, Data: data})
	env, _ := json.Marshal(AgentMessage{Action: "output", Payload: payload})
	fa.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := fa.conn.WriteMessage(websocket.TextMessage, env); err != nil {
		t.Fatalf("agent write %s: %v", typ, err)
	}
}

func summarySeenCount(srv *Server) int {
	srv.summarySeenMu.Lock()
	defer srv.summarySeenMu.Unlock()
	return len(srv.summarySeen)
}

func scheduleSummary(srv *Server, sc *Schedule) []byte {
	srv.schedulesMu.RLock()
	defer srv.schedulesMu.RUnlock()
	return append([]byte(nil), sc.LastSummary...)
}

// expectNextFrame reads the client's next frame and asserts it is the one the
// server should have forwarded. Frames the server must drop are always written
// *before* this one on the same agent connection, and one connection's frames
// are processed in order, so a dropped frame that leaked would arrive here
// instead — no timeouts or sleeps needed (and none are possible: a timed-out
// gorilla read poisons the connection for every later read).
func expectNextFrame(t *testing.T, c *websocket.Conn, wantType, wantData string) CommandResponse {
	t.Helper()
	resp := readCommandResponse(t, c)
	if resp.Type != wantType || (wantData != "" && resp.Data != wantData) {
		t.Fatalf("next client frame = {type:%q data:%q}, want {type:%q data:%q} — a frame that should have been dropped leaked",
			resp.Type, resp.Data, wantType, wantData)
	}
	return resp
}

// mustValidate is a small wrapper for the "this input must be accepted" cases.
func mustValidate(t *testing.T, in string) map[string]any {
	t.Helper()
	canonical, ok := validateSummary(in)
	if !ok {
		t.Fatalf("summary was rejected: %s", in)
	}
	var out map[string]any
	if err := json.Unmarshal(canonical, &out); err != nil {
		t.Fatalf("canonical form is not valid JSON: %v", err)
	}
	return out
}

// mtrSummaryJSON builds a valid n-hop mtr-shaped summary with hostnames of
// hostLen characters — the shape a real agent sends.
func mtrSummaryJSON(hops int, hostLen int) string {
	list := make([]any, 0, hops)
	for i := 1; i <= hops; i++ {
		host := fmt.Sprintf("hop-%02d.", i) + strings.Repeat("a", hostLen)
		loss := 0.0
		if i == hops {
			loss = 40
		}
		list = append(list, map[string]any{
			"hop":      i,
			"host":     host,
			"loss_pct": loss,
			"avg":      float64(i) * 1.5,
			"best":     float64(i),
			"worst":    float64(i) * 2,
		})
	}
	blob, _ := json.Marshal(map[string]any{
		"hops":      list,
		"hop_count": hops,
		"loss_pct":  40,
	})
	return string(blob)
}

// ---- validator: size and shape ---------------------------------------------

func TestSummaryOversizeIsDroppedBeforeParsing(t *testing.T) {
	padding := strings.Repeat("a", summaryMaxBytes)
	oversize := `{"note":"` + padding + `"}`
	if len(oversize) <= summaryMaxBytes {
		t.Fatalf("fixture is only %d bytes", len(oversize))
	}
	if _, ok := validateSummary(oversize); ok {
		t.Fatal("an oversize summary was accepted")
	}
	// The same payload just under the cap is accepted (and its string is
	// truncated), proving the rejection is the size check, not the content.
	underCap := `{"note":"` + strings.Repeat("a", 1000) + `"}`
	m := mustValidate(t, underCap)
	if got := len([]rune(m["note"].(string))); got != summaryMaxStringRunes {
		t.Errorf("note kept %d runes, want %d", got, summaryMaxStringRunes)
	}
}

func TestSummaryRejectedShapes(t *testing.T) {
	cases := map[string]string{
		"array at top level":            `[{"a":1}]`,
		"string at top level":           `"nope"`,
		"number at top level":           `42`,
		"null at top level":             `null`,
		"object as a top-level value":   `{"nested":{"a":1}}`,
		"object in object inside array": `{"hops":[{"inner":{"a":1}}]}`,
		"array of arrays":               `{"hops":[[1,2],[3,4]]}`,
		"array inside a hop object":     `{"hops":[{"a":[1,2]}]}`,
		"null value":                    `{"loss_pct":null}`,
		"null inside an array":          `{"hops":[1,null]}`,
		"null inside a hop":             `{"hops":[{"host":null}]}`,
		"mixed scalar/object array":     `{"hops":[1,{"host":"a"}]}`,
		"mixed object/scalar array":     `{"hops":[{"host":"a"},1]}`,
		"trailing object":               `{"a":1} {"b":2}`,
		"trailing garbage":              `{"a":1} nonsense`,
		"not json":                      `{"a":`,
		"empty":                         ``,
	}
	for name, in := range cases {
		if _, ok := validateSummary(in); ok {
			t.Errorf("%s was accepted: %s", name, in)
		}
	}
}

func TestSummaryKeyCap(t *testing.T) {
	build := func(n int) string {
		m := make(map[string]any, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("k%d", i)] = i
		}
		blob, _ := json.Marshal(m)
		return string(blob)
	}
	if _, ok := validateSummary(build(summaryMaxKeys)); !ok {
		t.Errorf("a %d-key summary was rejected", summaryMaxKeys)
	}
	if _, ok := validateSummary(build(summaryMaxKeys + 1)); ok {
		t.Errorf("a %d-key summary was accepted", summaryMaxKeys+1)
	}
}

func TestSummaryArrayLengthCap(t *testing.T) {
	build := func(n int) string {
		list := make([]any, n)
		for i := range list {
			list[i] = i
		}
		blob, _ := json.Marshal(map[string]any{"values": list})
		return string(blob)
	}
	if _, ok := validateSummary(build(summaryMaxArrayLen)); !ok {
		t.Errorf("a %d-element array was rejected", summaryMaxArrayLen)
	}
	if _, ok := validateSummary(build(summaryMaxArrayLen + 1)); ok {
		t.Errorf("a %d-element array was accepted", summaryMaxArrayLen+1)
	}
	// The same cap applies to an array of hop objects.
	if _, ok := validateSummary(mtrSummaryJSON(summaryMaxArrayLen, 8)); !ok {
		t.Errorf("a %d-hop summary was rejected", summaryMaxArrayLen)
	}
	if _, ok := validateSummary(mtrSummaryJSON(summaryMaxArrayLen+1, 8)); ok {
		t.Errorf("a %d-hop summary was accepted", summaryMaxArrayLen+1)
	}
}

func TestSummaryHopObjectKeyCap(t *testing.T) {
	build := func(n int) string {
		hop := make(map[string]any, n)
		for i := 0; i < n; i++ {
			hop[fmt.Sprintf("f%d", i)] = i
		}
		blob, _ := json.Marshal(map[string]any{"hops": []any{hop}})
		return string(blob)
	}
	if _, ok := validateSummary(build(summaryMaxObjectKeys)); !ok {
		t.Errorf("an %d-key hop object was rejected", summaryMaxObjectKeys)
	}
	if _, ok := validateSummary(build(summaryMaxObjectKeys + 1)); ok {
		t.Errorf("a %d-key hop object was accepted", summaryMaxObjectKeys+1)
	}
}

func TestSummaryMtrHopsAccepted(t *testing.T) {
	in := `{"hops":[{"hop":1,"host":"192.168.1.1","loss_pct":0,"avg":0.5,"best":0.4,"worst":0.7},
	         {"hop":2,"host":"one.one.one.one","loss_pct":20,"avg":12.5,"best":11.9,"worst":13.2}],
	        "hop_count":2,"loss_pct":20}`
	m := mustValidate(t, in)
	hops, ok := m["hops"].([]any)
	if !ok || len(hops) != 2 {
		t.Fatalf("hops = %#v", m["hops"])
	}
	second := hops[1].(map[string]any)
	if second["host"] != "one.one.one.one" {
		t.Errorf("hop host = %v", second["host"])
	}
	if m["loss_pct"] != 20.0 {
		t.Errorf("loss_pct = %v, want 20", m["loss_pct"])
	}
}

func TestSummaryScalarTypesSurviveCanonicalisation(t *testing.T) {
	in := `{"status":"NOERROR","answer_count":2,"query_time_ms":23.5,"cached":true,"neg":-1,"exp":1e3}`
	canonical, ok := validateSummary(in)
	if !ok {
		t.Fatal("rejected")
	}
	// UseNumber keeps the literal text: no float rounding, no 1e3 → 1000.
	for _, want := range []string{`"answer_count":2`, `"query_time_ms":23.5`, `"cached":true`, `"neg":-1`, `"exp":1e3`} {
		if !strings.Contains(string(canonical), want) {
			t.Errorf("canonical form %s is missing %s", canonical, want)
		}
	}
}

func TestSummaryStringsAreStrippedAndTruncated(t *testing.T) {
	payload := "head\n\x1b[2J" + strings.Repeat("é", 300)
	blob, _ := json.Marshal(map[string]any{
		"note": payload,
		"hops": []any{map[string]any{"host": payload}},
		"list": []any{payload},
	})
	m := mustValidate(t, string(blob))

	check := func(label, got string) {
		t.Helper()
		if strings.ContainsAny(got, "\n\r\x1b\x00") {
			t.Errorf("%s still holds control characters: %q", label, got)
		}
		if n := len([]rune(got)); n != summaryMaxStringRunes {
			t.Errorf("%s kept %d runes, want %d", label, n, summaryMaxStringRunes)
		}
		if !strings.HasPrefix(got, "head[2J") {
			t.Errorf("%s = %q, want the escape sequence stripped of its ESC", label, got[:16])
		}
	}
	check("note", m["note"].(string))
	check("hop host", m["hops"].([]any)[0].(map[string]any)["host"].(string))
	check("array element", m["list"].([]any)[0].(string))
}

func TestSummaryKeysAreSanitized(t *testing.T) {
	blob, _ := json.Marshal(map[string]any{"lo\nss_pct": 1, "hops": []any{map[string]any{"ho\x1bst": "a"}}})
	m := mustValidate(t, string(blob))
	if _, ok := m["loss_pct"]; !ok {
		t.Errorf("top-level key was not control-stripped: %v", m)
	}
	hop := m["hops"].([]any)[0].(map[string]any)
	if _, ok := hop["host"]; !ok {
		t.Errorf("hop key was not control-stripped: %v", hop)
	}
}

// ---- done payload + status derivation --------------------------------------

func TestParseExitOK(t *testing.T) {
	cases := map[string]bool{
		``:                   true, // legacy agent
		`   `:                true,
		`{"exit_ok":true}`:   true,
		`{"exit_ok":false}`:  false,
		`{"other":1}`:        true, // no information → treat as clean
		`not json`:           true,
		`{"exit_ok":"nope"}`: true,
	}
	for in, want := range cases {
		if got := parseExitOK(in); got != want {
			t.Errorf("parseExitOK(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScheduleStatusDerivation(t *testing.T) {
	cases := []struct {
		name    string
		exitOK  bool
		summary string
		want    string
	}{
		{"no summary, clean exit", true, "", "ok"},
		{"no summary, failed exit", false, "", "error"},
		{"dns summary without loss_pct", true, `{"status":"NOERROR","answer_count":1}`, "ok"},
		{"dns summary, failed exit", false, `{"status":"SERVFAIL","answer_count":0}`, "error"},
		{"ping 0% loss", true, `{"loss_pct":0}`, "ok"},
		{"ping 19.9% loss", true, `{"loss_pct":19.9}`, "ok"},
		{"ping 20% loss", true, `{"loss_pct":20}`, "degraded"},
		{"ping 40% loss", true, `{"loss_pct":40}`, "degraded"},
		{"ping 100% loss", true, `{"loss_pct":100}`, "error"},
		{"low loss but failed exit", false, `{"loss_pct":0}`, "error"},
		{"non-numeric loss_pct falls back to exit", true, `{"loss_pct":"none"}`, "ok"},
	}
	for _, c := range cases {
		if got := scheduleStatusFor(c.exitOK, []byte(c.summary)); got != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.want)
		}
	}
}

// ---- browser path -----------------------------------------------------------

// startBrowserCommand wires an agent, a browser client and one in-flight
// command, and returns the command id.
func startBrowserCommand(t *testing.T) (*Server, *fakeAgent, *websocket.Conn, string) {
	t.Helper()
	srv, ts := newPublicTestServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-summary")
	nodeID := waitForNodeID(t, srv, "node-summary")
	client := mustDialPublicClient(t, ts)
	cmdID := sendClientCommand(t, client, nodeID, "ping", "1.1.1.1", "")
	req := fa.awaitCommand(t)
	if req.ID != cmdID {
		t.Fatalf("agent received %q, want %q", req.ID, cmdID)
	}
	return srv, fa, client, cmdID
}

func TestBrowserCommandForwardsCanonicalSummaryAndClearsDedup(t *testing.T) {
	srv, fa, client, cmdID := startBrowserCommand(t)

	agentReply(t, fa, cmdID, "summary", `{"sent":5,"received":5,"loss_pct":0,"avg_ms":12.5,"note":"ok\nline"}`)
	resp := readCommandResponse(t, client)
	if resp.Type != "summary" {
		t.Fatalf("first frame = %q, want summary", resp.Type)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resp.Data), &m); err != nil {
		t.Fatalf("summary data %q is not JSON: %v", resp.Data, err)
	}
	if m["loss_pct"] != 0.0 || m["avg_ms"] != 12.5 {
		t.Errorf("summary lost values: %v", m)
	}
	if m["note"] != "okline" {
		t.Errorf("note = %q, want the newline stripped", m["note"])
	}
	if summarySeenCount(srv) != 1 {
		t.Fatalf("summarySeen holds %d entries, want 1", summarySeenCount(srv))
	}

	agentReply(t, fa, cmdID, "done", `{"exit_ok":true}`)
	done := readCommandResponse(t, client)
	if done.Type != "done" {
		t.Fatalf("second frame = %q, want done", done.Type)
	}
	waitFor(t, "summarySeen to drain after done", 2*time.Second, func() bool {
		return summarySeenCount(srv) == 0
	})
	waitFor(t, "cmdOwners to drain after done", 2*time.Second, func() bool {
		return !cmdOwnerExists(srv, cmdID)
	})
}

func TestSecondSummaryForOneCommandIsDropped(t *testing.T) {
	srv, fa, client, cmdID := startBrowserCommand(t)

	agentReply(t, fa, cmdID, "summary", `{"loss_pct":0}`)
	if resp := readCommandResponse(t, client); resp.Type != "summary" {
		t.Fatalf("first frame = %q, want summary", resp.Type)
	}
	// Second summary for the same command: dropped before it reaches anyone.
	// The `done` that follows it on the same connection is what the client
	// must see next.
	agentReply(t, fa, cmdID, "summary", `{"loss_pct":100}`)
	agentReply(t, fa, cmdID, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")
	waitFor(t, "summarySeen to drain", 2*time.Second, func() bool {
		return summarySeenCount(srv) == 0
	})
}

func TestInvalidSummaryIsDroppedButStreamContinues(t *testing.T) {
	srv, fa, client, cmdID := startBrowserCommand(t)

	agentReply(t, fa, cmdID, "summary", `{"hops":[[1,2]]}`) // array of arrays
	agentReply(t, fa, cmdID, "output", "64 bytes from 1.1.1.1")
	expectNextFrame(t, client, "output", "64 bytes from 1.1.1.1")
	if n := summarySeenCount(srv); n != 0 {
		t.Errorf("a rejected summary left %d dedup entries", n)
	}

	agentReply(t, fa, cmdID, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")
}

// F33: output is only accepted from the node the command was dispatched to.
func TestCrossNodeOutputIsDropped(t *testing.T) {
	srv, ts := newPublicTestServer(t, nil)
	nodeA := dialFakeAgent(t, ts, "node-a")
	nodeB := dialFakeAgent(t, ts, "node-b")
	nodeAID := waitForNodeID(t, srv, "node-a")
	nodeBID := waitForNodeID(t, srv, "node-b")

	client := mustDialPublicClient(t, ts)
	cmdA := sendClientCommand(t, client, nodeAID, "ping", "1.1.1.1", "")
	if req := nodeA.awaitCommand(t); req.ID != cmdA {
		t.Fatalf("node-a received %q, want %q", req.ID, cmdA)
	}
	cmdB := sendClientCommand(t, client, nodeBID, "ping", "9.9.9.9", "")
	if req := nodeB.awaitCommand(t); req.ID != cmdB {
		t.Fatalf("node-b received %q, want %q", req.ID, cmdB)
	}

	// node-b tries to inject output, an error and a summary into node-a's
	// command, then reports honestly on its own. Frames are processed in
	// order per connection, so the injections must be gone by the time the
	// honest one arrives.
	agentReply(t, nodeB, cmdA, "output", "INJECTED")
	agentReply(t, nodeB, cmdA, "error", "INJECTED")
	agentReply(t, nodeB, cmdA, "summary", `{"loss_pct":100}`)
	agentReply(t, nodeB, cmdB, "output", "node-b speaking for itself")
	resp := expectNextFrame(t, client, "output", "node-b speaking for itself")
	if resp.ID != cmdB || resp.NodeID != nodeBID {
		t.Fatalf("frame = %+v, want node-b's own command", resp)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Fatalf("cross-node summary was accepted (%d dedup entries)", n)
	}
	agentReply(t, nodeB, cmdB, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")

	// The legitimate node's own output still flows, unaltered.
	agentReply(t, nodeA, cmdA, "output", "64 bytes from 1.1.1.1")
	resp = expectNextFrame(t, client, "output", "64 bytes from 1.1.1.1")
	if resp.ID != cmdA || resp.NodeID != nodeAID {
		t.Fatalf("frame = %+v, want node-a's command", resp)
	}
	agentReply(t, nodeA, cmdA, "summary", `{"loss_pct":0}`)
	summary := expectNextFrame(t, client, "summary", "")
	if !strings.Contains(summary.Data, `"loss_pct":0`) {
		t.Errorf("summary = %s, want node-a's own", summary.Data)
	}
	agentReply(t, nodeA, cmdA, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")
}

// An unregistered command id (already finished, or never dispatched) is also
// dropped — the same check, exercised from the owning node.
func TestOutputForUnknownCommandIsDropped(t *testing.T) {
	srv, ts := newPublicTestServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-late")
	nodeID := waitForNodeID(t, srv, "node-late")
	client := mustDialPublicClient(t, ts)

	cmd1 := sendClientCommand(t, client, nodeID, "ping", "1.1.1.1", "")
	fa.awaitCommand(t)
	agentReply(t, fa, cmd1, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")
	waitFor(t, "cmdNodes to drain", 2*time.Second, func() bool {
		return !cmdNodeExists(srv, cmd1)
	})

	// A late summary for the finished command, then a second, live command.
	agentReply(t, fa, cmd1, "summary", `{"loss_pct":0}`)
	cmd2 := sendClientCommand(t, client, nodeID, "ping", "9.9.9.9", "")
	fa.awaitCommand(t)
	agentReply(t, fa, cmd2, "output", "second command output")
	resp := expectNextFrame(t, client, "output", "second command output")
	if resp.ID != cmd2 {
		t.Fatalf("frame belongs to %q, want %q", resp.ID, cmd2)
	}
	if n := summarySeenCount(srv); n != 0 {
		t.Errorf("late summary was accepted (%d dedup entries)", n)
	}
	agentReply(t, fa, cmd2, "done", `{"exit_ok":true}`)
	expectNextFrame(t, client, "done", "")
}

// Closing between summary and done reserves routing while cancellation is
// pending. Terminal acknowledgment must retire both that reservation and its
// summary dedup entry.
func TestSummarySeenDrainsWhenTheBrowserDisconnectsMidCommand(t *testing.T) {
	srv, ts := newPublicTestServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-drop")
	nodeID := waitForNodeID(t, srv, "node-drop")
	client := mustDialPublicClient(t, ts)

	cmdID := sendClientCommand(t, client, nodeID, "ping", "1.1.1.1", "")
	fa.awaitCommand(t)
	agentReply(t, fa, cmdID, "summary", `{"loss_pct":0}`)
	expectNextFrame(t, client, "summary", "")
	if summarySeenCount(srv) != 1 {
		t.Fatalf("summarySeen holds %d entries, want 1", summarySeenCount(srv))
	}

	client.Close()
	waitFor(t, "the client disconnect to dispatch cancellation", 5*time.Second, func() bool {
		srv.cmdOwnersMu.RLock()
		defer srv.cmdOwnersMu.RUnlock()
		closing := srv.closingCommands[cmdID]
		return closing != nil && !closing.cancelPending
	})

	agentReply(t, fa, cmdID, "done", `{"exit_ok":true}`)
	waitFor(t, "summary and reservation to drain after terminal acknowledgment", 5*time.Second, func() bool {
		return summarySeenCount(srv) == 0 && !cmdNodeExists(srv, cmdID) && !cmdOwnerExists(srv, cmdID)
	})
}

// The cross-node check must not break interactive shells: shell_start
// registers cmdNodes before the agent is told anything, so shell_output flows
// exactly as before.
func TestShellOutputStillReachesTheBrowser(t *testing.T) {
	srv, ts := newPublicTestServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-shell")
	nodeID := waitForNodeID(t, srv, "node-shell")
	client := mustDialPublicClient(t, ts)

	sessionID := "shell-session-1"
	msg, _ := json.Marshal(map[string]any{"action": "shell_start", "node_id": nodeID, "id": sessionID})
	client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := client.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write shell_start: %v", err)
	}
	waitFor(t, "the shell session to be registered", 5*time.Second, func() bool {
		return cmdNodeExists(srv, sessionID)
	})

	agentReply(t, fa, sessionID, "shell_output", "user@node:~$ ")
	resp := expectNextFrame(t, client, "shell_output", "user@node:~$ ")
	if resp.ID != sessionID {
		t.Fatalf("shell output belongs to %q, want %q", resp.ID, sessionID)
	}
	agentReply(t, fa, sessionID, "done", "")
	expectNextFrame(t, client, "done", "")
	waitFor(t, "the shell session to be torn down", 5*time.Second, func() bool {
		return !cmdNodeExists(srv, sessionID) && !cmdOwnerExists(srv, sessionID)
	})
}

// ---- schedule runs ----------------------------------------------------------

// runScheduleWithFrames drives one scheduled run end to end, replaying the
// given agent frames (each {type, data}) before waiting for the terminal
// status.
func runScheduleWithFrames(t *testing.T, srv *Server, fa *fakeAgent, sc *Schedule, frames [][2]string) {
	t.Helper()
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the run: %q", reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	if req := fa.awaitCommand(t); req.ID != cmdID {
		t.Fatalf("agent received %q, want %q", req.ID, cmdID)
	}
	for _, f := range frames {
		agentReply(t, fa, cmdID, f[0], f[1])
	}
	waitFor(t, "schedule to leave running", 5*time.Second, func() bool {
		status, _, _ := scheduleState(srv, sc)
		return status != "running"
	})
}

func TestScheduleDNSSummaryFinalizesOKAndPersists(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-dns")
	nodeID := waitForNodeID(t, srv, "node-dns")
	sc := addSchedule(srv, nodeID, "dns", "example.com")

	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"output", ";; ANSWER SECTION:"},
		{"summary", `{"status":"NOERROR","answer_count":2,"query_time_ms":23}`},
		{"done", `{"exit_ok":true}`},
	})

	status, _, _ := scheduleState(srv, sc)
	if status != "ok" {
		t.Fatalf("status = %q, want ok", status)
	}
	var stored map[string]any
	if err := json.Unmarshal(scheduleSummary(srv, sc), &stored); err != nil {
		t.Fatalf("LastSummary is not JSON: %v", err)
	}
	if stored["status"] != "NOERROR" || stored["answer_count"] != 2.0 {
		t.Errorf("LastSummary = %v", stored)
	}
	waitFor(t, "summarySeen to drain", 2*time.Second, func() bool {
		return summarySeenCount(srv) == 0
	})
	waitFor(t, "schedule run registration to drain", 2*time.Second, func() bool {
		return scheduleRunCount(srv) == 0
	})

	// The persisted file round-trips through a fresh server.
	waitFor(t, "schedules file to be written", 5*time.Second, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(data), "last_summary")
	})
	reloaded := NewServer()
	reloaded.loadSchedules()
	reloaded.schedulesMu.RLock()
	got, ok := reloaded.schedules[sc.ID]
	reloaded.schedulesMu.RUnlock()
	if !ok {
		t.Fatal("schedule did not survive the reload")
	}
	var reloadedSummary map[string]any
	if err := json.Unmarshal(got.LastSummary, &reloadedSummary); err != nil {
		t.Fatalf("reloaded LastSummary is not JSON: %v", err)
	}
	if reloadedSummary["status"] != "NOERROR" {
		t.Errorf("reloaded LastSummary = %v", reloadedSummary)
	}
	if got.LastStatus != "ok" {
		t.Errorf("reloaded status = %q, want ok", got.LastStatus)
	}
}

func TestSchedulePingLossFinalizesDegraded(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-ping")
	nodeID := waitForNodeID(t, srv, "node-ping")
	sc := addSchedule(srv, nodeID, "ping", "192.0.2.1")

	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"summary", `{"sent":10,"received":6,"loss_pct":40,"avg_ms":24.8}`},
		{"done", `{"exit_ok":true}`},
	})

	status, _, _ := scheduleState(srv, sc)
	if status != "degraded" {
		t.Fatalf("status = %q, want degraded", status)
	}
	if len(scheduleSummary(srv, sc)) == 0 {
		t.Error("a degraded run should still carry its summary")
	}
}

func TestScheduleExitNotOKFinalizesError(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-err")
	nodeID := waitForNodeID(t, srv, "node-err")
	sc := addSchedule(srv, nodeID, "ping", "192.0.2.1")

	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"error", "Command exited with error: exit status 1"},
		{"done", `{"exit_ok":false}`},
	})

	status, result, _ := scheduleState(srv, sc)
	if status != "error" {
		t.Fatalf("status = %q, want error", status)
	}
	if !strings.Contains(result, "exit status 1") {
		t.Errorf("result = %q", result)
	}
	if len(scheduleSummary(srv, sc)) != 0 {
		t.Errorf("a failed run must not carry a summary: %s", scheduleSummary(srv, sc))
	}
}

// A legacy agent sends an empty `done` payload; that must still mean "ok".
func TestScheduleLegacyDoneIsOK(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-legacy")
	nodeID := waitForNodeID(t, srv, "node-legacy")
	sc := addSchedule(srv, nodeID, "http", "example.com")

	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"output", "code=200  total=0.06s"},
		{"done", ""},
	})

	status, _, _ := scheduleState(srv, sc)
	if status != "ok" {
		t.Fatalf("status = %q, want ok", status)
	}
}

func TestScheduleAgentOfflineClearsPreviousSummary(t *testing.T) {
	srv, ts, _ := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-off")
	nodeID := waitForNodeID(t, srv, "node-off")
	sc := addSchedule(srv, nodeID, "ping", "1.1.1.1")

	// First run succeeds and stores a summary.
	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"summary", `{"sent":5,"received":5,"loss_pct":0,"avg_ms":12.5}`},
		{"done", `{"exit_ok":true}`},
	})
	if status, _, _ := scheduleState(srv, sc); status != "ok" {
		t.Fatalf("first run status = %q, want ok", status)
	}
	if len(scheduleSummary(srv, sc)) == 0 {
		t.Fatal("first run did not store a summary")
	}

	// Second run: the agent dies mid-run, after sending a summary.
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatalf("beginRun refused the second run: %q", reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	fa.awaitCommand(t)
	agentReply(t, fa, cmdID, "summary", `{"sent":5,"received":5,"loss_pct":0,"avg_ms":99}`)
	waitFor(t, "the second summary to be accepted", 2*time.Second, func() bool {
		return summarySeenCount(srv) == 1
	})
	fa.close()

	waitFor(t, "schedule to finalize as agent_offline", 5*time.Second, func() bool {
		status, _, _ := scheduleState(srv, sc)
		return status == "agent_offline"
	})
	if got := scheduleSummary(srv, sc); len(got) != 0 {
		t.Errorf("agent_offline left a stale summary: %s", got)
	}
	// The disconnect path drops the dedup entry for every orphaned command.
	waitFor(t, "summarySeen to drain after the disconnect", 2*time.Second, func() bool {
		return summarySeenCount(srv) == 0
	})
	waitFor(t, "run registration to drain after the disconnect", 2*time.Second, func() bool {
		return scheduleRunCount(srv) == 0 && !cmdNodeExists(srv, cmdID)
	})
}

// A summary larger than scheduleMaxSummary is forwarded to browsers in full
// and persisted in reduced form: the scalar fields survive so the card still
// shows badges, the `hops` array (which is what makes an mtr summary 8–13 KB)
// is dropped, and the file still reloads cleanly. Raw JSON is never truncated.
func TestScheduleOversizeSummaryIsNotPersisted(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	fa := dialFakeAgent(t, ts, "node-mtr")
	nodeID := waitForNodeID(t, srv, "node-mtr")
	sc := addSchedule(srv, nodeID, "mtr", "1.1.1.1")

	big := mtrSummaryJSON(64, 80)
	if len(big) <= scheduleMaxSummary {
		t.Fatalf("fixture is only %d bytes, needs > %d", len(big), scheduleMaxSummary)
	}
	if len(big) > summaryMaxBytes {
		t.Fatalf("fixture is %d bytes, the server drops anything over %d", len(big), summaryMaxBytes)
	}

	runScheduleWithFrames(t, srv, fa, sc, [][2]string{
		{"summary", big},
		{"done", `{"exit_ok":true}`},
	})

	status, _, _ := scheduleState(srv, sc)
	if status != "degraded" { // last hop reports 40 % loss
		t.Fatalf("status = %q, want degraded", status)
	}
	stored := scheduleSummary(srv, sc)
	if len(stored) == 0 {
		t.Fatal("the reduced summary was not persisted at all")
	}
	if len(stored) > scheduleMaxSummary {
		t.Fatalf("stored summary is %d bytes, over the %d cap", len(stored), scheduleMaxSummary)
	}
	var reduced map[string]any
	if err := json.Unmarshal(stored, &reduced); err != nil {
		t.Fatalf("stored summary is not valid JSON: %v", err)
	}
	if _, hasHops := reduced["hops"]; hasHops {
		t.Errorf("the hops array survived the reduction: %s", stored)
	}
	if reduced["loss_pct"] != 40.0 || reduced["hop_count"] != 64.0 {
		t.Errorf("reduced summary lost its scalars: %v", reduced)
	}

	waitFor(t, "schedules file to be written", 5*time.Second, func() bool {
		_, err := os.ReadFile(path)
		return err == nil
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schedules file: %v", err)
	}
	if strings.Contains(string(data), `"hops"`) {
		t.Errorf("schedules file carries the hops array: %s", data)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("schedules file is not valid JSON: %v", err)
	}
	reloaded := NewServer()
	reloaded.loadSchedules()
	reloaded.schedulesMu.RLock()
	got, ok := reloaded.schedules[sc.ID]
	reloaded.schedulesMu.RUnlock()
	if !ok {
		t.Fatal("schedule did not survive the reload")
	}
	var reloadedSummary map[string]any
	if err := json.Unmarshal(got.LastSummary, &reloadedSummary); err != nil {
		t.Fatalf("reloaded LastSummary is not JSON: %v", err)
	}
	if reloadedSummary["loss_pct"] != 40.0 {
		t.Errorf("reloaded LastSummary = %v", reloadedSummary)
	}
	if _, hasHops := reloadedSummary["hops"]; hasHops {
		t.Errorf("reloaded LastSummary carries hops: %v", reloadedSummary)
	}
	if got.LastStatus != "degraded" {
		t.Errorf("reloaded status = %q, want degraded", got.LastStatus)
	}
}

// A hand-edited schedules.json carrying a summary that no longer matches the
// grammar is dropped on load rather than re-served to browsers.
func TestLoadSchedulesDropsInvalidPersistedSummary(t *testing.T) {
	srv, _, path := newSchedulerTestServer(t)
	_ = srv
	writeSchedulesFile(t, path, []map[string]any{
		{
			"id": "sched-1", "node_id": "n1", "node_name": "n", "command": "ping",
			"target": "1.1.1.1", "interval_sec": 60, "enabled": true,
			"last_status":  "ok",
			"last_summary": map[string]any{"nested": map[string]any{"deep": 1}},
		},
		{
			"id": "sched-2", "node_id": "n1", "node_name": "n", "command": "ping",
			"target": "1.1.1.1", "interval_sec": 60, "enabled": true,
			"last_status":  "ok",
			"last_summary": map[string]any{"loss_pct": 0, "avg_ms": 12.5},
		},
	})
	loaded := NewServer()
	loaded.loadSchedules()
	loaded.schedulesMu.RLock()
	defer loaded.schedulesMu.RUnlock()
	if got := loaded.schedules["sched-1"]; got == nil || len(got.LastSummary) != 0 {
		t.Errorf("invalid persisted summary survived: %s", got.LastSummary)
	}
	if got := loaded.schedules["sched-2"]; got == nil || len(got.LastSummary) == 0 {
		t.Errorf("valid persisted summary was dropped")
	}
}

// scheduleView must carry the new field so the browser can render it.
func TestScheduleViewCarriesSummary(t *testing.T) {
	srv, _, _ := newSchedulerTestServer(t)
	sc := addSchedule(srv, "node-1", "ping", "1.1.1.1")
	srv.schedulesMu.Lock()
	sc.LastSummary = json.RawMessage(`{"loss_pct":0}`)
	srv.schedulesMu.Unlock()

	views := srv.scheduleViews()
	if len(views) != 1 {
		t.Fatalf("got %d views", len(views))
	}
	blob, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if !strings.Contains(string(blob), `"last_summary":{"loss_pct":0}`) {
		t.Errorf("view = %s", blob)
	}

	// …and stays out of the JSON entirely when there is no summary.
	srv.schedulesMu.Lock()
	sc.LastSummary = nil
	srv.schedulesMu.Unlock()
	blob, _ = json.Marshal(srv.scheduleViews()[0])
	if strings.Contains(string(blob), "last_summary") {
		t.Errorf("empty summary was serialized: %s", blob)
	}
}
