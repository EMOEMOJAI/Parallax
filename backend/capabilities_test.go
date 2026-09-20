package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// S5 — agent version + capabilities, and duplicate-name rejection.
//
// Every helper here carries an s5 prefix so it cannot collide with the fake
// agents in the other suites; those are well-behaved by design, while these
// tests need to send hostile and partial frames by hand.

func s5NewServer(t *testing.T) (*Server, *httptest.Server) {
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
	mux.HandleFunc("/api/nodes", srv.handleNodes)
	mux.HandleFunc("/api/nodes/health", srv.handleNodesHealth)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

// s5DialAgent opens /ws/agent without registering.
func s5DialAgent(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/agent"), nil)
	if err != nil {
		t.Fatalf("dial /ws/agent: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func s5Send(t *testing.T, conn *websocket.Conn, action string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", action, err)
	}
	env, err := json.Marshal(AgentMessage{Action: action, Payload: raw})
	if err != nil {
		t.Fatalf("marshal %s envelope: %v", action, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
		t.Fatalf("write %s: %v", action, err)
	}
}

// s5RegisterPayload is a minimal valid register frame; callers add or drop the
// capability fields per test.
func s5RegisterPayload(name string) map[string]any {
	return map[string]any{
		"name": name, "location": "Testville", "flag": "🏁",
		"ipv4": "203.0.113.7", "provider": "TestNet",
	}
}

// s5RegisterAgent registers and waits for the node to exist, returning the
// socket and the server-assigned node ID.
func s5RegisterAgent(t *testing.T, srv *Server, ts *httptest.Server, payload map[string]any) (*websocket.Conn, string) {
	t.Helper()
	conn := s5DialAgent(t, ts)
	s5Send(t, conn, "register", payload)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read register ack: %v", err)
	}
	if !strings.Contains(string(msg), `"registered"`) {
		t.Fatalf("unexpected register ack: %s", msg)
	}
	conn.SetReadDeadline(time.Time{})
	name, _ := payload["name"].(string)
	return conn, waitForNodeID(t, srv, name)
}

// s5NodeSnapshot copies the capability fields under nodesMu — reading them
// bare from another goroutine would be a race the -race build would report.
func s5NodeSnapshot(t *testing.T, srv *Server, id string) (version string, tools map[string]bool, online bool, hasConn bool) {
	t.Helper()
	srv.nodesMu.RLock()
	defer srv.nodesMu.RUnlock()
	n, ok := srv.nodes[id]
	if !ok {
		t.Fatalf("node %s is gone", id)
	}
	tools = map[string]bool{}
	for k, v := range n.Tools {
		tools[k] = v
	}
	if n.Tools == nil {
		tools = nil
	}
	return n.Version, tools, n.Online, n.conn != nil
}

// s5Client opens an authenticated-by-default browser socket (no key is
// configured in these tests) and returns a channel of node_status frames.
func s5Client(t *testing.T, ts *httptest.Server) <-chan map[string]any {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/client"), nil)
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	out := make(chan map[string]any, 32)
	go func() {
		defer close(out)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(msg, &frame) != nil {
				continue
			}
			if frame["type"] == "node_status" {
				select {
				case out <- frame:
				default:
				}
			}
		}
	}()
	return out
}

// s5NextStatus waits for the next node_status frame for the given node, or
// fails. An empty id accepts any node.
func s5NextStatus(t *testing.T, ch <-chan map[string]any, id string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("client socket closed while waiting for node_status")
			}
			if id == "" || frame["node_id"] == id {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for node_status for %q", id)
		}
	}
}

// s5NoStatus asserts no node_status arrives within the window.
func s5NoStatus(t *testing.T, ch <-chan map[string]any, window time.Duration) {
	t.Helper()
	select {
	case frame, ok := <-ch:
		if !ok {
			return
		}
		t.Fatalf("unexpected node_status broadcast: %v", frame)
	case <-time.After(window):
	}
}

func s5GetJSONList(t *testing.T, ts *httptest.Server, path string) []map[string]any {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, res.StatusCode)
	}
	var list []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return list
}

func s5FindByID(t *testing.T, list []map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range list {
		if item["id"] == id {
			return item
		}
	}
	t.Fatalf("node %s not present in response", id)
	return nil
}

// s5ToolMap converts a decoded JSON object's tools field to a Go map.
func s5ToolMap(t *testing.T, raw any) map[string]bool {
	t.Helper()
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("tools is not an object: %#v", raw)
	}
	out := make(map[string]bool, len(obj))
	for k, v := range obj {
		b, ok := v.(bool)
		if !ok {
			t.Fatalf("tools[%q] is not a bool: %#v", k, v)
		}
		out[k] = b
	}
	return out
}

func s5AssertTools(t *testing.T, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	for k, v := range want {
		if other, ok := got[k]; !ok || other != v {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	}
}

// ---- Tools is whitelist-bounded, advisory data ------------------------------

func TestS5RegisterStoresOnlyWhitelistedToolKeys(t *testing.T) {
	srv, ts := s5NewServer(t)
	reg := s5RegisterPayload("Whitelisted")
	reg["tools"] = map[string]any{
		"ping": true, "mtr": false, // real command types, kept
		"rm": true, "shell": true, // never command types, dropped
		"": true, "ping ": true, // near-misses, dropped
		strings.Repeat("x", 4096): true, // no unbounded growth
	}
	_, id := s5RegisterAgent(t, srv, ts, reg)

	_, tools, _, _ := s5NodeSnapshot(t, srv, id)
	s5AssertTools(t, tools, map[string]bool{"ping": true, "mtr": false})
}

func TestS5RegisterWithOnlyUnknownToolsStoresNothing(t *testing.T) {
	srv, ts := s5NewServer(t)
	reg := s5RegisterPayload("AllJunk")
	reg["tools"] = map[string]any{"rm": true, "shell": true, "curl": true}
	_, id := s5RegisterAgent(t, srv, ts, reg)

	_, tools, _, _ := s5NodeSnapshot(t, srv, id)
	if tools != nil {
		t.Fatalf("tools = %v, want nil so the field stays omitted", tools)
	}
	// And the projection must not grow a "tools" key.
	node := s5FindByID(t, s5GetJSONList(t, ts, "/api/nodes"), id)
	if _, present := node["tools"]; present {
		t.Fatalf("/api/nodes carries a tools key for a node whose report was all junk: %v", node)
	}
}

// A claimed tool is never an authorization input: the dispatch gate is
// allowedCommandTypes alone (client_ws.go), which this slice does not touch.
func TestS5ClaimedToolDoesNotMakeATypeDispatchable(t *testing.T) {
	srv, ts := s5NewServer(t)
	reg := s5RegisterPayload("Claimer")
	reg["tools"] = map[string]any{"rm": true, "ping": false}
	agent, id := s5RegisterAgent(t, srv, ts, reg)

	client, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/client"), nil)
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	defer client.Close()

	// 1. The claimed non-whitelisted type is refused by the server.
	req, _ := json.Marshal(map[string]any{
		"node_id": id,
		"command": map[string]any{"id": "s5-cmd-rm", "type": "rm", "target": "1.1.1.1"},
	})
	if err := client.WriteMessage(websocket.TextMessage, req); err != nil {
		t.Fatalf("send rm command: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sawRefusal bool
	for !sawRefusal {
		_, msg, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read refusal: %v", err)
		}
		var resp CommandResponse
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		if resp.Type == "error" && strings.Contains(resp.Data, `"rm" is not allowed`) {
			sawRefusal = true
		}
	}
	client.SetReadDeadline(time.Time{})

	// 2. A tool the node reports as ABSENT is still dispatched — the agent is
	//    the one that fails it, so a lying agent cannot deny itself service in a
	//    way the server has to reason about.
	req, _ = json.Marshal(map[string]any{
		"node_id": id,
		"command": map[string]any{"id": "s5-cmd-ping", "type": "ping", "target": "1.1.1.1"},
	})
	if err := client.WriteMessage(websocket.TextMessage, req); err != nil {
		t.Fatalf("send ping command: %v", err)
	}
	agent.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, msg, err := agent.ReadMessage()
		if err != nil {
			t.Fatalf("agent never received the ping command: %v", err)
		}
		var env AgentMessage
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		if env.Action != "command" {
			continue
		}
		var cmd CommandRequest
		if json.Unmarshal(env.Payload, &cmd) != nil {
			continue
		}
		if cmd.Type == "ping" && cmd.ID == "s5-cmd-ping" {
			break
		}
	}
	agent.SetReadDeadline(time.Time{})
}

// ---- version sanitization ---------------------------------------------------

func TestS5VersionIsStrippedAndTruncatedTo32Runes(t *testing.T) {
	srv, ts := s5NewServer(t)
	// 200 runes, no HTML-escapable character anywhere: sanitizeString
	// strips control characters and then truncates by rune count; it does
	// not escape, so an escapable char near the boundary is irrelevant here.
	raw := "v1.2.3-\x00\x1b\x7f" + strings.Repeat("abcdefghij", 20)
	stripped := "v1.2.3-" + strings.Repeat("abcdefghij", 20)
	want := string([]rune(stripped)[:32])

	reg := s5RegisterPayload("LongVersion")
	reg["version"] = raw
	_, id := s5RegisterAgent(t, srv, ts, reg)

	version, _, _, _ := s5NodeSnapshot(t, srv, id)
	if version != want {
		t.Fatalf("version = %q, want %q", version, want)
	}
	if len([]rune(version)) != 32 {
		t.Fatalf("version is %d runes, want 32", len([]rune(version)))
	}
	if strings.ContainsAny(version, "\x00\x1b\x7f") {
		t.Fatalf("version still carries control characters: %q", version)
	}
}

// ---- the three server→browser projections ----------------------------------

func TestS5ProjectionsCarryVersionAndTools(t *testing.T) {
	srv, ts := s5NewServer(t)
	statuses := s5Client(t, ts)

	reg := s5RegisterPayload("Projected")
	reg["version"] = "v9.9.9"
	reg["tools"] = map[string]any{"ping": true, "mtr": false}
	_, id := s5RegisterAgent(t, srv, ts, reg)

	frame := s5NextStatus(t, statuses, id, 5*time.Second)
	if frame["version"] != "v9.9.9" {
		t.Fatalf("node_status version = %#v, want v9.9.9", frame["version"])
	}
	s5AssertTools(t, s5ToolMap(t, frame["tools"]), map[string]bool{"ping": true, "mtr": false})

	for _, path := range []string{"/api/nodes", "/api/nodes/health"} {
		node := s5FindByID(t, s5GetJSONList(t, ts, path), id)
		if node["version"] != "v9.9.9" {
			t.Fatalf("%s version = %#v, want v9.9.9", path, node["version"])
		}
		s5AssertTools(t, s5ToolMap(t, node["tools"]), map[string]bool{"ping": true, "mtr": false})
	}
}

func TestS5ProjectionsOmitBothKeysForAnOlderAgent(t *testing.T) {
	srv, ts := s5NewServer(t)
	statuses := s5Client(t, ts)

	// An agent from before this slice: neither field in the register frame.
	_, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Legacy"))

	frame := s5NextStatus(t, statuses, id, 5*time.Second)
	for _, k := range []string{"version", "tools"} {
		if _, present := frame[k]; present {
			t.Fatalf("node_status carries %q for a legacy agent: %v", k, frame)
		}
	}
	for _, path := range []string{"/api/nodes", "/api/nodes/health"} {
		node := s5FindByID(t, s5GetJSONList(t, ts, path), id)
		for _, k := range []string{"version", "tools"} {
			if _, present := node[k]; present {
				t.Fatalf("%s carries %q for a legacy agent: %v", path, k, node)
			}
		}
	}
}

// ---- health frames ----------------------------------------------------------

func TestS5HealthUpdatesCapabilitiesAndBroadcastsOnlyOnChange(t *testing.T) {
	srv, ts := s5NewServer(t)
	agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Healthy"))

	// Subscribe after registration so the register broadcast isn't counted.
	statuses := s5Client(t, ts)

	health := func(extra map[string]any) map[string]any {
		p := map[string]any{"uptime": "1s", "load_avg": "0 0 0", "cpus": 2, "os": "linux/amd64"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	// 1. First capability report: stored and broadcast.
	s5Send(t, agent, "health", health(map[string]any{
		"version": "v1", "tools": map[string]any{"ping": true},
	}))
	frame := s5NextStatus(t, statuses, id, 5*time.Second)
	if frame["version"] != "v1" {
		t.Fatalf("broadcast version = %#v, want v1", frame["version"])
	}
	s5AssertTools(t, s5ToolMap(t, frame["tools"]), map[string]bool{"ping": true})
	version, tools, _, _ := s5NodeSnapshot(t, srv, id)
	if version != "v1" {
		t.Fatalf("stored version = %q, want v1", version)
	}
	s5AssertTools(t, tools, map[string]bool{"ping": true})

	// 2. Identical frame: nothing changed, so no broadcast.
	s5Send(t, agent, "health", health(map[string]any{
		"version": "v1", "tools": map[string]any{"ping": true},
	}))
	s5NoStatus(t, statuses, 400*time.Millisecond)

	// 3. Same length, different value — the length-only comparison a map-unsafe
	//    implementation would use must not miss this.
	s5Send(t, agent, "health", health(map[string]any{
		"version": "v1", "tools": map[string]any{"ping": false},
	}))
	frame = s5NextStatus(t, statuses, id, 5*time.Second)
	s5AssertTools(t, s5ToolMap(t, frame["tools"]), map[string]bool{"ping": false})

	// 4. Different length.
	s5Send(t, agent, "health", health(map[string]any{
		"version": "v1", "tools": map[string]any{"ping": false, "mtr": true},
	}))
	frame = s5NextStatus(t, statuses, id, 5*time.Second)
	s5AssertTools(t, s5ToolMap(t, frame["tools"]), map[string]bool{"ping": false, "mtr": true})

	// 5. Version only.
	s5Send(t, agent, "health", health(map[string]any{
		"version": "v2", "tools": map[string]any{"ping": false, "mtr": true},
	}))
	frame = s5NextStatus(t, statuses, id, 5*time.Second)
	if frame["version"] != "v2" {
		t.Fatalf("broadcast version = %#v, want v2", frame["version"])
	}

	// 6. A frame that omits both fields leaves the stored values alone and
	//    broadcasts nothing — an older agent's health frame must not wipe the
	//    capabilities a newer one reported for this node.
	s5Send(t, agent, "health", health(nil))
	s5NoStatus(t, statuses, 400*time.Millisecond)
	version, tools, _, _ = s5NodeSnapshot(t, srv, id)
	if version != "v2" {
		t.Fatalf("stored version = %q after a capability-less frame, want v2", version)
	}
	s5AssertTools(t, tools, map[string]bool{"ping": false, "mtr": true})
}

func TestS5HealthDropsNonWhitelistedToolKeys(t *testing.T) {
	srv, ts := s5NewServer(t)
	agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("HealthJunk"))

	s5Send(t, agent, "health", map[string]any{
		"uptime": "1s", "cpus": 1,
		"tools": map[string]any{"dns": true, "rm": true, "shell": true},
	})
	waitFor(t, "the health frame to land", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return len(srv.nodes[id].Tools) > 0
	})
	_, tools, _, _ := s5NodeSnapshot(t, srv, id)
	s5AssertTools(t, tools, map[string]bool{"dns": true})
}

// A reconnecting older build must not inherit the capabilities its previous
// build reported — register assigns both fields unconditionally, like Health.
func TestS5ReRegisterClearsStaleCapabilities(t *testing.T) {
	srv, ts := s5NewServer(t)
	reg := s5RegisterPayload("Downgraded")
	reg["version"] = "v1"
	reg["tools"] = map[string]any{"ping": true}
	agent, id := s5RegisterAgent(t, srv, ts, reg)

	// Kill the first connection and wait for the server to mark it offline, so
	// the second register is a reconnect rather than a refused duplicate.
	agent.Close()
	waitFor(t, "the node to go offline", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return !srv.nodes[id].Online
	})

	_, sameID := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Downgraded"))
	if sameID != id {
		t.Fatalf("reconnect got a new node id %s, want %s", sameID, id)
	}
	version, tools, _, _ := s5NodeSnapshot(t, srv, id)
	if version != "" {
		t.Fatalf("version = %q after an older agent reconnected, want empty", version)
	}
	if tools != nil {
		t.Fatalf("tools = %v after an older agent reconnected, want nil", tools)
	}
}

// Capability writes and the projections that encode Tools outside nodesMu must
// not race: the stored map is only ever replaced, never mutated in place.
func TestS5CapabilityUpdatesAreRaceFreeAgainstProjections(t *testing.T) {
	srv, ts := s5NewServer(t)
	agent, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("Racy"))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 120; i++ {
			tools := map[string]any{"ping": i%2 == 0}
			if i%3 == 0 {
				tools["mtr"] = true
			}
			raw, _ := json.Marshal(map[string]any{
				"uptime": "1s", "cpus": 1,
				"version": "v" + strings.Repeat("1", i%5+1),
				"tools":   tools,
			})
			env, _ := json.Marshal(AgentMessage{Action: "health", Payload: raw})
			if err := agent.WriteMessage(websocket.TextMessage, env); err != nil {
				return
			}
		}
		close(stop)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, path := range []string{"/api/nodes", "/api/nodes/health"} {
				res, err := http.Get(ts.URL + path)
				if err != nil {
					return
				}
				res.Body.Close()
			}
		}
	}()

	wg.Wait()
	// The node survived the storm with a sane, whitelist-bounded map.
	_, tools, online, _ := s5NodeSnapshot(t, srv, id)
	if !online {
		t.Fatalf("node went offline during the update storm")
	}
	for name := range tools {
		if !allowedCommandTypes[name] {
			t.Fatalf("stored tool key %q is not a command type", name)
		}
	}
}

// ---- duplicate-name rejection ----------------------------------------------

func TestS5DuplicateNameIsRefusedWithReasonAndCloseCode(t *testing.T) {
	srv, ts := s5NewServer(t)
	first, id := s5RegisterAgent(t, srv, ts, s5RegisterPayload("OnlyOne"))

	// The impostor gets an explicit reason, then a 4409 close.
	second := s5DialAgent(t, ts)
	s5Send(t, second, "register", s5RegisterPayload("OnlyOne"))

	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := second.ReadMessage()
	if err != nil {
		t.Fatalf("read refusal frame: %v", err)
	}
	var env AgentMessage
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("refusal is not an envelope: %s", msg)
	}
	if env.Action != "error" {
		t.Fatalf("refusal action = %q, want error (frame: %s)", env.Action, msg)
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(env.Payload, &reason); err != nil {
		t.Fatalf("refusal payload: %v (%s)", err, msg)
	}
	if reason.Reason != duplicateNameReason {
		t.Fatalf("reason = %q, want %q", reason.Reason, duplicateNameReason)
	}

	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err = second.ReadMessage(); err == nil {
		t.Fatalf("expected the refused socket to be closed")
	}
	if !websocket.IsCloseError(err, wsCloseDuplicateName) {
		t.Fatalf("close error = %v, want code %d", err, wsCloseDuplicateName)
	}

	// The live node is untouched: one entry, same UUID, same connection, online.
	srv.nodesMu.RLock()
	count := len(srv.nodes)
	srv.nodesMu.RUnlock()
	if count != 1 {
		t.Fatalf("server holds %d nodes, want 1", count)
	}
	_, _, online, hasConn := s5NodeSnapshot(t, srv, id)
	if !online || !hasConn {
		t.Fatalf("live node disturbed by the refusal: online=%v hasConn=%v", online, hasConn)
	}

	// And it still serves: a health frame from the original socket lands.
	s5Send(t, first, "health", map[string]any{
		"uptime": "2s", "cpus": 1, "version": "v-still-here",
	})
	waitFor(t, "the surviving agent's health frame", 5*time.Second, func() bool {
		srv.nodesMu.RLock()
		defer srv.nodesMu.RUnlock()
		return srv.nodes[id].Version == "v-still-here"
	})
}

// The refusal must not be mistaken for a reason to drop the live node's
// capabilities either.
func TestS5DuplicateNameLeavesCapabilitiesIntact(t *testing.T) {
	srv, ts := s5NewServer(t)
	reg := s5RegisterPayload("KeepMine")
	reg["version"] = "v-original"
	reg["tools"] = map[string]any{"ping": true, "http": false}
	_, id := s5RegisterAgent(t, srv, ts, reg)

	impostor := s5DialAgent(t, ts)
	impostorReg := s5RegisterPayload("KeepMine")
	impostorReg["version"] = "v-impostor"
	impostorReg["tools"] = map[string]any{"ping": false}
	s5Send(t, impostor, "register", impostorReg)

	impostor.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := impostor.ReadMessage(); err != nil {
		t.Fatalf("read refusal frame: %v", err)
	}

	version, tools, online, _ := s5NodeSnapshot(t, srv, id)
	if version != "v-original" {
		t.Fatalf("version = %q, want v-original", version)
	}
	s5AssertTools(t, tools, map[string]bool{"ping": true, "http": false})
	if !online {
		t.Fatalf("node went offline after a refused duplicate")
	}
}

// ---- helper-level guards ----------------------------------------------------

func TestS5SanitizeAgentToolsIsWhitelistBoundedAndFresh(t *testing.T) {
	in := map[string]bool{"ping": true, "rm": true}
	out := sanitizeAgentTools(in)
	s5AssertTools(t, out, map[string]bool{"ping": true})

	// A fresh map: mutating the agent-supplied input must not reach the stored
	// value, and vice versa.
	in["ping"] = false
	if out["ping"] != true {
		t.Fatalf("stored map aliases the agent-supplied map")
	}
	if sanitizeAgentTools(nil) != nil || sanitizeAgentTools(map[string]bool{}) != nil {
		t.Fatalf("an empty report must produce nil so the field stays omitted")
	}
}

func TestS5SameToolSetComparesLengthThenKeys(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs empty", nil, map[string]bool{}, true},
		{"equal", map[string]bool{"ping": true}, map[string]bool{"ping": true}, true},
		{"value differs", map[string]bool{"ping": true}, map[string]bool{"ping": false}, false},
		{"key differs", map[string]bool{"ping": true}, map[string]bool{"mtr": true}, false},
		{"length differs", map[string]bool{"ping": true}, map[string]bool{"ping": true, "mtr": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameToolSet(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameToolSet(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
