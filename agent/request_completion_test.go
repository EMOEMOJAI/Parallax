package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCompletionWaitsForCleanupBeforeAllowingIDReuse(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	old := nativeProbes["tcp"]
	t.Cleanup(func() { nativeProbes["tcp"] = old })
	nativeProbes["tcp"] = probeFunc(func(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
		if calls.Add(1) == 1 {
			close(ready)
			<-release
			if err := emit.emitLine("output before failure"); err != nil {
				return err
			}
			return errors.New("expected first-request failure")
		}
		return emit.emitLine("reused ID ran successfully")
	})
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			t.Error(err)
			return
		}
		type result struct {
			frame CommandResponse
			err   error
		}
		frames := make(chan result, 16)
		go func() {
			for {
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				msg, _, err := s5ReadEnvelope(conn)
				var frame CommandResponse
				if err == nil {
					err = json.Unmarshal(msg.Payload, &frame)
				}
				frames <- result{frame, err}
				if err != nil {
					return
				}
			}
		}()
		read := func() (CommandResponse, bool) {
			select {
			case r := <-frames:
				if r.err != nil {
					t.Error(r.err)
					return r.frame, false
				}
				return r.frame, true
			case <-time.After(4 * time.Second):
				t.Error("timed out waiting for output")
				return CommandResponse{}, false
			}
		}
		payload, _ := json.Marshal(CommandRequest{ID: "reuse", Type: "tcp", Target: "example.test:443"})
		send := func() bool {
			if err := conn.WriteJSON(AgentMessage{Action: "command", Payload: payload}); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
		if !send() {
			return
		}
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Error("probe did not start")
			return
		}
		// Pause only the first worker's deferred unregister, after its probe has
		// finished. Completion must not release the server's ID while this lock
		// still prevents the agent from releasing its own reservation.
		runningCmdsMu.Lock()
		locked := true
		defer func() {
			if locked {
				runningCmdsMu.Unlock()
			}
		}()
		close(release)
		for _, typ := range []string{"output", "error"} {
			frame, ok := read()
			if !ok {
				return
			}
			if frame.Type != typ {
				t.Errorf("before cleanup: got %q, want %q", frame.Type, typ)
				return
			}
		}
		select {
		case r := <-frames:
			t.Errorf("terminal output published before cleanup finished: %+v", r)
			return
		case <-time.After(50 * time.Millisecond):
		}
		runningCmdsMu.Unlock()
		locked = false
		frame, ok := read()
		if !ok {
			return
		}
		if frame.Type != "done" || frame.Data != doneData(false) {
			t.Errorf("first terminal: %+v", frame)
			return
		}
		if !send() {
			return
		}
		frame, ok = read()
		if !ok {
			return
		}
		if frame.Type != "output" || frame.Data != "reused ID ran successfully" {
			t.Errorf("reused request did not run: %+v", frame)
			return
		}
		frame, ok = read()
		if !ok {
			return
		}
		if frame.Type != "done" || frame.Data != doneData(true) {
			t.Errorf("second terminal: %+v", frame)
		}
		if calls.Load() != 2 {
			t.Errorf("probe ran %d times, want 2", calls.Load())
		}
	})
}

func TestDuplicateRequestsCannotRetireAnActiveCommand(t *testing.T) {
	oldProbe, oldShell := nativeProbes["tcp"], allowShell
	t.Cleanup(func() { nativeProbes["tcp"], allowShell = oldProbe, oldShell })
	allowShell = false
	var calls atomic.Int32
	nativeProbes["tcp"] = probeFunc(func(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
		calls.Add(1)
		if err := emit.emitLine("started"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		s5ReadEnvelope(conn)
		payload, _ := json.Marshal(CommandRequest{ID: "active", Type: "tcp", Target: "example.test:443"})
		send := func(action string, data json.RawMessage) {
			if err := conn.WriteJSON(AgentMessage{Action: action, Payload: data}); err != nil {
				t.Error(err)
			}
		}
		read := func() CommandResponse {
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			envelope, _, err := s5ReadEnvelope(conn)
			if err != nil {
				t.Error(err)
				return CommandResponse{}
			}
			var frame CommandResponse
			if err := json.Unmarshal(envelope.Payload, &frame); err != nil {
				t.Error(err)
			}
			return frame
		}
		send("command", payload)
		if frame := read(); frame.Type != "output" || frame.Data != "started" {
			t.Errorf("unexpected start: %+v", frame)
			return
		}
		send("command", payload)
		send("shell_start", json.RawMessage(`{"id":"active"}`))
		send("cancel", json.RawMessage(`{"id":"active"}`))
		frame := read()
		if frame.Type != "error" || !strings.Contains(frame.Data, "canceled") {
			t.Errorf("duplicate request displaced cancellation result: %+v", frame)
			return
		}
		if frame := read(); frame.Type != "done" || frame.Data != doneData(false) {
			t.Errorf("unexpected completion: %+v", frame)
		}
		if calls.Load() != 1 {
			t.Errorf("duplicate ran %d times", calls.Load())
		}
	})
}
