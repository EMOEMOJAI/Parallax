package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestQueuedNodeBroadcastUsesCurrentMetadata(t *testing.T) {
	srv, ts := s11NewServer(t)
	client := s11DialClient(t, ts)
	serverConn := s12OnlyClientConn(t, srv)
	srv.clientsMu.RLock()
	mu := srv.clients[serverConn]
	srv.clientsMu.RUnlock()
	srv.nodesMu.Lock()
	srv.nodes["queued-node"] = &Node{ID: "queued-node", Name: "old", Online: true}
	srv.nodesMu.Unlock()
	mu.Lock()
	srv.broadcastNodeStatus("queued-node", true)
	srv.nodesMu.Lock()
	srv.nodes["queued-node"] = &Node{ID: "queued-node", Name: "new", Online: true}
	srv.nodesMu.Unlock()
	mu.Unlock()
	client.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	if frame["name"] != "new" {
		t.Fatalf("stale queued status: %s", raw)
	}
}

func TestDeletedNodeCannotBroadcastOnline(t *testing.T) {
	srv, _ := s11NewServer(t)
	var frame map[string]any
	if err := json.Unmarshal(srv.nodeStatusMessage("deleted"), &frame); err != nil {
		t.Fatal(err)
	}
	if frame["online"] != false {
		t.Fatalf("deleted node is online: %v", frame)
	}
}
