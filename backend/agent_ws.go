package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Preserve full Git revisions and release tags while bounding untrusted labels.
const maxAgentVersionRunes = 128

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	// Authenticate agent via Authorization header first, fall back to query param
	if s.agentAPIKey != "" {
		key := extractBearerKey(r)
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.agentAPIKey)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Agent upgrade error: %v", err)
		return
	}
	if !s.trackWebSocket(conn) {
		return
	}
	defer s.untrackWebSocket(conn)
	defer conn.Close()

	conn.SetReadLimit(wsReadLimitAgent)

	// WebSocket-level ping/pong to detect dead agent connections.
	// If the agent doesn't respond to a ping within 90s, the read will timeout.
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	var node *Node
	// connMu serializes all writes to this agent connection.
	// Used by the ping goroutine and shared with handlers via node.mu after registration.
	// Before registration, only the ping goroutine writes, so no contention.
	var connMu sync.Mutex

	// Send WebSocket-level pings every 30s to detect dead connections
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				connMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				conn.SetWriteDeadline(time.Time{})
				connMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	defer close(pingDone)

	for {
		_, msg, err := conn.ReadMessage()
		// Reset read deadline on any successful read (output, heartbeat, health, etc.)
		// so active agents aren't disconnected while streaming data.
		if err == nil {
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		}
		if err != nil {
			if node != nil {
				log.Printf("Agent %s (%s) disconnected", node.Name, node.ID)
				s.nodesMu.Lock()
				node.Online = false
				node.disconnecting = true
				s.nodesMu.Unlock()
				// Notify command owners whose commands were running on this node
				s.cmdNodesMu.RLock()
				orphanedCmdIDs := make([]string, 0)
				for cmdID, nodeID := range s.cmdNodes {
					if nodeID == node.ID {
						orphanedCmdIDs = append(orphanedCmdIDs, cmdID)
					}
				}
				s.cmdNodesMu.RUnlock()
				// Each terminal path below retires its own routing. Retain these
				// mappings until then, so simultaneous browser disconnect cannot
				// release an ID before this agent's error/done has been delivered.

				for _, cmdID := range orphanedCmdIDs {
					// Whatever the command was, its summary dedup entry dies
					// with the connection — neither branch below can reach a
					// `done` for it any more.
					s.forgetSummary(cmdID)
					// Scheduled runs have no browser owner; they get an
					// explicit terminal status instead of error+done, so they
					// can never stay stuck in "running".
					if s.scheduleAgentDisconnected(cmdID) {
						continue
					}
					// Mesh probes have no browser owner either; reaping frees
					// the waiting dispatcher so the run can finish.
					if s.meshAgentDisconnected(cmdID) {
						continue
					}
					s.sendToCommandOwner(CommandResponse{ID: cmdID, NodeID: node.ID, Type: "error", Data: "Agent disconnected"})
					s.sendToCommandOwner(CommandResponse{ID: cmdID, NodeID: node.ID, Type: "done", Data: ""})
				}

				// Keep the identity reserved until every old command is reaped.
				// A reconnect must not race this cleanup and lose its new commands.
				s.nodesMu.Lock()
				node.disconnecting = false
				s.nodesMu.Unlock()
				s.broadcastNodeStatus(node.ID, false)
			}
			return
		}

		var envelope AgentMessage
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}

		switch envelope.Action {
		case "register":
			s.handleAgentRegister(conn, &connMu, envelope.Payload, &node)
		case "output":
			s.handleAgentOutput(envelope.Payload, node)
		case "health":
			s.handleAgentHealth(envelope.Payload, node)
		}
	}
}

func (s *Server) handleAgentRegister(conn *websocket.Conn, connMu *sync.Mutex, payload json.RawMessage, nodePtr **Node) {
	// Registration is a one-time operation per connection. Renaming an active
	// connection could otherwise leave multiple node identities pointing to it.
	if *nodePtr != nil {
		return
	}
	var reg struct {
		Name     string  `json:"name"`
		Location string  `json:"location"`
		Flag     string  `json:"flag"`
		IPv4     string  `json:"ipv4"`
		IPv6     string  `json:"ipv6"`
		Provider string  `json:"provider"`
		Lat      float64 `json:"lat"`
		Lon      float64 `json:"lon"`
		// Optional capability fields. An older agent sends neither; both are
		// advisory and the server never gates dispatch on them.
		Version string          `json:"version"`
		Tools   map[string]bool `json:"tools"`
	}
	if err := json.Unmarshal(payload, &reg); err != nil {
		log.Printf("Invalid register payload: %v", err)
		return
	}

	// Sanitize registration data — strip control characters to prevent log injection,
	// then truncate to a bounded rune length. Storage does not HTML-escape; render-layer
	// sinks are responsible for their own escaping.
	reg.Name = sanitizeString(stripControlChars(reg.Name), 64)
	reg.Location = sanitizeString(stripControlChars(reg.Location), 128)
	reg.Flag = sanitizeString(stripControlChars(reg.Flag), 32)
	reg.IPv4 = sanitizeString(stripControlChars(reg.IPv4), 45)
	reg.IPv6 = sanitizeString(stripControlChars(reg.IPv6), 45)
	reg.Provider = sanitizeString(stripControlChars(reg.Provider), 128)
	reg.Version = sanitizeString(stripControlChars(reg.Version), maxAgentVersionRunes)
	// Built before the lock (it reads nothing but the agent's payload and the
	// static whitelist) and assigned under nodesMu below.
	regTools := sanitizeAgentTools(reg.Tools)

	node := &Node{ID: uuid.New().String(), mu: connMu, conn: conn}

	s.nodesMu.Lock()
	// Only allow reconnection if the existing node's connection is dead
	for _, existing := range s.nodes {
		if existing.Name == reg.Name {
			alive := existing.Online || existing.disconnecting
			if alive {
				// Existing connection is still alive — reject duplicate.
				// Release nodesMu FIRST: the refusal write below must not
				// happen while holding it (lock ordering), and the live node's
				// entry is left exactly as it was — same UUID, same connection,
				// still online.
				s.nodesMu.Unlock()
				log.Printf("Rejected duplicate agent registration for name: %s", reg.Name)
				// Tell the refused agent why, then close with a dedicated code
				// so it can back off instead of reconnect-looping blindly. The
				// write holds connMu — *this* connection's own mutex, the
				// function parameter — never existing.mu, which serializes
				// writes on a different socket we must not touch.
				s.refuseDuplicateName(conn, connMu)
				return
			}
			// Keep the UUID, but never mutate an old connection's mutex or
			// socket: dispatchers may still hold that Node outside nodesMu.
			node.ID = existing.ID
			break
		}
	}
	s.nodes[node.ID] = node
	node.Name = reg.Name
	node.Location = reg.Location
	node.Flag = reg.Flag
	node.IPv4 = reg.IPv4
	node.IPv6 = reg.IPv6
	node.Provider = reg.Provider
	node.Lat = clampFloat(reg.Lat, -90, 90)
	node.Lon = clampFloat(reg.Lon, -180, 180)
	node.Online = true
	node.LastSeen = time.Now()
	node.Health = nil // Clear stale health data from previous connection; agent will resend within 30s
	// Assigned unconditionally, like Health above: an older agent reconnecting
	// onto an existing Node must clear the values its previous build reported,
	// not inherit them. Fresh map, assigned (never mutated) under nodesMu.
	node.Version = reg.Version
	node.Tools = regTools
	s.nodesMu.Unlock()

	*nodePtr = node

	log.Printf("Agent registered: %s (%s) at %s", node.Name, node.ID, node.Location)
	resp, _ := json.Marshal(map[string]string{"action": "registered", "id": node.ID})
	node.mu.Lock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, resp)
	conn.SetWriteDeadline(time.Time{})
	node.mu.Unlock()
	s.broadcastNodeStatus(node.ID, true)
}

func (s *Server) handleAgentOutput(payload json.RawMessage, node *Node) {
	if node == nil {
		// Unregistered agent — discard output since we can't attribute it
		return
	}
	var output CommandResponse
	if err := json.Unmarshal(payload, &output); err != nil {
		return
	}
	// Validate output type to prevent agents from injecting arbitrary message types
	// (e.g., "node_status") that clients might misinterpret.
	switch output.Type {
	case "output", "error", "done", "shell_output", "summary":
		// valid
	default:
		return
	}

	// F33: an agent may only report on commands that were dispatched to it.
	// Every producer registers cmdNodes *before* the command leaves the server
	// (handleClientCommand, the shell_start path, beginRun), so a missing or
	// mismatched entry means this node was never asked to run that id —
	// either a stale message after teardown or one agent trying to inject
	// output into another node's command.
	s.cmdNodesMu.RLock()
	owningNodeID, registered := s.cmdNodes[output.ID]
	s.cmdNodesMu.RUnlock()
	if !registered {
		// Nothing is tracking this id any more, so no live command can own a
		// summary dedup entry for it. Dropping the entry here is what keeps
		// summarySeen bounded when routing was torn down by someone other
		// than the `done` path, such as a schedule timeout. Disconnected
		// browser commands retain routing until cancellation and terminal
		// acknowledgment complete, then retire through cleanupCommand.
		s.forgetSummary(output.ID)
		return
	}
	if owningNodeID != node.ID {
		return
	}

	output.NodeID = node.ID

	// Structured summaries are validated, sanitized and deduplicated before
	// they go anywhere. An invalid or duplicate summary is dropped whole; the
	// command's output/done stream is unaffected.
	if output.Type == "summary" {
		canonical, ok := s.acceptSummary(output.ID, output.Data)
		if !ok {
			return
		}
		output.Data = string(canonical)
	}

	// If this output belongs to a scheduled run, route it into the schedule's
	// buffer instead of forwarding to a (nonexistent) browser owner. The
	// schedule layer takes care of finalization on `done`.
	if s.scheduleAcceptOutput(output) {
		return
	}

	// Server-side latency mesh probes are likewise server-owned: they have no
	// cmdOwners entry, and consuming the frame here keeps mesh traffic off
	// every browser socket.
	if s.meshAcceptOutput(output) {
		return
	}

	s.sendToCommandOwner(output)
}

func (s *Server) handleAgentHealth(payload json.RawMessage, node *Node) {
	if node == nil {
		return
	}
	// Health payloads now carry the agent's current detected public IPs
	// alongside the usual stats. We sanitize each field independently and
	// broadcast a fresh node_status if either IP changed so connected
	// browsers update their NodeInfo card without waiting for a refetch.
	var combined struct {
		HealthInfo
		IPv4 string `json:"ipv4,omitempty"`
		IPv6 string `json:"ipv6,omitempty"`
		// Capability fields, applied only when present (mirroring the
		// `if combined.IPv4 != ""` guard below) so a frame that omits them
		// leaves the stored values alone.
		Version string          `json:"version,omitempty"`
		Tools   map[string]bool `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(payload, &combined); err != nil {
		return
	}
	sanitized := sanitizeHealthInfo(&combined.HealthInfo)
	ipChanged := false
	capsChanged := false
	s.nodesMu.Lock()
	node.Health = sanitized
	node.LastSeen = time.Now()
	if combined.IPv4 != "" {
		v4 := sanitizeString(stripControlChars(combined.IPv4), 45)
		if v4 != node.IPv4 {
			node.IPv4 = v4
			ipChanged = true
		}
	}
	if combined.IPv6 != "" {
		v6 := sanitizeString(stripControlChars(combined.IPv6), 45)
		if v6 != node.IPv6 {
			node.IPv6 = v6
			ipChanged = true
		}
	}
	if combined.Version != "" {
		v := sanitizeString(stripControlChars(combined.Version), maxAgentVersionRunes)
		if v != node.Version {
			node.Version = v
			capsChanged = true
		}
	}
	if combined.Tools != nil {
		// Compared and assigned inside this one nodesMu section: a map cannot
		// be compared with != , and an unlocked read of node.Tools to decide
		// whether to broadcast would race the assignment.
		tools := sanitizeAgentTools(combined.Tools)
		if !sameToolSet(node.Tools, tools) {
			node.Tools = tools
			capsChanged = true
		}
	}
	s.nodesMu.Unlock()
	if ipChanged {
		log.Printf("Agent %s (%s) IPs updated: ipv4=%s ipv6=%s", node.Name, node.ID, node.IPv4, node.IPv6)
		// online=true reuses the broadcast that includes ipv4/ipv6 in the
		// envelope, which is what the frontend's useNodes hook merges into
		// existing node state.
		s.broadcastNodeStatus(node.ID, true)
	} else if capsChanged {
		// Same broadcast, but only once: a frame that changed both an IP and a
		// capability is covered by the branch above.
		log.Printf("Agent %s (%s) capabilities updated", node.Name, node.ID)
		s.broadcastNodeStatus(node.ID, true)
	}
}

// duplicateNameReason and wsCloseDuplicateName are the wire contract for a
// refused registration. The agent matches on both (agent/main.go).
const (
	duplicateNameReason  = "duplicate name"
	wsCloseDuplicateName = 4409
)

// refuseDuplicateName tells a rejected agent why its registration was refused
// and closes the socket with a dedicated code, so it can back off instead of
// reconnecting every 5 seconds forever. Must be called with nodesMu released,
// and connMu must be the rejected connection's own write mutex.
func (s *Server) refuseDuplicateName(conn *websocket.Conn, connMu *sync.Mutex) {
	refusal, _ := json.Marshal(map[string]any{
		"action":  "error",
		"payload": map[string]string{"reason": duplicateNameReason},
	})
	connMu.Lock()
	defer connMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, refusal)
	conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(wsCloseDuplicateName, duplicateNameReason),
		time.Now().Add(5*time.Second),
	)
	conn.SetWriteDeadline(time.Time{})
}

// sanitizeAgentTools rebuilds an agent-supplied tool map key by key into a
// freshly allocated map, keeping only keys that are real command types. A
// hostile agent therefore cannot grow the map without bound (it is capped by
// len(allowedCommandTypes)) nor store a key the UI would treat as a command
// type. The values stay advisory: the only dispatch gate is
// allowedCommandTypes in handleClientCommand, so a node claiming
// tools:{"rm":true} changes nothing, and a node claiming a tool is absent is
// still sent that command (the agent fails it cleanly).
//
// Returns nil for "nothing usable reported" so the field keeps its omitempty
// behaviour and a node that reported no tools serializes exactly as before.
func sanitizeAgentTools(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(allowedCommandTypes))
	for name, available := range in {
		if allowedCommandTypes[name] {
			out[name] = available
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sameToolSet compares two tool maps by length then per key — maps are not
// comparable with ==. Callers hold nodesMu.
func sameToolSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name, available := range a {
		if other, present := b[name]; !present || other != available {
			return false
		}
	}
	return true
}
