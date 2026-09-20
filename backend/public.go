package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gorilla/websocket"
)

// Public read-only mode lets a deployment expose a stripped Parallax UI
// without an API key. Enabled by env vars; disabled by default.
//
//	PUBLIC_MODE=1                              — turn it on
//	PUBLIC_TARGETS=8.8.8.8,1.1.1.1,example.com — comma-separated allowlist
//	PUBLIC_COMMANDS=ping,traceroute,mtr,dns    — comma-separated allowlist
//	                                             (default: all of the above)
//
// Anonymous /ws/client connections are tagged "public" and their commands
// must match BOTH the target allowlist and the command allowlist. Public
// connections also can't open shell sessions.

func publicModeEnabled() bool {
	return os.Getenv("PUBLIC_MODE") == "1"
}

// requestIsPublic is the single predicate that decides whether a request — WS
// or plain HTTP — is an anonymous public-mode request. One definition so the
// WebSocket handler, /api/runs and /ws/speedtest can never disagree.
//
//	public mode off                → never public (auth behaves as before)
//	public mode on, key set        → public unless the request proves the key
//	public mode on, key empty      → always public (F31): there is no credential
//	                                 that could prove otherwise, so a
//	                                 misconfigured deployment fails safe rather
//	                                 than handing everyone a full session.
//
// A non-public request that fails authentication still gets 401 from the
// caller's own authClient check; this predicate only answers "is it public".
func requestIsPublic(r *http.Request) bool {
	if !publicModeEnabled() {
		return false
	}
	key := os.Getenv("CLIENT_API_KEY")
	if key == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(extractBearerKey(r)), []byte(key)) != 1
}

// publicOptionToken is the entire option grammar a public session may use:
// address family selection and a ping/mtr count. Everything else is dropped
// server-side, so no flag a public visitor invents can reach an agent builder.
var publicOptionToken = regexp.MustCompile(`^(-4|-6|count=[0-9]+)$`)

// filterPublicOptions reduces an options string to the allowed tokens.
func filterPublicOptions(options string) string {
	fields := strings.Fields(options)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if publicOptionToken.MatchString(f) {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " ")
}

// splitEnvList parses a comma-separated env var into a trimmed, non-empty
// string slice. Returns the fallback when the var is unset.
func splitEnvList(name string, fallback []string) []string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func publicAllowedCommands() []string {
	return splitEnvList("PUBLIC_COMMANDS", []string{"ping", "traceroute", "mtr", "dns"})
}

func publicAllowedTargets() []string {
	return splitEnvList("PUBLIC_TARGETS", []string{})
}

// publicCommandAllowed reports whether a command type is in the public
// allowlist. Lookup is O(N) but the list is short (≤10).
func publicCommandAllowed(cmd string) bool {
	for _, c := range publicAllowedCommands() {
		if c == cmd {
			return true
		}
	}
	return false
}

// publicTargetAllowed reports whether a target string is in the public
// allowlist. Exact-match only — wildcards aren't supported because misuse
// is too easy.
func publicTargetAllowed(target string) bool {
	target = strings.TrimSpace(target)
	for _, t := range publicAllowedTargets() {
		if t == target {
			return true
		}
	}
	return false
}

// markPublic / isPublic / forgetPublic manage the publicClients set.
func (s *Server) markPublic(conn *websocket.Conn) {
	s.publicClientsMu.Lock()
	s.publicClients[conn] = struct{}{}
	s.publicClientsMu.Unlock()
}

func (s *Server) isPublic(conn *websocket.Conn) bool {
	s.publicClientsMu.RLock()
	_, ok := s.publicClients[conn]
	s.publicClientsMu.RUnlock()
	return ok
}

func (s *Server) forgetPublic(conn *websocket.Conn) {
	s.publicClientsMu.Lock()
	delete(s.publicClients, conn)
	s.publicClientsMu.Unlock()
}

// handlePublicConfig exposes the public-mode configuration to the frontend
// so it can render the stripped UI. Always returns a valid object — when
// public mode is off, the booleans are false and the lists are empty.
func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	resp := map[string]any{
		"public_mode": publicModeEnabled(),
		// auth_required tells the UI to ask for the client key before it dials
		// anything. It reports configuration, never the key itself, and is the
		// same predicate the read endpoints enforce.
		"auth_required":    clientAuthRequired(),
		"allowed_commands": []string{},
		"allowed_targets":  []string{},
	}
	if publicModeEnabled() {
		resp["allowed_commands"] = publicAllowedCommands()
		resp["allowed_targets"] = publicAllowedTargets()
	}
	json.NewEncoder(w).Encode(resp)
}
