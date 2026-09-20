package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// handleRuns dispatches POST/GET on /api/runs. POST creates a new permalink
// and returns its id; GET /api/runs/<id> retrieves the stored payload.
//
// Storage is in-memory (server restart wipes it) with a 24h TTL and a hard
// cap of runStoreMaxRecords entries. Authentication mirrors the rest of the
// client API: if CLIENT_API_KEY is set, the POST must include it.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case "POST":
		s.handleRunCreate(w, r)
	case "GET":
		s.handleRunGet(w, r)
	default:
		writeJSONError(w, "method not allowed", 405)
	}
}

func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	// This handler is reachable through both /api/runs and /api/runs/.
	// Enforce the bound here so no routing alias can bypass it.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	// Public sessions never write to the shared run store: it is server memory
	// that every visitor could fill, and the permalink is world-readable.
	// Checked before auth so the answer is the same whether or not a key is set.
	if requestIsPublic(r) {
		writeJSONError(w, "not available in public mode", 403)
		return
	}
	// Same auth as /ws/client — anyone who can run commands can save a run.
	if !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return
	}
	// Quota for key-holding users of a *public* deployment only. With public
	// mode off there is no quota at all, so a private install (no key, behind a
	// reverse proxy that collapses every visitor onto one address) is unchanged.
	if publicModeEnabled() && !s.rateAllowRunCreate(clientRateKey(r)) {
		w.Header().Set("Retry-After", "3600")
		writeJSONError(w, "rate limit exceeded", 429)
		return
	}

	var req struct {
		NodeName     string    `json:"node_name"`
		NodeFlag     string    `json:"node_flag"`
		NodeLocation string    `json:"node_location"`
		Command      string    `json:"command"`
		Target       string    `json:"target"`
		Options      string    `json:"options,omitempty"`
		Lines        []runLine `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "bad request", 400)
		return
	}
	if req.Command == "" {
		writeJSONError(w, "command is required", 400)
		return
	}
	if len(req.Lines) == 0 {
		writeJSONError(w, "lines is required", 400)
		return
	}
	if len(req.Lines) > runMaxLines {
		writeJSONError(w, "too many lines", 400)
		return
	}

	// Sanitize each field. Lines arrive from the user's own browser session
	// but we still strip control chars + truncate so a malicious client
	// can't poison stored runs that other users will fetch.
	rec := &runRecord{
		ID:           uuid.New().String(),
		NodeName:     sanitizeString(stripControlChars(req.NodeName), 64),
		NodeFlag:     sanitizeString(stripControlChars(req.NodeFlag), 32),
		NodeLocation: sanitizeString(stripControlChars(req.NodeLocation), 128),
		Command:      sanitizeString(stripControlChars(req.Command), 32),
		Target:       sanitizeString(stripControlChars(req.Target), 1024),
		Options:      sanitizeString(stripControlChars(req.Options), 512),
		CreatedAt:    time.Now(),
		expiresAt:    time.Now().Add(runStoreTTL),
		Lines:        make([]runLine, 0, len(req.Lines)),
	}
	allowedTypes := map[string]bool{"info": true, "output": true, "error": true, "success": true}
	for _, l := range req.Lines {
		typ := strings.ToLower(l.Type)
		if !allowedTypes[typ] {
			typ = "output"
		}
		text := l.Text
		if len(text) > runMaxLineLen {
			text = text[:runMaxLineLen]
		}
		// Don't HTML-escape line text — the frontend renders it as text inside
		// React (auto-escaped). We strip control chars except newline + tab,
		// which are valid in command output.
		text = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\t' {
				return r
			}
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, text)
		rec.Lines = append(rec.Lines, runLine{Type: typ, Text: text})
	}

	s.runStoreMu.Lock()
	if len(s.runStore) >= runStoreMaxRecords {
		// Evict expired first; if still over, drop the oldest by CreatedAt.
		now := time.Now()
		for k, v := range s.runStore {
			if now.After(v.expiresAt) {
				delete(s.runStore, k)
			}
		}
		if len(s.runStore) >= runStoreMaxRecords {
			var oldestKey string
			var oldestAt time.Time
			first := true
			for k, v := range s.runStore {
				if first || v.CreatedAt.Before(oldestAt) {
					oldestKey = k
					oldestAt = v.CreatedAt
					first = false
				}
			}
			if oldestKey != "" {
				delete(s.runStore, oldestKey)
			}
		}
	}
	s.runStore[rec.ID] = rec
	s.runStoreMu.Unlock()

	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]string{"id": rec.ID})
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/runs/"))
	// Permalink reads are public on purpose — the URL itself is the access
	// token. The id is a v4 uuid (122 bits of entropy) so it's not feasibly
	// guessable. We only validate length/charset to keep junk out.
	if id == "" || len(id) > 64 {
		writeJSONError(w, "invalid id", 400)
		return
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			writeJSONError(w, "invalid id", 400)
			return
		}
	}

	s.runStoreMu.Lock()
	rec, ok := s.runStore[id]
	if ok && time.Now().After(rec.expiresAt) {
		delete(s.runStore, id)
		ok = false
	}
	s.runStoreMu.Unlock()
	if !ok {
		writeJSONError(w, "not found", 404)
		return
	}
	// The permalink id is the access token, so the body is not shared-cacheable
	// even though it is unauthenticated: only the requesting browser may keep a
	// copy (F36).
	w.Header().Set("Cache-Control", "private, max-age=300")
	json.NewEncoder(w).Encode(rec)
}

// runStoreCleanup removes expired records on a 30-minute timer. Stops when
// the supplied done channel is closed.
func (s *Server) runStoreCleanup(done <-chan struct{}) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			now := time.Now()
			s.runStoreMu.Lock()
			for k, v := range s.runStore {
				if now.After(v.expiresAt) {
					delete(s.runStore, k)
				}
			}
			s.runStoreMu.Unlock()
		}
	}
}
