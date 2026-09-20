package main

import (
	"container/list"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ── Server encapsulates all shared state ──

type Server struct {
	webSocketsMu      sync.Mutex
	webSockets        map[*websocket.Conn]struct{}
	webSocketsClosing bool
	webSocketsWG      sync.WaitGroup
	nodes             map[string]*Node
	nodesMu           sync.RWMutex

	cmdOwners   map[string]*websocket.Conn
	cmdOwnersMu sync.RWMutex
	// Retain IDs while disconnected owners await cancellation acknowledgment.
	closingCommands map[string]*closingCommand

	// cmdNodes tracks which node each command was sent to, for cleanup on agent disconnect
	cmdNodes   map[string]string
	cmdNodesMu sync.RWMutex

	clients   map[*websocket.Conn]*sync.Mutex
	clientsMu sync.RWMutex
	// Tracks which client connections are anonymous "public mode" sessions
	// so handleClientCommand can apply the allowlist filter. A nil value just
	// means "this conn was not authenticated and PUBLIC_MODE was on at connect".
	publicClients   map[*websocket.Conn]struct{}
	publicClientsMu sync.RWMutex

	// LRU-ordered geo cache. geoCache maps IP → list element holding *geoCacheEntry.
	// geoCacheList is ordered most-recent-at-front, oldest-at-back, so eviction
	// is O(1) under cache pressure (vs the previous O(n) two-pass scan).
	geoCache     map[string]*list.Element
	geoCacheList *list.List
	geoCacheMu   sync.Mutex

	// Per-source-IP token buckets for rate-limiting outbound geoip lookups.
	// Prevents a single client from exhausting ip-api.com's quota or saturating
	// the geoClient pool with slow upstream calls.
	geoRateMu      sync.Mutex
	geoRateBuckets map[string]*ipRateBucket

	// Per-rate-key state for public-mode limits (connection slots, command
	// token bucket, run-create quota). Declared here because Go wants all
	// fields in one place; every operation on them lives in ratelimit.go, and
	// rateMu is never held together with any other mutex in this struct.
	rateMu      sync.Mutex
	rateEntries map[string]*rateEntry

	// RDAP responses are cached separately from geoip because the upstream
	// (rdap.org) has different cache semantics and a 6h TTL is reasonable
	// for AS / abuse contact data that rarely changes.
	rdapCache   map[string]*rdapEntry
	rdapCacheMu sync.Mutex

	// Saved command runs for shareable permalinks. In-memory only — losing
	// these on restart is acceptable; they're convenience snapshots, not
	// authoritative records.
	runStore   map[string]*runRecord
	runStoreMu sync.Mutex

	// Scheduled probes: in-memory + JSON-persisted recurring commands.
	// scheduleRuns maps the cmdID assigned to a scheduled execution back to
	// its Schedule so handleAgentOutput can route output to the schedule's
	// buffer instead of trying to forward it to a nonexistent browser owner.
	schedules map[string]*Schedule
	// Failed startup loads must never be overwritten by an empty snapshot.
	scheduleLoadFailed atomic.Bool
	schedulesMu        sync.RWMutex
	scheduleRuns       map[string]*Schedule
	scheduleRunsMu     sync.RWMutex

	// Coalescing state for saveSchedules (leading edge + one trailing write
	// per scheduleSaveInterval). It lives on Server rather than in a package
	// var so each test server starts from a clean slate and its first save is
	// always synchronous. saveWriteMu serializes the actual file writes.
	saveMu       sync.Mutex
	saveLastAt   time.Time
	saveDirty    bool
	saveTrailing bool
	savePath     string
	saveWriteMu  sync.Mutex

	// Webhook alerting for scheduled probes. Owns its own mutex and its
	// dispatcher goroutine; see alerts.go.
	alerts *alertManager

	// summarySeen deduplicates structured probe summaries: an agent gets to
	// send exactly one accepted `summary` per command id. Entries are added in
	// acceptSummary and removed at every teardown point — cleanupCommand,
	// clearRunRegistration and the agent-disconnect loop — so no path leaks
	// one. Cleanup may take summarySeenMu under cmdOwnersMu; no path acquires
	// cmdOwnersMu while holding summarySeenMu.
	summarySeen   map[string]struct{}
	summarySeenMu sync.Mutex

	latencyMatrix   map[string]map[string]matrixEntry
	latencyMatrixMu sync.RWMutex

	// Server-side latency mesh (mesh.go). meshRuns maps the cmdID of an
	// in-flight mesh probe to its state, exactly as scheduleRuns does for a
	// scheduled run, so handleAgentOutput can route the probe's frames without
	// a browser owner. meshRunsMu also guards meshRunning (only one
	// measurement at a time) and meshSkipLogged (one "tick skipped" line per
	// run); it is taken with no other lock held.
	meshRuns       map[string]*meshProbe
	meshRunsMu     sync.RWMutex
	meshRunning    bool
	meshSkipLogged bool
	// meshShutdown is the process-wide shutdown channel, assigned once in main
	// before any goroutine starts. In-flight probes abandon their wait when it
	// closes. It stays nil in tests that never shut down, and a nil channel
	// simply never fires in a select.
	meshShutdown <-chan struct{}

	// Semaphore that caps concurrent broadcastNodeStatus fan-out goroutines so
	// a reconnect storm cannot spawn an unbounded number of writers.
	broadcastSem chan struct{}

	// Captured at startup for /api/version.
	startTime time.Time
	buildInfo versionInfo

	// Prometheus-style counters. Atomic so /metrics can read them without
	// taking the geoCache or geoRate mutexes.
	geoLookupsTotal     atomic.Int64
	geoCacheHitsTotal   atomic.Int64
	geoRateLimitedTotal atomic.Int64

	// HTTP request latency histogram. Buckets are fixed boundaries; each
	// counter is incremented for every bucket whose le >= duration. The
	// _sum and _count fields produce p_avg and rate-of-requests.
	httpLatencyMu      sync.Mutex
	httpLatencyBuckets [len(httpLatencyBucketBounds)]uint64
	httpLatencyCount   uint64
	httpLatencySum     float64

	agentAPIKey string
	geoClient   *http.Client
	upgrader    websocket.Upgrader
}

// ipRateBucket is a tiny per-IP token bucket for the geoip endpoint.
type ipRateBucket struct {
	tokens   float64
	lastFill time.Time
}

// versionInfo is exposed via /api/version.
type versionInfo struct {
	GoVersion string `json:"go_version"`
	Revision  string `json:"revision"`
	Modified  bool   `json:"modified"`
	BuildTime string `json:"build_time,omitempty"`
}

const (
	// Allow ~10 requests/sec sustained per source IP, with a small burst.
	geoRateRefillPerSec = 10.0
	geoRateBurst        = 30.0
	// Cap fan-out fan-out goroutines at the smaller of (4×NumCPU, 256).
	broadcastConcurrencyMin = 16
	broadcastConcurrencyMax = 256
)

// Histogram bucket boundaries (seconds). Standard Prometheus default-ish set,
// covering everything from a fast file-server response to a slow geoip lookup.
var httpLatencyBucketBounds = [...]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

const (
	maxRequestBodySize    = 1 << 20 // 1 MB
	maxGeoIPBatchSize     = 50
	geoCacheMaxSize       = 10000
	geoCacheTTL           = 1 * time.Hour
	wsReadLimitAgent      = 1 << 20   // 1 MB
	wsReadLimitClient     = 256 << 10 // 256 KB
	speedTestWriteTimeout = 5 * time.Second
	uploadPhaseTimeout    = 30 * time.Second
	maxCommandsPerClient  = 20 // max concurrent commands (including shells) per client
	maxCommandsPerNode    = 5  // max concurrent commands a single client can target at one node

	// Permalink store caps. Lines + chars are double-counted protection
	// against a client posting a giant blob — limitBody also caps the
	// request at 1 MB total.
	runStoreMaxRecords = 1000
	runStoreTTL        = 24 * time.Hour
	runMaxLines        = 5000
	runMaxLineLen      = 4096
)

// Node represents a remote agent node
type Node struct {
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
	// Version is the agent's self-reported build identity, stored as an opaque
	// sanitized string and never parsed.
	Version string `json:"version,omitempty"`
	// Tools is the agent's self-reported tool availability, rebuilt key by key
	// against allowedCommandTypes on arrival. It is advisory UI data only —
	// never an authorization input; dispatch is gated by allowedCommandTypes in
	// handleClientCommand. Written only under nodesMu and always by assigning a
	// freshly allocated map, never mutated in place, so a reader may copy the
	// field under the lock and encode it outside.
	Tools map[string]bool `json:"tools,omitempty"`
	conn  *websocket.Conn
	mu    *sync.Mutex
	// nodesMu guards disconnecting; the UUID stays reserved while old
	// command routing is being cleaned up, even though dispatch is disabled.
	disconnecting bool
}

type HealthInfo struct {
	Uptime    string  `json:"uptime"`
	LoadAvg   string  `json:"load_avg"`
	MemUsedPc float64 `json:"mem_used_pc"`
	CPUs      int     `json:"cpus"`
	OS        string  `json:"os"`
}

type CommandRequest struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Target  string `json:"target"`
	Options string `json:"options,omitempty"`
}

type CommandResponse struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
	Data   string `json:"data"`
}

type AgentMessage struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

// geoCacheEntry stores cached GeoIP data with expiry. The pointer is held both
// by the geoCache map and as the Value of an element in geoCacheList, so the
// list provides LRU ordering without a second copy of the data.
type geoCacheEntry struct {
	ip        string
	data      []byte
	expiresAt time.Time
}

// rdapEntry caches an RDAP JSON response with expiry.
type rdapEntry struct {
	data      []byte
	expiresAt time.Time
}

// runRecord is one saved command run, retrievable by ID via /api/runs/<id>.
// Stored in-memory with a 24-hour TTL; the goal is "send your colleague the
// exact output you just saw" not durable archival.
type runRecord struct {
	ID           string    `json:"id"`
	NodeName     string    `json:"node_name"`
	NodeFlag     string    `json:"node_flag"`
	NodeLocation string    `json:"node_location"`
	Command      string    `json:"command"`
	Target       string    `json:"target"`
	Options      string    `json:"options,omitempty"`
	Lines        []runLine `json:"lines"`
	CreatedAt    time.Time `json:"created_at"`
	expiresAt    time.Time
}

type runLine struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// allowedCommandTypes is the server-side whitelist of command types clients may request.
// "shell" is excluded — interactive shells use the dedicated shell_start action.
var allowedCommandTypes = map[string]bool{
	"ping":       true,
	"traceroute": true,
	"mtr":        true,
	"nexttrace":  true,
	"iperf3":     true,
	"speedtest":  true,
	"dns":        true,
	"http":       true,
	// The four Go-native agent probes (S7). They need no binary on the node,
	// so every S7 agent can run them; the agent enforces its own address
	// policy on every target. `download` is deliberately absent from
	// scheduleAllowedCommands (backend/scheduler.go) — a 100 MB transfer on a
	// timer is not a monitoring probe.
	"tcp":      true,
	"tls":      true,
	"dnsbench": true,
	"download": true,
}

// jsonLogWriter wraps each log line as a single JSON object on stdout. Field
// shape ({ts, level, msg}) is intentionally minimal so any log shipper can
// pick it up without a parser config. Level is always "info" because Go's
// stdlib log package has no level concept; callers that want warn/error
// should encode it in the message.
type jsonLogWriter struct {
	out io.Writer
}

func (w *jsonLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	enc, err := json.Marshal(map[string]string{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": "info",
		"msg":   msg,
	})
	if err != nil {
		return w.out.Write(p)
	}
	enc = append(enc, '\n')
	if _, err := w.out.Write(enc); err != nil {
		return 0, err
	}
	return len(p), nil
}

func NewServer() *Server {
	broadcastCap := 4 * runtime.NumCPU()
	if broadcastCap < broadcastConcurrencyMin {
		broadcastCap = broadcastConcurrencyMin
	}
	if broadcastCap > broadcastConcurrencyMax {
		broadcastCap = broadcastConcurrencyMax
	}
	s := &Server{
		webSockets:      make(map[*websocket.Conn]struct{}),
		nodes:           make(map[string]*Node),
		cmdOwners:       make(map[string]*websocket.Conn),
		closingCommands: make(map[string]*closingCommand),
		cmdNodes:        make(map[string]string),
		clients:         make(map[*websocket.Conn]*sync.Mutex),
		publicClients:   make(map[*websocket.Conn]struct{}),
		geoCache:        make(map[string]*list.Element),
		geoCacheList:    list.New(),
		geoRateBuckets:  make(map[string]*ipRateBucket),
		rateEntries:     make(map[string]*rateEntry),
		rdapCache:       make(map[string]*rdapEntry),
		runStore:        make(map[string]*runRecord),
		schedules:       make(map[string]*Schedule),
		scheduleRuns:    make(map[string]*Schedule),
		summarySeen:     make(map[string]struct{}),
		meshRuns:        make(map[string]*meshProbe),
		latencyMatrix:   make(map[string]map[string]matrixEntry),
		broadcastSem:    make(chan struct{}, broadcastCap),
		startTime:       time.Now(),
		buildInfo:       readVersionInfo(),
		agentAPIKey:     os.Getenv("AGENT_API_KEY"),
		geoClient:       newMetadataClient(),
		alerts:          newAlertManager(),
	}
	s.upgrader = websocket.Upgrader{
		CheckOrigin: s.checkOrigin,
	}
	return s
}

// readVersionInfo extracts VCS info from the embedded build metadata.
// Falls back to "unknown" if the binary was built without VCS info (go build
// from a non-git source tree, or with -buildvcs=false).
func readVersionInfo() versionInfo {
	v := versionInfo{GoVersion: runtime.Version(), Revision: "unknown"}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			v.Revision = s.Value
		case "vcs.modified":
			v.Modified = s.Value == "true"
		case "vcs.time":
			v.BuildTime = s.Value
		}
	}
	return v
}

// startupWarnings logs every insecure-default warning. Extracted from main so a
// test can capture the output via log.SetOutput and assert on the exact lines.
func startupWarnings() {
	if os.Getenv("ALLOWED_ORIGINS") == "" {
		if strictOriginEnabled() {
			log.Println("WARNING: ALLOWED_ORIGINS is not set — STRICT_ORIGIN=1 is active, so only browser origins whose host matches the request Host are accepted.")
		} else {
			log.Println("WARNING: ALLOWED_ORIGINS is not set — WebSocket origin checking is disabled: any web page a user visits can drive /ws/client on their behalf, including shell_start. Set ALLOWED_ORIGINS, or STRICT_ORIGIN=1 to accept same-host origins only.")
		}
	}
	if os.Getenv("CLIENT_API_KEY") == "" {
		log.Println("WARNING: CLIENT_API_KEY is not set — client endpoints are unauthenticated. Set it in production.")
	}
	if os.Getenv("AGENT_API_KEY") == "" {
		log.Println("WARNING: AGENT_API_KEY is not set — agent endpoints are unauthenticated. Set it in production.")
	}
	if publicModeEnabled() && os.Getenv("CLIENT_API_KEY") == "" {
		log.Println("WARNING: PUBLIC_MODE=1 with an empty CLIENT_API_KEY — every client session and API request is treated as public/anonymous, since no credential can prove otherwise.")
	}
	if publicModeEnabled() && os.Getenv("TRUST_PROXY") != "1" {
		log.Println("WARNING: PUBLIC_MODE is on without TRUST_PROXY=1 — behind a reverse proxy all per-IP limits key on the proxy address and are shared by every visitor.")
	}
	if os.Getenv("METRICS_TOKEN") == "" && os.Getenv("CLIENT_API_KEY") == "" && !publicModeEnabled() {
		log.Println("WARNING: METRICS_TOKEN and CLIENT_API_KEY are both unset — /metrics is open to anyone who can reach the port. The per-schedule gauges, which carry schedule IDs, node names, commands and probe targets, are withheld from every scrape while METRICS_TOKEN is unset; set METRICS_TOKEN to require a scrape credential and enable them.")
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if os.Getenv("LOG_FORMAT") == "json" {
		log.SetFlags(0) // jsonLogWriter supplies its own timestamp
		log.SetOutput(&jsonLogWriter{out: os.Stderr})
	}

	srv := NewServer()

	startupWarnings()

	staticDir := "../frontend/dist"
	if _, err := os.Stat("frontend/dist"); err == nil {
		staticDir = "frontend/dist"
	}

	mux := http.NewServeMux()
	fsHandler := http.FileServer(http.Dir(staticDir))
	// Serve static files with appropriate cache headers
	mux.HandleFunc("/", securityHeaders(func(w http.ResponseWriter, r *http.Request) {
		// Hashed assets (JS/CSS) can be cached aggressively; index.html should not
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fsHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/nodes", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleNodes)))))
	mux.HandleFunc("/api/nodes/health", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(srv.handleNodesHealth))))
	mux.HandleFunc("/api/latency-matrix", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleLatencyMatrix)))))
	// /api/latency-matrix is an exact ServeMux pattern (no trailing slash), so
	// the measure endpoint needs its own entry.
	mux.HandleFunc("/api/latency-matrix/measure", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleLatencyMatrixMeasure)))))
	mux.HandleFunc("/api/geoip/", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleGeoIP)))))
	mux.HandleFunc("/api/rdap/", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(srv.handleRDAP))))
	mux.HandleFunc("/api/runs", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleRuns)))))
	mux.HandleFunc("/api/runs/", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(srv.handleRuns))))
	mux.HandleFunc("/api/schedules", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleSchedules)))))
	mux.HandleFunc("/api/schedules/", securityHeaders(srv.loggerWithMetrics(srv.corsMiddleware(limitBody(srv.handleSchedule)))))
	mux.HandleFunc("/api/version", securityHeaders(srv.corsMiddleware(srv.handleVersion)))
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/metrics", srv.handleMetrics)
	mux.HandleFunc("/api/public-config", securityHeaders(srv.corsMiddleware(srv.handlePublicConfig)))
	mux.HandleFunc("/ws/agent", srv.handleAgentWS)
	mux.HandleFunc("/ws/client", srv.handleClientWS)
	mux.HandleFunc("/ws/speedtest", srv.handleSpeedTestWS)

	httpSrv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// NOTE: ReadTimeout is intentionally omitted — it applies to the entire
		// connection lifecycle and would kill long-lived WebSocket connections.
		// WriteTimeout is also omitted for the same reason (WebSocket handlers
		// need unbounded write time for streaming). Timeouts for non-WS routes
		// are handled via per-handler context deadlines if needed.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Load persisted schedules (best-effort, no-op if file missing).
	srv.loadSchedules()

	// Start cleanup goroutines (geo cache + run permalink store + schedule
	// ticker + latency mesh). They all share one done-channel so shutdown
	// closes them in lockstep.
	geoCacheDone := make(chan struct{})
	// Assigned before any goroutine that can read it, so in-flight mesh probes
	// can abandon their wait at shutdown.
	srv.meshShutdown = geoCacheDone
	go srv.geoCacheCleanup(geoCacheDone)
	go srv.runStoreCleanup(geoCacheDone)
	go srv.scheduleTicker(geoCacheDone)
	// Server-side latency mesh; returns immediately when MESH_INTERVAL_SEC=0.
	go srv.meshTicker(geoCacheDone)
	// Single webhook dispatcher; it returns when geoCacheDone closes.
	go srv.alerts.run(geoCacheDone)

	// Main must wait for draining, not just for the listener to close.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		<-sigCh
		log.Println("Shutting down gracefully...")
		close(geoCacheDone)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.shutdown(ctx, httpSrv)
	}()

	log.Printf("Parallax server starting on :%s", port)
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-shutdownDone
	log.Println("Server stopped.")
}
