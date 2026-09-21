package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestReauditRunQuotaSurvivesIdleGC(t *testing.T) {
	s := NewServer()
	const key = "192.0.2.1/32"
	for i := 0; i < 10; i++ {
		if !s.rateAllowRunCreate(key) {
			t.Fatal("initial quota was unavailable")
		}
	}
	if s.rateAllowRunCreate(key) {
		t.Fatal("quota did not exhaust")
	}
	s.rateMu.Lock()
	s.rateEntries[key].lastFill = time.Now().Add(-11 * time.Minute)
	s.rateMu.Unlock()
	s.rateGC(time.Now())
	admitted := 0
	for i := 0; i < 10; i++ {
		if s.rateAllowRunCreate(key) {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("11 idle minutes admitted %d requests; expected one replenished token", admitted)
	}
	// Once all hourly tokens have recovered, GC can safely discard the entry.
	s.rateMu.Lock()
	s.rateEntries[key].lastFill = time.Now().Add(-61 * time.Minute)
	s.rateMu.Unlock()
	s.rateGC(time.Now())
	if s.rateEntryCount() != 0 {
		t.Fatal("fully replenished idle quota was retained")
	}
}

func TestReauditCapacityEvictionPreservesDepletedQuotas(t *testing.T) {
	s := NewServer()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	s.rateEntries["depleted"] = &rateEntry{cmdTokens: publicCmdBurst, runTokens: 0, lastFill: now.Add(-11 * time.Minute)}
	s.rateEntries["recovered"] = &rateEntry{cmdTokens: publicCmdBurst, runTokens: runCreateBurst, lastFill: now}
	if !s.evictOldestRateEntryLocked() {
		t.Fatal("fully recovered entry should be evictable")
	}
	if _, ok := s.rateEntries["depleted"]; !ok {
		t.Fatal("eviction reset a depleted hourly quota")
	}
	if _, ok := s.rateEntries["recovered"]; ok {
		t.Fatal("safe entry was not evicted")
	}
	if s.evictOldestRateEntryLocked() {
		t.Fatal("must fail closed when only a depleted quota remains")
	}
}

func TestReauditRequestPathsCannotForgeLogLines(t *testing.T) {
	s := NewServer()
	for name, wrap := range map[string]func(http.HandlerFunc) http.HandlerFunc{
		"access": requestLogger, "metrics": s.loggerWithMetrics,
	} {
		t.Run(name, func(t *testing.T) {
			for _, encoded := range []string{"%0AFORGED", "%0DFORGED", "%1B%5B2JFORGED"} {
				logs := captureLog(t, func() {
					wrap(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })(
						httptest.NewRecorder(), httptest.NewRequest("GET", "/api/rdap/"+encoded+"?key=must-not-log", nil),
					)
				})
				if strings.Contains(logs, "\nFORGED") || strings.ContainsAny(logs, "\r\x1b") {
					t.Fatalf("request controls reached text logs: %q", logs)
				}
				if !strings.Contains(logs, "FORGED") || strings.Contains(logs, "must-not-log") {
					t.Fatalf("path missing or query credential logged: %q", logs)
				}
			}
		})
	}
}

func TestReauditDuplicateRejectionsLeaveOriginalStreamOpen(t *testing.T) {
	cases := []struct {
		name, action, typ, target string
		saturate, public          bool
	}{
		{name: "command at node cap", action: "command", typ: "ping", target: "192.0.2.1", saturate: true},
		{name: "shell at node cap", action: "shell_start", saturate: true},
		{name: "invalid command type", action: "command", typ: "invalid", target: "192.0.2.1"},
		{name: "oversized target", action: "command", typ: "ping", target: strings.Repeat("x", 1025)},
		{name: "public target refusal", action: "command", typ: "ping", target: "192.0.2.2", public: true},
		{name: "public shell refusal", action: "shell_start", public: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := s11NewServer(t)
			ids, agents := s12Agents(t, srv, ts, "reaudit-node", 1)
			conn := s11DialClient(t, ts)
			serverConn := s12OnlyClientConn(t, srv)
			count := 1
			if tc.saturate {
				count = maxCommandsPerNode
			}
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("reaudit-command-%d", i)
				s11SendCommand(t, conn, ids[0], CommandRequest{ID: id, Type: "ping", Target: "192.0.2.1"})
				agents[0].awaitCommand(t)
			}
			if tc.public {
				t.Setenv("PUBLIC_TARGETS", "192.0.2.1")
				srv.markPublic(serverConn)
			}
			const original = "reaudit-command-0"
			if tc.action == "shell_start" {
				s12SendShellStart(t, conn, ids[0], original)
			} else {
				s11SendCommand(t, conn, ids[0], CommandRequest{ID: original, Type: tc.typ, Target: tc.target})
			}
			// A distinct invalid request is a read-loop barrier: its error must be the
			// next frame, proving the replay generated neither error nor done.
			s11SendCommand(t, conn, ids[0], CommandRequest{ID: "barrier", Type: "invalid"})
			frame, ok := s11ReadFrame(t, conn, time.Second)
			if !ok || frame.ID != "barrier" || frame.Type != "error" {
				t.Fatalf("replay altered original stream: %+v", frame)
			}
			frame, ok = s11ReadFrame(t, conn, time.Second)
			if !ok || frame.ID != "barrier" || frame.Type != "done" {
				t.Fatalf("missing barrier done: %+v", frame)
			}
			agentReply(t, agents[0], original, "output", "still running")
			frame, ok = s11ReadFrame(t, conn, time.Second)
			if !ok || frame.ID != original || frame.Type != "output" || frame.Data != "still running" {
				t.Fatalf("original stream lost: %+v", frame)
			}
			if srv.countClientCommands(serverConn) != count {
				t.Fatal("replay changed command ownership")
			}
		})
	}
}

func TestReauditShellStartForwardsOptionalDimensions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows uint16
		omit       bool
	}{
		{name: "legacy absent", omit: true}, {name: "zero"}, {name: "browser fit", cols: 83, rows: 27}, {name: "agent clamps", cols: 65535, rows: 65535},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := s11NewServer(t)
			agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Shell dimensions"))
			client := s11DialClient(t, ts)
			frame := map[string]any{"action": "shell_start", "node_id": id, "id": "shell-dimensions"}
			if !tc.omit {
				frame["cols"] = tc.cols
				frame["rows"] = tc.rows
			}
			if err := client.WriteJSON(frame); err != nil {
				t.Fatal(err)
			}
			agent.SetReadDeadline(time.Now().Add(3 * time.Second))
			kind, raw, err := agent.ReadMessage()
			if err != nil || kind != websocket.TextMessage {
				t.Fatalf("read shell start: kind=%d err=%v", kind, err)
			}
			var envelope AgentMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			var payload struct {
				ID   string `json:"id"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if envelope.Action != "shell_start" || payload.ID != "shell-dimensions" || payload.Cols != tc.cols || payload.Rows != tc.rows {
				t.Fatalf("shell dimensions lost or changed: %s", raw)
			}
			if tc.cols == 0 && tc.rows == 0 && string(envelope.Payload) != `{"id":"shell-dimensions"}` {
				t.Fatalf("legacy wire payload changed: %s", envelope.Payload)
			}
		})
	}
}

func TestCommandLogOmitsSensitiveTarget(t *testing.T) {
	_, ts := newPublicTestServer(t, nil)
	conn, _, err := dialClientWS(ts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	text := captureLog(t, func() {
		runCommandExpectingError(t, conn, "missing-node", "http", "https://example.com/private-token-path?key=synthetic-secret")
	})
	if strings.Contains(text, "synthetic-secret") || strings.Contains(text, "private-token-path") || !strings.Contains(text, "Command: http") {
		t.Fatal("command logs must preserve identity without target credentials")
	}
}
