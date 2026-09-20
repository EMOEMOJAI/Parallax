package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- harness ----------------------------------------------------------------
//
// S8 covers the browser credential (Authorization / lg.bearer subprotocol /
// ?key=) and the authenticated read endpoints. The shape follows
// public_test.go's harness — a real Server behind an httptest.Server and a real
// gorilla dialer — but every helper here is named auth*/seed* so nothing
// collides with public_test.go, scheduler_test.go or lru_test.go, none of which
// is modified. pubEnvDefaults (public_test.go) is reused read-only as the
// "homelab" baseline.

const authTestKey = "secret-key"

// newAuthTestServer applies the homelab baseline plus env and returns a Server
// with every route S8 touches mounted.
func newAuthTestServer(t *testing.T, env map[string]string) (*Server, *httptest.Server) {
	t.Helper()
	for k, v := range pubEnvDefaults {
		t.Setenv(k, v)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	// Schedule mutations persist; keep every test's writes inside the temp root
	// TestMain created.
	t.Setenv("SCHEDULES_FILE", testSchedulesPath(t))

	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/ws/client", srv.handleClientWS)
	mux.HandleFunc("/ws/speedtest", srv.handleSpeedTestWS)
	mux.HandleFunc("/api/nodes", srv.handleNodes)
	mux.HandleFunc("/api/nodes/health", srv.handleNodesHealth)
	mux.HandleFunc("/api/latency-matrix", srv.handleLatencyMatrix)
	mux.HandleFunc("/api/geoip/", srv.handleGeoIP)
	mux.HandleFunc("/api/rdap/", srv.handleRDAP)
	mux.HandleFunc("/api/runs", srv.handleRuns)
	mux.HandleFunc("/api/runs/", srv.handleRuns)
	mux.HandleFunc("/api/schedules", srv.handleSchedules)
	mux.HandleFunc("/api/schedules/", srv.handleSchedule)
	mux.HandleFunc("/api/public-config", srv.handlePublicConfig)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

// authWSURL is the ws:// form of an httptest URL (wsURL in public_test.go does
// the same; kept separate so this file stands alone if that one moves).
func authWSURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

// authCredential describes how a test supplies (or withholds) the client key.
type authCredential struct {
	header      string // Authorization: Bearer <header>
	subprotocol string // Sec-WebSocket-Protocol: lg.bearer, <subprotocol>
	query       string // ?key=<query>
	offerBearer bool   // offer lg.bearer with no credential after it
}

func (c authCredential) dialer() *websocket.Dialer {
	d := *websocket.DefaultDialer
	switch {
	case c.subprotocol != "":
		d.Subprotocols = []string{wsBearerSubprotocol, c.subprotocol}
	case c.offerBearer:
		d.Subprotocols = []string{wsBearerSubprotocol}
	}
	return &d
}

func (c authCredential) header0() http.Header {
	h := http.Header{}
	if c.header != "" {
		h.Set("Authorization", "Bearer "+c.header)
	}
	return h
}

func (c authCredential) url(base string) string {
	if c.query == "" {
		return base
	}
	return base + "?key=" + c.query
}

// authDialClient opens /ws/client with the given credential. Both the
// connection and the HTTP response are returned: the interesting refusals
// (403, 429) never upgrade, while the F19 auth failure does.
func authDialClient(t *testing.T, ts *httptest.Server, c authCredential) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, resp, err := c.dialer().Dial(c.url(authWSURL(ts, "/ws/client")), c.header0())
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

// authDialSpeedTest returns the handshake status (200 when it upgraded) and the
// subprotocol the server selected.
func authDialSpeedTest(t *testing.T, ts *httptest.Server, c authCredential) (int, string) {
	t.Helper()
	conn, resp, err := c.dialer().Dial(c.url(authWSURL(ts, "/ws/speedtest")), c.header0())
	if err == nil {
		sub := conn.Subprotocol()
		conn.Close()
		return 200, sub
	}
	if resp == nil {
		t.Fatalf("dial /ws/speedtest: %v", err)
	}
	return resp.StatusCode, ""
}

// authSessionProbe drives one shell_start on a non-existent node and reports
// what the session looked like: whether the server announced a public session
// kind, and the error text the refusal carried. A public session is refused
// with the public-mode message; an authenticated one reaches the node lookup
// and is told the node is offline. That difference is the S9 matrix assertion.
func authSessionProbe(t *testing.T, c *websocket.Conn) (sessionKind, errText string) {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{
		"action":  "shell_start",
		"node_id": "no-such-node",
		"id":      "probe-session-id",
	})
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write shell_start: %v", err)
	}
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read client frame: %v", err)
		}
		var resp CommandResponse
		if json.Unmarshal(data, &resp) != nil {
			t.Fatalf("decode client frame %s", data)
		}
		switch resp.Type {
		case "session_kind":
			var hello struct {
				Kind string `json:"kind"`
			}
			json.Unmarshal(data, &hello)
			sessionKind = hello.Kind
		case "error":
			errText = resp.Data
		case "done":
			return sessionKind, errText
		}
	}
}

// authRequest issues a plain HTTP request with an optional bearer credential.
func authRequest(t *testing.T, ts *httptest.Server, method, path, key, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func authReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// seedGeoCache puts a valid JSON answer in the geo cache so a handler test never
// reaches ip-api.com (and never spends a rate-limit token).
func seedGeoCache(s *Server, ip string) {
	s.geoCacheMu.Lock()
	defer s.geoCacheMu.Unlock()
	entry := &geoCacheEntry{
		ip:        ip,
		data:      []byte(`{"status":"success","query":"` + ip + `"}`),
		expiresAt: time.Now().Add(time.Hour),
	}
	s.geoCache[ip] = s.geoCacheList.PushFront(entry)
}

// seedRDAPCache does the same for RDAP, so no request leaves the test binary.
func seedRDAPCache(s *Server, query string) {
	s.rdapCacheMu.Lock()
	defer s.rdapCacheMu.Unlock()
	s.rdapCache[query] = &rdapEntry{
		data:      []byte(`{"handle":"TEST"}`),
		expiresAt: time.Now().Add(time.Hour),
	}
}

// authReadEndpoints is the set of browser read endpoints S8 puts behind the
// client key. Each seeds whatever cache it needs so a 200 is reachable offline.
var authReadEndpoints = []struct {
	name   string
	method string
	path   string
	body   string
	seed   func(*Server)
}{
	{"nodes", "GET", "/api/nodes", "", nil},
	{"nodes health", "GET", "/api/nodes/health", "", nil},
	{"latency matrix", "GET", "/api/latency-matrix", "", nil},
	{"geoip GET", "GET", "/api/geoip/1.1.1.1", "", func(s *Server) { seedGeoCache(s, "1.1.1.1") }},
	{"geoip POST", "POST", "/api/geoip/", `{"ips":["1.1.1.1"]}`, func(s *Server) { seedGeoCache(s, "1.1.1.1") }},
	{"rdap", "GET", "/api/rdap/1.1.1.1", "", func(s *Server) { seedRDAPCache(s, "1.1.1.1") }},
}

// ---- F18: the credential never reaches a log line ---------------------------

func TestNoHandlerLogsRequestURL(t *testing.T) {
	// CLI clients still pass the key as ?key=<key>, so any log statement that
	// renders the raw URL would write the key to disk. Line-level so a log call
	// that merely sits near a URL expression is not flagged.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read backend dir: %v", err)
	}
	logCall := regexp.MustCompile(`log\.(Print|Printf|Println|Fatal)`)
	urlLeak := regexp.MustCompile(`RawQuery|URL\.String\(\)|r\.URL\)`)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if logCall.MatchString(line) && urlLeak.MatchString(line) {
				t.Errorf("%s:%d logs a raw request URL (keys travel in query strings): %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no backend source files were checked — the grep is vacuous")
	}
}

func TestInstallTemplateDocumentsProxyLogStripping(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "deploy", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	text := string(data)
	for _, want := range []string{"strip query strings", "/ws/*"} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy/install.sh is missing the proxy access-log note (%q)", want)
		}
	}
}

// ---- credential normalization ----------------------------------------------

func TestWSSubprotocolCredentialParsing(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   string
		offers bool
	}{
		{"single header, key follows marker", []string{"lg.bearer, secret-key"}, "secret-key", true},
		{"no spaces", []string{"lg.bearer,secret-key"}, "secret-key", true},
		{"split across header lines", []string{"lg.bearer", "secret-key"}, "", true},
		{"marker with nothing after it", []string{"lg.bearer"}, "", true},
		{"marker last in list", []string{"chat, lg.bearer"}, "", true},
		{"unrelated subprotocol only", []string{"chat, superchat"}, "", false},
		{"credential not directly after marker", []string{"chat, secret-key"}, "", false},
		{"no header at all", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://lg.example.com/ws/client", nil)
			for _, v := range tc.values {
				r.Header.Add("Sec-WebSocket-Protocol", v)
			}
			if got := wsSubprotocolCredential(r); got != tc.want {
				t.Errorf("wsSubprotocolCredential = %q, want %q", got, tc.want)
			}
			if got := wsOffersBearer(r); got != tc.offers {
				t.Errorf("wsOffersBearer = %v, want %v", got, tc.offers)
			}
		})
	}
}

func TestNormalizeWSCredentialPrecedence(t *testing.T) {
	newReq := func(auth, sub, query string) *http.Request {
		url := "http://lg.example.com/ws/client"
		if query != "" {
			url += "?key=" + query
		}
		r := httptest.NewRequest("GET", url, nil)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		if sub != "" {
			r.Header.Set("Sec-WebSocket-Protocol", wsBearerSubprotocol+", "+sub)
		}
		return r
	}

	t.Run("header wins over subprotocol", func(t *testing.T) {
		r := newReq("from-header", "from-subprotocol", "")
		normalizeWSCredential(r)
		if got := extractBearerKey(r); got != "from-header" {
			t.Errorf("extractBearerKey = %q, want the Authorization value", got)
		}
	})

	t.Run("subprotocol wins over query", func(t *testing.T) {
		r := newReq("", "from-subprotocol", "from-query")
		normalizeWSCredential(r)
		if got := extractBearerKey(r); got != "from-subprotocol" {
			t.Errorf("extractBearerKey = %q, want the subprotocol value", got)
		}
	})

	t.Run("query survives when no subprotocol is offered", func(t *testing.T) {
		r := newReq("", "", "from-query")
		normalizeWSCredential(r)
		if got := extractBearerKey(r); got != "from-query" {
			t.Errorf("extractBearerKey = %q, want the query value", got)
		}
	})

	t.Run("no credential at all stays empty", func(t *testing.T) {
		r := newReq("", "", "")
		normalizeWSCredential(r)
		if got := extractBearerKey(r); got != "" {
			t.Errorf("extractBearerKey = %q, want empty", got)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization was synthesized from nothing: %q", r.Header.Get("Authorization"))
		}
	})
}

// ---- /ws/client credential transports ---------------------------------------

func TestClientWSAcceptsEveryCredentialTransport(t *testing.T) {
	cases := []struct {
		name string
		cred authCredential
	}{
		{"subprotocol", authCredential{subprotocol: authTestKey}},
		{"authorization header", authCredential{header: authTestKey}},
		{"query parameter", authCredential{query: authTestKey}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{"CLIENT_API_KEY": authTestKey})
			conn, resp, err := authDialClient(t, ts, tc.cred)
			if err != nil {
				t.Fatalf("dial /ws/client: %v (status %v)", err, resp)
			}
			if tc.cred.subprotocol != "" && conn.Subprotocol() != wsBearerSubprotocol {
				t.Errorf("selected subprotocol = %q, want %q", conn.Subprotocol(), wsBearerSubprotocol)
			}
			if tc.cred.subprotocol == "" && conn.Subprotocol() != "" {
				t.Errorf("server echoed %q for a client that offered no subprotocol", conn.Subprotocol())
			}
			kind, errText := authSessionProbe(t, conn)
			if kind != "" {
				t.Errorf("session kind = %q, want an authenticated (unannounced) session", kind)
			}
			if !strings.Contains(errText, "Node is offline") {
				t.Errorf("shell_start error = %q, want the authenticated node-lookup path", errText)
			}
		})
	}
}

func TestClientWSPrecedenceAcrossTransports(t *testing.T) {
	cases := []struct {
		name    string
		cred    authCredential
		wantOK  bool
		because string
	}{
		{"good header beats bad subprotocol", authCredential{header: authTestKey, subprotocol: "wrong"}, true, "Authorization wins"},
		{"bad header beats good subprotocol", authCredential{header: "wrong", subprotocol: authTestKey}, false, "Authorization wins"},
		{"good subprotocol beats bad query", authCredential{subprotocol: authTestKey, query: "wrong"}, true, "subprotocol wins over ?key="},
		{"bad subprotocol beats good query", authCredential{subprotocol: "wrong", query: authTestKey}, false, "subprotocol wins over ?key="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{"CLIENT_API_KEY": authTestKey})
			conn, _, err := authDialClient(t, ts, tc.cred)
			if err != nil {
				t.Fatalf("dial /ws/client: %v", err)
			}
			authorized := authIsAuthorized(t, conn)
			if authorized != tc.wantOK {
				t.Errorf("session authorized = %v, want %v (%s)", authorized, tc.wantOK, tc.because)
			}
		})
	}
}

// authIsAuthorized reports whether the first frame on a freshly dialed socket is
// the F19 auth_error. The socket is consumed either way.
func authIsAuthorized(t *testing.T, c *websocket.Conn) bool {
	t.Helper()
	// The auth_error frame is written before the handler returns, so it is
	// already in flight when Dial resolves; a short deadline is enough, and the
	// authenticated case (no frame at all) pays it only once.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		// No frame at all within the deadline means the server left the socket
		// open and idle — an authenticated session.
		if websocket.IsCloseError(err, wsCloseUnauthorized) {
			t.Fatalf("socket closed 4401 without the auth_error frame")
		}
		return true
	}
	var resp CommandResponse
	if json.Unmarshal(data, &resp) != nil {
		t.Fatalf("decode first frame %s", data)
	}
	return resp.Type != "auth_error"
}

func TestClientWSAuthFailureSendsAuthErrorAndCloses4401(t *testing.T) {
	_, ts := newAuthTestServer(t, map[string]string{"CLIENT_API_KEY": authTestKey})

	for _, tc := range []struct {
		name string
		cred authCredential
	}{
		{"wrong key via subprotocol", authCredential{subprotocol: "wrong-key"}},
		{"no credential at all", authCredential{offerBearer: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := authDialClient(t, ts, tc.cred)
			if err != nil {
				status := 0
				if resp != nil {
					status = resp.StatusCode
				}
				t.Fatalf("upgrade must be accepted so the browser can read the reason; got %v (status %d)", err, status)
			}
			// The offered subprotocol must be echoed even on refusal, or a real
			// browser fails the socket with 1006 and never sees the 4401.
			if conn.Subprotocol() != wsBearerSubprotocol {
				t.Errorf("selected subprotocol = %q, want %q on the auth-failure upgrade",
					conn.Subprotocol(), wsBearerSubprotocol)
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read auth_error frame: %v", err)
			}
			var first CommandResponse
			if json.Unmarshal(data, &first) != nil {
				t.Fatalf("decode first frame %s", data)
			}
			if first.Type != "auth_error" {
				t.Fatalf("first frame = %s, want {\"type\":\"auth_error\"}", data)
			}

			// Nothing else is ever sent on this socket: the next read is the close.
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, extra, err := conn.ReadMessage()
			if err == nil {
				t.Fatalf("server sent another frame after auth_error: %s", extra)
			}
			ce, ok := err.(*websocket.CloseError)
			if !ok {
				t.Fatalf("second read = %v, want a close error", err)
			}
			if ce.Code != wsCloseUnauthorized {
				t.Errorf("close code = %d, want %d", ce.Code, wsCloseUnauthorized)
			}
			if ce.Text != "unauthorized" {
				t.Errorf("close reason = %q, want %q", ce.Text, "unauthorized")
			}
		})
	}
}

func TestClientWSHomelabNeedsNoCredential(t *testing.T) {
	// No key configured: the handshake carries no Authorization header, no
	// subprotocol and no query parameter — byte-for-byte what it was before S8.
	_, ts := newAuthTestServer(t, nil)
	conn, _, err := authDialClient(t, ts, authCredential{})
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	if conn.Subprotocol() != "" {
		t.Errorf("server selected subprotocol %q for a plain handshake", conn.Subprotocol())
	}
	kind, errText := authSessionProbe(t, conn)
	if kind != "" {
		t.Errorf("session kind = %q, want an unannounced (authenticated) session", kind)
	}
	if !strings.Contains(errText, "Node is offline") {
		t.Errorf("shell_start error = %q, want the node-lookup path", errText)
	}
}

// ---- S9 requestIsPublic matrix, credential supplied only via subprotocol ----

// authSubprotocolMatrix mirrors publicMatrix (public_test.go) but states what
// each configuration means for a session dialed with lg.bearer only.
var authSubprotocolMatrix = []struct {
	name        string
	publicMode  string
	key         string
	subprotocol string
	wantPublic  bool // server announces session_kind=public and refuses shell_start
	wantAuthErr bool // F19: auth_error + 4401 instead of a session
	wantSpeed   int
}{
	{"public mode, empty key", "1", "", "", true, false, 403},
	{"public mode, key set, no credential", "1", authTestKey, "", true, false, 403},
	{"public mode, key set, wrong credential", "1", authTestKey, "nope", true, false, 403},
	{"public mode, key set, correct credential", "1", authTestKey, authTestKey, false, false, 200},
	{"private mode, key set, no credential", "", authTestKey, "", false, true, 401},
	{"private mode, key set, correct credential", "", authTestKey, authTestKey, false, false, 200},
	{"private mode, no key (homelab)", "", "", "", false, false, 200},
}

func TestClientWSPublicMatrixViaSubprotocol(t *testing.T) {
	for _, tc := range authSubprotocolMatrix {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{
				"PUBLIC_MODE":    tc.publicMode,
				"CLIENT_API_KEY": tc.key,
			})
			cred := authCredential{subprotocol: tc.subprotocol, offerBearer: true}
			conn, _, err := authDialClient(t, ts, cred)
			if err != nil {
				t.Fatalf("dial /ws/client: %v", err)
			}
			if conn.Subprotocol() != wsBearerSubprotocol {
				t.Errorf("selected subprotocol = %q, want it echoed", conn.Subprotocol())
			}
			if tc.wantAuthErr {
				if authIsAuthorized(t, conn) {
					t.Fatal("expected the F19 auth_error frame")
				}
				return
			}
			kind, errText := authSessionProbe(t, conn)
			if tc.wantPublic {
				if kind != "public" {
					t.Errorf("session kind = %q, want %q", kind, "public")
				}
				if !strings.Contains(errText, "interactive shell is not allowed") {
					t.Errorf("shell_start error = %q, want the public-mode refusal", errText)
				}
				return
			}
			if kind != "" {
				t.Errorf("session kind = %q, want an authenticated session", kind)
			}
			if !strings.Contains(errText, "Node is offline") {
				t.Errorf("shell_start error = %q, want the authenticated node-lookup path", errText)
			}
		})
	}
}

func TestSpeedTestWSPublicMatrixViaSubprotocol(t *testing.T) {
	for _, tc := range authSubprotocolMatrix {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{
				"PUBLIC_MODE":    tc.publicMode,
				"CLIENT_API_KEY": tc.key,
			})
			code, sub := authDialSpeedTest(t, ts, authCredential{subprotocol: tc.subprotocol, offerBearer: true})
			if code != tc.wantSpeed {
				t.Fatalf("/ws/speedtest handshake = %d, want %d", code, tc.wantSpeed)
			}
			if code == 200 && sub != wsBearerSubprotocol {
				t.Errorf("selected subprotocol = %q, want it echoed on /ws/speedtest", sub)
			}
		})
	}
}

// ---- read endpoints ----------------------------------------------------------

func TestReadEndpointsRequireClientKey(t *testing.T) {
	for _, ep := range authReadEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			srv, ts := newAuthTestServer(t, map[string]string{"CLIENT_API_KEY": authTestKey})
			if ep.seed != nil {
				ep.seed(srv)
			}

			resp := authRequest(t, ts, ep.method, ep.path, "", ep.body)
			if resp.StatusCode != 401 {
				t.Errorf("%s %s without a key = %d, want 401 (body %s)",
					ep.method, ep.path, resp.StatusCode, strings.TrimSpace(authReadBody(t, resp)))
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control on the 401 = %q, want no-store", got)
			}

			ok := authRequest(t, ts, ep.method, ep.path, authTestKey, ep.body)
			if ok.StatusCode != 200 {
				t.Errorf("%s %s with the key = %d, want 200 (body %s)",
					ep.method, ep.path, ok.StatusCode, strings.TrimSpace(authReadBody(t, ok)))
			}
			if got := ok.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control on success = %q, want no-store", got)
			}
		})
	}
}

func TestReadEndpointsOpenInPublicMode(t *testing.T) {
	for _, ep := range authReadEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			srv, ts := newAuthTestServer(t, map[string]string{
				"PUBLIC_MODE":    "1",
				"CLIENT_API_KEY": authTestKey,
			})
			if ep.seed != nil {
				ep.seed(srv)
			}
			resp := authRequest(t, ts, ep.method, ep.path, "", ep.body)
			if resp.StatusCode != 200 {
				t.Errorf("%s %s in public mode = %d, want 200 (body %s)",
					ep.method, ep.path, resp.StatusCode, strings.TrimSpace(authReadBody(t, resp)))
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store regardless of mode", got)
			}
		})
	}
}

func TestReadEndpointsUnchangedWithoutKey(t *testing.T) {
	for _, ep := range authReadEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			srv, ts := newAuthTestServer(t, nil)
			if ep.seed != nil {
				ep.seed(srv)
			}
			resp := authRequest(t, ts, ep.method, ep.path, "", ep.body)
			if resp.StatusCode != 200 {
				t.Errorf("%s %s on a keyless homelab = %d, want 200 (body %s)",
					ep.method, ep.path, resp.StatusCode, strings.TrimSpace(authReadBody(t, resp)))
			}
		})
	}
}

// ---- S9 advisory A1: privileged endpoints refuse public sessions ------------

// authPrivilegedEndpoints are the mutating/operator routes that must answer 403
// to an anonymous public visitor, together with the status the same request
// produces on a keyless homelab (proving the check is public-mode-only).
var authPrivilegedEndpoints = []struct {
	name         string
	method       string
	path         string
	body         string
	wantHomelab  int
	homelabIsNot int // guard: the homelab status must not be the public refusal
}{
	{"delete node", "DELETE", "/api/nodes?id=missing", "", 404, 403},
	{"post latency", "POST", "/api/latency-matrix", `{"from_id":"a","to_id":"b","latency_ms":1}`, 400, 403},
	{"list schedules", "GET", "/api/schedules", "", 200, 403},
	{"create schedule", "POST", "/api/schedules", `{"node_id":"a","command":"shell","target":"x","interval_sec":60}`, 400, 403},
	{"delete schedule", "DELETE", "/api/schedules/missing", "", 404, 403},
	{"toggle schedule", "POST", "/api/schedules/missing/toggle", "", 404, 403},
	{"run schedule", "POST", "/api/schedules/missing/run", "", 404, 403},
}

func TestPrivilegedEndpointsRefusePublicSessions(t *testing.T) {
	for _, ep := range authPrivilegedEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			// Keyless public mode: authClient alone would say yes to everyone,
			// so requestIsPublic has to be consulted first.
			_, ts := newAuthTestServer(t, map[string]string{"PUBLIC_MODE": "1"})
			resp := authRequest(t, ts, ep.method, ep.path, "", ep.body)
			body := authReadBody(t, resp)
			if resp.StatusCode != 403 {
				t.Fatalf("%s %s in keyless public mode = %d, want 403 (body %s)",
					ep.method, ep.path, resp.StatusCode, strings.TrimSpace(body))
			}
			if !strings.Contains(body, "not available in public mode") {
				t.Errorf("403 body = %s, want the public-mode message", strings.TrimSpace(body))
			}
		})
	}
}

func TestPrivilegedEndpointsUnchangedOnHomelab(t *testing.T) {
	for _, ep := range authPrivilegedEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, nil)
			resp := authRequest(t, ts, ep.method, ep.path, "", ep.body)
			body := authReadBody(t, resp)
			if resp.StatusCode == ep.homelabIsNot {
				t.Fatalf("%s %s on a keyless homelab = %d — the public check must not fire here",
					ep.method, ep.path, resp.StatusCode)
			}
			if resp.StatusCode != ep.wantHomelab {
				t.Errorf("%s %s on a keyless homelab = %d, want %d (body %s)",
					ep.method, ep.path, resp.StatusCode, ep.wantHomelab, strings.TrimSpace(body))
			}
		})
	}
}

func TestPrivilegedEndpointsReachableWithKeyInPublicMode(t *testing.T) {
	// A key-holder on a public deployment is not a public session, so the
	// operator surface stays available to them.
	for _, ep := range authPrivilegedEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{
				"PUBLIC_MODE":    "1",
				"CLIENT_API_KEY": authTestKey,
			})
			resp := authRequest(t, ts, ep.method, ep.path, authTestKey, ep.body)
			if resp.StatusCode == 403 {
				t.Errorf("%s %s with the correct key = 403, want the normal path (%d)",
					ep.method, ep.path, ep.wantHomelab)
			}
		})
	}
}

// ---- S9 advisory A2: shell_input from a public session is dropped -----------

// authAgentMessages registers a fake agent and reports every envelope the
// server sends it. scheduler_test.go's fakeAgent only forwards "command"
// actions, so this file uses its own.
type authAgent struct {
	conn   *websocket.Conn
	nodeID string
	msgs   chan AgentMessage
}

func dialAuthAgent(t *testing.T, srv *Server, ts *httptest.Server, name string) *authAgent {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(authWSURL(ts, "/ws/agent"), nil)
	if err != nil {
		t.Fatalf("dial /ws/agent: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	reg, _ := json.Marshal(map[string]any{
		"name": name, "location": "Testville", "flag": "🏁",
		"ipv4": "192.0.2.20", "provider": "TestNet",
	})
	env, _ := json.Marshal(AgentMessage{Action: "register", Payload: reg})
	if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
		t.Fatalf("send register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read register ack: %v", err)
	} else if !strings.Contains(string(msg), `"registered"`) {
		t.Fatalf("unexpected register ack: %s", msg)
	}
	conn.SetReadDeadline(time.Time{})

	a := &authAgent{conn: conn, nodeID: waitForNodeID(t, srv, name), msgs: make(chan AgentMessage, 16)}
	go func() {
		for {
			_, msg, err := a.conn.ReadMessage()
			if err != nil {
				return
			}
			var env AgentMessage
			if json.Unmarshal(msg, &env) != nil {
				continue
			}
			select {
			case a.msgs <- env:
			default:
			}
		}
	}()
	return a
}

// awaitAction waits for one envelope with the given action, or reports that
// none arrived.
func (a *authAgent) awaitAction(action string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case env := <-a.msgs:
			if env.Action == action {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestShellInputFromPublicSessionIsDropped(t *testing.T) {
	// Both halves register the same command id to the same socket, so the only
	// difference is the session's public flag: an authenticated owner reaches
	// the agent, a public one is dropped before any lookup.
	run := func(t *testing.T, public bool) bool {
		srv, ts := newAuthTestServer(t, nil)
		// Short, fixed name: node names are rune-truncated to 64 on registration,
		// and each subtest gets its own Server anyway.
		agent := dialAuthAgent(t, srv, ts, "shell-input-agent")
		conn, _, err := authDialClient(t, ts, authCredential{})
		if err != nil {
			t.Fatalf("dial /ws/client: %v", err)
		}
		// Find the server-side socket for this client and register a shell
		// session it owns on the live node.
		const sessionID = "owned-shell-session"
		waitFor(t, "client registered", 5*time.Second, func() bool {
			srv.clientsMu.RLock()
			defer srv.clientsMu.RUnlock()
			return len(srv.clients) == 1
		})
		srv.clientsMu.RLock()
		var serverSide *websocket.Conn
		for c := range srv.clients {
			serverSide = c
		}
		srv.clientsMu.RUnlock()
		srv.cmdOwnersMu.Lock()
		srv.cmdOwners[sessionID] = serverSide
		srv.cmdOwnersMu.Unlock()
		srv.cmdNodesMu.Lock()
		srv.cmdNodes[sessionID] = agent.nodeID
		srv.cmdNodesMu.Unlock()
		if public {
			srv.markPublic(serverSide)
		}

		msg, _ := json.Marshal(map[string]any{
			"action":  "shell_input",
			"node_id": agent.nodeID,
			"id":      sessionID,
			"input":   "id\n",
		})
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Fatalf("write shell_input: %v", err)
		}
		return agent.awaitAction("shell_input", 2*time.Second)
	}

	t.Run("authenticated owner reaches the agent", func(t *testing.T) {
		if !run(t, false) {
			t.Fatal("agent never received shell_input from its owning session")
		}
	})
	t.Run("public session is dropped", func(t *testing.T) {
		if run(t, true) {
			t.Fatal("a public session's shell_input reached the agent")
		}
	})
}

// ---- /api/public-config + /api/runs cache header ----------------------------

func TestPublicConfigReportsAuthRequired(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		publicMode string
		want       bool
	}{
		{"key set, public off", authTestKey, "", true},
		{"key set, public on", authTestKey, "1", false},
		{"no key, public off (homelab)", "", "", false},
		{"no key, public on", "", "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newAuthTestServer(t, map[string]string{
				"CLIENT_API_KEY": tc.key,
				"PUBLIC_MODE":    tc.publicMode,
			})
			resp := authRequest(t, ts, "GET", "/api/public-config", "", "")
			if resp.StatusCode != 200 {
				t.Fatalf("GET /api/public-config = %d, want 200", resp.StatusCode)
			}
			var cfg struct {
				AuthRequired bool `json:"auth_required"`
				PublicMode   bool `json:"public_mode"`
			}
			if err := json.Unmarshal([]byte(authReadBody(t, resp)), &cfg); err != nil {
				t.Fatalf("decode public-config: %v", err)
			}
			if cfg.AuthRequired != tc.want {
				t.Errorf("auth_required = %v, want %v", cfg.AuthRequired, tc.want)
			}
		})
	}
}

func TestRunGetIsPrivatelyCacheable(t *testing.T) {
	_, ts := newAuthTestServer(t, map[string]string{"CLIENT_API_KEY": authTestKey})
	created := authRequest(t, ts, "POST", "/api/runs", authTestKey,
		`{"command":"ping","target":"1.1.1.1","lines":[{"type":"output","text":"64 bytes"}]}`)
	if created.StatusCode != 201 {
		t.Fatalf("POST /api/runs = %d, want 201", created.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(authReadBody(t, created)), &out); err != nil {
		t.Fatalf("decode run id: %v", err)
	}

	got := authRequest(t, ts, "GET", "/api/runs/"+out.ID, "", "")
	if got.StatusCode != 200 {
		t.Fatalf("GET /api/runs/<id> = %d, want 200", got.StatusCode)
	}
	if cc := got.Header.Get("Cache-Control"); cc != "private, max-age=300" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, max-age=300")
	}
}
