package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func pass3Barrier(t *testing.T, conn *websocket.Conn, nodeID string) {
	t.Helper()
	s11SendCommand(t, conn, nodeID, CommandRequest{ID: "barrier", Type: "invalid"})
	for _, kind := range []string{"error", "done"} {
		frame, ok := s11ReadFrame(t, conn, time.Second)
		if !ok || frame.ID != "barrier" || frame.Type != kind {
			t.Fatalf("unexpected frame before barrier: %+v", frame)
		}
		if kind == "done" {
			var completion struct {
				ExitOK *bool `json:"exit_ok"`
			}
			if json.Unmarshal([]byte(frame.Data), &completion) != nil || completion.ExitOK == nil || *completion.ExitOK {
				t.Fatalf("validation error must explicitly report failure: %+v", frame)
			}
		}
	}
}

func TestPass3DisconnectedReservationWaitsForCancelAndDone(t *testing.T) {
	for _, doneFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("done-before-cancel=%t", doneFirst), func(t *testing.T) {
			srv, ts := s11NewServer(t)
			agent, nodeID := s5RegisterAgent(t, srv, ts, s5RegisterPayload("teardown"))
			first := s11DialClient(t, ts)
			s11SendCommand(t, first, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "1.1.1.1"})
			var msg AgentMessage
			if err := agent.ReadJSON(&msg); err != nil {
				t.Fatal(err)
			}
			oldOwner := s12CmdOwner(srv, "reuse")
			srv.nodesMu.RLock()
			node := srv.nodes[nodeID]
			srv.nodesMu.RUnlock()
			node.mu.Lock()
			locked := true
			defer func() {
				if locked {
					node.mu.Unlock()
				}
			}()
			first.Close()
			waitFor(t, "closing reservation", time.Second, func() bool {
				srv.cmdOwnersMu.RLock()
				defer srv.cmdOwnersMu.RUnlock()
				return srv.closingCommands["reuse"] != nil
			})
			second := s11DialClient(t, ts)
			s11SendCommand(t, second, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
			pass3Barrier(t, second, nodeID)
			if s12CmdOwner(srv, "reuse") != oldOwner {
				t.Fatal("disconnected reservation stolen")
			}
			// Output while cancel is blocked still belongs to the old connection.
			s5Send(t, agent, "output", map[string]any{"id": "reuse", "type": "output", "data": "old output"})
			if doneFirst {
				s5Send(t, agent, "output", map[string]any{"id": "reuse", "type": "done"})
				waitFor(t, "terminal recorded behind queued cancel", time.Second, func() bool {
					srv.cmdOwnersMu.RLock()
					defer srv.cmdOwnersMu.RUnlock()
					state := srv.closingCommands["reuse"]
					return state != nil && state.terminal
				})
				s11SendCommand(t, second, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
				pass3Barrier(t, second, nodeID)
				if s12CmdOwner(srv, "reuse") != oldOwner {
					t.Fatal("done released ID ahead of queued cancel")
				}
			}
			node.mu.Unlock()
			locked = false
			agent.SetReadDeadline(time.Now().Add(time.Second))
			if err := agent.ReadJSON(&msg); err != nil || msg.Action != "cancel" {
				t.Fatalf("missing cancel: %+v %v", msg, err)
			}
			if !doneFirst {
				s11SendCommand(t, second, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
				pass3Barrier(t, second, nodeID)
				if s12CmdOwner(srv, "reuse") != oldOwner {
					t.Fatal("cancel dispatch released ID before done")
				}
				s5Send(t, agent, "output", map[string]any{"id": "reuse", "type": "done"})
			}
			waitFor(t, "old reservation retired", time.Second, func() bool { return s12CmdOwner(srv, "reuse") == nil })
			s11SendCommand(t, second, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
			if err := agent.ReadJSON(&msg); err != nil || msg.Action != "command" {
				t.Fatalf("missing fresh command: %+v %v", msg, err)
			}
			s5Send(t, agent, "output", map[string]any{"id": "reuse", "type": "output", "data": "new output"})
			frame, ok := s11ReadFrame(t, second, time.Second)
			if !ok || frame.Data != "new output" {
				t.Fatalf("old output leaked or new output lost: %+v", frame)
			}
		})
	}
}

func TestPass3UnacknowledgedCancelClosesAgentAndReleasesReservation(t *testing.T) {
	srv, ts := s11NewServer(t)
	ids, agents := s12Agents(t, srv, ts, "no-cancel-ack", 1)
	client := s11DialClient(t, ts)
	s11SendCommand(t, client, ids[0], CommandRequest{ID: "orphan", Type: "ping", Target: "1.1.1.1"})
	agents[0].awaitCommand(t)
	client.Close()
	waitFor(t, "cancellation acknowledgment deadline armed", time.Second, func() bool {
		srv.cmdOwnersMu.Lock()
		defer srv.cmdOwnersMu.Unlock()
		state := srv.closingCommands["orphan"]
		if state == nil || state.timer == nil {
			return false
		}
		state.timer.Reset(20 * time.Millisecond)
		return true
	})
	waitFor(t, "unresponsive agent disconnect and reservation cleanup", time.Second, func() bool {
		srv.nodesMu.RLock()
		node := srv.nodes[ids[0]]
		offline := !node.Online && !node.disconnecting
		srv.nodesMu.RUnlock()
		return offline && s12CmdOwner(srv, "orphan") == nil && s12CmdNode(srv, "orphan") == ""
	})
	srv.cmdOwnersMu.RLock()
	defer srv.cmdOwnersMu.RUnlock()
	if len(srv.closingCommands) != 0 {
		t.Fatal("closing reservation leaked")
	}
}

func TestPass3PublicConfigAlwaysReturnsTargetArray(t *testing.T) {
	t.Setenv("PUBLIC_MODE", "1")
	for _, raw := range []string{"", ", ,", "1.1.1.1"} {
		t.Setenv("PUBLIC_TARGETS", raw)
		rec := httptest.NewRecorder()
		NewServer().handlePublicConfig(rec, httptest.NewRequest("GET", "/api/public-config", nil))
		var config map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &config); err != nil {
			t.Fatal(err)
		}
		if _, ok := config["allowed_targets"].([]any); !ok {
			t.Fatalf("targets must be array: %s", rec.Body.String())
		}
	}
}

func TestPass3ShutdownHelper(t *testing.T) {
	if os.Getenv("PARALLAX_SHUTDOWN_TEST_CHILD") != "1" {
		return
	}
	os.Setenv("SCHEDULES_FILE", os.Getenv("PARALLAX_SHUTDOWN_TEST_SCHEDULES"))
	main()
	os.RemoveAll(testTempRoot)
	os.Exit(0)
}

func TestPass3ShutdownDrainsMutationAndPersistsBeforeExit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	path := filepath.Join(t.TempDir(), "schedules.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPass3ShutdownHelper$")
	cmd.Env = append(os.Environ(), "PARALLAX_SHUTDOWN_TEST_CHILD=1", "PARALLAX_SHUTDOWN_TEST_SCHEDULES="+path, "PORT="+strconv.Itoa(port), "SCHEDULES_FILE="+path, "MESH_INTERVAL_SEC=0", "PUBLIC_MODE=0", "CLIENT_API_KEY=", "AGENT_API_KEY=", "ALLOWED_ORIGINS=")
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() { cmd.Process.Kill() }()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitFor(t, "subprocess listener", 5*time.Second, func() bool {
		c, e := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if e == nil {
			c.Close()
		}
		return e == nil
	})
	agent, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err := agent.WriteJSON(map[string]any{"action": "register", "payload": map[string]string{"name": "shutdown-node"}}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]string
	if err := agent.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"node_id":%q,"command":"ping","target":"1.1.1.1","interval_sec":60}`, ack["id"])
	seed, err := http.Post("http://"+addr+"/api/schedules", "application/json", strings.NewReader(strings.Replace(body, "1.1.1.1", "1.0.0.1", 1)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, seed.Body)
	seed.Body.Close()
	if seed.StatusCode != 201 {
		t.Fatalf("seed schedule: %d", seed.StatusCode)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST /api/schedules HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n", len(body))
	reader := bufio.NewReader(conn)
	interim, err := http.ReadResponse(reader, nil)
	if err != nil || interim.StatusCode != 100 {
		t.Fatalf("handler not waiting for body: %v %v", interim, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	fmt.Fprint(conn, body)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("shutdown abandoned mutation: %v", err)
	}
	reply, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("mutation failed: %d %s", response.StatusCode, reply)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("server failed: %v %s", err, logs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish draining")
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(persisted, []byte(`"target": "1.1.1.1"`)) {
		t.Fatalf("final mutation not persisted: %v %s; %s", err, persisted, logs.String())
	}
}

func TestPass3ShutdownDeadlineClosesIncompleteBody(t *testing.T) {
	t.Setenv("SCHEDULES_FILE", filepath.Join(t.TempDir(), "schedules.json"))
	srv := NewServer()
	entered := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); io.ReadAll(r.Body) })}
	go server.Serve(listener)
	defer server.Close()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n{")
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	srv.shutdown(ctx, server)
	if time.Since(started) > time.Second {
		t.Fatal("shutdown deadline was not bounded")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = bufio.NewReader(conn).ReadString('\n')
	if err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("incomplete body connection was not closed: %v", err)
	}
}

func TestPass3ShutdownPersistsWebSocketTerminalState(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	agent := dialFakeAgent(t, ts, "shutdown-finalizer")
	nodeID := waitForNodeID(t, srv, "shutdown-finalizer")
	sc := addSchedule(srv, nodeID, "ping", "1.1.1.1")
	cmdID, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatal(reason)
	}
	srv.dispatchSchedule(sc, cmdID)
	agent.awaitCommand(t)
	srv.saveSchedules() // Prime the coalescer; disconnect finalization must be flushed.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv.shutdown(ctx, ts.Config)
	status, _, _ := scheduleState(srv, sc)
	if status != "agent_offline" || scheduleRunCount(srv) != 0 || cmdNodeExists(srv, cmdID) {
		t.Fatalf("run not finalized or registration leaked: %s", status)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(persisted, []byte(`"last_status": "agent_offline"`)) {
		t.Fatalf("terminal state missing: %v %s", err, persisted)
	}
	srv.webSocketsMu.Lock()
	remaining := len(srv.webSockets)
	srv.webSocketsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d socket handlers outlived shutdown", remaining)
	}
}

func TestPass3ShutdownWaitsForPersistenceAlreadyInFlight(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	addSchedule(srv, "test-node", "ping", "1.1.1.1")
	srv.saveMu.Lock()
	srv.savePath, srv.saveDirty = path, true
	srv.saveMu.Unlock()
	srv.saveWriteMu.Lock()
	writerDone := make(chan struct{})
	go func() { srv.flushSchedules(); close(writerDone) }()
	waitFor(t, "trailing writer claimed dirty state", time.Second, func() bool {
		srv.saveMu.Lock()
		defer srv.saveMu.Unlock()
		return !srv.saveDirty
	})
	shutdownDone := make(chan struct{})
	go func() { srv.shutdown(context.Background(), ts.Config); close(shutdownDone) }()
	returnedEarly := false
	select {
	case <-shutdownDone:
		returnedEarly = true
	case <-time.After(50 * time.Millisecond):
	}
	srv.saveWriteMu.Unlock()
	<-writerDone
	<-shutdownDone
	if returnedEarly {
		t.Fatal("shutdown returned while the final schedule write was still blocked")
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(persisted, []byte(`"target": "1.1.1.1"`)) {
		t.Fatalf("missing final state: %v %s", err, persisted)
	}
}

func TestPass3SimultaneousAgentAndBrowserDisconnectKeepsReservation(t *testing.T) {
	srv, ts := s11NewServer(t)
	ids, agents := s12Agents(t, srv, ts, "simultaneous-disconnect", 2)
	first := s11DialClient(t, ts)
	s11SendCommand(t, first, ids[0], CommandRequest{ID: "reuse", Type: "ping", Target: "1.1.1.1"})
	agents[0].awaitCommand(t)
	oldOwner := s12CmdOwner(srv, "reuse")
	srv.scheduleRunsMu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.scheduleRunsMu.Unlock()
		}
	}()
	agents[0].close()
	waitFor(t, "agent terminal processing paused", time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return srv.nodes[ids[0]].disconnecting
	})
	first.Close()
	waitFor(t, "browser cancellation processed", time.Second, func() bool {
		srv.cmdOwnersMu.RLock()
		defer srv.cmdOwnersMu.RUnlock()
		state := srv.closingCommands["reuse"]
		return state != nil && !state.cancelPending
	})
	second := s11DialClient(t, ts)
	s11SendCommand(t, second, ids[1], CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
	pass3Barrier(t, second, ids[1])
	if s12CmdOwner(srv, "reuse") != oldOwner || s12CmdNode(srv, "reuse") != ids[0] {
		t.Fatal("old agent terminal response can target a reused ID")
	}
	srv.scheduleRunsMu.Unlock()
	locked = false
	waitFor(t, "terminal reservation retired", time.Second, func() bool { return s12CmdOwner(srv, "reuse") == nil })
	s11SendCommand(t, second, ids[1], CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
	agents[1].awaitCommand(t)
	agentReply(t, agents[1], "reuse", "output", "new command only")
	frame, ok := s11ReadFrame(t, second, time.Second)
	for ok && frame.Type == "node_status" {
		frame, ok = s11ReadFrame(t, second, time.Second)
	}
	if !ok || frame.Type != "output" || frame.Data != "new command only" {
		t.Fatalf("old terminal reached new owner: %+v", frame)
	}
}

func TestPass3StaleControlsCannotReachReusedCommand(t *testing.T) {
	for _, action := range []string{"cancel", "shell_input"} {
		t.Run(action, func(t *testing.T) {
			srv, ts := s11NewServer(t)
			agent, nodeID := s5RegisterAgent(t, srv, ts, s5RegisterPayload("control-reuse"))
			first := s11DialClient(t, ts)
			s11SendCommand(t, first, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "1.1.1.1"})
			var msg AgentMessage
			if err := agent.ReadJSON(&msg); err != nil {
				t.Fatal(err)
			}
			oldOwner := s12CmdOwner(srv, "reuse")
			srv.nodesMu.RLock()
			node := srv.nodes[nodeID]
			srv.nodesMu.RUnlock()
			node.mu.Lock()
			locked := true
			defer func() {
				if locked {
					node.mu.Unlock()
				}
			}()
			// Model a control queued after its initial owner/node snapshot, with the
			// writer unavailable until the old command ends and another owner reuses ID.
			payload, _ := json.Marshal(map[string]any{"id": "reuse", "input": map[string]string{"data": "stale input"}})
			control, _ := json.Marshal(AgentMessage{Action: action, Payload: payload})
			controlDone := make(chan struct{})
			go func() { srv.writeOwnedControl(oldOwner, "reuse", node, control); close(controlDone) }()
			s5Send(t, agent, "output", map[string]any{"id": "reuse", "type": "done"})
			frame, ok := s11ReadFrame(t, first, time.Second)
			if !ok || frame.Type != "done" {
				t.Fatalf("old terminal: %+v", frame)
			}
			second := s11DialClient(t, ts)
			s11SendCommand(t, second, nodeID, CommandRequest{ID: "reuse", Type: "ping", Target: "8.8.8.8"})
			waitFor(t, "new owner admitted behind queued control", time.Second, func() bool {
				owner := s12CmdOwner(srv, "reuse")
				return owner != nil && owner != oldOwner
			})
			node.mu.Unlock()
			locked = false
			<-controlDone
			agent.SetReadDeadline(time.Now().Add(time.Second))
			if err := agent.ReadJSON(&msg); err != nil || msg.Action != "command" {
				t.Fatalf("stale control reached agent: %+v %v", msg, err)
			}
			agent.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if err := agent.ReadJSON(&msg); err == nil {
				t.Fatalf("unexpected control after reused command: %+v", msg)
			}
		})
	}
}
