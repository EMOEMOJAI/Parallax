package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ---- harness ----------------------------------------------------------------
//
// Same shape as scheduler_test.go's harness (real Server + httptest.Server +
// gorilla dialer) but with the S9 routes and an explicit environment, so every
// case states the exact configuration it is asserting about. scheduler_test.go
// is not modified; its dialFakeAgent / waitForNodeID / waitFor helpers are
// reused where a real agent is needed.

// pubEnvDefaults is the "homelab" baseline: no keys, no origin policy, no
// proxy trust, public mode off. Each test overrides only what it exercises.
var pubEnvDefaults = map[string]string{
	"AGENT_API_KEY":      "",
	"CLIENT_API_KEY":     "",
	"ALLOWED_ORIGINS":    "",
	"STRICT_ORIGIN":      "",
	"TRUST_PROXY":        "",
	"TRUSTED_PROXY_HOPS": "",
	"PUBLIC_MODE":        "",
	"PUBLIC_COMMANDS":    "",
	"PUBLIC_TARGETS":     "",
	"METRICS_TOKEN":      "",
}

// newPublicTestServer applies the baseline environment plus env, then builds a
// Server and an httptest.Server carrying the routes S9 touches.
func newPublicTestServer(t *testing.T, env map[string]string) (*Server, *httptest.Server) {
	t.Helper()
	for k, v := range pubEnvDefaults {
		t.Setenv(k, v)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/ws/client", srv.handleClientWS)
	mux.HandleFunc("/ws/speedtest", srv.handleSpeedTestWS)
	mux.HandleFunc("/api/runs", srv.handleRuns)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

func wsURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

// dialClientWS opens /ws/client. The caller inspects both the error and the
// HTTP response, since the interesting cases (401, 429, 403) never upgrade.
func dialClientWS(ts *httptest.Server, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial(wsURL(ts, "/ws/client"), hdr)
}

func mustDialPublicClient(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c, _, err := dialClientWS(ts, nil)
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// pubTotalConns is the number of public connection slots currently held.
func pubTotalConns(srv *Server) int {
	srv.rateMu.Lock()
	defer srv.rateMu.Unlock()
	n := 0
	for _, e := range srv.rateEntries {
		n += e.conns
	}
	return n
}

// awaitSessionKind reads the first frame and reports the session kind the
// server announced ("" when the first frame is not a session_kind hello).
func awaitSessionKind(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read session_kind: %v", err)
	}
	var hello struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(data, &hello) != nil || hello.Type != "session_kind" {
		return ""
	}
	return hello.Kind
}

// readCommandResponse returns the next CommandResponse, skipping hello frames.
func readCommandResponse(t *testing.T, c *websocket.Conn) CommandResponse {
	t.Helper()
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read client message: %v", err)
		}
		var resp CommandResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("decode client message %s: %v", data, err)
		}
		if resp.Type == "session_kind" {
			continue
		}
		return resp
	}
}

// sendClientCommand writes one command frame and returns its command id.
func sendClientCommand(t *testing.T, c *websocket.Conn, nodeID, cmdType, target, options string) string {
	t.Helper()
	id := uuid.New().String()
	msg, _ := json.Marshal(map[string]any{
		"action":  "command",
		"node_id": nodeID,
		"command": map[string]string{"id": id, "type": cmdType, "target": target, "options": options},
	})
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return id
}

// runCommandExpectingError drives one command to its "done" frame and returns
// the error text that preceded it.
func runCommandExpectingError(t *testing.T, c *websocket.Conn, nodeID, cmdType, target string) (cmdID, errText string) {
	t.Helper()
	cmdID = sendClientCommand(t, c, nodeID, cmdType, target, "")
	for {
		resp := readCommandResponse(t, c)
		switch resp.Type {
		case "error":
			errText = resp.Data
		case "done":
			return cmdID, errText
		}
	}
}

func cmdOwnerExists(srv *Server, cmdID string) bool {
	srv.cmdOwnersMu.RLock()
	defer srv.cmdOwnersMu.RUnlock()
	_, ok := srv.cmdOwners[cmdID]
	return ok
}

// postRunRequest posts a minimal valid run payload, optionally with a bearer
// credential, and returns the status code and body.
func postRunRequest(t *testing.T, ts *httptest.Server, key string) (int, string) {
	t.Helper()
	body := `{"command":"ping","target":"1.1.1.1","lines":[{"type":"output","text":"64 bytes"}]}`
	req, err := http.NewRequest("POST", ts.URL+"/api/runs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build run request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// dialSpeedTest reports the handshake status code (200 when the upgrade
// succeeded).
func dialSpeedTest(t *testing.T, ts *httptest.Server, key string) int {
	t.Helper()
	hdr := http.Header{}
	if key != "" {
		hdr.Set("Authorization", "Bearer "+key)
	}
	c, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/speedtest"), hdr)
	if err == nil {
		c.Close()
		return 200
	}
	if resp == nil {
		t.Fatalf("dial /ws/speedtest: %v", err)
	}
	return resp.StatusCode
}

func originRequest(host, origin string) *http.Request {
	r := httptest.NewRequest("GET", "http://"+host+"/ws/client", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// captureLog collects everything written through the standard logger while fn
// runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()
	fn()
	return buf.String()
}

// ---- F32: origin policy -----------------------------------------------------

func TestCheckOriginPolicy(t *testing.T) {
	srv := NewServer()
	cases := []struct {
		name    string
		allowed string
		strict  string
		host    string
		origin  string
		want    bool
	}{
		{"strict: same host", "", "1", "lg.example.com", "https://lg.example.com", true},
		{"strict: same host, other scheme", "", "1", "lg.example.com", "http://lg.example.com", true},
		{"strict: same host, case-insensitive", "", "1", "LG.Example.com", "https://lg.example.COM", true},
		{"strict: same host with port", "", "1", "lg.example.com:8443", "https://lg.example.com:8443", true},
		{"strict: cross host", "", "1", "lg.example.com", "https://evil.example.net", false},
		{"strict: port mismatch", "", "1", "lg.example.com:8443", "https://lg.example.com", false},
		{"strict: opaque origin", "", "1", "lg.example.com", "null", false},
		{"strict: no Origin header", "", "1", "lg.example.com", "", true},
		{"unset: cross host permitted (unchanged)", "", "", "lg.example.com", "https://evil.example.net", true},
		{"allowlist: exact match", "https://a.example.com,https://b.example.com", "", "lg.example.com", "https://b.example.com", true},
		{"allowlist: no match", "https://a.example.com", "", "lg.example.com", "https://evil.example.net", false},
		{"allowlist: no Origin header", "https://a.example.com", "", "lg.example.com", "", true},
		{"allowlist wins over STRICT_ORIGIN", "https://a.example.com", "1", "lg.example.com", "https://a.example.com", true},
		{"allowlist wins: same host not enough", "https://a.example.com", "1", "lg.example.com", "https://lg.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ALLOWED_ORIGINS", tc.allowed)
			t.Setenv("STRICT_ORIGIN", tc.strict)
			if got := srv.checkOrigin(originRequest(tc.host, tc.origin)); got != tc.want {
				t.Fatalf("checkOrigin(host=%q origin=%q) = %v, want %v", tc.host, tc.origin, got, tc.want)
			}
		})
	}
}

func TestCORSStrictOriginNeverWildcard(t *testing.T) {
	srv := NewServer()
	handler := srv.corsMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	t.Run("strict echoes a matching origin", func(t *testing.T) {
		t.Setenv("ALLOWED_ORIGINS", "")
		t.Setenv("STRICT_ORIGIN", "1")
		rec := httptest.NewRecorder()
		handler(rec, originRequest("lg.example.com", "https://lg.example.com"))
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://lg.example.com" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the request origin", got)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})

	t.Run("strict refuses a cross origin", func(t *testing.T) {
		t.Setenv("ALLOWED_ORIGINS", "")
		t.Setenv("STRICT_ORIGIN", "1")
		rec := httptest.NewRecorder()
		handler(rec, originRequest("lg.example.com", "https://evil.example.net"))
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})

	t.Run("unset keeps the wildcard", func(t *testing.T) {
		t.Setenv("ALLOWED_ORIGINS", "")
		t.Setenv("STRICT_ORIGIN", "")
		rec := httptest.NewRecorder()
		handler(rec, originRequest("lg.example.com", "https://evil.example.net"))
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want * (unchanged default)", got)
		}
	})
}

// ---- F13: client IP extraction ---------------------------------------------

func TestClientIPIgnoresForwardedHeadersWithoutTrustProxy(t *testing.T) {
	t.Setenv("TRUST_PROXY", "")
	t.Setenv("TRUSTED_PROXY_HOPS", "")
	r := httptest.NewRequest("GET", "/api/geoip/1.1.1.1", nil)
	r.RemoteAddr = "203.0.113.9:44321"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 8.8.8.8")
	r.Header.Set("X-Real-IP", "7.7.7.7")

	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want the RemoteAddr 203.0.113.9 (forged headers must be ignored)", got)
	}
	if got := clientRateKey(r); got != "203.0.113.9/32" {
		t.Fatalf("clientRateKey = %q, want 203.0.113.9/32", got)
	}
}

func TestClientIPTrustProxyHops(t *testing.T) {
	cases := []struct {
		name   string
		hops   string
		xff    string
		realIP string
		remote string
		want   string
	}{
		{"default hop count takes the right-most entry", "", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "10.0.0.5:1234", "3.3.3.3"},
		{"explicit one hop", "1", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "10.0.0.5:1234", "3.3.3.3"},
		{"two hops", "2", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "10.0.0.5:1234", "2.2.2.2"},
		{"hop count beyond the list clamps to the left-most", "9", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "10.0.0.5:1234", "1.1.1.1"},
		{"zero hops clamps to one", "0", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "10.0.0.5:1234", "3.3.3.3"},
		{"garbage hop count clamps to one", "abc", "1.1.1.1, 2.2.2.2", "", "10.0.0.5:1234", "2.2.2.2"},
		{"unparseable entry falls back to RemoteAddr", "1", "1.1.1.1, not-an-ip", "", "10.0.0.5:1234", "10.0.0.5"},
		{"entry with a port is accepted", "1", "1.1.1.1, 4.4.4.4:9999", "", "10.0.0.5:1234", "4.4.4.4"},
		{"v4-mapped v6 is unmapped", "1", "::ffff:5.6.7.8", "", "10.0.0.5:1234", "5.6.7.8"},
		{"X-Real-IP used only without XFF", "1", "", "7.7.7.7", "10.0.0.5:1234", "7.7.7.7"},
		{"XFF wins over X-Real-IP", "1", "3.3.3.3", "7.7.7.7", "10.0.0.5:1234", "3.3.3.3"},
		{"bad X-Real-IP falls back to RemoteAddr", "1", "", "nope", "10.0.0.5:1234", "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUST_PROXY", "1")
			t.Setenv("TRUSTED_PROXY_HOPS", tc.hops)
			r := httptest.NewRequest("GET", "/api/geoip/1.1.1.1", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- F14: rate keys and map cap --------------------------------------------

func TestRateKeyPrefixes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.2.3.4", "1.2.3.4/32"},
		{"1.2.3.5", "1.2.3.5/32"},
		{"::ffff:1.2.3.4", "1.2.3.4/32"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::ffff", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tc := range cases {
		if got := rateKey(tc.in); got != tc.want {
			t.Errorf("rateKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if rateKey("2001:db8:1:2:3:4:5:6") != rateKey("2001:db8:1:2::ffff") {
		t.Error("two addresses in the same /64 must share a rate key")
	}
	if rateKey("1.2.3.4") == rateKey("1.2.3.5") {
		t.Error("two distinct v4 addresses must not share a rate key")
	}
}

func TestRateEntryCapEvictsOldestButNeverLiveConnections(t *testing.T) {
	srv := NewServer()
	now := time.Now()

	srv.rateMu.Lock()
	for i := 0; i < rateEntryMaxCount; i++ {
		srv.rateEntries[fmt.Sprintf("k-%d", i)] = &rateEntry{
			cmdTokens: publicCmdBurst,
			runTokens: runCreateBurst,
			// k-0 is the newest, k-(max-1) the oldest.
			lastFill: now.Add(-time.Duration(i) * time.Second),
		}
	}
	oldestKey := fmt.Sprintf("k-%d", rateEntryMaxCount-1)
	nextOldestKey := fmt.Sprintf("k-%d", rateEntryMaxCount-2)
	srv.rateEntries[oldestKey].conns = 1 // pinned: a client is connected on it
	srv.rateMu.Unlock()

	if !srv.rateAllowPublicCommand("brand-new-key") {
		t.Fatal("a fresh key must start with a full command bucket")
	}

	srv.rateMu.Lock()
	defer srv.rateMu.Unlock()
	if got := len(srv.rateEntries); got > rateEntryMaxCount {
		t.Errorf("map grew past the cap: %d > %d", got, rateEntryMaxCount)
	}
	if _, ok := srv.rateEntries["brand-new-key"]; !ok {
		t.Error("new key was not inserted")
	}
	if _, ok := srv.rateEntries[oldestKey]; !ok {
		t.Errorf("%s holds a live connection and must never be evicted", oldestKey)
	}
	if _, ok := srv.rateEntries[nextOldestKey]; ok {
		t.Errorf("%s was the oldest evictable entry and should have been evicted", nextOldestKey)
	}
}

func TestRateGCKeepsEntriesWithLiveConnections(t *testing.T) {
	srv := NewServer()
	if !srv.rateAcquireConn("live") {
		t.Fatal("acquire failed")
	}
	if !srv.rateAllowPublicCommand("idle") {
		t.Fatal("command bucket refused a fresh key")
	}

	srv.rateMu.Lock()
	srv.rateEntries["live"].lastFill = time.Now().Add(-time.Hour)
	srv.rateEntries["idle"].lastFill = time.Now().Add(-time.Hour)
	srv.rateMu.Unlock()

	srv.rateGC(time.Now())

	srv.rateMu.Lock()
	defer srv.rateMu.Unlock()
	if _, ok := srv.rateEntries["live"]; !ok {
		t.Error("GC dropped an entry holding a live connection")
	}
	if _, ok := srv.rateEntries["idle"]; ok {
		t.Error("GC kept an idle entry")
	}
}

func TestRateReleaseIsSafeAndNeverNegative(t *testing.T) {
	srv := NewServer()
	srv.rateReleaseConn("never-seen") // must not panic or create an entry
	if n := srv.rateEntryCount(); n != 0 {
		t.Fatalf("release created %d entries, want 0", n)
	}
	if !srv.rateAcquireConn("k") {
		t.Fatal("acquire failed")
	}
	srv.rateReleaseConn("k")
	srv.rateReleaseConn("k")
	srv.rateReleaseConn("k")
	if n := srv.rateConnCount("k"); n != 0 {
		t.Fatalf("conn count = %d after repeated release, want 0", n)
	}
	// A negative counter would show up here as a free slot beyond the cap.
	for i := 0; i < maxPublicConnsPerKey; i++ {
		if !srv.rateAcquireConn("k") {
			t.Fatalf("acquire %d refused, want granted", i)
		}
	}
	if srv.rateAcquireConn("k") {
		t.Fatal("acquire beyond the cap was granted")
	}
}

// ---- F15: public connection slots ------------------------------------------

func TestPublicConnectionCapAndSlotReuse(t *testing.T) {
	srv, ts := newPublicTestServer(t, map[string]string{"PUBLIC_MODE": "1"})

	conns := make([]*websocket.Conn, 0, maxPublicConnsPerKey)
	for i := 0; i < maxPublicConnsPerKey; i++ {
		c, _, err := dialClientWS(ts, nil)
		if err != nil {
			t.Fatalf("public dial %d failed: %v", i, err)
		}
		defer c.Close()
		if kind := awaitSessionKind(t, c); kind != "public" {
			t.Fatalf("session kind = %q, want public", kind)
		}
		conns = append(conns, c)
	}

	_, resp, err := dialClientWS(ts, nil)
	if err == nil {
		t.Fatal("connection beyond the cap was accepted")
	}
	if resp == nil || resp.StatusCode != 429 {
		t.Fatalf("over-cap dial status = %v, want 429", resp)
	}

	// Freeing one slot lets the next client in.
	conns[0].Close()
	waitFor(t, "the closed connection's slot to be released", 5*time.Second, func() bool {
		return pubTotalConns(srv) == maxPublicConnsPerKey-1
	})
	c, _, err := dialClientWS(ts, nil)
	if err != nil {
		t.Fatalf("dial after freeing a slot failed: %v", err)
	}
	c.Close()
}

func TestPublicSlotReleasedWhenUpgradeFails(t *testing.T) {
	srv, ts := newPublicTestServer(t, map[string]string{
		"PUBLIC_MODE":   "1",
		"STRICT_ORIGIN": "1",
	})

	hdr := http.Header{}
	hdr.Set("Origin", "https://evil.example.net")
	for i := 0; i < maxPublicConnsPerKey; i++ {
		_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/client"), hdr)
		if err == nil {
			t.Fatalf("cross-origin dial %d was upgraded", i)
		}
		if resp == nil || resp.StatusCode != 403 {
			t.Fatalf("cross-origin dial %d status = %v, want 403", i, resp)
		}
	}

	waitFor(t, "slots taken by failed upgrades to be released", 5*time.Second, func() bool {
		return pubTotalConns(srv) == 0
	})

	// A legitimate client (no Origin header — CLI / same-host) still gets in.
	c, _, err := dialClientWS(ts, nil)
	if err != nil {
		t.Fatalf("valid public dial after failed upgrades: %v", err)
	}
	defer c.Close()
	if kind := awaitSessionKind(t, c); kind != "public" {
		t.Fatalf("session kind = %q, want public", kind)
	}
}

func TestPublicSlotSurvivesIdleGC(t *testing.T) {
	srv, ts := newPublicTestServer(t, map[string]string{"PUBLIC_MODE": "1"})

	for i := 0; i < maxPublicConnsPerKey; i++ {
		c := mustDialPublicClient(t, ts)
		if kind := awaitSessionKind(t, c); kind != "public" {
			t.Fatalf("session kind = %q, want public", kind)
		}
	}

	// Rewind the bucket and force a GC pass, the way lru_test.go rewinds
	// lastFill for the geoip buckets.
	srv.rateMu.Lock()
	for _, e := range srv.rateEntries {
		e.lastFill = time.Now().Add(-time.Hour)
	}
	srv.rateMu.Unlock()
	srv.rateGC(time.Now())

	if n := pubTotalConns(srv); n != maxPublicConnsPerKey {
		t.Fatalf("live connections after GC = %d, want %d", n, maxPublicConnsPerKey)
	}
	_, resp, err := dialClientWS(ts, nil)
	if err == nil {
		t.Fatal("connection beyond the cap was accepted after a GC pass")
	}
	if resp == nil || resp.StatusCode != 429 {
		t.Fatalf("over-cap dial status after GC = %v, want 429", resp)
	}
}

// ---- F15: public command bucket --------------------------------------------

func TestPublicCommandRateLimit(t *testing.T) {
	srv, ts := newPublicTestServer(t, map[string]string{
		"PUBLIC_MODE":     "1",
		"PUBLIC_COMMANDS": "ping",
		"PUBLIC_TARGETS":  "192.0.2.1",
	})

	c := mustDialPublicClient(t, ts)
	if kind := awaitSessionKind(t, c); kind != "public" {
		t.Fatalf("session kind = %q, want public", kind)
	}

	// The burst is 10; every one of these reaches the (offline) node check.
	for i := 0; i < int(publicCmdBurst); i++ {
		_, errText := runCommandExpectingError(t, c, "no-such-node", "ping", "192.0.2.1")
		if strings.Contains(errText, "rate limit") {
			t.Fatalf("command %d was throttled inside the burst: %q", i, errText)
		}
	}

	cmdID, errText := runCommandExpectingError(t, c, "no-such-node", "ping", "192.0.2.1")
	if !strings.Contains(errText, "rate limit") {
		t.Fatalf("command beyond the burst returned %q, want a rate-limit error", errText)
	}
	if cmdOwnerExists(srv, cmdID) {
		t.Error("a throttled command was still inserted into cmdOwners")
	}
}

func TestPublicShellStartRefusedWithEmptyKey(t *testing.T) {
	// PUBLIC_MODE=1 with no CLIENT_API_KEY: every session is public (F31).
	_, ts := newPublicTestServer(t, map[string]string{"PUBLIC_MODE": "1"})

	c := mustDialPublicClient(t, ts)
	if kind := awaitSessionKind(t, c); kind != "public" {
		t.Fatalf("session kind = %q, want public", kind)
	}

	msg, _ := json.Marshal(map[string]string{"action": "shell_start", "node_id": "any", "id": "shell-1"})
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write shell_start: %v", err)
	}
	resp := readCommandResponse(t, c)
	if resp.Type != "error" || !strings.Contains(resp.Data, "interactive shell is not allowed") {
		t.Fatalf("shell_start response = %+v, want the public-mode refusal", resp)
	}
}

// ---- F16: option filtering --------------------------------------------------

func TestPublicOptionsFilteredServerSide(t *testing.T) {
	srv, ts := newPublicTestServer(t, map[string]string{
		"PUBLIC_MODE":     "1",
		"PUBLIC_COMMANDS": "ping",
		"PUBLIC_TARGETS":  "192.0.2.1",
	})
	fa := dialFakeAgent(t, ts, "node-public-opts")
	nodeID := waitForNodeID(t, srv, "node-public-opts")

	c := mustDialPublicClient(t, ts)
	if kind := awaitSessionKind(t, c); kind != "public" {
		t.Fatalf("session kind = %q, want public", kind)
	}

	sendClientCommand(t, c, nodeID, "ping", "192.0.2.1", "count=5 -6 -f --unsafe size=1400 count=abc")
	req := fa.awaitCommand(t)
	if req.Options != "count=5 -6" {
		t.Fatalf("agent received options %q, want %q", req.Options, "count=5 -6")
	}
}

func TestNonPublicOptionsUntouched(t *testing.T) {
	// Homelab mode: no key, no PUBLIC_MODE — options must pass through as-is.
	srv, ts := newPublicTestServer(t, nil)
	fa := dialFakeAgent(t, ts, "node-private-opts")
	nodeID := waitForNodeID(t, srv, "node-private-opts")

	c := mustDialPublicClient(t, ts)
	sendClientCommand(t, c, nodeID, "ping", "192.0.2.1", "count=5 size=1400")
	req := fa.awaitCommand(t)
	if req.Options != "count=5 size=1400" {
		t.Fatalf("agent received options %q, want them unchanged", req.Options)
	}
}

func TestFilterPublicOptions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"-4", "-4"},
		{"-6", "-6"},
		{"count=10", "count=10"},
		{"count=10 -4", "count=10 -4"},
		// "count=5;" is a distinct token from "count=5" and matches nothing.
		{"count=5; rm -rf /", ""},
		{"count=5 ; rm -rf /", "count=5"},
		{"--head", ""},
		{"count=-1", ""},
		{"count=", ""},
		{"+short", ""},
		{"-4extra", ""},
	}
	for _, tc := range cases {
		if got := filterPublicOptions(tc.in); got != tc.want {
			t.Errorf("filterPublicOptions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- F17: requestIsPublic matrix -------------------------------------------

// publicMatrix is the configuration matrix both plain-HTTP call sites share.
var publicMatrix = []struct {
	name       string
	publicMode string
	key        string
	credential string
	wantRun    int // status from POST /api/runs
	wantSpeed  int // handshake status from /ws/speedtest
}{
	{"public mode, empty key", "1", "", "", 403, 403},
	{"public mode, key set, no credential", "1", "secret-key", "", 403, 403},
	{"public mode, key set, correct credential", "1", "secret-key", "secret-key", 201, 200},
	{"private mode, key set, no credential", "", "secret-key", "", 401, 401},
	{"private mode, no key (homelab)", "", "", "", 201, 200},
}

func TestRunCreatePublicMatrix(t *testing.T) {
	for _, tc := range publicMatrix {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newPublicTestServer(t, map[string]string{
				"PUBLIC_MODE":    tc.publicMode,
				"CLIENT_API_KEY": tc.key,
			})
			code, body := postRunRequest(t, ts, tc.credential)
			if code != tc.wantRun {
				t.Fatalf("POST /api/runs = %d (%s), want %d", code, strings.TrimSpace(body), tc.wantRun)
			}
			if tc.wantRun == 403 && !strings.Contains(body, "not available in public mode") {
				t.Errorf("403 body = %s, want the public-mode message", strings.TrimSpace(body))
			}
		})
	}
}

func TestSpeedTestPublicMatrix(t *testing.T) {
	for _, tc := range publicMatrix {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newPublicTestServer(t, map[string]string{
				"PUBLIC_MODE":    tc.publicMode,
				"CLIENT_API_KEY": tc.key,
			})
			if code := dialSpeedTest(t, ts, tc.credential); code != tc.wantSpeed {
				t.Fatalf("/ws/speedtest handshake = %d, want %d", code, tc.wantSpeed)
			}
		})
	}
}

// ---- F17: run-create quota --------------------------------------------------

func TestRunCreateHasNoQuotaWithoutPublicMode(t *testing.T) {
	// The homelab case: no key, no PUBLIC_MODE, TRUST_PROXY unset, so every
	// request arrives from the same RemoteAddr. It must not be throttled.
	_, ts := newPublicTestServer(t, nil)
	for i := 0; i < int(runCreateBurst)+1; i++ {
		code, body := postRunRequest(t, ts, "")
		if code != 201 {
			t.Fatalf("POST /api/runs #%d = %d (%s), want 201 — private mode has no quota", i+1, code, strings.TrimSpace(body))
		}
	}
}

func TestRunCreateQuotaInPublicMode(t *testing.T) {
	_, ts := newPublicTestServer(t, map[string]string{
		"PUBLIC_MODE":    "1",
		"CLIENT_API_KEY": "secret-key",
	})
	for i := 0; i < int(runCreateBurst); i++ {
		code, body := postRunRequest(t, ts, "secret-key")
		if code != 201 {
			t.Fatalf("POST /api/runs #%d = %d (%s), want 201", i+1, code, strings.TrimSpace(body))
		}
	}
	code, body := postRunRequest(t, ts, "secret-key")
	if code != 429 {
		t.Fatalf("POST /api/runs #%d = %d (%s), want 429", int(runCreateBurst)+1, code, strings.TrimSpace(body))
	}
}

// ---- startup warnings + deploy template ------------------------------------

func TestStartupWarnings(t *testing.T) {
	t.Run("public mode without TRUST_PROXY", func(t *testing.T) {
		for k, v := range pubEnvDefaults {
			t.Setenv(k, v)
		}
		t.Setenv("PUBLIC_MODE", "1")
		out := captureLog(t, startupWarnings)
		if !strings.Contains(out, "PUBLIC_MODE is on without TRUST_PROXY=1") {
			t.Errorf("missing the TRUST_PROXY warning, got:\n%s", out)
		}
		if !strings.Contains(out, "PUBLIC_MODE=1 with an empty CLIENT_API_KEY") {
			t.Errorf("missing the empty-key public-mode warning, got:\n%s", out)
		}
		if !strings.Contains(out, "STRICT_ORIGIN=1") {
			t.Errorf("permissive-origin warning must name STRICT_ORIGIN=1, got:\n%s", out)
		}
		if !strings.Contains(out, "ALLOWED_ORIGINS") {
			t.Errorf("permissive-origin warning must name ALLOWED_ORIGINS, got:\n%s", out)
		}
	})

	t.Run("public mode with TRUST_PROXY", func(t *testing.T) {
		for k, v := range pubEnvDefaults {
			t.Setenv(k, v)
		}
		t.Setenv("PUBLIC_MODE", "1")
		t.Setenv("TRUST_PROXY", "1")
		out := captureLog(t, startupWarnings)
		if strings.Contains(out, "PUBLIC_MODE is on without TRUST_PROXY=1") {
			t.Errorf("TRUST_PROXY warning emitted despite TRUST_PROXY=1:\n%s", out)
		}
	})

	t.Run("fully configured deployment stays quiet about origins", func(t *testing.T) {
		for k, v := range pubEnvDefaults {
			t.Setenv(k, v)
		}
		t.Setenv("ALLOWED_ORIGINS", "https://lg.example.com")
		t.Setenv("CLIENT_API_KEY", "k")
		t.Setenv("AGENT_API_KEY", "k")
		out := captureLog(t, startupWarnings)
		if strings.Contains(out, "WARNING") {
			t.Errorf("unexpected warnings:\n%s", out)
		}
	})
}

func TestInstallTemplateDocumentsOptIns(t *testing.T) {
	data, err := os.ReadFile("../deploy/install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	for _, want := range []string{"# STRICT_ORIGIN=1", "# TRUST_PROXY=1"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("deploy/install.sh .env template is missing %q", want)
		}
	}
}
