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

// S11 — the client command validation matrix.
//
// handleClientCommand (client_ws.go) is the primary untrusted-input boundary:
// every browser-supplied command flows through it before anything is sent to
// an agent. It had no direct test before this file. Every helper here carries
// an s11 prefix so it cannot collide with the other suites' fakes.
//
// Two failure shapes matter and must not be conflated:
//   - a rejected command that the caller *caused* (bad type, oversize/control
//     fields) gets an "error" frame followed by a "done" frame
//     (sendErrorAndDone, util.go);
//   - an oversize or control-character node_id / command.ID is a silent
//     `return` with no frame at all (client_ws.go:439-449, :499-501) — that
//     must be observed as the *absence* of any frame, not as an error message.

func s11NewServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	for _, k := range []string{
		"AGENT_API_KEY", "CLIENT_API_KEY", "ALLOWED_ORIGINS", "PUBLIC_MODE",
		"STRICT_ORIGIN", "TRUST_PROXY", "METRICS_TOKEN", "MESH_INTERVAL_SEC",
		"ALERT_WEBHOOK_URL",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("SCHEDULES_FILE", testSchedulesPath(t))

	srv := NewServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/ws/client", srv.handleClientWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

// s11DialClient opens an authenticated-by-default browser socket (no API key
// is configured in these tests).
func s11DialClient(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := dialClientWS(ts, nil)
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// s11SendCommand marshals and sends a {node_id, command} frame exactly as the
// browser client does.
func s11SendCommand(t *testing.T, conn *websocket.Conn, nodeID string, cmd CommandRequest) {
	t.Helper()
	req := struct {
		NodeID  string         `json:"node_id"`
		Command CommandRequest `json:"command"`
	}{NodeID: nodeID, Command: cmd}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("write command: %v", err)
	}
}

// s11ReadFrame reads one CommandResponse frame within the timeout. ok is
// false if nothing arrived (deadline exceeded) or the socket closed.
func s11ReadFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) (resp CommandResponse, ok bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return CommandResponse{}, false
	}
	if err := json.Unmarshal(msg, &resp); err != nil {
		t.Fatalf("unmarshal frame: %v (raw: %s)", err, msg)
	}
	return resp, true
}

// s11ExpectError asserts an "error" frame containing wantSubstr, immediately
// followed by a "done" frame — the sendErrorAndDone pattern every rejection in
// handleClientCommand uses.
func s11ExpectError(t *testing.T, conn *websocket.Conn, wantSubstr string) {
	t.Helper()
	errFrame, ok := s11ReadFrame(t, conn, 3*time.Second)
	if !ok {
		t.Fatalf("expected error frame containing %q, got none", wantSubstr)
	}
	if errFrame.Type != "error" || !strings.Contains(errFrame.Data, wantSubstr) {
		t.Fatalf("got frame %+v, want error containing %q", errFrame, wantSubstr)
	}
	doneFrame, ok := s11ReadFrame(t, conn, 3*time.Second)
	if !ok || doneFrame.Type != "done" {
		t.Fatalf("expected done frame after error, got %+v (ok=%v)", doneFrame, ok)
	}
}

// s11ExpectSilence asserts no frame arrives within window — the only way to
// observe the silent-return rejection paths.
func s11ExpectSilence(t *testing.T, conn *websocket.Conn, window time.Duration) {
	t.Helper()
	if resp, ok := s11ReadFrame(t, conn, window); ok {
		t.Fatalf("expected silence, got frame %+v", resp)
	}
}

// ---- unknown command type ----------------------------------------------------

func TestS11UnknownCommandTypeRejected(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	s11SendCommand(t, conn, "some-node", CommandRequest{ID: "s11-unknown-type", Type: "rm-rf", Target: "1.1.1.1"})
	s11ExpectError(t, conn, `Command type "rm-rf" is not allowed`)
}

func TestS11ShellTypeRejected(t *testing.T) {
	// "shell" is deliberately excluded from allowedCommandTypes; interactive
	// shells use the separate shell_start/shell_input action pair.
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	s11SendCommand(t, conn, "some-node", CommandRequest{ID: "s11-shell-type", Type: "shell", Target: "1.1.1.1"})
	s11ExpectError(t, conn, `Command type "shell" is not allowed`)
}

// ---- field length caps: Type/Target/Options at 32/1024/512 ------------------
//
// No whitelisted command type exceeds ten characters, so "accepted just under
// the cap" is only observable through which error message comes back:
// "Command fields too long" (over cap) vs. "Command type ... is not allowed"
// or "Node is offline or not found" (under cap, rejected or accepted later
// for an unrelated reason).

func TestS11TypeLengthCap(t *testing.T) {
	_, ts := s11NewServer(t)

	t.Run("at cap is not a length rejection", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		typ := strings.Repeat("a", 32) // == cap, not a real command type
		s11SendCommand(t, conn, "some-node", CommandRequest{ID: "s11-type-at-cap", Type: typ, Target: "1.1.1.1"})
		s11ExpectError(t, conn, "is not allowed")
	})

	t.Run("over cap is rejected as too long", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		typ := strings.Repeat("a", 33)
		s11SendCommand(t, conn, "some-node", CommandRequest{ID: "s11-type-over-cap", Type: typ, Target: "1.1.1.1"})
		s11ExpectError(t, conn, "Command fields too long")
	})
}

func TestS11TargetLengthCap(t *testing.T) {
	_, ts := s11NewServer(t)

	t.Run("at cap passes the length gate", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		target := strings.Repeat("a", 1024) // == cap
		s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-target-at-cap", Type: "ping", Target: target})
		s11ExpectError(t, conn, "Node is offline or not found")
	})

	t.Run("over cap is rejected as too long", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		target := strings.Repeat("a", 1025)
		s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-target-over-cap", Type: "ping", Target: target})
		s11ExpectError(t, conn, "Command fields too long")
	})
}

func TestS11OptionsLengthCap(t *testing.T) {
	_, ts := s11NewServer(t)

	t.Run("at cap passes the length gate", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		opts := strings.Repeat("a", 512) // == cap
		s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-options-at-cap", Type: "ping", Target: "1.1.1.1", Options: opts})
		s11ExpectError(t, conn, "Node is offline or not found")
	})

	t.Run("over cap is rejected as too long", func(t *testing.T) {
		conn := s11DialClient(t, ts)
		opts := strings.Repeat("a", 513)
		s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-options-over-cap", Type: "ping", Target: "1.1.1.1", Options: opts})
		s11ExpectError(t, conn, "Command fields too long")
	})
}

// ---- control characters in target / options ----------------------------------
//
// Rejection covers exactly "\n\r\t\x00" (client_ws.go:487,493). A case built on
// \x1b or \x07 would fail against correct code, since those are not in scope.

func TestS11TargetControlCharactersRejected(t *testing.T) {
	_, ts := s11NewServer(t)
	for _, ch := range []string{"\n", "\r", "\t", "\x00"} {
		ch := ch
		t.Run(fmt.Sprintf("%q", ch), func(t *testing.T) {
			conn := s11DialClient(t, ts)
			s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-target-ctrl-" + strings.TrimPrefix(fmt.Sprintf("%q", ch), "\""), Type: "ping", Target: "1.1.1.1" + ch})
			s11ExpectError(t, conn, "Target contains invalid characters")
		})
	}
}

func TestS11OptionsControlCharactersRejected(t *testing.T) {
	_, ts := s11NewServer(t)
	for _, ch := range []string{"\n", "\r", "\t", "\x00"} {
		ch := ch
		t.Run(fmt.Sprintf("%q", ch), func(t *testing.T) {
			conn := s11DialClient(t, ts)
			s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "s11-options-ctrl-" + strings.TrimPrefix(fmt.Sprintf("%q", ch), "\""), Type: "ping", Target: "1.1.1.1", Options: "count=3" + ch})
			s11ExpectError(t, conn, "Options contain invalid characters")
		})
	}
}

// ---- oversize / control-character node_id and command.ID: silent rejection --

func TestS11OversizeNodeIDIsSilentlyDropped(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	oversizeNodeID := strings.Repeat("n", 129) // > 128 cap
	s11SendCommand(t, conn, oversizeNodeID, CommandRequest{ID: "s11-oversize-node-id", Type: "ping", Target: "1.1.1.1"})
	s11ExpectSilence(t, conn, 300*time.Millisecond)
}

func TestS11ControlCharacterNodeIDIsSilentlyDropped(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	s11SendCommand(t, conn, "bad\nnode\rid\t\x00", CommandRequest{ID: "s11-ctrl-node-id", Type: "ping", Target: "1.1.1.1"})
	s11ExpectSilence(t, conn, 300*time.Millisecond)
}

func TestS11OversizeCommandIDIsSilentlyDropped(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	oversizeID := strings.Repeat("c", 129) // > 128 cap
	// A valid-length command.ID against this unregistered node would produce a
	// "Node is offline or not found" error, proving the oversize case really is
	// caught earlier rather than merely failing to match a node.
	s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: oversizeID, Type: "ping", Target: "1.1.1.1"})
	s11ExpectSilence(t, conn, 300*time.Millisecond)
}

func TestS11ControlCharacterCommandIDIsSilentlyDropped(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)
	s11SendCommand(t, conn, "s11-unregistered-node", CommandRequest{ID: "bad\nid", Type: "ping", Target: "1.1.1.1"})
	s11ExpectSilence(t, conn, 300*time.Millisecond)
}

// ---- concurrency caps: per-node (5) and per-client (20) ----------------------
//
// maxCommandsPerNode is 5 and trips before maxCommandsPerClient (20), so a
// per-node cap test needs one online node saturated to 5, and a per-client cap
// test needs four online nodes each saturated to 5 (20 total) plus a 21st
// frame at a fifth, unregistered node_id — the per-client check
// (client_ws.go:508) runs before the node lookup (:525), so that 21st frame
// reaches the per-client branch rather than an offline-node error. Every
// command below uses a distinct ID: reusing one hits the pre-existing gap F37
// and would surface it as a false defect here. The fake agent never answers
// output/done, so dispatched commands stay counted as in-flight for the
// duration of the test.

func TestS11PerNodeConcurrencyCap(t *testing.T) {
	srv, ts := s11NewServer(t)
	dialFakeAgent(t, ts, "s11-node-cap-agent")
	nodeID := waitForNodeID(t, srv, "s11-node-cap-agent")

	conn := s11DialClient(t, ts)
	for i := 0; i < maxCommandsPerNode; i++ {
		s11SendCommand(t, conn, nodeID, CommandRequest{ID: fmt.Sprintf("s11-node-cap-%d", i), Type: "ping", Target: "1.1.1.1"})
	}
	// Give the sequential per-connection read loop a moment to dispatch all
	// maxCommandsPerNode commands before the one that should be rejected.
	time.Sleep(150 * time.Millisecond)

	s11SendCommand(t, conn, nodeID, CommandRequest{ID: "s11-node-cap-overflow", Type: "ping", Target: "1.1.1.1"})
	s11ExpectError(t, conn, fmt.Sprintf("Too many concurrent commands on this node (max %d)", maxCommandsPerNode))
}

func TestS11PerClientConcurrencyCap(t *testing.T) {
	srv, ts := s11NewServer(t)
	var nodeIDs []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("s11-client-cap-agent-%d", i)
		dialFakeAgent(t, ts, name)
		nodeIDs = append(nodeIDs, waitForNodeID(t, srv, name))
	}

	conn := s11DialClient(t, ts)
	n := 0
	for _, nodeID := range nodeIDs {
		for j := 0; j < maxCommandsPerNode; j++ {
			s11SendCommand(t, conn, nodeID, CommandRequest{ID: fmt.Sprintf("s11-client-cap-%d", n), Type: "ping", Target: "1.1.1.1"})
			n++
		}
	}
	if n != maxCommandsPerClient {
		t.Fatalf("test setup should saturate exactly maxCommandsPerClient (%d), sent %d", maxCommandsPerClient, n)
	}
	time.Sleep(200 * time.Millisecond)

	// The 21st frame targets a fifth, unregistered node so the per-client
	// check (which runs before the node lookup) is what rejects it.
	s11SendCommand(t, conn, "s11-fifth-unregistered-node", CommandRequest{ID: "s11-client-cap-overflow", Type: "ping", Target: "1.1.1.1"})
	s11ExpectError(t, conn, "Too many concurrent commands")
}
