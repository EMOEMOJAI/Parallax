package main

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// Cancellation should normally acknowledge immediately. An agent that never
// acknowledges is disconnected, rather than releasing an ID while it can
// still emit output for the previous command.
const orphanCancelTimeout = 30 * time.Second

type closingCommand struct {
	cancelPending bool
	terminal      bool
	timer         *time.Timer
}

func (s *Server) cancelClientCommands(conn *websocket.Conn) {
	s.cmdOwnersMu.Lock()
	if s.closingCommands == nil {
		s.closingCommands = make(map[string]*closingCommand)
	}
	pending := make(map[string]*closingCommand)
	for id, owner := range s.cmdOwners {
		if owner == conn {
			state := &closingCommand{cancelPending: true}
			s.closingCommands[id] = state
			pending[id] = state
		}
	}
	s.cmdOwnersMu.Unlock()

	for id, state := range pending {
		s.cmdNodesMu.RLock()
		nodeID, tracked := s.cmdNodes[id]
		s.cmdNodesMu.RUnlock()
		s.nodesMu.RLock()
		node := s.nodes[nodeID]
		online := node != nil && node.Online
		disconnecting := node != nil && node.disconnecting
		s.nodesMu.RUnlock()
		var writeErr error
		if tracked && online {
			payload, _ := json.Marshal(map[string]string{"id": id})
			msg, _ := json.Marshal(AgentMessage{Action: "cancel", Payload: payload})
			node.mu.Lock()
			if node.conn != nil {
				node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				writeErr = node.conn.WriteMessage(websocket.TextMessage, msg)
				node.conn.SetWriteDeadline(time.Time{})
			}
			node.mu.Unlock()
		}

		// cleanupCommand may have received done while the cancellation was queued
		// behind another agent write. Only now is it safe to release that reservation.
		s.cmdOwnersMu.Lock()
		state.cancelPending = false
		terminal := state.terminal || (!tracked && !disconnecting) || (!online && !disconnecting) || node == nil || node.conn == nil
		if !terminal && node != nil && node.conn != nil {
			state.timer = time.AfterFunc(orphanCancelTimeout, func() {
				s.cmdOwnersMu.RLock()
				current := s.closingCommands[id] == state
				if current {
					node.conn.Close()
				}
				s.cmdOwnersMu.RUnlock()
			})
		}
		if terminal {
			s.cleanupCommandLocked(id)
		}
		s.cmdOwnersMu.Unlock()
		if writeErr != nil && node != nil && node.conn != nil {
			node.conn.Close()
		}
	}
}

// writeOwnedControl revalidates a cancel or shell input after obtaining the
// agent writer. An earlier ownership snapshot can outlive its command. Once
// revalidated, keeping node.mu through the write orders this control ahead of
// any newly admitted command with the same ID on that agent. Reuse on another
// node is unaffected. No server map mutex is held during network I/O.
func (s *Server) writeOwnedControl(owner *websocket.Conn, id string, node *Node, msg []byte) {
	node.mu.Lock()
	defer node.mu.Unlock()
	s.cmdOwnersMu.RLock()
	s.cmdNodesMu.RLock()
	current := s.cmdOwners[id] == owner && s.cmdNodes[id] == node.ID
	s.cmdNodesMu.RUnlock()
	s.cmdOwnersMu.RUnlock()
	if !current || node.conn == nil {
		return
	}
	node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := node.conn.WriteMessage(websocket.TextMessage, msg)
	node.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		node.conn.Close()
	}
}
