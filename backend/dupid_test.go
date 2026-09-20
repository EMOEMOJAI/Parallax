package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// S12 — F37: the per-client concurrency cap is keyed by a client-chosen command
// ID, so reusing one ID kept countClientCommands at 1 while N agent goroutines
// ran, and a second *connection* reusing another client's ID overwrote the
// cmdOwners entry and started receiving that client's output.
//
// The fix is a global duplicate-ID refusal performed in the same cmdOwnersMu
// write-locked section as the insert, with a refused frame dropped silently.
// Three properties are load-bearing and each has a test below:
//
//  1. Global, not per-connection — the cross-connection output hijack
//     (TestS12CrossConnectionIDReuseIsRefusedAndDoesNotStealOutput) is only
//     closed by the global form.
//  2. Atomic — check and insert share one write-locked section
//     (TestS12ConcurrentDuplicateSubmissionsAdmitExactlyOne, which needs at
//     least two connections: handleClientCommand runs synchronously from the
//     per-connection read loop, so two frames on one socket can never
//     interleave and a single-connection race test would be vacuous).
//  3. Silent — no error, no done. A done would carry the ID of the
//     *still-running* command and terminate its stream in any ID-keyed client.
//
// The harnesses from client_ws_test.go (s11NewServer, s11DialClient,
// s11SendCommand, s11ReadFrame, s11ExpectError, s11ExpectSilence),
// scheduler_test.go (dialFakeAgent, waitFor, waitForNodeID) and summary_test.go
// (agentReply) are reused unchanged; only the S12-specific helpers live here.

// s12CmdOwner returns the server-side connection currently tracked as the owner
// of cmdID, or nil when the ID is not in flight. Comparing the pointer across
// two client connections is how "the owner entry was not overwritten" is
// observed without needing to identify which server conn belongs to which
// client.
func s12CmdOwner(srv *Server, cmdID string) *websocket.Conn {
	srv.cmdOwnersMu.RLock()
	defer srv.cmdOwnersMu.RUnlock()
	return srv.cmdOwners[cmdID]
}

// s12CmdNode returns the node a tracked command is bound to, or "" when it is
// not tracked. A duplicate shell_start naming a *different* node is how the
// refusal is made observable: without it the second frame rebinds a live
// session's node as well as overwriting its owner.
func s12CmdNode(srv *Server, cmdID string) string {
	srv.cmdNodesMu.RLock()
	defer srv.cmdNodesMu.RUnlock()
	return srv.cmdNodes[cmdID]
}

// s12CmdOwnersLen is the number of in-flight commands the server is tracking,
// across every connection.
func s12CmdOwnersLen(srv *Server) int {
	srv.cmdOwnersMu.RLock()
	defer srv.cmdOwnersMu.RUnlock()
	return len(srv.cmdOwners)
}

// s12OnlyClientConn returns the single registered client connection, so
// countClientCommands — which takes the *server* side of the socket — can be
// called with the connection the test's client is talking through. Fails the
// test unless exactly one client is connected.
func s12OnlyClientConn(t *testing.T, srv *Server) *websocket.Conn {
	t.Helper()
	var found *websocket.Conn
	waitFor(t, "exactly one registered client connection", 5*time.Second, func() bool {
		srv.clientsMu.RLock()
		defer srv.clientsMu.RUnlock()
		if len(srv.clients) != 1 {
			return false
		}
		for c := range srv.clients {
			found = c
		}
		return true
	})
	return found
}

// s12SendShellStart writes a shell_start frame exactly as ShellTerminal does.
func s12SendShellStart(t *testing.T, conn *websocket.Conn, nodeID, id string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"action": "shell_start", "node_id": nodeID, "id": id,
	})
	if err != nil {
		t.Fatalf("marshal shell_start: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("write shell_start: %v", err)
	}
}

// s12ExpectNoFurtherCommand asserts the agent receives no second command within
// window — the agent-side view of "the duplicate was never admitted".
//
// Its client-side counterpart is s11ExpectSilence, which every test below calls
// **once per connection and last**: a timed-out gorilla read poisons that
// connection for every later read, so a second silence check on the same socket
// would pass vacuously. Where a frame must also be shown *not* to have been
// written earlier, the ordering does the work instead — frames on one
// connection are delivered in order, so a leaked error/done would arrive before
// the frame the test does expect.
func s12ExpectNoFurtherCommand(t *testing.T, fa *fakeAgent, window time.Duration) {
	t.Helper()
	select {
	case req := <-fa.cmds:
		t.Fatalf("the duplicate reached the agent: %+v", req)
	case <-time.After(window):
	}
}

// s12Agents registers n fake agents and returns their node IDs. Used for the
// per-client cap, which needs four online nodes: maxCommandsPerNode (5) trips
// before maxCommandsPerClient (20).
func s12Agents(t *testing.T, srv *Server, ts *httptest.Server, prefix string, n int) ([]string, []*fakeAgent) {
	t.Helper()
	ids := make([]string, 0, n)
	agents := make([]*fakeAgent, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		agents = append(agents, dialFakeAgent(t, ts, name))
		ids = append(ids, waitForNodeID(t, srv, name))
	}
	return ids, agents
}

// ---- one connection: the duplicate is dropped, the original keeps running ----

func TestS12DuplicateCommandIDAdmitsExactlyOne(t *testing.T) {
	srv, ts := s11NewServer(t)
	nodeIDs, agents := s12Agents(t, srv, ts, "s12-dup-agent", 1)
	nodeID, fa := nodeIDs[0], agents[0]

	conn := s11DialClient(t, ts)
	serverConn := s12OnlyClientConn(t, srv)

	const id = "s12-duplicate-id"
	s11SendCommand(t, conn, nodeID, CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})
	if got := fa.awaitCommand(t); got.ID != id {
		t.Fatalf("agent received command %q, want %q", got.ID, id)
	}

	// The duplicate: same ID, same connection.
	s11SendCommand(t, conn, nodeID, CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})

	// It never reaches the agent.
	s12ExpectNoFurtherCommand(t, fa, 400*time.Millisecond)

	// The count is 1, not 2: the fix refuses the second frame rather than
	// re-keying it, which the slice's non-goals forbid.
	if got := srv.countClientCommands(serverConn); got != 1 {
		t.Errorf("countClientCommands = %d, want 1", got)
	}
	if got := s12CmdOwnersLen(srv); got != 1 {
		t.Errorf("cmdOwners holds %d entries, want 1", got)
	}

	// The original is still live and still routes to its owner — and the very
	// next frame on this socket is that output, which is also how "the refused
	// duplicate produced no frame at all" is proven: an error/done pair for the
	// duplicate was written (if at all) well before this output, so it would be
	// read here instead. A done above all: it would carry the still-running
	// command's ID and end its stream in any ID-keyed client.
	agentReply(t, fa, id, "output", "64 bytes from 1.1.1.1")
	frame, ok := s11ReadFrame(t, conn, 3*time.Second)
	if !ok {
		t.Fatal("the original command's output never arrived")
	}
	if frame.Type != "output" || frame.Data != "64 bytes from 1.1.1.1" {
		t.Fatalf("got frame %+v, want the original command's output — a frame for the refused duplicate leaked", frame)
	}
	// Nothing follows it either.
	s11ExpectSilence(t, conn, 400*time.Millisecond)
}

// ---- the cap is reachable only with distinct IDs ----------------------------

func TestS12CapIsReachableOnlyWithDistinctIDs(t *testing.T) {
	srv, ts := s11NewServer(t)
	// Four online nodes: maxCommandsPerNode (5) trips before
	// maxCommandsPerClient (20), so one node can never fill the client cap.
	nodeIDs, _ := s12Agents(t, srv, ts, "s12-cap-agent", 4)

	conn := s11DialClient(t, ts)
	serverConn := s12OnlyClientConn(t, srv)

	// Six frames sharing one ID take exactly one slot on the first node.
	const repeated = "s12-cap-repeated-id"
	for i := 0; i < 6; i++ {
		s11SendCommand(t, conn, nodeIDs[0], CommandRequest{ID: repeated, Type: "ping", Target: "1.1.1.1"})
	}

	// 19 distinct IDs bring the total to exactly maxCommandsPerClient: four
	// more on the first node (5 there in total) and five on each of the rest.
	sent := 0
	for i, nodeID := range nodeIDs {
		want := maxCommandsPerNode
		if i == 0 {
			want = maxCommandsPerNode - 1 // the repeated ID already holds one
		}
		for j := 0; j < want; j++ {
			s11SendCommand(t, conn, nodeID, CommandRequest{ID: fmt.Sprintf("s12-cap-distinct-%d-%d", i, j), Type: "ping", Target: "1.1.1.1"})
			sent++
		}
	}
	if sent != maxCommandsPerClient-1 {
		t.Fatalf("test setup sent %d distinct commands, want %d", sent, maxCommandsPerClient-1)
	}

	// The read loop is sequential per connection, so once the last distinct
	// frame is tracked every earlier duplicate has already been processed:
	// 25 frames, 20 slots.
	waitFor(t, "maxCommandsPerClient commands in flight", 5*time.Second, func() bool {
		return s12CmdOwnersLen(srv) == maxCommandsPerClient
	})
	if got := srv.countClientCommands(serverConn); got != maxCommandsPerClient {
		t.Fatalf("countClientCommands = %d, want %d — repeating an ID must never raise the count", got, maxCommandsPerClient)
	}

	// The 21st frame carries a distinct ID and targets a fifth, unregistered
	// node, so the per-client check (which runs before the node lookup) is what
	// rejects it. It is also the first frame this connection has been sent:
	// every duplicate above was silent.
	s11SendCommand(t, conn, "s12-cap-fifth-unregistered-node", CommandRequest{ID: "s12-cap-overflow", Type: "ping", Target: "1.1.1.1"})
	s11ExpectError(t, conn, "Too many concurrent commands")
}

// ---- two connections: no hijack of another client's output stream -----------

func TestS12CrossConnectionIDReuseIsRefusedAndDoesNotStealOutput(t *testing.T) {
	srv, ts := s11NewServer(t)
	nodeIDs, agents := s12Agents(t, srv, ts, "s12-hijack-agent", 1)
	nodeID, fa := nodeIDs[0], agents[0]

	victim := s11DialClient(t, ts)
	const id = "s12-victim-command-id"
	s11SendCommand(t, victim, nodeID, CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})
	if got := fa.awaitCommand(t); got.ID != id {
		t.Fatalf("agent received command %q, want %q", got.ID, id)
	}
	owner := s12CmdOwner(srv, id)
	if owner == nil {
		t.Fatal("the victim's command was never tracked")
	}

	// A second connection guesses the victim's ID. Per-connection refusal would
	// let this through and overwrite the owner entry; the global form does not.
	attacker := s11DialClient(t, ts)
	s11SendCommand(t, attacker, nodeID, CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})
	s12ExpectNoFurtherCommand(t, fa, 400*time.Millisecond)

	if got := s12CmdOwner(srv, id); got != owner {
		t.Fatal("the owner entry was overwritten by the second connection")
	}
	if got := s12CmdOwnersLen(srv); got != 1 {
		t.Errorf("cmdOwners holds %d entries, want 1", got)
	}

	// The victim's output still goes to the victim…
	agentReply(t, fa, id, "output", "victim-only-output")
	frame, ok := s11ReadFrame(t, victim, 3*time.Second)
	if !ok || frame.Type != "output" || frame.Data != "victim-only-output" {
		t.Fatalf("victim got frame %+v (ok=%v), want its own output", frame, ok)
	}
	// …and the attacker never receives anything at all: not the victim's
	// output, and not an error/done for its own refused frame.
	s11ExpectSilence(t, attacker, 500*time.Millisecond)
}

// ---- concurrency: check and insert are one atomic section -------------------

func TestS12ConcurrentDuplicateSubmissionsAdmitExactlyOne(t *testing.T) {
	srv, ts := s11NewServer(t)
	// Four online nodes and five contested IDs per node: 20 admitted commands
	// in total, so neither maxCommandsPerNode (5, per client per node) nor
	// maxCommandsPerClient (20) can ever reject a frame here. A cap rejection
	// would otherwise be indistinguishable from a duplicate refusal.
	nodeIDs, agents := s12Agents(t, srv, ts, "s12-race-agent", 4)

	type contested struct {
		nodeID string
		id     string
		raw    []byte
	}
	var frames []contested
	wantByNode := make(map[string]map[string]bool, len(nodeIDs))
	for i, nodeID := range nodeIDs {
		wantByNode[nodeID] = map[string]bool{}
		for j := 0; j < maxCommandsPerNode; j++ {
			id := fmt.Sprintf("s12-race-id-%d-%d", i, j)
			req := struct {
				NodeID  string         `json:"node_id"`
				Command CommandRequest `json:"command"`
			}{NodeID: nodeID, Command: CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"}}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal command: %v", err)
			}
			frames = append(frames, contested{nodeID: nodeID, id: id, raw: raw})
			wantByNode[nodeID][id] = true
		}
	}

	// Two connections, because handleClientCommand is called synchronously from
	// the per-connection read loop (client_ws.go:196-227): two frames on one
	// socket are serialized by the server and could never race. Each connection
	// gets one writer goroutine — a gorilla conn does not tolerate concurrent
	// writes on the client side either — and both walk the same ID order, so
	// every ID is submitted by both connections at nearly the same instant.
	conns := []*websocket.Conn{s11DialClient(t, ts), s11DialClient(t, ts)}
	start := make(chan struct{})
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func(i int, conn *websocket.Conn) {
			defer wg.Done()
			<-start
			for _, f := range frames {
				if err := conn.WriteMessage(websocket.TextMessage, f.raw); err != nil {
					errs[i] = err
					return
				}
			}
		}(i, conn)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("connection %d write: %v", i, err)
		}
	}

	// Every contested ID was admitted exactly once: each agent sees its own
	// five IDs, each exactly once, and nothing else.
	for i, fa := range agents {
		want := wantByNode[nodeIDs[i]]
		seen := map[string]int{}
		for len(seen) < len(want) {
			select {
			case req := <-fa.cmds:
				seen[req.ID]++
			case <-time.After(5 * time.Second):
				t.Fatalf("agent %d received %d of %d commands: %v", i, len(seen), len(want), seen)
			}
		}
		for id, n := range seen {
			if !want[id] {
				t.Errorf("agent %d received an unexpected command %q", i, id)
			}
			if n != 1 {
				t.Errorf("agent %d received %q %d times — two frames were both admitted", i, id, n)
			}
		}
		s12ExpectNoFurtherCommand(t, fa, 300*time.Millisecond)
	}
	if got, want := s12CmdOwnersLen(srv), len(frames); got != want {
		t.Errorf("cmdOwners holds %d entries, want %d", got, want)
	}
}

// ---- shell_start carries the same refusal ----------------------------------

func TestS12DuplicateShellSessionIDIsRefused(t *testing.T) {
	srv, ts := s11NewServer(t)
	// Two nodes: the duplicate names the *second* one, so admitting it would
	// rebind the live session to a different agent — an effect the test can see
	// even though a fake agent does not report shell_start frames.
	nodeIDs, agents := s12Agents(t, srv, ts, "s12-shell-agent", 2)

	conn := s11DialClient(t, ts)
	serverConn := s12OnlyClientConn(t, srv)

	const id = "s12-shell-session-id"
	s12SendShellStart(t, conn, nodeIDs[0], id)
	waitFor(t, "the shell session to be tracked", 5*time.Second, func() bool {
		return s12CmdOwner(srv, id) != nil
	})
	owner := s12CmdOwner(srv, id)

	// A second shell_start reusing the session ID is dropped silently, and so is
	// a `command` frame reusing it: cmdOwners is one map, and the refusal is
	// keyed on the map rather than on the action that created the entry. One
	// silence window covers both refused frames.
	s12SendShellStart(t, conn, nodeIDs[1], id)
	s11SendCommand(t, conn, nodeIDs[1], CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})
	s11ExpectSilence(t, conn, 500*time.Millisecond)

	// The reused `command` frame never reached either agent.
	for i, fa := range agents {
		select {
		case req := <-fa.cmds:
			t.Fatalf("agent %d ran a command reusing the live shell session ID: %+v", i, req)
		default:
		}
	}
	if got := s12CmdOwner(srv, id); got != owner {
		t.Error("the shell session's owner entry was overwritten")
	}
	if got := s12CmdNode(srv, id); got != nodeIDs[0] {
		t.Errorf("the shell session was rebound to node %q, want %q", got, nodeIDs[0])
	}
	if got := srv.countClientCommands(serverConn); got != 1 {
		t.Errorf("countClientCommands = %d, want 1", got)
	}
	if got := s12CmdOwnersLen(srv); got != 1 {
		t.Errorf("cmdOwners holds %d entries, want 1", got)
	}
}

// ---- reuse straight after teardown is admitted, deterministically ----------
//
// The teardown paths in client_ws.go delete the tracking entries *before*
// writing the final error/done pair. With the opposite order a client that
// resubmits the moment it reads `done` would be refused non-deterministically.

func TestS12ReuseAfterTeardownIsAdmitted(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)

	const id = "s12-reuse-after-teardown"
	for i := 0; i < 5; i++ {
		s11SendCommand(t, conn, "s12-unregistered-node", CommandRequest{ID: id, Type: "ping", Target: "1.1.1.1"})
		// Reading the done frame is the earliest a real client could resubmit.
		s11ExpectError(t, conn, "Node is offline or not found")
	}
}

func TestS12ShellStartReuseAfterTeardownIsAdmitted(t *testing.T) {
	_, ts := s11NewServer(t)
	conn := s11DialClient(t, ts)

	const id = "s12-shell-reuse-after-teardown"
	for i := 0; i < 5; i++ {
		s12SendShellStart(t, conn, "s12-unregistered-node", id)
		s11ExpectError(t, conn, "Node is offline")
	}
}
