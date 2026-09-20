package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// S5 — tool probing, the synchronized tool cache, the reported build identity
// and the duplicate-name sentinel. Helpers carry an s5 prefix so they cannot
// collide with the existing builder/summary suites.

// ---- commandBinaries coverage ----------------------------------------------

// Rewritten by S7: an allowed command type now has a binary *or* is a native
// probe type, never both. Native types (tcp/tls/dnsbench/download) are
// implemented in Go and have no binary to look up, so the nativeProbes table
// stands in for commandBuilders/commandBinaries for them.
func TestS5CommandBinariesCoverEveryAllowedCommand(t *testing.T) {
	native := map[string]bool{}
	for _, cmdType := range nativeProbeTypes {
		native[cmdType] = true
	}
	for cmdType := range allowedCommands {
		bin, hasBin := commandBinaries[cmdType]
		switch {
		case hasBin && native[cmdType]:
			t.Errorf("allowedCommands[%q] is both a native probe type and has a commandBinaries entry — it must be exactly one", cmdType)
		case hasBin:
			if strings.TrimSpace(bin) == "" {
				t.Errorf("commandBinaries[%q] is empty", cmdType)
			}
		case native[cmdType]:
			if _, ok := nativeProbes[cmdType]; !ok {
				t.Errorf("allowedCommands[%q] is a native probe type with no nativeProbes entry", cmdType)
			}
			if _, ok := commandBuilders[cmdType]; ok {
				t.Errorf("native probe type %q also has a commandBuilders entry", cmdType)
			}
		default:
			t.Errorf("allowedCommands[%q] has neither a commandBinaries entry nor native-probe status — probeTools would report it absent", cmdType)
		}
	}
	for cmdType := range commandBinaries {
		if !allowedCommands[cmdType] {
			t.Errorf("commandBinaries[%q] is not an allowed command type", cmdType)
		}
	}
	// The two entries whose binary is deliberately not the command type. If a
	// refactor ever "simplifies" the table into an identity map, this fails.
	if commandBinaries["dns"] != "dig" {
		t.Errorf("commandBinaries[dns] = %q, want dig (buildDns execs dig)", commandBinaries["dns"])
	}
	if commandBinaries["http"] != "curl" {
		t.Errorf("commandBinaries[http] = %q, want curl (buildHTTP execs curl)", commandBinaries["http"])
	}
}

// ---- PATH probing -----------------------------------------------------------

// s5EmptyPath points PATH at a directory of the test's own making, so the probe
// result is fully determined by what the test puts there.
func s5EmptyPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return dir
}

func s5FakeBinary(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestS5ProbeToolsFollowsThePathTheExecutorUses(t *testing.T) {
	dir := s5EmptyPath(t)

	// Rewritten by S7: native probe types report true without any PATH lookup,
	// so the empty-PATH expectation covers the binary-backed types only.
	native := map[string]bool{}
	for _, cmdType := range nativeProbeTypes {
		native[cmdType] = true
	}
	tools := probeTools()
	for cmdType := range allowedCommands {
		if native[cmdType] {
			if !tools[cmdType] {
				t.Errorf("native probe type %q reported absent with an empty PATH — it needs no binary", cmdType)
			}
			continue
		}
		if tools[cmdType] {
			t.Errorf("probeTools reported %q present with an empty PATH", cmdType)
		}
	}
	if len(tools) != len(allowedCommands) {
		t.Fatalf("probeTools returned %d entries, want one per allowed command (%d)", len(tools), len(allowedCommands))
	}

	// Install the two binaries whose names differ from their command type; the
	// probe must follow commandBinaries, not the command name.
	s5FakeBinary(t, dir, "dig")
	s5FakeBinary(t, dir, "curl")
	tools = probeTools()
	if !tools["dns"] {
		t.Errorf("dns reported absent although dig is on PATH")
	}
	if !tools["http"] {
		t.Errorf("http reported absent although curl is on PATH")
	}
	if tools["mtr"] {
		t.Errorf("mtr reported present although it is not on PATH")
	}
	// A binary literally named after the command type must NOT satisfy dns.
	s5FakeBinary(t, dir, "ping")
	if tools := probeTools(); !tools["ping"] {
		t.Errorf("ping reported absent although ping is on PATH")
	}
}

func TestS5NativeProbeTypeNeedsNoPathLookup(t *testing.T) {
	s5EmptyPath(t)

	// S5 ships nativeProbeTypes empty; the native probes append to it. Inject a
	// fixture member to prove the mechanism, and prove it does not go through
	// PATH by using a name no binary could have and declaring no binary for it.
	const fixture = "s5-native-fixture"
	if _, declared := commandBinaries[fixture]; declared {
		t.Fatalf("fixture name %q unexpectedly has a binary entry", fixture)
	}
	original := nativeProbeTypes
	nativeProbeTypes = append(append([]string{}, original...), fixture)
	t.Cleanup(func() { nativeProbeTypes = original })

	tools := probeTools()
	if !tools[fixture] {
		t.Fatalf("native probe type %q was not reported available", fixture)
	}
	// And the real, binary-backed types are still absent on an empty PATH, so
	// the native shortcut did not become a blanket "everything is present".
	if tools["ping"] {
		t.Fatalf("ping reported present with an empty PATH")
	}
}

// Rewritten by S7: nativeProbeTypes now carries exactly the four native probes.
func TestS5NativeProbeTypesAreTheFourNativeProbes(t *testing.T) {
	want := []string{"dnsbench", "download", "tcp", "tls"}
	got := append([]string{}, nativeProbeTypes...)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("nativeProbeTypes = %v, want exactly %v", got, want)
	}
}

// ---- the shared tool cache --------------------------------------------------

func TestS5SnapshotToolsReturnsACopy(t *testing.T) {
	original := snapshotTools()
	t.Cleanup(func() { storeTools(original) })

	storeTools(map[string]bool{"ping": true})
	snap := snapshotTools()
	snap["ping"] = false
	snap["injected"] = true
	if again := snapshotTools(); !again["ping"] || again["injected"] {
		t.Fatalf("mutating a snapshot reached the cache: %v", again)
	}
}

// The cache is written by the 5-minute refresher and read by the register writer
// and the 30-second health goroutine. An in-place rebuild would be a concurrent
// map read/write — a fatal throw on a live node, not a test failure — so this
// runs a refresh and snapshot+marshal side by side under -race.
func TestS5ToolCacheRefreshRacesSafelyWithSnapshotAndMarshal(t *testing.T) {
	original := snapshotTools()
	t.Cleanup(func() { storeTools(original) })
	storeTools(probeTools())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the writer: the ticker's job
		defer wg.Done()
		for i := 0; i < 300; i++ {
			refreshTools()
		}
		close(stop)
	}()

	for r := 0; r < 3; r++ { // the readers: register + health
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := snapshotTools()
				if _, err := json.Marshal(snap); err != nil {
					t.Errorf("marshal tool snapshot: %v", err)
					return
				}
				for range snap {
				}
			}
		}()
	}
	wg.Wait()
}

// Nothing may read the cache directly: every reader must go through
// snapshotTools(), or the -race build above proves nothing about the real code.
func TestS5NoRawToolCacheAccessOutsideTheAccessors(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	allowed := map[string]bool{
		"currentTools map[string]bool":                    true, // declaration
		"currentTools = tools":                            true, // storeTools
		"out := make(map[string]bool, len(currentTools))": true, // snapshotTools
		"for name, ok := range currentTools {":            true, // snapshotTools
	}
	seen := 0
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "currentTools") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		seen++
		if !allowed[trimmed] {
			t.Errorf("main.go:%d touches the tool cache outside storeTools/snapshotTools: %s", i+1, trimmed)
		}
	}
	if seen != len(allowed) {
		t.Errorf("found %d tool-cache accesses, expected exactly %d (the accessors)", seen, len(allowed))
	}
}

// ---- reported build identity ------------------------------------------------

func TestS5VersionDefaultsToDevWithoutALdflag(t *testing.T) {
	// `go test` links without -X, which is exactly the `go run .` case.
	if version != "dev" {
		t.Fatalf("version = %q, want dev when no -X main.version is passed", version)
	}
}

// ---- register / health frames and the duplicate-name sentinel ---------------

// s5RunAgainstServer points the agent at a throwaway WebSocket server, runs the
// real run() once and returns its error. The handler must not call t.Fatal — it
// runs on the server's goroutine.
func s5RunAgainstServer(t *testing.T, handler func(conn *websocket.Conn)) error {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	defer ts.Close()

	oldURL := serverURL
	serverURL = "ws" + strings.TrimPrefix(ts.URL, "http")
	defer func() { serverURL = oldURL }()

	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("run() never returned")
		return nil
	}
}

// s5ReadEnvelope reads one frame and decodes the envelope.
func s5ReadEnvelope(conn *websocket.Conn) (AgentMessage, map[string]any, error) {
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return AgentMessage{}, nil, err
	}
	var env AgentMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return AgentMessage{}, nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return env, nil, err
	}
	return env, payload, nil
}

func s5AssertCapabilityPayload(t *testing.T, kind string, payload map[string]any) {
	t.Helper()
	if payload["version"] != version {
		t.Errorf("%s frame version = %#v, want %q", kind, payload["version"], version)
	}
	tools, ok := payload["tools"].(map[string]any)
	if !ok {
		t.Fatalf("%s frame carries no tools object: %#v", kind, payload["tools"])
	}
	for cmdType := range allowedCommands {
		if _, present := tools[cmdType]; !present {
			t.Errorf("%s frame omits tool %q", kind, cmdType)
		}
	}
	for name, v := range tools {
		if _, ok := v.(bool); !ok {
			t.Errorf("%s frame tools[%q] is not a bool: %#v", kind, name, v)
		}
	}
}

func TestS5RegisterFrameCarriesVersionAndTools(t *testing.T) {
	original := snapshotTools()
	t.Cleanup(func() { storeTools(original) })
	storeTools(probeTools())

	type result struct {
		env     AgentMessage
		payload map[string]any
		err     error
	}
	got := make(chan result, 1)
	err := s5RunAgainstServer(t, func(conn *websocket.Conn) {
		env, payload, err := s5ReadEnvelope(conn)
		got <- result{env, payload, err}
		// Dropping the socket makes run() return so the test does not wait for
		// the health tick.
	})
	if err == nil {
		t.Fatalf("run() returned nil after the server closed the socket")
	}

	r := <-got
	if r.err != nil {
		t.Fatalf("read register frame: %v", r.err)
	}
	if r.env.Action != "register" {
		t.Fatalf("first frame action = %q, want register", r.env.Action)
	}
	s5AssertCapabilityPayload(t, "register", r.payload)
}

func TestS5HealthFrameCarriesVersionAndTools(t *testing.T) {
	original := snapshotTools()
	t.Cleanup(func() { storeTools(original) })
	storeTools(probeTools())

	// The 30-second tick is not worth waiting for; healthPayload() is the exact
	// bytes that goroutine sends.
	var payload map[string]any
	if err := json.Unmarshal(healthPayload(), &payload); err != nil {
		t.Fatalf("health payload is not an object: %v", err)
	}
	s5AssertCapabilityPayload(t, "health", payload)
	// It still carries its usual stats.
	if _, present := payload["cpus"]; !present {
		t.Errorf("health frame lost its stats: %#v", payload)
	}
	if _, present := payload["os"]; !present {
		t.Errorf("health frame lost its stats: %#v", payload)
	}
}

func TestS5DuplicateNameErrorFrameStopsTheConnection(t *testing.T) {
	err := s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			return
		}
		payload, _ := json.Marshal(map[string]string{"reason": duplicateNameReason})
		frame, _ := json.Marshal(AgentMessage{Action: "error", Payload: payload})
		conn.WriteMessage(websocket.TextMessage, frame)
		// Hold the socket open: the sentinel must come from the frame, not from
		// the socket dying.
		time.Sleep(500 * time.Millisecond)
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("run() = %v, want the duplicate-name sentinel", err)
	}
	if err != errDuplicateName {
		t.Fatalf("run() = %v, want exactly errDuplicateName", err)
	}
}

func TestS5DuplicateNameCloseCodeAloneStopsTheConnection(t *testing.T) {
	err := s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			return
		}
		// Close code only — the error frame can be lost if the socket dies
		// first, so the code has to be enough on its own.
		conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(wsCloseDuplicateName, duplicateNameReason),
			time.Now().Add(5*time.Second),
		)
		time.Sleep(200 * time.Millisecond)
	})
	if err != errDuplicateName {
		t.Fatalf("run() = %v, want errDuplicateName from close code %d", err, wsCloseDuplicateName)
	}
}

func TestS5OtherServerErrorsAreNotTheDuplicateSentinel(t *testing.T) {
	err := s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			return
		}
		payload, _ := json.Marshal(map[string]string{"reason": "something else"})
		frame, _ := json.Marshal(AgentMessage{Action: "error", Payload: payload})
		conn.WriteMessage(websocket.TextMessage, frame)
		time.Sleep(100 * time.Millisecond)
	})
	if err == errDuplicateName {
		t.Fatalf("an unrelated server error was treated as a duplicate name")
	}
}

func TestS5ReconnectDelaysAreFiveAndThirtySeconds(t *testing.T) {
	if reconnectDelay != 5*time.Second {
		t.Errorf("reconnectDelay = %v, want 5s (unchanged for every other failure)", reconnectDelay)
	}
	if duplicateNameDelay != 30*time.Second {
		t.Errorf("duplicateNameDelay = %v, want 30s", duplicateNameDelay)
	}
	if wsCloseDuplicateName != 4409 {
		t.Errorf("wsCloseDuplicateName = %d, want 4409 (must match the server)", wsCloseDuplicateName)
	}
	if duplicateNameReason != "duplicate name" {
		t.Errorf("duplicateNameReason = %q, want %q (must match the server)", duplicateNameReason, "duplicate name")
	}
}
