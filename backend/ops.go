package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Register atomically with the shutdown gate so an upgrade racing a signal
// either belongs to the drain or is closed immediately. Wait begins only once
// the gate prevents any further WaitGroup additions.
func (s *Server) trackWebSocket(conn *websocket.Conn) bool {
	s.webSocketsMu.Lock()
	defer s.webSocketsMu.Unlock()
	if s.webSocketsClosing {
		conn.Close()
		return false
	}
	s.webSockets[conn] = struct{}{}
	s.webSocketsWG.Add(1)
	return true
}

func (s *Server) untrackWebSocket(conn *websocket.Conn) {
	s.webSocketsMu.Lock()
	delete(s.webSockets, conn)
	s.webSocketsMu.Unlock()
	s.webSocketsWG.Done()
}

func (s *Server) closeAllWebSockets() {
	s.webSocketsMu.Lock()
	s.webSocketsClosing = true
	sockets := make([]*websocket.Conn, 0, len(s.webSockets))
	for conn := range s.webSockets {
		sockets = append(sockets, conn)
	}
	s.webSocketsMu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
	for _, conn := range sockets {
		// Gorilla allows WriteControl and Close concurrently with every method.
		// Taking a data-write mutex here could exceed the shutdown deadline.
		conn.WriteControl(websocket.CloseMessage, closeMsg, deadline)
		conn.Close()
	}
}

func (s *Server) shutdown(ctx context.Context, httpSrv *http.Server) {
	s.closeAllWebSockets()
	if err := httpSrv.Shutdown(ctx); err != nil {
		// Force-close incomplete bodies after the grace deadline, waking decoders.
		httpSrv.Close()
	}
	wsDone := make(chan struct{})
	go func() { s.webSocketsWG.Wait(); close(wsDone) }()
	select {
	case <-wsDone:
	case <-ctx.Done():
		log.Println("WebSocket shutdown deadline reached")
	}
	// HTTP mutations and normal socket terminal-state updates have drained.
	// Persist the final snapshot synchronously even if a trailing writer already
	// claimed saveDirty. Its flag can be clear while disk I/O is still in flight;
	// persistSchedules serializes this write with every earlier writer.
	s.persistSchedules(schedulesFilePath())
}

// handleHealthz is a lightweight liveness probe. It does no work — its purpose
// is to reflect that the process is running and the listener is accepting.
// Distinct from /api/version (which fetches build metadata).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleMetrics exposes Prometheus-format metrics. Hand-rolled text format
// (not text/plain version=0.0.4 spec, but the simpler v1 form most scrapers
// accept) avoids pulling in the prometheus client library for ~10 metrics.
//
// Auth, one rule: the per-schedule gauges — an inventory of what this
// deployment probes (schedule ids, node names, commands, probe targets) — are
// served **only when METRICS_TOKEN is set and matched**. With METRICS_TOKEN set
// the token is required for the whole endpoint (Authorization: Bearer <token>
// or ?key=) and the output is complete; with METRICS_TOKEN empty the endpoint
// stays open — no new 401 — but the per-schedule gauges are withheld from
// every request, credentialed or not.
//
// The client key is deliberately *not* an alternative credential for them
// (S12): authClient returns true when CLIENT_API_KEY is empty, so keying the
// inventory on it handed the schedule list to any unauthenticated scrape of a
// keyless deployment — this fleet's default — that handleSchedulesList refuses.
// Process-level metrics stay open and unchanged. writeScheduleMetrics below is
// the sole emission path for the per-schedule series, so this is the only gate
// needed.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	includeSchedules := false
	if tok := os.Getenv("METRICS_TOKEN"); tok != "" {
		if subtle.ConstantTimeCompare([]byte(extractBearerKey(r)), []byte(tok)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		includeSchedules = true
	}

	// Snapshot the relevant counters under their respective locks. Each lock
	// is held for as little time as possible.
	s.nodesMu.RLock()
	nodesTotal := len(s.nodes)
	nodesOnline := 0
	for _, n := range s.nodes {
		if n.Online {
			nodesOnline++
		}
	}
	s.nodesMu.RUnlock()

	s.clientsMu.RLock()
	clientsConnected := len(s.clients)
	s.clientsMu.RUnlock()

	s.cmdOwnersMu.RLock()
	commandsInFlight := len(s.cmdOwners)
	s.cmdOwnersMu.RUnlock()

	s.geoCacheMu.Lock()
	geoCacheEntries := len(s.geoCache)
	s.geoCacheMu.Unlock()

	uptime := time.Since(s.startTime).Seconds()
	broadcastInFlight := len(s.broadcastSem)

	// Histogram snapshot for HTTP request latency.
	s.httpLatencyMu.Lock()
	hCount := s.httpLatencyCount
	hSum := s.httpLatencySum
	hBuckets := s.httpLatencyBuckets
	s.httpLatencyMu.Unlock()

	// build_info encodes labels but the value itself is always 1 — the standard
	// Prometheus pattern for static info gauges.
	buildLabels := fmt.Sprintf(`go_version=%q,revision=%q,modified=%q`,
		s.buildInfo.GoVersion, s.buildInfo.Revision, fmt.Sprintf("%t", s.buildInfo.Modified))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, `# HELP lookingglass_uptime_seconds Time since server start.
# TYPE lookingglass_uptime_seconds gauge
lookingglass_uptime_seconds %f
# HELP lookingglass_nodes_total Total registered agent nodes.
# TYPE lookingglass_nodes_total gauge
lookingglass_nodes_total %d
# HELP lookingglass_nodes_online Currently online agent nodes.
# TYPE lookingglass_nodes_online gauge
lookingglass_nodes_online %d
# HELP lookingglass_clients_connected Active browser WebSocket clients.
# TYPE lookingglass_clients_connected gauge
lookingglass_clients_connected %d
# HELP lookingglass_commands_in_flight Commands currently running on agents.
# TYPE lookingglass_commands_in_flight gauge
lookingglass_commands_in_flight %d
# HELP lookingglass_geoip_cache_entries Number of entries in the geo cache.
# TYPE lookingglass_geoip_cache_entries gauge
lookingglass_geoip_cache_entries %d
# HELP lookingglass_geoip_lookups_total Total geo lookups attempted.
# TYPE lookingglass_geoip_lookups_total counter
lookingglass_geoip_lookups_total %d
# HELP lookingglass_geoip_cache_hits_total Geo lookups served from cache.
# TYPE lookingglass_geoip_cache_hits_total counter
lookingglass_geoip_cache_hits_total %d
# HELP lookingglass_geoip_rate_limited_total Geo lookups denied by rate limiter.
# TYPE lookingglass_geoip_rate_limited_total counter
lookingglass_geoip_rate_limited_total %d
# HELP lookingglass_broadcast_in_flight Concurrent broadcast fan-out goroutines.
# TYPE lookingglass_broadcast_in_flight gauge
lookingglass_broadcast_in_flight %d
# HELP lookingglass_build_info Build metadata (value is always 1).
# TYPE lookingglass_build_info gauge
lookingglass_build_info{%s} 1
`,
		uptime,
		nodesTotal,
		nodesOnline,
		clientsConnected,
		commandsInFlight,
		geoCacheEntries,
		s.geoLookupsTotal.Load(),
		s.geoCacheHitsTotal.Load(),
		s.geoRateLimitedTotal.Load(),
		broadcastInFlight,
		buildLabels,
	)

	// Per-IP rate-limit table size, so an operator can see the map approaching
	// its 50 000-entry cap (i.e. a key-churning flood) before eviction starts.
	fmt.Fprintf(w, `# HELP lookingglass_ratelimit_entries Tracked per-IP rate-limit entries.
# TYPE lookingglass_ratelimit_entries gauge
lookingglass_ratelimit_entries %d
`, s.rateEntryCount())

	// HTTP request latency histogram. Format follows the Prometheus
	// histogram convention: one _bucket sample per le boundary, plus
	// _bucket{le="+Inf"} == _count, plus _sum.
	fmt.Fprintf(w, `# HELP lookingglass_http_request_duration_seconds Latency of HTTP requests served by the API + static handlers.
# TYPE lookingglass_http_request_duration_seconds histogram
`)
	for i, le := range httpLatencyBucketBounds {
		fmt.Fprintf(w, "lookingglass_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", le, hBuckets[i])
	}
	fmt.Fprintf(w, "lookingglass_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", hCount)
	fmt.Fprintf(w, "lookingglass_http_request_duration_seconds_sum %f\n", hSum)
	fmt.Fprintf(w, "lookingglass_http_request_duration_seconds_count %d\n", hCount)

	// Alerts that never reached the webhook because the dispatcher queue was
	// full. Aggregate (no labels), so it is safe for any scraper to see.
	fmt.Fprintf(w, `# HELP lookingglass_alerts_dropped_total Webhook alerts dropped because the dispatch queue was full.
# TYPE lookingglass_alerts_dropped_total counter
lookingglass_alerts_dropped_total %d
`, s.alerts.droppedTotal())

	if includeSchedules {
		s.writeScheduleMetrics(w)
	}
}

// metricStatusCodes is the total encoding of Schedule.LastStatus. Every value
// the scheduler can write appears here; a status that is not in the map (only
// "" today — never run, or reset by loadSchedules) produces no sample at all,
// so a gap in the series means "no data", never "healthy".
var metricStatusCodes = map[string]int{
	"ok":            0,
	"degraded":      1,
	"error":         2,
	"agent_offline": 3,
	"running":       4,
}

// metricsLabelValue makes one label value safe for the exposition format.
//
// Truncation happens *first* and escaping second: escaping first could cut a
// backslash escape in half at the 64-rune boundary and corrupt the rest of the
// page for every scraper.
func metricsLabelValue(v string) string {
	v = truncateRunes(v, 64)
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// writeScheduleMetrics emits the three per-schedule gauges. Cardinality is
// bounded by scheduleMaxCount (100 schedules × 3 series).
func (s *Server) writeScheduleMetrics(w http.ResponseWriter) {
	metrics := s.scheduleMetrics()
	fmt.Fprint(w, `# HELP lookingglass_probe_rtt_ms Average round-trip time of the last scheduled probe run (ping only).
# TYPE lookingglass_probe_rtt_ms gauge
# HELP lookingglass_probe_loss_pct Packet loss of the last scheduled probe run.
# TYPE lookingglass_probe_loss_pct gauge
# HELP lookingglass_probe_status Status of the last scheduled probe run (0=ok 1=degraded 2=error 3=agent_offline 4=running).
# TYPE lookingglass_probe_status gauge
`)
	for _, m := range metrics {
		code, known := metricStatusCodes[m.Status]
		if !known {
			// Never run yet: emit nothing rather than a misleading zero.
			continue
		}
		labels := fmt.Sprintf(`schedule_id="%s",node="%s",command="%s",target="%s"`,
			metricsLabelValue(m.ID),
			metricsLabelValue(m.Node),
			metricsLabelValue(m.Command),
			metricsLabelValue(m.Target))
		if m.RttMs != nil {
			fmt.Fprintf(w, "lookingglass_probe_rtt_ms{%s} %g\n", labels, *m.RttMs)
		}
		if m.LossPct != nil {
			fmt.Fprintf(w, "lookingglass_probe_loss_pct{%s} %g\n", labels, *m.LossPct)
		}
		fmt.Fprintf(w, "lookingglass_probe_status{%s} %d\n", labels, code)
	}
}

// handleVersion exposes the build VCS info plus uptime for ops health checks.
// Unauthenticated by design — none of the fields are sensitive.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	resp := map[string]any{
		"go_version": s.buildInfo.GoVersion,
		"revision":   s.buildInfo.Revision,
		"modified":   s.buildInfo.Modified,
		"build_time": s.buildInfo.BuildTime,
		"uptime_sec": int(time.Since(s.startTime).Seconds()),
	}
	json.NewEncoder(w).Encode(resp)
}
