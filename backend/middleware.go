package main

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// strictOriginEnabled reports whether the opt-in same-host origin policy is on.
// It only takes effect when ALLOWED_ORIGINS is unset — an explicit allowlist
// always wins.
func strictOriginEnabled() bool {
	return os.Getenv("STRICT_ORIGIN") == "1"
}

// originAllowedExact reports whether origin is listed verbatim in the
// comma-separated ALLOWED_ORIGINS value.
func originAllowedExact(allowed, origin string) bool {
	for _, o := range strings.Split(allowed, ",") {
		if strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}

// originMatchesHost implements the STRICT_ORIGIN=1 policy: a request with no
// Origin header is allowed (curl / websocat / the CLI never send one, and a
// browser always does), and a request that carries one is allowed only when the
// origin's host:port equals the request's Host, compared case-insensitively.
func originMatchesHost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// checkOrigin validates WebSocket origin.
//
//	ALLOWED_ORIGINS set  → exact match against the list (unchanged).
//	STRICT_ORIGIN=1      → same-host only (see originMatchesHost).
//	neither              → permissive, as before; startupWarnings() says so.
func (s *Server) checkOrigin(r *http.Request) bool {
	allowed := os.Getenv("ALLOWED_ORIGINS")
	if allowed == "" {
		if strictOriginEnabled() && !originMatchesHost(r) {
			log.Printf("Rejected WebSocket origin (STRICT_ORIGIN): %s", stripControlChars(r.Header.Get("Origin")))
			return false
		}
		return true // backward compatible default
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if originAllowedExact(allowed, origin) {
		return true
	}
	log.Printf("Rejected WebSocket origin: %s", stripControlChars(origin))
	return false
}

// corsMiddleware applies CORS headers using ALLOWED_ORIGINS when set, or the
// same-host rule under STRICT_ORIGIN=1. The strict branch never emits "*".
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowed := os.Getenv("ALLOWED_ORIGINS")
		switch {
		case allowed == "" && !strictOriginEnabled():
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case allowed == "":
			// STRICT_ORIGIN=1: echo the Origin only when it passes the same
			// check the WebSocket upgrade uses.
			w.Header().Set("Vary", "Origin")
			if origin := r.Header.Get("Origin"); origin != "" && originMatchesHost(r) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		default:
			// Always set Vary: Origin when response depends on the Origin header,
			// to prevent CDN/proxy caching issues across different origins.
			w.Header().Set("Vary", "Origin")
			if origin := r.Header.Get("Origin"); originAllowedExact(allowed, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next(w, r)
	}
}

// limitBody wraps a handler to enforce request body size limits
func limitBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		next(w, r)
	}
}

// securityHeaders adds basic security headers to all responses.
// Strict-Transport-Security is opt-in via HSTS=1 because dev runs over plain
// http://localhost; setting HSTS there would brick the dev session.
func securityHeaders(next http.HandlerFunc) http.HandlerFunc {
	hstsEnabled := os.Getenv("HSTS") == "1"
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; img-src 'self' https://tile.openstreetmap.org data:; connect-src 'self' ws: wss:; font-src 'self' https://fonts.gstatic.com")
		if hstsEnabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next(w, r)
	}
}

// authClient returns true if the request passes the CLIENT_API_KEY check, or
// if the env var is unset (open mode). Centralized so handlers don't repeat
// the constant-time-compare boilerplate.
func (s *Server) authClient(r *http.Request) bool {
	key := os.Getenv("CLIENT_API_KEY")
	if key == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(extractBearerKey(r)), []byte(key)) == 1
}

// extractBearerKey extracts an API key from the Authorization header (Bearer scheme)
// or falls back to the "key" query parameter.
func extractBearerKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth != "" {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("key")
}

// wsBearerSubprotocol is the WebSocket subprotocol a browser uses to carry the
// client API key. The browser WebSocket API can't set request headers, and a
// key in the query string ends up in proxy access logs, so the key travels as
// the second entry of Sec-WebSocket-Protocol: "lg.bearer, <key>".
const wsBearerSubprotocol = "lg.bearer"

// wsCloseUnauthorized is the private-use close code (4000-4999) the server uses
// to tell a browser its client key was rejected, so the frontend can prompt for
// a new one instead of reconnecting with the same bad key forever.
const wsCloseUnauthorized = 4401

// wsSubprotocolCredential returns the credential offered after the lg.bearer
// marker in Sec-WebSocket-Protocol, or "" when the header carries none. The
// header may arrive as several lines or one comma-separated list; both are
// handled. Only the entry immediately following the marker is considered, so a
// client can't smuggle a credential behind an unrelated subprotocol name.
func wsSubprotocolCredential(r *http.Request) string {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		parts := strings.Split(h, ",")
		for i, p := range parts {
			if strings.TrimSpace(p) != wsBearerSubprotocol {
				continue
			}
			if i+1 < len(parts) {
				return strings.TrimSpace(parts[i+1])
			}
			return ""
		}
	}
	return ""
}

// wsOffersBearer reports whether the handshake offered the lg.bearer
// subprotocol at all (with or without a following credential).
func wsOffersBearer(r *http.Request) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if strings.TrimSpace(p) == wsBearerSubprotocol {
				return true
			}
		}
	}
	return false
}

// wsBearerResponseHeader is the header to pass to Upgrade so the selected
// subprotocol is echoed back. A browser fails (and never opens) a socket whose
// offered subprotocol the server didn't select, which would surface as a 1006
// close instead of the 4401 the UI needs to distinguish a bad key — so the echo
// also applies on the authentication-failure upgrade. Returns nil when the
// client offered nothing, leaving the handshake byte-for-byte as before.
func wsBearerResponseHeader(r *http.Request) http.Header {
	if !wsOffersBearer(r) {
		return nil
	}
	return http.Header{"Sec-WebSocket-Protocol": {wsBearerSubprotocol}}
}

// normalizeWSCredential folds a subprotocol-borne credential into the
// Authorization header, as the first statement of every WebSocket handler that
// authenticates a browser. This is the single point where the new transport is
// recognised: extractBearerKey, authClient and requestIsPublic stay unchanged,
// so every existing decision (including the public-mode matrix) holds when the
// key arrives this way.
//
// Precedence is Authorization → subprotocol → ?key=: an explicit header always
// wins, and the query fallback survives for CLI clients.
func normalizeWSCredential(r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		return
	}
	if v := wsSubprotocolCredential(r); v != "" {
		r.Header.Set("Authorization", "Bearer "+v)
	}
}

// clientAuthRequired reports whether browser-facing read endpoints must demand
// the client key: a key is configured and the deployment is not in public mode.
// Public mode deliberately keeps reads open — S9 owns those semantics.
func clientAuthRequired() bool {
	return os.Getenv("CLIENT_API_KEY") != "" && !publicModeEnabled()
}

// requireClientAuth enforces clientAuthRequired on a read endpoint, writing the
// 401 itself. Returns false when the caller should stop.
func (s *Server) requireClientAuth(w http.ResponseWriter, r *http.Request) bool {
	if clientAuthRequired() && !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return false
	}
	return true
}

// refusePublic answers a privileged or mutating HTTP endpoint for an anonymous
// public-mode visitor with 403, and reports whether it did. Hiding the control
// in the UI is cosmetic; this is the control.
func refusePublic(w http.ResponseWriter, r *http.Request) bool {
	if requestIsPublic(r) {
		writeJSONError(w, "not available in public mode", 403)
		return true
	}
	return false
}

// requestLogger logs method, path, and client IP for audit trail. Wrapping
// it on the Server type lets us record duration into the latency histogram
// for /metrics — the package-level helper exists for routes that don't need
// histogram coverage (currently none).
// Quote the decoded path so percent-encoded controls cannot forge log lines;
// query strings remain excluded because they can carry API credentials.
func requestLogger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next(w, r)
		log.Printf("%s %q from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
	}
}

// loggerWithMetrics wraps a handler so its duration feeds the HTTP latency
// histogram in addition to the access log line. Used by routes that should
// show up in /metrics; static-file routes don't, since they're served by
// FileServer directly and would dominate the histogram with cache hits.
func (s *Server) loggerWithMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next(w, r)
		dur := time.Since(start)
		s.observeHTTPLatency(dur.Seconds())
		log.Printf("%s %q from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, dur.Round(time.Millisecond))
	}
}

// observeHTTPLatency increments the bucket counters for a single request.
// Held lock is short — buckets are a fixed array of 11 entries.
func (s *Server) observeHTTPLatency(seconds float64) {
	s.httpLatencyMu.Lock()
	defer s.httpLatencyMu.Unlock()
	s.httpLatencyCount++
	s.httpLatencySum += seconds
	for i, b := range httpLatencyBucketBounds {
		if seconds <= b {
			s.httpLatencyBuckets[i]++
		}
	}
}

// trustedProxyHops is the number of proxies in front of this server, counted
// from the right-hand (closest) end of X-Forwarded-For. Default 1 — a single
// reverse proxy. Values below 1 are clamped to 1.
func trustedProxyHops() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TRUSTED_PROXY_HOPS")))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// normalizeIP strips an optional port, parses the result as an IP and returns
// its canonical unmapped text form (so ::ffff:1.2.3.4 and 1.2.3.4 share a rate
// key). Reports false for anything that isn't an IP address.
func normalizeIP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.Trim(s, "[]")
	ip := net.ParseIP(s)
	if ip == nil {
		return "", false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return "", false
	}
	return addr.Unmap().String(), true
}

// remoteAddrIP is the transport-level peer address, which no client can forge.
func remoteAddrIP(r *http.Request) string {
	if ip, ok := normalizeIP(r.RemoteAddr); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP extracts the source IP for rate-limiting.
//
// TRUST_PROXY=1 is the gate: with it unset (the default) X-Forwarded-For and
// X-Real-IP are ignored entirely, because any client can forge them. Inside the
// gate, XFF is split on commas and the entry TRUSTED_PROXY_HOPS from the right
// is taken — entries further left were appended by untrusted hops. Anything
// that doesn't parse as an IP falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if os.Getenv("TRUST_PROXY") == "1" {
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			entries := strings.Split(strings.Join(values, ","), ",")
			idx := len(entries) - trustedProxyHops()
			if idx < 0 {
				return remoteAddrIP(r)
			}
			if ip, ok := normalizeIP(entries[idx]); ok {
				return ip
			}
		} else if v := r.Header.Get("X-Real-IP"); v != "" {
			if ip, ok := normalizeIP(v); ok {
				return ip
			}
		}
	}
	return remoteAddrIP(r)
}

// rateKey collapses an IP into the unit we rate-limit: a single v4 address
// (/32) or a v6 /64, which is the smallest block reliably assigned to one
// subscriber. Unparseable input is used verbatim so a caller can never end up
// sharing a bucket with everyone else by accident.
func rateKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	bits := 64
	if addr.Is4() {
		bits = 32
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return addr.String()
	}
	return p.String()
}

// clientRateKey is the per-request rate-limiting identity: clientIP reduced to
// its /32 or /64. Used by the geoip/RDAP buckets and by every public-mode limit.
func clientRateKey(r *http.Request) string {
	return rateKey(clientIP(r))
}
