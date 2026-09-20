package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// sanitizeHealthInfo validates health data from agents to prevent injection
func sanitizeHealthInfo(h *HealthInfo) *HealthInfo {
	if h == nil {
		return nil
	}
	return &HealthInfo{
		Uptime:    sanitizeString(h.Uptime, 128),
		LoadAvg:   sanitizeString(h.LoadAvg, 64),
		MemUsedPc: clampFloat(h.MemUsedPc, 0, 100),
		CPUs:      clampInt(h.CPUs, 0, 1024),
		OS:        sanitizeString(h.OS, 64),
	}
}

// sanitizeString is the storage-layer defence for agent- and client-supplied
// strings: strip control characters, then truncate by rune count. It does
// **not** HTML-escape (S10/F29) — escaping is a render-layer concern and lives
// where HTML is actually built (GeoMap.escapeHtml is the frontend's only HTML
// sink; every other sink is a JSX text child, which React escapes). Escaping
// here produced `AT&amp;T` in storage, in /metrics, in the JSON API and in
// every export.
//
// Order matters and is observable: sanitizeHealthInfo above is the one caller
// that does *not* pre-apply stripControlChars, so this is those three fields'
// sole control-character defence. Stripping first also means the rune budget is
// spent on characters that survive, and there is no longer an HTML entity that
// truncation could split in half.
func sanitizeString(s string, maxLen int) string {
	return truncateRunes(stripControlChars(s), maxLen)
}

// stripControlChars removes ASCII control characters (newlines, tabs, null bytes, etc.)
// to prevent log injection and other control-character-based attacks.
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // drop the character
		}
		return r
	}, s)
}

func clampFloat(v, min, max float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// writeJSONError writes a JSON error response without leaking internals
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(resp)
}

// sendErrorAndDone sends an error response followed by a done response to a client WebSocket.
// This pattern is repeated across many handlers; centralizing it prevents inconsistencies.
func sendErrorAndDone(conn *websocket.Conn, mu *sync.Mutex, cmdID, nodeID, errMsg string) {
	errResp, _ := json.Marshal(CommandResponse{ID: cmdID, NodeID: nodeID, Type: "error", Data: errMsg})
	doneResp, _ := json.Marshal(CommandResponse{ID: cmdID, NodeID: nodeID, Type: "done", Data: `{"exit_ok":false}`})
	mu.Lock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, errResp)
	conn.WriteMessage(websocket.TextMessage, doneResp)
	conn.SetWriteDeadline(time.Time{})
	mu.Unlock()
}

// cleanupCommand retires routing while admission is blocked by cmdOwnersMu.
// A disconnected owner keeps its reservation until both cancel dispatch and
// terminal output complete, so a late cancel/output cannot affect an ID reuse.
func (s *Server) cleanupCommand(cmdID string) {
	s.cmdOwnersMu.Lock()
	defer s.cmdOwnersMu.Unlock()
	s.cleanupCommandLocked(cmdID)
}

// Caller holds cmdOwnersMu; nested order is cmdOwnersMu then cmdNodesMu or
// summarySeenMu. Neither subordinate mutex acquires cmdOwnersMu.
func (s *Server) cleanupCommandLocked(cmdID string) {
	if closing := s.closingCommands[cmdID]; closing != nil {
		closing.terminal = true
		if closing.cancelPending {
			return
		}
		if closing.timer != nil {
			closing.timer.Stop()
		}
		delete(s.closingCommands, cmdID)
	}
	s.cmdNodesMu.Lock()
	delete(s.cmdNodes, cmdID)
	s.cmdNodesMu.Unlock()
	s.forgetSummary(cmdID)
	delete(s.cmdOwners, cmdID)
}

// countClientCommands returns the number of active commands owned by the given connection.
// Must be called without holding cmdOwnersMu.
func (s *Server) countClientCommands(conn *websocket.Conn) int {
	s.cmdOwnersMu.RLock()
	defer s.cmdOwnersMu.RUnlock()
	count := 0
	for _, c := range s.cmdOwners {
		if c == conn {
			count++
		}
	}
	return count
}

// countClientCommandsOnNode returns the number of in-flight commands the given
// client connection has targeted at the given node. Used to stop a single
// client from saturating one agent (e.g., 20 simultaneous traceroutes against
// a residential node). Must be called without holding cmdOwnersMu or cmdNodesMu.
func (s *Server) countClientCommandsOnNode(conn *websocket.Conn, nodeID string) int {
	// Lock order matches every other call site: cmdOwnersMu before cmdNodesMu.
	s.cmdOwnersMu.RLock()
	defer s.cmdOwnersMu.RUnlock()
	s.cmdNodesMu.RLock()
	defer s.cmdNodesMu.RUnlock()
	count := 0
	for cmdID, c := range s.cmdOwners {
		if c == conn && s.cmdNodes[cmdID] == nodeID {
			count++
		}
	}
	return count
}
