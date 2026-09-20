package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// The node list identifies every probe location of the deployment, so it is
	// never stored by a shared cache — in authenticated mode the response is
	// per-key, and switching modes must not serve a stale body (F20).
	w.Header().Set("Cache-Control", "no-store")

	// DELETE /api/nodes?id=xxx — remove an offline node (requires auth)
	if r.Method == "DELETE" {
		// An anonymous public visitor may never delete a node, whether or not a
		// key is configured (with an empty key authClient would say yes).
		if refusePublic(w, r) {
			return
		}
		if !s.authClient(r) {
			writeJSONError(w, "unauthorized", 401)
			return
		}

		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSONError(w, "missing id", 400)
			return
		}
		s.nodesMu.Lock()
		node, ok := s.nodes[id]
		if !ok {
			s.nodesMu.Unlock()
			writeJSONError(w, "node not found", 404)
			return
		}
		if node.Online {
			s.nodesMu.Unlock()
			writeJSONError(w, "cannot remove an online node", 409)
			return
		}
		delete(s.nodes, id)
		s.nodesMu.Unlock()
		// Clean up latency matrix entries — acquire latencyMatrixMu AFTER releasing
		// nodesMu to maintain consistent lock ordering and prevent deadlocks.
		s.latencyMatrixMu.Lock()
		delete(s.latencyMatrix, id)
		for fromID := range s.latencyMatrix {
			delete(s.latencyMatrix[fromID], id)
		}
		s.latencyMatrixMu.Unlock()
		log.Printf("Node removed: %s (%s) by %s", node.Name, id, r.RemoteAddr)
		s.broadcastNodeStatus(id, false)
		w.Write([]byte(`{"ok":true}`))
		return
	}

	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}

	if !s.requireClientAuth(w, r) {
		return
	}

	// Snapshot node data under lock, then encode outside the lock
	// to avoid holding nodesMu while writing to a potentially slow HTTP client.
	type SafeNode struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Location string      `json:"location"`
		Flag     string      `json:"flag"`
		IPv4     string      `json:"ipv4"`
		IPv6     string      `json:"ipv6"`
		Provider string      `json:"provider"`
		Lat      float64     `json:"lat"`
		Lon      float64     `json:"lon"`
		Online   bool        `json:"online"`
		Health   *HealthInfo `json:"health,omitempty"`
		LastSeen time.Time   `json:"last_seen"`
		// Agent capabilities. Both omitempty, so a node that reported neither
		// serializes exactly as it did before. Tools is copied as a reference
		// and encoded outside the lock, which is safe because the map is only
		// ever replaced, never mutated in place (see Node.Tools).
		Version string          `json:"version,omitempty"`
		Tools   map[string]bool `json:"tools,omitempty"`
	}
	s.nodesMu.RLock()
	list := make([]SafeNode, 0, len(s.nodes))
	for _, n := range s.nodes {
		list = append(list, SafeNode{
			ID: n.ID, Name: n.Name, Location: n.Location,
			Flag: n.Flag, IPv4: n.IPv4, IPv6: n.IPv6,
			Provider: n.Provider, Lat: n.Lat, Lon: n.Lon,
			Online: n.Online, Health: n.Health, LastSeen: n.LastSeen,
			Version: n.Version, Tools: n.Tools,
		})
	}
	s.nodesMu.RUnlock()
	json.NewEncoder(w).Encode(list)
}

func (s *Server) handleNodesHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	if !s.requireClientAuth(w, r) {
		return
	}

	type NodeHealth struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Location string      `json:"location"`
		Flag     string      `json:"flag"`
		Provider string      `json:"provider"`
		Online   bool        `json:"online"`
		LastSeen time.Time   `json:"last_seen"`
		Health   *HealthInfo `json:"health,omitempty"`
		// Same two optional capability fields as SafeNode, so the Health modal
		// can show the agent build and grey out missing tools.
		Version string          `json:"version,omitempty"`
		Tools   map[string]bool `json:"tools,omitempty"`
	}

	// Snapshot under lock, encode outside to avoid holding nodesMu during network I/O
	s.nodesMu.RLock()
	list := make([]NodeHealth, 0, len(s.nodes))
	for _, n := range s.nodes {
		list = append(list, NodeHealth{
			ID: n.ID, Name: n.Name, Location: n.Location,
			Flag: n.Flag, Provider: n.Provider, Online: n.Online,
			LastSeen: n.LastSeen, Health: n.Health,
			Version: n.Version, Tools: n.Tools,
		})
	}
	s.nodesMu.RUnlock()
	json.NewEncoder(w).Encode(list)
}

func (s *Server) handleLatencyMatrix(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == "POST" {
		// Anonymous public visitors never write measurements into the shared
		// matrix; with an empty key authClient alone would let them.
		if refusePublic(w, r) {
			return
		}
		// Require authentication for latency data submission to prevent data poisoning
		if !s.authClient(r) {
			writeJSONError(w, "unauthorized", 401)
			return
		}

		var entry struct {
			FromID  string  `json:"from_id"`
			ToID    string  `json:"to_id"`
			Latency float64 `json:"latency_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			writeJSONError(w, "bad request", 400)
			return
		}

		// Validate that from_id and to_id correspond to real nodes
		s.nodesMu.RLock()
		_, fromOK := s.nodes[entry.FromID]
		_, toOK := s.nodes[entry.ToID]
		s.nodesMu.RUnlock()
		if !fromOK || !toOK {
			writeJSONError(w, "invalid node ID", 400)
			return
		}
		if !validLatencyMs(entry.Latency) {
			writeJSONError(w, "invalid latency value", 400)
			return
		}

		// The legacy browser-driven measurement still works; its cells are
		// tagged source:"client" so the UI can tell them from mesh runs.
		s.recordMatrixEntry(entry.FromID, entry.ToID, entry.Latency, meshSourceClient)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
		return
	}

	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}

	if !s.requireClientAuth(w, r) {
		return
	}

	// GET: return the full matrix
	// Acquire nodesMu FIRST, then latencyMatrixMu — consistent with handleNodes DELETE
	// to prevent ABBA deadlock.
	s.nodesMu.RLock()
	type MatrixNode struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Flag     string `json:"flag"`
	}
	nodeList := make([]MatrixNode, 0)
	for _, n := range s.nodes {
		nodeList = append(nodeList, MatrixNode{ID: n.ID, Name: n.Name, Location: n.Location, Flag: n.Flag})
	}
	s.nodesMu.RUnlock()

	// Each cell is an object: {latency_ms, measured_at, source}. The top-level
	// {nodes, latency} shape is unchanged.
	matrixCopy := s.snapshotMatrix()

	json.NewEncoder(w).Encode(map[string]any{
		"nodes":   nodeList,
		"latency": matrixCopy,
	})
}

func (s *Server) broadcastNodeStatus(nodeID string, online bool) {
	s.nodesMu.RLock()
	node, exists := s.nodes[nodeID]
	if exists {
		// An older connection's delayed disconnect broadcast must not
		// overwrite the status of a replacement that is already online.
		online = node.Online
	}
	var payload any
	if exists && online {
		onlinePayload := map[string]any{
			"type":     "node_status",
			"node_id":  nodeID,
			"online":   online,
			"name":     node.Name,
			"location": node.Location,
			"flag":     node.Flag,
			"ipv4":     node.IPv4,
			"ipv6":     node.IPv6,
			"provider": node.Provider,
			"lat":      node.Lat,
			"lon":      node.Lon,
		}
		// Conditional, not unconditional: a node that reported neither field
		// produces exactly the payload it produces today, so an older agent's
		// node_status frame is byte-identical to before this slice.
		if node.Version != "" {
			onlinePayload["version"] = node.Version
		}
		if len(node.Tools) > 0 {
			onlinePayload["tools"] = node.Tools
		}
		payload = onlinePayload
	} else {
		payload = map[string]any{
			"type":    "node_status",
			"node_id": nodeID,
			"online":  online,
		}
	}
	s.nodesMu.RUnlock()

	msg, _ := json.Marshal(payload)

	// Snapshot clients under lock, then write concurrently.
	// Fire-and-forget: don't block the caller (e.g., agent read loop)
	// waiting for slow clients — each write has a 5s deadline.
	s.clientsMu.RLock()
	snapshot := make(map[*websocket.Conn]*sync.Mutex, len(s.clients))
	for c, mu := range s.clients {
		snapshot[c] = mu
	}
	s.clientsMu.RUnlock()

	for c, mu := range snapshot {
		// Bounded fan-out: block briefly if the semaphore is saturated rather
		// than spawning unbounded goroutines during a reconnect storm.
		s.broadcastSem <- struct{}{}
		go func(c *websocket.Conn, mu *sync.Mutex) {
			defer func() { <-s.broadcastSem }()
			mu.Lock()
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			c.WriteMessage(websocket.TextMessage, msg)
			c.SetWriteDeadline(time.Time{})
			mu.Unlock()
		}(c, mu)
	}
}
