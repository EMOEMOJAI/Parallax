package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

func TestExecuteCommandRejectsOversizedOutputLine(t *testing.T) {
	withTestCommand(t, "testlongline", func(CommandRequest) (string, []string, error) {
		return "/bin/sh", []string{"-c", "head -c 1100000 /dev/zero | tr '\\000' x"}, nil
	})
	start := time.Now()
	fr := runExecuteCommand(t, CommandRequest{ID: "long-line", Type: "testlongline", Target: "unused"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
	if err, ok := fr.first("error"); !ok || !strings.Contains(err.Data, "Failed to read command output") {
		t.Fatalf("missing read error: %+v", fr.frames)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("oversized output did not stop promptly")
	}
}

func TestExecuteCommandOutputCapCountsBlankLinesAndEscapes(t *testing.T) {
	old := maxOutputBytes
	maxOutputBytes = 32
	defer func() { maxOutputBytes = old }()
	for _, script := range []string{"i=0; while [ $i -lt 100 ]; do printf '\\n'; i=$((i+1)); done", "i=0; while [ $i -lt 100 ]; do printf '\\033[31m\\n'; i=$((i+1)); done"} {
		t.Run(script, func(t *testing.T) {
			withTestCommand(t, "testrawcap", func(CommandRequest) (string, []string, error) { return "/bin/sh", []string{"-c", script}, nil })
			fr := runExecuteCommand(t, CommandRequest{ID: "raw-cap", Type: "testrawcap", Target: "unused"})
			assertExitOK(t, fr, false)
			if err, ok := fr.first("error"); !ok || !strings.Contains(err.Data, "Output size limit") {
				t.Fatalf("cap not enforced: %+v", fr.frames)
			}
		})
	}
}

func TestExecRunRegistrationRejectsDuplicatesAcrossProbeKinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, refusal := registerExecRun("shared-id", cancel)
	if refusal != "" {
		t.Fatal(refusal)
	}
	defer unregisterExecRun("shared-id", run)
	if _, refusal := registerExecRun("shared-id", func() {}); refusal == "" {
		t.Fatal("duplicate exec run admitted")
	}
	if refusal := registerNativeRun(CommandRequest{ID: "shared-id", Type: "tcp"}, 1, func() {}); refusal == "" {
		t.Fatal("native run overwrote exec run")
	}
	unregisterExecRun("shared-id", &execRun{})
	runningCmdsMu.Lock()
	registered := runningCmds["shared-id"]
	registered.cancel()
	runningCmdsMu.Unlock()
	if registered != run {
		t.Fatal("unrelated cleanup removed current run")
	}
	if ctx.Err() == nil {
		t.Fatal("registered cancellation did not cancel context")
	}
}

func TestConnectionDropCancelsExecAndWaitsForCleanup(t *testing.T) {
	withTestCommand(t, "testdisconnect", func(CommandRequest) (string, []string, error) {
		return "/bin/sh", []string{"-c", "echo started; sleep 30"}, nil
	})
	start := time.Now()
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		s5ReadEnvelope(conn)
		payload, _ := json.Marshal(CommandRequest{ID: "drop-exec", Type: "testdisconnect", Target: "unused"})
		conn.WriteJSON(AgentMessage{Action: "command", Payload: payload})
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		s5ReadEnvelope(conn) // command started, with a helper inheriting stdout
	})
	if time.Since(start) > 5*time.Second {
		t.Fatal("disconnect left a blocked exec worker")
	}
	runningCmdsMu.Lock()
	n := len(runningCmds)
	runningCmdsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d exec runs remain after disconnect", n)
	}
}

func TestImmediateExecCancelRacesSafelyWithStartup(t *testing.T) {
	withTestCommand(t, "testcancelrace", func(CommandRequest) (string, []string, error) { return "/bin/sh", []string{"-c", "sleep 0.01"}, nil })
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		s5ReadEnvelope(conn)
		for i := 0; i < 50; i++ {
			id := fmt.Sprintf("race-%d", i)
			payload, _ := json.Marshal(CommandRequest{ID: id, Type: "testcancelrace", Target: "unused"})
			conn.WriteJSON(AgentMessage{Action: "command", Payload: payload})
			payload, _ = json.Marshal(map[string]string{"id": id})
			conn.WriteJSON(AgentMessage{Action: "cancel", Payload: payload})
		}
	})
}

func TestHTTPProbeIgnoresCurlConfigAndURLGlobs(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	dir := t.TempDir()
	// A hostile/stale user config must not enable redirect following.
	if err := os.WriteFile(filepath.Join(dir, ".curlrc"), []byte("location\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasPrefix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "/unexpected", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	for _, suffix := range []string{"/redirect", "/{one,two}"} {
		requests.Store(0)
		bin, args, err := buildHTTP(CommandRequest{Target: ts.URL + suffix, Options: "-4"})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "CURL_HOME="+dir, "NO_PROXY=*", "no_proxy=*")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("curl: %v: %s", err, out)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("%s made %d requests, want one", suffix, got)
		}
	}
}

func TestShellExitReleasesSessionAndWatcher(t *testing.T) {
	disconnected := make(chan struct{})
	stopped := make(chan struct{})
	frames := make(chan CommandResponse, 64)
	go func() {
		defer close(stopped)
		startShellSession("shell-exit", func(id, typ, data string) { frames <- CommandResponse{id, typ, data} }, disconnected)
	}()
	select {
	case frame := <-frames:
		if frame.Type == "error" {
			t.Fatal(frame.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not start")
	}
	// A duplicate ID must neither replace this shell nor consume a second slot.
	var duplicate []CommandResponse
	startShellSession("shell-exit", func(id, typ, data string) { duplicate = append(duplicate, CommandResponse{id, typ, data}) }, disconnected)
	if len(duplicate) != 2 || duplicate[0].Type != "error" {
		t.Fatalf("duplicate session admitted: %+v", duplicate)
	}
	handleShellInput("shell-exit", ShellInput{Action: "resize", Cols: 100, Rows: 35})
	handleShellInput("shell-exit", ShellInput{Action: "data", Data: "exit\n"})
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		close(disconnected)
		t.Fatal("shell exit left its disconnect watcher running")
	}
	shellSessionsMu.RLock()
	_, present := shellSessions["shell-exit"]
	shellSessionsMu.RUnlock()
	if present {
		t.Fatal("shell session remained registered")
	}
}

func TestShellInputBackpressureDoesNotBlockConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &ShellSession{input: make(chan ShellInput, 1), cancel: cancel}
	shellSessionsMu.Lock()
	shellSessions["blocked-input"] = session
	shellSessionsMu.Unlock()
	defer func() { shellSessionsMu.Lock(); delete(shellSessions, "blocked-input"); shellSessionsMu.Unlock() }()
	handleShellInput("blocked-input", ShellInput{Action: "data", Data: strings.Repeat("x", 20000)})
	if got := len((<-session.input).Data); got != 16384 {
		t.Fatalf("input size = %d", got)
	}
	handleShellInput("blocked-input", ShellInput{Action: "data", Data: "one"})
	handleShellInput("blocked-input", ShellInput{Action: "data", Data: "two"})
	if ctx.Err() == nil {
		t.Fatal("full input queue did not cancel stalled session")
	}
}

func TestPollablePTYCloseInterruptsBlockedRead(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	file, err := pollablePTY(master)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { var b [1]byte; _, err := file.Read(b[:]); readDone <- err }()
	if err := file.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("PTY does not support deadlines: %v", err)
	}
	file.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("closing PTY did not unblock read")
	}
}

func TestImmediateCancelBeforeCommandRegistrationPreventsExec(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	path := filepath.Join(t.TempDir(), "should-not-exist")
	withTestCommand(t, "testcancelpending", func(CommandRequest) (string, []string, error) {
		close(ready)
		<-release
		return "/bin/sh", []string{"-c", "touch \"$1\"", "sh", path}, nil
	})
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		s5ReadEnvelope(conn)
		payload, _ := json.Marshal(CommandRequest{ID: "pending", Type: "testcancelpending", Target: "unused"})
		conn.WriteJSON(AgentMessage{Action: "command", Payload: payload})
		<-ready
		payload, _ = json.Marshal(map[string]string{"id": "pending"})
		conn.WriteJSON(AgentMessage{Action: "cancel", Payload: payload})
		// Ping/pong acknowledges that the read loop processed the preceding cancel.
		ack := make(chan struct{})
		conn.SetPongHandler(func(string) error { close(ack); return nil })
		conn.WriteControl(websocket.PingMessage, []byte("processed"), time.Now().Add(time.Second))
		go func() { <-ack; close(release) }()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			env, _, err := s5ReadEnvelope(conn)
			if err != nil {
				return
			}
			var response CommandResponse
			if env.Action == "output" && json.Unmarshal(env.Payload, &response) == nil && response.Type == "done" {
				if response.Data != doneData(false) {
					t.Errorf("cancelled command completed successfully: %s", response.Data)
				}
				return
			}
		}
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled command executed: %v", err)
	}
}

func TestServerLogURLRedactsCredentials(t *testing.T) {
	for _, raw := range []string{"ws://host/ws?key=secret", "wss://user:password@host/ws?key=secret#private-fragment"} {
		got := serverLogURL(raw)
		for _, secret := range []string{"secret", "password", "user", "private-fragment"} {
			if strings.Contains(got, secret) {
				t.Fatalf("URL log leaked %s: %s", secret, got)
			}
		}
		if !strings.Contains(got, "host/ws") {
			t.Fatalf("log lost server identity: %s", got)
		}
	}
}

func TestNativeAddressPolicyBlocksLocalUseTranslation(t *testing.T) {
	s7Seams(t)
	for _, allowPrivate := range []bool{false, true} {
		probeAllowPrivate.Store(allowPrivate)
		for _, raw := range []string{"64:ff9b:1::7f00:1", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff"} {
			addr := netip.MustParseAddr(raw)
			if !isBlockedAddr(addr) {
				t.Errorf("private=%v permits local-use translation %s", allowPrivate, addr)
			}
			if err := probeDialControl("tcp6", netip.AddrPortFrom(addr, 80).String(), nil); err == nil {
				t.Errorf("dial allowed translation %s", addr)
			}
		}
	}
	got := embeddedIPv4s(netip.MustParseAddr("64:ff9b::c0a8:1"))
	if len(got) != 1 || got[0].String() != "192.168.0.1" {
		t.Fatalf("NAT64 embedded address = %v", got)
	}
}

func TestCommandLogOmitsSensitiveTarget(t *testing.T) {
	withTestCommand(t, "testlogprivacy", func(CommandRequest) (string, []string, error) {
		return "/bin/true", nil, nil
	})
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	runExecuteCommand(t, CommandRequest{ID: "log-test", Type: "testlogprivacy", Target: "https://example.com/private-token-path?key=synthetic-secret"})
	if text := output.String(); strings.Contains(text, "synthetic-secret") || strings.Contains(text, "private-token-path") || !strings.Contains(text, "log-test") {
		t.Fatal("command logs must preserve identity without target credentials")
	}
}
