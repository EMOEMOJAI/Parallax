package main

// Server-side latency mesh.
//
// The browser used to drive the latency matrix itself: it sent a ping command
// per ordered node pair over its own WebSocket, scraped "avg" out of the text
// output with a regex, and POSTed the number back. That only worked with a tab
// open, and it could not measure IPv6-only nodes.
//
// This file moves the measurement into the server. A mesh run enumerates the
// online nodes that have a usable address, dispatches `ping count=3` for every
// ordered pair through the same agent path a browser command uses, and reads
// the latency out of the structured S2 summary (`avg_ms`) — no text parsing
// anywhere.
//
// Security note (the reason target validation lives here): for a mesh probe the
// *server* is the dispatcher, and a node's address is agent-supplied. Agent
// registration puts those fields through sanitizeString, which strips control
// characters and truncates but never parses, so an agent is free to register
// `ipv4:"evil.example.com"`. Without the net.ParseIP gate below, one hostile
// agent would make the server ping an attacker-chosen host from every node in
// the fleet, on a timer. A node whose stored addresses are not IP literals is
// skipped exactly like one with no address at all; a hostname is never resolved.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// meshDefaultIntervalSec is the tick interval when MESH_INTERVAL_SEC is
	// unset or unparsable. MESH_INTERVAL_SEC=0 disables the mesh entirely.
	meshDefaultIntervalSec = 300

	// meshMinIntervalSec floors a configured interval, mirroring
	// scheduleMinInterval. Below this the fleet would be under near-continuous
	// probe load, since one run covers every ordered pair of nodes.
	meshMinIntervalSec = 30

	// meshPerNodeConcurrency caps how many mesh probes may be in flight from
	// one source node. It is deliberately below maxCommandsPerNode (5) so a
	// mesh run never fills a node's command budget. Excess pairs wait for a
	// slot; they are never dropped.
	meshPerNodeConcurrency = 4

	// meshPingCount is the probe shape: three pings is enough for a usable
	// average without keeping the agent busy.
	meshPingCount = "count=3"

	// Matrix cell provenance.
	meshSourceMesh   = "mesh"
	meshSourceClient = "client"

	// maxLatencyMs bounds an accepted measurement. Same ceiling the legacy
	// client-side POST has always used.
	maxLatencyMs = 60000
)

// meshProbeTimeout is how long the server waits for a probe's `done` before it
// gives up, cancels the command on the agent and records the pair as failed.
// It is a package var (like scheduleTickInterval / scheduleRunTimeout /
// alertTimeout) so tests need no 30-second waits.
var meshProbeTimeout = 30 * time.Second

// meshTickInterval overrides the interval derived from MESH_INTERVAL_SEC when
// it is > 0. Tests set it; production leaves it at zero. It never re-enables a
// mesh that MESH_INTERVAL_SEC=0 disabled.
var meshTickInterval time.Duration

// matrixEntry is one latency matrix cell: the measurement plus when it was
// taken and who took it ("mesh" for a server-side run, "client" for the legacy
// browser-driven POST).
type matrixEntry struct {
	LatencyMs  float64   `json:"latency_ms"`
	MeasuredAt time.Time `json:"measured_at"`
	Source     string    `json:"source"`
}

// meshTarget is the snapshot of a node a mesh run works from: taken once under
// nodesMu, then used without the lock.
type meshTarget struct {
	id   string
	name string
	addr string // canonical IP literal, already validated
}

// meshProbe tracks one in-flight pair measurement. Its lifetime is exactly the
// lifetime of the meshRuns entry keyed by the probe's command id.
type meshProbe struct {
	fromID string
	toID   string

	mu      sync.Mutex
	summary []byte
	ok      bool

	// done is closed exactly once, when the probe reaches a terminal state
	// (`done` frame, deadline, agent disconnect, or shutdown). The dispatching
	// goroutine waits on it, so a run ends only when every probe it dispatched
	// has finished.
	doneOnce sync.Once
	done     chan struct{}
}

func (p *meshProbe) finish() { p.doneOnce.Do(func() { close(p.done) }) }

func (p *meshProbe) succeeded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ok
}

// meshStartResult is why a measurement did or did not start.
type meshStartResult int

const (
	meshStarted meshStartResult = iota
	meshBusy
	meshNotEnoughNodes
)

// ---- configuration ----------------------------------------------------------

// meshConfig reports the tick interval and whether the mesh ticker runs at all.
// MESH_INTERVAL_SEC=0 disables it entirely; an unset, negative or unparsable
// value falls back to the 300-second default.
func meshConfig() (time.Duration, bool) {
	secs := meshDefaultIntervalSec
	if raw := os.Getenv("MESH_INTERVAL_SEC"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			secs = n
		}
	}
	if secs == 0 {
		return 0, false
	}
	// Floor the interval the way schedules floor theirs (scheduleMinInterval).
	// A mesh run pings every ordered pair of nodes, so an interval of a second
	// or two would keep real WAN links under continuous probe load. Runs never
	// overlap, but without a floor they would follow each other back to back.
	if secs < meshMinIntervalSec {
		log.Printf("MESH_INTERVAL_SEC=%d is below the %d-second minimum; using %d.", secs, meshMinIntervalSec, meshMinIntervalSec)
		secs = meshMinIntervalSec
	}
	if meshTickInterval > 0 {
		return meshTickInterval, true
	}
	// Environment values can exceed time.Duration even on a 64-bit host.
	// Clamp before multiplication so NewTicker can never panic on overflow.
	if secs > scheduleMaxInterval {
		secs = scheduleMaxInterval
	}
	return time.Duration(secs) * time.Second, true
}

// ---- target selection -------------------------------------------------------

// meshNodeAddress returns the literal to ping for a node, preferring IPv4 and
// falling back to IPv6. The stored string is only usable if net.ParseIP accepts
// it — see the security note at the top of this file. The canonical form
// (ip.String()) is returned rather than the stored bytes, so what reaches an
// agent's command line is always a normalized IP literal.
func meshNodeAddress(ipv4, ipv6 string) (string, bool) {
	if ip := net.ParseIP(ipv4); ip != nil {
		return ip.String(), true
	}
	if ip := net.ParseIP(ipv6); ip != nil {
		return ip.String(), true
	}
	return "", false
}

// meshTargets snapshots every online node with a usable address. Nodes with no
// address, or with an address that is not an IP literal, are skipped silently —
// they are simply not part of the mesh.
func (s *Server) meshTargets() []meshTarget {
	s.nodesMu.RLock()
	defer s.nodesMu.RUnlock()
	out := make([]meshTarget, 0, len(s.nodes))
	for _, n := range s.nodes {
		if !n.Online {
			continue
		}
		addr, ok := meshNodeAddress(n.IPv4, n.IPv6)
		if !ok {
			continue
		}
		out = append(out, meshTarget{id: n.ID, name: n.Name, addr: addr})
	}
	return out
}

// ---- run lifecycle ----------------------------------------------------------

// tryClaimMesh takes the single mesh run slot. meshRunsMu is taken with no
// other lock held, and it also guards meshRunning / meshSkipLogged.
func (s *Server) tryClaimMesh() bool {
	s.meshRunsMu.Lock()
	defer s.meshRunsMu.Unlock()
	if s.meshRunning {
		return false
	}
	s.meshRunning = true
	s.meshSkipLogged = false
	return true
}

func (s *Server) releaseMesh() {
	s.meshRunsMu.Lock()
	s.meshRunning = false
	s.meshRunsMu.Unlock()
}

// meshInProgress reports whether a run currently owns the slot.
func (s *Server) meshInProgress() bool {
	s.meshRunsMu.RLock()
	defer s.meshRunsMu.RUnlock()
	return s.meshRunning
}

// noteMeshSkip records that a tick was skipped because a run was still going,
// and reports whether this is the first skip of that run — the log line is
// emitted at most once per run so a long run cannot flood the log.
func (s *Server) noteMeshSkip() bool {
	s.meshRunsMu.Lock()
	defer s.meshRunsMu.Unlock()
	if s.meshSkipLogged {
		return false
	}
	s.meshSkipLogged = true
	return true
}

// startMeshRun claims the run slot, builds the pair list and hands the actual
// probing to a background goroutine. It returns the number of pairs that will
// be probed, so the HTTP caller can answer immediately.
func (s *Server) startMeshRun() (int, meshStartResult) {
	if !s.tryClaimMesh() {
		return 0, meshBusy
	}
	targets := s.meshTargets()
	if len(targets) < 2 {
		s.releaseMesh()
		return 0, meshNotEnoughNodes
	}
	pairs := len(targets) * (len(targets) - 1)
	go s.meshRun(targets)
	return pairs, meshStarted
}

// meshRun probes every ordered pair and releases the run slot only once every
// dispatched probe has reached a terminal state.
func (s *Server) meshRun(targets []meshTarget) {
	defer s.releaseMesh()

	started := time.Now()
	var okCount, failCount atomic.Int64
	var wg sync.WaitGroup

	for _, from := range targets {
		wg.Add(1)
		// One bounded worker pool per source node: at most
		// meshPerNodeConcurrency probes leave a given node at a time, and a
		// pair that finds the pool busy waits its turn rather than being
		// dropped. The pool shape also keeps the goroutine count linear in the
		// fleet size instead of quadratic.
		go func(from meshTarget) {
			defer wg.Done()
			queue := make(chan meshTarget, len(targets))
			for _, to := range targets {
				if to.id != from.id {
					queue <- to
				}
			}
			close(queue)

			workers := meshPerNodeConcurrency
			if pending := len(targets) - 1; pending < workers {
				workers = pending
			}
			var pool sync.WaitGroup
			for i := 0; i < workers; i++ {
				pool.Add(1)
				go func() {
					defer pool.Done()
					for to := range queue {
						select {
						case <-s.meshShutdown:
							return
						default:
						}
						if s.meshProbePair(from, to) {
							okCount.Add(1)
						} else {
							failCount.Add(1)
						}
					}
				}()
			}
			pool.Wait()
		}(from)
	}
	wg.Wait()
	log.Printf("Mesh run finished: %d nodes, %d measured, %d failed, %s",
		len(targets), okCount.Load(), failCount.Load(), time.Since(started).Round(time.Millisecond))
}

// meshProbePair runs one ordered pair and reports whether it produced a
// measurement. It returns only when the probe is terminal.
func (s *Server) meshProbePair(from, to meshTarget) bool {
	cmdID := uuid.New().String()
	probe := &meshProbe{fromID: from.id, toID: to.id, done: make(chan struct{})}

	// Registration mirrors the scheduler's beginRun: both maps are populated
	// synchronously *before* the agent write, so the first frame the agent
	// sends back routes correctly (and passes handleAgentOutput's F33
	// node-match check). Each mutex is taken on its own.
	s.meshRunsMu.Lock()
	s.meshRuns[cmdID] = probe
	s.meshRunsMu.Unlock()

	s.cmdNodesMu.Lock()
	s.cmdNodes[cmdID] = from.id
	s.cmdNodesMu.Unlock()

	if err := s.meshSendPing(from, to, cmdID); err != nil {
		// Every failure path drops both registrations.
		s.meshReap(cmdID)
		return false
	}

	timer := time.NewTimer(meshProbeTimeout)
	defer timer.Stop()

	select {
	case <-probe.done:
		// done frame, agent disconnect, or an earlier reap.
		return probe.succeeded()
	case <-timer.C:
		// The agent still believes it is pinging; tell it to stop before the
		// registration goes away, then record the pair as failed.
		s.meshSendCancel(from.id, cmdID)
		s.meshReap(cmdID)
		return false
	case <-s.meshShutdown:
		s.meshReap(cmdID)
		return false
	}
}

// meshSendPing writes the probe command to the source node's agent connection,
// re-checking that the node is still online inside nodesMu (the agent read loop
// writes Online) and holding the connection's own mutex for the write.
func (s *Server) meshSendPing(from, to meshTarget, cmdID string) error {
	s.nodesMu.RLock()
	node, nodeOK := s.nodes[from.id]
	// Sample Online under nodesMu. The captured Node retains its connection
	// and mutex even if a reconnect later replaces the map entry.
	online := nodeOK && node.Online
	var connMu *sync.Mutex
	if online {
		connMu = node.mu
	}
	s.nodesMu.RUnlock()
	if !online {
		return fmt.Errorf("node %s is not online", from.id)
	}

	payload, _ := json.Marshal(CommandRequest{ID: cmdID, Type: "ping", Target: to.addr, Options: meshPingCount})
	agentMsg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})

	connMu.Lock()
	defer connMu.Unlock()
	if node.conn == nil {
		return fmt.Errorf("agent connection is nil")
	}
	node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := node.conn.WriteMessage(websocket.TextMessage, agentMsg)
	node.conn.SetWriteDeadline(time.Time{})
	return err
}

// meshSendCancel asks the source agent to stop a probe that outlived its
// deadline. Best effort: if the node went away there is nothing to cancel.
func (s *Server) meshSendCancel(fromID, cmdID string) {
	s.nodesMu.RLock()
	node, nodeOK := s.nodes[fromID]
	online := nodeOK && node.Online
	var connMu *sync.Mutex
	if online {
		connMu = node.mu
	}
	s.nodesMu.RUnlock()
	if !online {
		return
	}
	payload, _ := json.Marshal(map[string]string{"id": cmdID})
	cancelMsg, _ := json.Marshal(AgentMessage{Action: "cancel", Payload: payload})
	connMu.Lock()
	defer connMu.Unlock()
	if node.conn == nil {
		return
	}
	node.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	node.conn.WriteMessage(websocket.TextMessage, cancelMsg)
	node.conn.SetWriteDeadline(time.Time{})
}

// meshReap drops every registration for a mesh command id and wakes the
// goroutine waiting on the probe. It is idempotent, and it always clears
// cmdNodes and the summary dedup entry even if the probe is already gone.
func (s *Server) meshReap(cmdID string) *meshProbe {
	s.meshRunsMu.Lock()
	probe, ok := s.meshRuns[cmdID]
	if ok {
		delete(s.meshRuns, cmdID)
	}
	s.meshRunsMu.Unlock()

	s.cmdNodesMu.Lock()
	delete(s.cmdNodes, cmdID)
	s.cmdNodesMu.Unlock()

	s.forgetSummary(cmdID)

	if !ok {
		return nil
	}
	probe.finish()
	return probe
}

// ---- agent output -----------------------------------------------------------

// meshAcceptOutput is called from handleAgentOutput after scheduleAcceptOutput
// and before the browser-forwarding path. Mesh command ids have no cmdOwners
// entry, so consuming the frame here is what guarantees mesh traffic never
// reaches a browser.
func (s *Server) meshAcceptOutput(resp CommandResponse) bool {
	s.meshRunsMu.RLock()
	probe, ok := s.meshRuns[resp.ID]
	s.meshRunsMu.RUnlock()
	if !ok {
		return false
	}

	switch resp.Type {
	case "summary":
		// Already validated, sanitized and deduplicated by handleAgentOutput.
		probe.mu.Lock()
		probe.summary = []byte(resp.Data)
		probe.mu.Unlock()
	case "done":
		probe.mu.Lock()
		summary := probe.summary
		probe.mu.Unlock()
		// The value comes from the structured summary's top-level avg_ms and
		// nowhere else. At 100 % loss the agent omits avg_ms, and a failed
		// command carries no summary at all — both record a failure rather
		// than a zero, so a dead link never looks like a 0 ms link.
		loss, hasLoss := summaryLossPct(summary)
		if avg, got := summaryAvgMs(summary); parseExitOK(resp.Data) && got && validLatencyMs(avg) && (!hasLoss || loss < 100) {
			s.recordMatrixEntry(probe.fromID, probe.toID, avg, meshSourceMesh)
			probe.mu.Lock()
			probe.ok = true
			probe.mu.Unlock()
		}
		s.meshReap(resp.ID)
	}
	// "output" and "error" frames are consumed and discarded: the mesh needs
	// no raw text, and there is no browser to forward them to.
	return true
}

// meshAgentDisconnected is the terminal path for a probe whose agent died
// mid-run. Called from the agent disconnect loop for every orphaned cmdID;
// returns true when the id belonged to a mesh probe.
func (s *Server) meshAgentDisconnected(cmdID string) bool {
	return s.meshReap(cmdID) != nil
}

// ---- matrix storage ---------------------------------------------------------

// validLatencyMs rejects values that would poison the matrix or break JSON
// encoding (NaN and ±Inf are not representable in JSON).
func validLatencyMs(ms float64) bool {
	if math.IsNaN(ms) || math.IsInf(ms, 0) {
		return false
	}
	return ms >= 0 && ms <= maxLatencyMs
}

// recordMatrixEntry stores one cell. latencyMatrixMu is the only lock held.
func (s *Server) recordMatrixEntry(fromID, toID string, ms float64, source string) {
	s.latencyMatrixMu.Lock()
	defer s.latencyMatrixMu.Unlock()
	if s.latencyMatrix[fromID] == nil {
		s.latencyMatrix[fromID] = make(map[string]matrixEntry)
	}
	s.latencyMatrix[fromID][toID] = matrixEntry{
		LatencyMs:  ms,
		MeasuredAt: time.Now().UTC(),
		Source:     source,
	}
}

// snapshotMatrix deep-copies the matrix so the response can be encoded without
// holding the lock.
func (s *Server) snapshotMatrix() map[string]map[string]matrixEntry {
	s.latencyMatrixMu.RLock()
	defer s.latencyMatrixMu.RUnlock()
	out := make(map[string]map[string]matrixEntry, len(s.latencyMatrix))
	for from, targets := range s.latencyMatrix {
		inner := make(map[string]matrixEntry, len(targets))
		for to, entry := range targets {
			inner[to] = entry
		}
		out[from] = inner
	}
	return out
}

// ---- ticker -----------------------------------------------------------------

// meshTicker runs a mesh measurement every MESH_INTERVAL_SEC seconds until done
// closes. MESH_INTERVAL_SEC=0 means it returns immediately without starting a
// ticker at all.
func (s *Server) meshTicker(done <-chan struct{}) {
	interval, enabled := meshConfig()
	if !enabled {
		log.Println("Latency mesh disabled (MESH_INTERVAL_SEC=0)")
		return
	}
	log.Printf("Latency mesh enabled, interval %s", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			switch _, result := s.startMeshRun(); result {
			case meshBusy:
				// A run outlasting its interval is the normal outcome of a big
				// fleet, not an error — log it once per run, not once a tick.
				if s.noteMeshSkip() {
					log.Println("Mesh tick skipped: the previous measurement is still running")
				}
			case meshNotEnoughNodes:
				// Nothing to measure; stay quiet.
			}
		}
	}
}

// ---- HTTP -------------------------------------------------------------------

// handleLatencyMatrixMeasure triggers one server-side mesh run.
//
// An anonymous public-mode visitor may never make the fleet probe itself, so
// refusePublic runs before the key check (with an empty CLIENT_API_KEY,
// authClient alone would say yes to everyone).
func (s *Server) handleLatencyMatrixMeasure(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if refusePublic(w, r) {
		return
	}
	if !s.authClient(r) {
		writeJSONError(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		writeJSONError(w, "method not allowed", 405)
		return
	}

	pairs, result := s.startMeshRun()
	switch result {
	case meshBusy:
		writeJSONError(w, "a measurement is already running", 409)
	case meshNotEnoughNodes:
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"started": false,
			"reason":  "need at least two online nodes with an address",
		})
	default:
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{"started": true, "pairs": pairs})
	}
}
