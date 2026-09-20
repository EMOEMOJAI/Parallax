package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
	// A browser can't set request headers on a WebSocket, so it offers the key
	// as the "lg.bearer" subprotocol. Folding it into Authorization here — and
	// only here — keeps every downstream decision (requestIsPublic, authClient)
	// identical to the header and ?key= cases.
	normalizeWSCredential(r)
	wsHdr := wsBearerResponseHeader(r)

	// Session kind comes from the one shared predicate (see public.go). In
	// public mode an unauthenticated client is allowed in but tagged for
	// allowlist enforcement on every command; outside public mode a bad or
	// missing credential is refused below.
	publicSession := requestIsPublic(r)
	if !publicSession && !s.authClient(r) {
		// F19: the upgrade is accepted so the browser learns *why* it was
		// refused. An HTTP 401 on a WebSocket handshake surfaces in the browser
		// as an opaque failure, and the hook's backoff loop would then retry a
		// key that can never work. One frame, then a 4401 close, then nothing.
		conn, err := s.upgrader.Upgrade(w, r, wsHdr)
		if err != nil {
			return
		}
		defer conn.Close()
		authErr, _ := json.Marshal(map[string]string{"type": "auth_error"})
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.WriteMessage(websocket.TextMessage, authErr)
		conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(wsCloseUnauthorized, "unauthorized"),
			time.Now().Add(5*time.Second),
		)
		conn.SetWriteDeadline(time.Time{})
		return
	}

	// Public sessions are capped per rate key. The slot is taken BEFORE the
	// upgrade so a flood can't even reach the handshake, and released on every
	// exit path — including an upgrade failure (bad origin, malformed
	// handshake) — via the guard installed immediately below.
	rateK := ""
	if publicSession {
		rateK = clientRateKey(r)
		if rateK == "" {
			// No usable peer address (should not happen over TCP). Fail safe:
			// every such session shares one bucket rather than going unmetered,
			// since rateK == "" is also the "not public" sentinel below.
			rateK = "unknown"
		}
		if !s.rateAcquireConn(rateK) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many connections", 429)
			return
		}
		released := false
		release := func() {
			if released {
				return
			}
			released = true
			s.rateReleaseConn(rateK)
		}
		defer release()
	}

	conn, err := s.upgrader.Upgrade(w, r, wsHdr)
	if err != nil {
		log.Printf("Client upgrade error: %v", err)
		return
	}

	if !s.trackWebSocket(conn) {
		return
	}
	defer s.untrackWebSocket(conn)
	conn.SetReadLimit(wsReadLimitClient)

	// WebSocket-level ping/pong to detect dead client connections
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	clientMu := &sync.Mutex{}
	s.clientsMu.Lock()
	s.clients[conn] = clientMu
	s.clientsMu.Unlock()
	if publicSession {
		s.markPublic(conn)
		// Tell the browser its session kind so the UI can render the stripped
		// public-mode chrome immediately, before any commands are tried.
		hello, _ := json.Marshal(map[string]string{"type": "session_kind", "kind": "public"})
		clientMu.Lock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.WriteMessage(websocket.TextMessage, hello)
		conn.SetWriteDeadline(time.Time{})
		clientMu.Unlock()
	}

	// Send WebSocket-level pings every 30s
	clientPingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-clientPingDone:
				return
			case <-ticker.C:
				clientMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				conn.SetWriteDeadline(time.Time{})
				clientMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	defer close(clientPingDone)

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
		s.forgetPublic(conn)
	}()
	defer s.cancelClientCommands(conn)
	defer conn.Close()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// Reset read deadline on any successful read
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var generic struct {
			Action    string `json:"action"`
			NodeID    string `json:"node_id"`
			CommandID string `json:"command_id"`
		}
		if err := json.Unmarshal(msg, &generic); err != nil {
			continue
		}

		switch generic.Action {
		case "ping":
			// Client keepalive — handled by the read deadline reset above
		case "cancel":
			s.handleClientCancel(conn, generic.NodeID, generic.CommandID, rateK)
		case "shell_start":
			s.handleClientShellStart(conn, clientMu, msg, rateK)
		case "shell_input":
			s.handleClientShellInput(conn, msg)
		case "", "command":
			s.handleClientCommand(conn, clientMu, msg, rateK)
		default:
			// Ignore unknown actions
		}
	}
}

func (s *Server) handleClientCancel(conn *websocket.Conn, _ string, commandID string, rateK string) {
	if len(commandID) > 128 || strings.ContainsAny(commandID, "\n\r\t\x00") {
		return
	}
	// Public sessions spend a token per cancel too, so the action can't be used
	// as an un-metered way to make the server write to agents. Silently dropped
	// — a cancel has no error channel of its own.
	if rateK != "" && s.isPublic(conn) && !s.rateAllowPublicCommand(rateK) {
		return
	}
	// Verify the requesting client owns this command
	s.cmdOwnersMu.RLock()
	owner, ownerOK := s.cmdOwners[commandID]
	s.cmdOwnersMu.RUnlock()
	if !ownerOK || owner != conn {
		return
	}

	// Use the server-tracked node ID instead of trusting the client-supplied value
	s.cmdNodesMu.RLock()
	nodeID, nodeOK := s.cmdNodes[commandID]
	s.cmdNodesMu.RUnlock()
	if !nodeOK {
		return
	}

	s.nodesMu.RLock()
	node, ok := s.nodes[nodeID]
	online := ok && node.Online
	s.nodesMu.RUnlock()
	if online {
		cancelPayload, _ := json.Marshal(map[string]string{"id": commandID})
		cancelMsg, _ := json.Marshal(AgentMessage{Action: "cancel", Payload: cancelPayload})
		s.writeOwnedControl(conn, commandID, node, cancelMsg)
		// Don't delete cmdOwners/cmdNodes here — let the agent's "done" response
		// clean them up via handleAgentOutput so the client receives the final message.
	}
	// Offline nodes retire commands through their disconnect terminal path.
	// Removing ownership here could race that path's old error/done delivery.
}

func (s *Server) handleClientShellStart(conn *websocket.Conn, clientMu *sync.Mutex, msg []byte, rateK string) {
	var shellReq struct {
		NodeID string `json:"node_id"`
		ID     string `json:"id"`
		Cols   uint16 `json:"cols,omitempty"`
		Rows   uint16 `json:"rows,omitempty"`
	}
	if err := json.Unmarshal(msg, &shellReq); err != nil {
		return
	}
	if shellReq.ID == "" {
		shellReq.ID = uuid.New().String()
	}

	// Validate node_id and session ID length
	if len(shellReq.NodeID) > 128 {
		return
	}
	if len(shellReq.ID) > 128 {
		return
	}

	// Reject IDs with control characters to prevent log injection
	if strings.ContainsAny(shellReq.NodeID, "\n\r\t\x00") || strings.ContainsAny(shellReq.ID, "\n\r\t\x00") {
		return
	}
	if s.commandIDInFlight(shellReq.ID) {
		return
	}

	// Public sessions can never open a shell — that's full-host RCE. For new
	// IDs, charge the rate bucket before refusal and before any map insert.
	if s.isPublic(conn) {
		if rateK != "" && !s.rateAllowPublicCommand(rateK) {
			sendErrorAndDone(conn, clientMu, shellReq.ID, shellReq.NodeID, publicRateLimitMessage)
			return
		}
		sendErrorAndDone(conn, clientMu, shellReq.ID, shellReq.NodeID, "Public mode: interactive shell is not allowed")
		return
	}

	// Enforce per-client concurrency limits — global and per-node.
	if s.countClientCommands(conn) >= maxCommandsPerClient {
		sendErrorAndDone(conn, clientMu, shellReq.ID, "", "Too many concurrent commands")
		return
	}
	if s.countClientCommandsOnNode(conn, shellReq.NodeID) >= maxCommandsPerNode {
		sendErrorAndDone(conn, clientMu, shellReq.ID, "", fmt.Sprintf("Too many concurrent commands on this node (max %d)", maxCommandsPerNode))
		return
	}

	// F37 — refuse an ID that is already tracked in cmdOwners, whoever owns it.
	// The check and the insert share one cmdOwnersMu write-locked section:
	// check-then-insert races, and an RWMutex cannot be upgraded from RLock.
	// That is also why the two cap counts above stay *outside* this section —
	// countClientCommands / countClientCommandsOnNode must be called without
	// the lock (util.go:113-114), so the 20/5 caps remain non-atomic exactly as
	// before.
	//
	// The predicate is global, not per-connection: cmdOwners is a single map
	// keyed by ID, so a second connection reusing another client's ID would
	// otherwise overwrite the owner entry and start receiving that client's
	// output. The global form closes that hijack as well as the cap bypass.
	//
	// A refused duplicate is dropped silently — no error, no done — matching
	// the ID-validation returns above. A done would carry the ID of the
	// *still-running* command and so terminate its stream in any ID-keyed
	// client. Nothing leaks: a refused frame never took a slot.
	s.cmdOwnersMu.Lock()
	if _, inFlight := s.cmdOwners[shellReq.ID]; inFlight {
		s.cmdOwnersMu.Unlock()
		return
	}
	s.cmdOwners[shellReq.ID] = conn
	s.cmdOwnersMu.Unlock()

	s.cmdNodesMu.Lock()
	s.cmdNodes[shellReq.ID] = shellReq.NodeID
	s.cmdNodesMu.Unlock()

	s.nodesMu.RLock()
	node, ok := s.nodes[shellReq.NodeID]
	online := ok && node.Online
	s.nodesMu.RUnlock()
	if !online {
		// Delete the tracking entries *before* writing the final frame. With
		// the duplicate-ID refusal above, a client that reuses an ID the moment
		// it reads `done` would otherwise be refused non-deterministically.
		// sendToCommandOwner follows the same ordering after snapshotting the
		// recipient for normal agent completion and disconnect messages.
		s.cleanupCommand(shellReq.ID)
		sendErrorAndDone(conn, clientMu, shellReq.ID, "", "Node is offline")
		return
	}

	log.Printf("Shell session started: %s on node %s by %s", shellReq.ID, shellReq.NodeID, conn.RemoteAddr())

	// Optional dimensions preserve older clients; the agent supplies defaults
	// for absent/zero fields and applies its PTY size bounds.
	payload, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Cols uint16 `json:"cols,omitempty"`
		Rows uint16 `json:"rows,omitempty"`
	}{ID: shellReq.ID, Cols: shellReq.Cols, Rows: shellReq.Rows})
	agentMsg, _ := json.Marshal(AgentMessage{Action: "shell_start", Payload: payload})
	node.mu.Lock()
	var writeErr error
	if node.conn != nil {
		node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		writeErr = node.conn.WriteMessage(websocket.TextMessage, agentMsg)
		node.conn.SetWriteDeadline(time.Time{})
	} else {
		writeErr = fmt.Errorf("agent connection is nil")
	}
	node.mu.Unlock()

	if writeErr != nil {
		log.Printf("Error sending shell_start to agent %s: %v", shellReq.NodeID, writeErr)
		// Delete before the final frame, as on the offline path above.
		s.cleanupCommand(shellReq.ID)
		sendErrorAndDone(conn, clientMu, shellReq.ID, "", "Failed to start shell on agent")
	}
}

func (s *Server) handleClientShellInput(conn *websocket.Conn, msg []byte) {
	// A public session can never own a shell (shell_start is refused for it), so
	// its shell_input can only be a probe for someone else's session id. Drop it
	// before any map lookup — there is nothing to charge a token for.
	if s.isPublic(conn) {
		return
	}
	var shellInput struct {
		NodeID string          `json:"node_id"`
		ID     string          `json:"id"`
		Input  json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(msg, &shellInput); err != nil {
		return
	}

	// Verify the requesting client owns this shell session
	if len(shellInput.ID) > 128 || strings.ContainsAny(shellInput.ID, "\n\r\t\x00") {
		return
	}
	s.cmdOwnersMu.RLock()
	owner, ownerOK := s.cmdOwners[shellInput.ID]
	s.cmdOwnersMu.RUnlock()
	if !ownerOK || owner != conn {
		return
	}

	// Use server-tracked node ID instead of trusting client-supplied value
	s.cmdNodesMu.RLock()
	nodeID, nodeOK := s.cmdNodes[shellInput.ID]
	s.cmdNodesMu.RUnlock()
	if !nodeOK {
		return
	}

	s.nodesMu.RLock()
	node, ok := s.nodes[nodeID]
	online := ok && node.Online
	s.nodesMu.RUnlock()
	if online {
		payload, _ := json.Marshal(map[string]any{
			"id":    shellInput.ID,
			"input": json.RawMessage(shellInput.Input),
		})
		agentMsg, _ := json.Marshal(AgentMessage{Action: "shell_input", Payload: payload})
		s.writeOwnedControl(conn, shellInput.ID, node, agentMsg)
	}
}

// publicRateLimitMessage is what a throttled public session sees. Deliberately
// vague about the limit so it isn't a tuning oracle.
const publicRateLimitMessage = "Public mode: rate limit exceeded, slow down"

// commandIDInFlight runs before every rejection that could send error/done.
// A replay must not end the original stream, including at a concurrency cap
// or with invalid fields. Admission still checks and inserts atomically below
// because other connections may submit the same ID after this snapshot.
func (s *Server) commandIDInFlight(id string) bool {
	s.cmdOwnersMu.RLock()
	defer s.cmdOwnersMu.RUnlock()
	_, exists := s.cmdOwners[id]
	return exists
}

func (s *Server) handleClientCommand(conn *websocket.Conn, clientMu *sync.Mutex, msg []byte, rateK string) {
	var req struct {
		NodeID  string         `json:"node_id"`
		Command CommandRequest `json:"command"`
	}
	if err := json.Unmarshal(msg, &req); err != nil {
		return
	}

	if req.Command.ID == "" {
		req.Command.ID = uuid.New().String()
	}

	// Validate node_id and command ID length to prevent map key abuse
	if len(req.NodeID) > 128 {
		return
	}
	if len(req.Command.ID) > 128 {
		return
	}
	if s.commandIDInFlight(req.Command.ID) {
		return
	}

	// Reject node_id with control characters to prevent log injection
	if strings.ContainsAny(req.NodeID, "\n\r\t\x00") {
		return
	}

	// Validate command fields to prevent abuse
	if len(req.Command.Type) > 32 || len(req.Command.Target) > 1024 || len(req.Command.Options) > 512 {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Command fields too long")
		return
	}

	// Server-side command type whitelist — "shell" type is not allowed here;
	// interactive shells use the dedicated shell_start action instead.
	if !allowedCommandTypes[req.Command.Type] {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, fmt.Sprintf("Command type %q is not allowed", req.Command.Type))
		return
	}

	// Public-mode connections are limited to a narrower command set + a
	// hardcoded target allowlist. Both lists come from env at startup.
	if s.isPublic(conn) {
		// Token bucket first: it must be charged before any map insert, and a
		// rejected command should cost the sender something either way.
		if rateK != "" && !s.rateAllowPublicCommand(rateK) {
			sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, publicRateLimitMessage)
			return
		}
		if !publicCommandAllowed(req.Command.Type) {
			sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, fmt.Sprintf("Public mode: command %q is not allowed", req.Command.Type))
			return
		}
		if !publicTargetAllowed(req.Command.Target) {
			sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Public mode: target not in allowlist")
			return
		}
		// Options are rewritten, not validated: only -4 / -6 / count=N survive,
		// so nothing a public visitor sends can reach an agent's argv builder.
		req.Command.Options = filterPublicOptions(req.Command.Options)
	}

	// Reject targets with control characters to prevent log injection and command confusion
	if strings.ContainsAny(req.Command.Target, "\n\r\t\x00") {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Target contains invalid characters")
		return
	}

	// Reject options with control characters for consistency
	if strings.ContainsAny(req.Command.Options, "\n\r\t\x00") {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Options contain invalid characters")
		return
	}

	// Reject command IDs with control characters to prevent log injection
	if strings.ContainsAny(req.Command.ID, "\n\r\t\x00") {
		return
	}

	// Targets may contain URL credentials or signed query parameters.
	log.Printf("Command: %s (id=%q) on node %q by %s", req.Command.Type, req.Command.ID, req.NodeID, conn.RemoteAddr())

	// Enforce per-client concurrency limit to prevent resource exhaustion.
	// Global cap protects the server; per-node cap protects a single agent
	// from being saturated by one client.
	if s.countClientCommands(conn) >= maxCommandsPerClient {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Too many concurrent commands")
		return
	}
	if s.countClientCommandsOnNode(conn, req.NodeID) >= maxCommandsPerNode {
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, fmt.Sprintf("Too many concurrent commands on this node (max %d)", maxCommandsPerNode))
		return
	}

	// F37 — the same global duplicate-ID refusal as in handleClientShellStart:
	// check and insert in one cmdOwnersMu write-locked section, cap counts
	// deliberately left outside it, a duplicate dropped silently. See the
	// comment there for why each of those three is the way it is. Without this,
	// a client that reuses one ID keeps countClientCommands at 1 while N agent
	// goroutines run, and a second connection reusing another client's ID takes
	// over its output stream.
	s.cmdOwnersMu.Lock()
	if _, inFlight := s.cmdOwners[req.Command.ID]; inFlight {
		s.cmdOwnersMu.Unlock()
		return
	}
	s.cmdOwners[req.Command.ID] = conn
	s.cmdOwnersMu.Unlock()

	s.cmdNodesMu.Lock()
	s.cmdNodes[req.Command.ID] = req.NodeID
	s.cmdNodesMu.Unlock()

	s.nodesMu.RLock()
	node, ok := s.nodes[req.NodeID]
	online := ok && node.Online
	s.nodesMu.RUnlock()

	if !online {
		// Delete before the final frame, so reuse of this ID right after `done`
		// is deterministically admitted. See handleClientShellStart.
		s.cleanupCommand(req.Command.ID)
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Node is offline or not found")
		return
	}

	payload, _ := json.Marshal(req.Command)
	agentMsg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})

	node.mu.Lock()
	var err error
	if node.conn != nil {
		node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err = node.conn.WriteMessage(websocket.TextMessage, agentMsg)
		node.conn.SetWriteDeadline(time.Time{})
	} else {
		err = fmt.Errorf("agent connection is nil")
	}
	node.mu.Unlock()

	if err != nil {
		log.Printf("Error sending command to agent %s: %v", req.NodeID, err)
		// Delete before the final frame, as on the offline path above.
		s.cleanupCommand(req.Command.ID)
		sendErrorAndDone(conn, clientMu, req.Command.ID, req.NodeID, "Failed to send command to agent")
	}
}

func (s *Server) sendToCommandOwner(resp CommandResponse) {
	s.cmdOwnersMu.RLock()
	conn, ok := s.cmdOwners[resp.ID]
	s.cmdOwnersMu.RUnlock()

	// Snapshot the recipient first, then retire the command before delivering
	// done. A client may reuse the ID as soon as it receives that frame.
	if resp.Type == "done" {
		s.cleanupCommand(resp.ID)
	}
	if !ok {
		return
	}

	data, _ := json.Marshal(resp)

	s.clientsMu.RLock()
	mu, exists := s.clients[conn]
	s.clientsMu.RUnlock()

	if exists {
		mu.Lock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := conn.WriteMessage(websocket.TextMessage, data)
		conn.SetWriteDeadline(time.Time{})
		mu.Unlock()
		if err != nil {
			// Closing wakes the reader, whose disconnect cleanup cancels every
			// owned command and removes all routing/dedup registrations.
			conn.Close()
		}
	}
}
