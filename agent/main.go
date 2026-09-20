package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Strip ANSI escape sequences from output
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()][AB012]|\x1b\[[\?]?[0-9;]*[hl]|\x1b=|\x1b>|\x1b\[[0-9]*[ABCDJKST]`)

type AgentMessage struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

type CommandRequest struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Target  string `json:"target"`
	Options string `json:"options,omitempty"`
}

type CommandResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"` // output, error, summary, done
	Data string `json:"data"`
}

var (
	serverURL  string
	nodeName   string
	location   string
	flagEmoji  string
	ipv4       string
	ipv6       string
	provider   string
	lat        float64
	lon        float64
	allowShell bool
	autoIP     bool

	// Track running commands so we can cancel them
	runningCmds   = make(map[string]*execRun)
	runningCmdsMu sync.Mutex

	// Protect WebSocket writes from concurrent goroutines
	connWriteMu sync.Mutex

	// connClosed signals that the current connection is dead — guards sendOutput
	connClosed atomic.Bool

	// Resolved absolute paths for system binaries used by gatherHealth.
	// Populated once at startup so we don't rely on PATH at runtime.
	healthBinPaths = map[string]string{}

	// Current detected public IPs, updated by detectAndStorePublicIPs() at
	// startup and every 5 minutes. Protected by currentIPMu — read by the
	// register and health-reporting goroutines.
	currentIPv4 string
	currentIPv6 string
	currentIPMu sync.RWMutex

	// version is this agent's build identity, reported to the server on
	// register and in every health frame. Set at build time with
	// -ldflags "-X main.version=..." (deploy/update.sh,
	// deploy/agent-only-install.sh and Dockerfile.agent all do); a plain
	// `go build` or `go run .` leaves it "dev". The server stores it as an
	// opaque string and never parses it.
	version = "dev"

	// currentTools is the detected tool-availability map, refreshed by
	// probeTools() at startup and every 5 minutes. Protected by toolsMu and
	// handled exactly like the public-IP cache above: the refresher is the
	// only writer and always assigns a *freshly built* map, and every reader
	// goes through snapshotTools(). The cached map is never mutated in
	// place — the readers are the register writer and the 30-second health
	// goroutine, so an in-place rebuild would be a concurrent map read/write,
	// which Go turns into a fatal throw that would kill the agent on a live
	// node.
	currentTools map[string]bool
	toolsMu      sync.RWMutex
)

func init() {
	flag.StringVar(&serverURL, "server", "ws://localhost:8080/ws/agent", "Server WebSocket URL")
	flag.StringVar(&nodeName, "name", "Node-1", "Node display name")
	flag.StringVar(&location, "location", "Unknown", "Node location (e.g. 'Hong Kong, CN')")
	flag.StringVar(&flagEmoji, "flag", "🏳️", "Flag emoji for the location")
	flag.StringVar(&ipv4, "ipv4", "", "Node IPv4 address")
	flag.StringVar(&ipv6, "ipv6", "", "Node IPv6 address")
	flag.StringVar(&provider, "provider", "", "Hosting provider name")
	flag.Float64Var(&lat, "lat", 0, "Node latitude")
	flag.Float64Var(&lon, "lon", 0, "Node longitude")
	flag.BoolVar(&allowShell, "allow-shell", true, "Allow the server to open interactive shell sessions on this node. Set to false on hardened nodes — if the server is compromised, an attacker with shell access has full control of the agent host.")
	flag.BoolVar(&allowTCPTraceroute, "allow-tcp-traceroute", false, "Allow traceroute's TCP mode (mode=tcp, traceroute -T) on this node. Off by default: -T opens real TCP connections to the target port, which is a port-scan primitive, so it stays behind an explicit operator opt-in. When off, mode=tcp is dropped and the requester is told why.")
	flag.BoolVar(&autoIP, "auto-ip", true, "Auto-detect this node's public IPv4/IPv6 at startup and refresh periodically. When true, overrides any value passed via -ipv4/-ipv6 (so dynamic-IP residential ISPs always show the real address). Set to false to use the literal -ipv4/-ipv6 values.")
}

// resolvePublicIP queries the given list of plain-text echo endpoints (each
// expected to return just the requesting client's IP) and returns the first
// successful response that parses as an IP of the desired family. `family`
// is "ip4" or "ip6"; an empty string means either is acceptable.
func resolvePublicIP(family string, endpoints []string) string {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			net4or6 := network
			switch family {
			case "ip4":
				net4or6 = "tcp4"
			case "ip6":
				net4or6 = "tcp6"
			}
			return dialer.DialContext(ctx, net4or6, addr)
		},
		// Force a fresh connection each call so IP detection isn't sticky to
		// a single keep-alive socket.
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
	for _, url := range endpoints {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("User-Agent", "parallax-agent")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		cancel()
		ip := strings.TrimSpace(string(body))
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		switch family {
		case "ip4":
			if parsed.To4() == nil {
				continue
			}
		case "ip6":
			// To4() returns non-nil for v4-mapped v6 — we want a real v6 only.
			if parsed.To4() != nil {
				continue
			}
		}
		return ip
	}
	return ""
}

// detectAndStorePublicIPs refreshes currentIPv4/currentIPv6. Called once at
// startup and on a 5-minute timer thereafter. ipv6 detection failure is
// expected on v4-only networks and is silent.
func detectAndStorePublicIPs() {
	v4 := resolvePublicIP("ip4", []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ifconfig.me/ip",
	})
	v6 := resolvePublicIP("ip6", []string{
		"https://api6.ipify.org",
		"https://ipv6.icanhazip.com",
	})
	currentIPMu.Lock()
	if v4 != "" && v4 != currentIPv4 {
		log.Printf("Public IPv4: %s%s", v4, ipChangeSuffix(currentIPv4, v4))
		currentIPv4 = v4
	}
	if v6 != "" && v6 != currentIPv6 {
		log.Printf("Public IPv6: %s%s", v6, ipChangeSuffix(currentIPv6, v6))
		currentIPv6 = v6
	}
	currentIPMu.Unlock()
}

func ipChangeSuffix(prev, curr string) string {
	if prev == "" || prev == curr {
		return ""
	}
	return " (was " + prev + ")"
}

func snapshotCurrentIPs() (string, string) {
	currentIPMu.RLock()
	defer currentIPMu.RUnlock()
	return currentIPv4, currentIPv6
}

// storeTools publishes a freshly built tool map. The previous map is dropped,
// never edited, so any reader still encoding a snapshot of it is unaffected.
func storeTools(tools map[string]bool) {
	toolsMu.Lock()
	currentTools = tools
	toolsMu.Unlock()
}

// snapshotTools returns a copy of the tool cache. Every reader of the cache
// must go through this helper: the copy is what makes it safe to marshal the
// result outside the lock while the refresher replaces the cached map.
func snapshotTools() map[string]bool {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	out := make(map[string]bool, len(currentTools))
	for name, ok := range currentTools {
		out[name] = ok
	}
	return out
}

// refreshTools re-probes the tool set and publishes it, logging only when the
// available set actually changed (e.g. an operator installed mtr) so the
// 5-minute ticker stays quiet.
func refreshTools() {
	prev := snapshotTools()
	fresh := probeTools()
	storeTools(fresh)
	if !sameTools(prev, fresh) {
		logToolProbe(fresh)
	}
}

func sameTools(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name, ok := range a {
		if other, present := b[name]; !present || other != ok {
			return false
		}
	}
	return true
}

// logToolProbe names the missing tools once, so an operator can see why the UI
// greys a command out without turning on debug logging.
func logToolProbe(tools map[string]bool) {
	missing := make([]string, 0, len(tools))
	for name, ok := range tools {
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		log.Printf("Tool probe: all %d probe binaries present", len(tools))
		return
	}
	slices.Sort(missing)
	log.Printf("Tool probe: %d of %d probe binaries missing (%s) — the UI will show those commands as unavailable on this node",
		len(missing), len(tools), strings.Join(missing, ", "))
}

// resolveHealthBinaries looks up absolute paths for /usr/bin/uptime, sysctl, and
// vm_stat at startup. Falls back silently to PATH lookup if not found.
func resolveHealthBinaries() {
	candidates := map[string][]string{
		"uptime":  {"/usr/bin/uptime", "/bin/uptime"},
		"sysctl":  {"/usr/sbin/sysctl", "/sbin/sysctl", "/usr/bin/sysctl"},
		"vm_stat": {"/usr/bin/vm_stat"},
	}
	for name, paths := range candidates {
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				healthBinPaths[name] = p
				break
			}
		}
	}
}

// healthBin returns the resolved absolute path for a binary if known,
// otherwise the bare name (which falls back to PATH lookup in exec.Command).
func healthBin(name string) string {
	if p, ok := healthBinPaths[name]; ok {
		return p
	}
	return name
}

func main() {
	flag.Parse()

	if !strings.HasPrefix(serverURL, "ws://") && !strings.HasPrefix(serverURL, "wss://") {
		log.Fatal("Server URL must start with ws:// or wss://")
	}
	if strings.HasPrefix(serverURL, "ws://") && serverURL != "ws://localhost:8080/ws/agent" {
		log.Printf("WARNING: Using unencrypted WebSocket connection to %s. Use wss:// in production.", serverLogURL(serverURL))
	}

	resolveHealthBinaries()

	// Native-probe setup (S7), before any command can arrive:
	// PROBE_ALLOW_PRIVATE is read exactly once here, and the on-link prefix set
	// is populated so the very first probe is already covered by it.
	initProbeEnv()
	refreshLocalPrefixes()
	logLocalPrefixes()

	// Probe the tool set once before the first register frame, then keep it
	// fresh on a 5-minute ticker. The ticker lives in main() for the whole
	// process: the agent has no graceful shutdown, and a per-connection
	// ticker would restart on every reconnect.
	//
	// The on-link prefix set rides this ticker rather than the IP-detection one
	// below: this one always runs, while that one only runs under -auto-ip and
	// would leave -auto-ip=false nodes with a prefix set frozen at boot.
	storeTools(probeTools())
	logToolProbe(snapshotTools())
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			refreshTools()
			refreshLocalPrefixes()
		}
	}()

	if !allowShell {
		log.Println("Interactive shell sessions DISABLED on this node (-allow-shell=false).")
	}

	// Seed the IP cache so the first connect's register payload carries the
	// real address. If -auto-ip is off, fall back to the literal flag values.
	if autoIP {
		detectAndStorePublicIPs()
		// If detection failed entirely, still fall back to the static flags
		// rather than registering with empty IPs.
		currentIPMu.Lock()
		if currentIPv4 == "" {
			currentIPv4 = ipv4
		}
		if currentIPv6 == "" {
			currentIPv6 = ipv6
		}
		currentIPMu.Unlock()
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				detectAndStorePublicIPs()
			}
		}()
	} else {
		currentIPMu.Lock()
		currentIPv4 = ipv4
		currentIPv6 = ipv6
		currentIPMu.Unlock()
	}

	for {
		// A refused duplicate name is not a transient failure: the other agent
		// keeps the name until it dies, so retrying every 5s is a reconnect
		// storm against a server that will keep saying no. Every other failure
		// keeps the original 5s path.
		delay := reconnectDelay
		if err := run(); err != nil {
			if errors.Is(err, errDuplicateName) {
				delay = duplicateNameDelay
				log.Printf("duplicate name %q — another agent is already registered under it; sleeping %s",
					nodeName, duplicateNameDelay)
			} else {
				log.Printf("Connection error: %v, reconnecting in %s...", err, reconnectDelay)
			}
		}
		time.Sleep(delay)
	}
}

// errDuplicateName is returned by run() when the server refuses this agent
// because another live connection already holds its name — either as an
// {"action":"error","payload":{"reason":"duplicate name"}} frame or as a close
// with code wsCloseDuplicateName.
var errDuplicateName = errors.New("duplicate node name")

const (
	// wsCloseDuplicateName must match the server's close code in
	// backend/agent_ws.go.
	wsCloseDuplicateName = 4409
	// duplicateNameReason must match the server's refusal reason string.
	duplicateNameReason = "duplicate name"

	reconnectDelay     = 5 * time.Second
	duplicateNameDelay = 30 * time.Second
)

// healthPayload builds the payload of one `health` frame: the usual stats, the
// latest detected public IPs, and this build's identity plus its detected tool
// set. Extracted from the reporting goroutine so a test can assert the frame's
// shape without waiting 30 seconds for a tick.
func healthPayload() []byte {
	health := gatherHealth()
	v4, v6 := snapshotCurrentIPs()
	// The server reads ipv4/ipv6 separately so they update node identity even
	// if the rest of HealthInfo is unchanged; version/tools ride along on the
	// same frame — no new action.
	payload, _ := json.Marshal(struct {
		HealthInfo
		IPv4    string          `json:"ipv4,omitempty"`
		IPv6    string          `json:"ipv6,omitempty"`
		Version string          `json:"version,omitempty"`
		Tools   map[string]bool `json:"tools,omitempty"`
	}{HealthInfo: health, IPv4: v4, IPv6: v6, Version: version, Tools: snapshotTools()})
	return payload
}

// serverLogURL never includes query credentials or URL userinfo in logs.
func serverLogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid server URL]"
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	return u.String()
}

func run() error {
	log.Printf("Connecting to %s...", serverLogURL(serverURL))

	// Send API key via Authorization header instead of query parameter.
	// Supports both AGENT_API_KEY env var and legacy ?key= query param.
	header := http.Header{}
	apiKey := os.Getenv("AGENT_API_KEY")
	if apiKey != "" {
		header.Set("Authorization", "Bearer "+apiKey)
	}

	conn, _, err := websocket.DefaultDialer.Dial(serverURL, header)
	if err != nil {
		// url.Error quotes its original URL, which may contain a legacy key.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("dial: %s %s: %v", urlErr.Op, serverLogURL(urlErr.URL), urlErr.Err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	// Mark connection as alive
	connClosed.Store(false)

	// Set read deadline so the agent detects a dead server.
	// The server sends WebSocket-level pings every 30s; if we don't receive
	// anything within 90s, the server is likely dead.
	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		// Respond with Pong (default behavior)
		connWriteMu.Lock()
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
		connWriteMu.Unlock()
		return err
	})

	var workers sync.WaitGroup
	defer func() {
		// Acquire the write mutex so any in-flight sendOutput finishes before
		// we mark the connection closed and tear it down. This eliminates the
		// race where a Write proceeds against a connection that is being closed.
		connWriteMu.Lock()
		connClosed.Store(true)
		conn.Close()
		connWriteMu.Unlock()
		// Kill all running commands from this connection to free resources.
		// Native probes (S7) live in their own map under the same mutex and are
		// cancelled here too — otherwise a dropped browser could leave a
		// download running to its deadline.
		runningCmdsMu.Lock()
		for _, proc := range runningCmds {
			proc.cancel()
		}
		cancelAllNativeRunsLocked()
		runningCmdsMu.Unlock()
		// All workers inherit cancellation and bounded output writes. Waiting
		// here prevents an old connection's late worker registering on the next.
		workers.Wait()
	}()

	// Done channel to signal goroutines on disconnect
	done := make(chan struct{})
	defer close(done)
	ctx, cancelConnection := context.WithCancel(context.Background())
	defer cancelConnection()

	// Reserve cancellation in the read loop before launching a worker. A
	// cancel frame may arrive before that worker has registered its process.
	var requestsMu sync.Mutex
	requests := make(map[string]*execRun)
	startRequest := func(id string, work func(context.Context)) {
		requestCtx, cancel := context.WithCancel(ctx)
		request := &execRun{cancel: cancel}
		requestsMu.Lock()
		if _, busy := requests[id]; busy {
			requestsMu.Unlock()
			cancel()
			sendOutput(conn, id, "error", "A command with this id is already running on this node.")
			sendOutput(conn, id, "done", doneData(false))
			return
		}
		requests[id] = request
		requestsMu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer cancel()
			// A terminal frame releases the server's ID reservation. Publish it
			// only after the worker's process/PTY/probe cleanup and our own
			// reservation release, so immediate ID reuse is safe end to end.
			output := &requestOutput{conn: conn}
			requestCtx = context.WithValue(requestCtx, requestOutputKey{}, output)
			defer func() {
				requestsMu.Lock()
				delete(requests, id)
				requestsMu.Unlock()
				output.finish()
			}()
			work(requestCtx)
		}()
	}

	// Register — use the freshly detected IPs (when -auto-ip is on) so a
	// dynamic-IP residential ISP shows the real address, not whatever was
	// hardcoded in the systemd unit at provision time.
	regV4, regV6 := snapshotCurrentIPs()
	regPayload, _ := json.Marshal(map[string]any{
		"name":     nodeName,
		"location": location,
		"flag":     flagEmoji,
		"ipv4":     regV4,
		"ipv6":     regV6,
		"provider": provider,
		"lat":      lat,
		"lon":      lon,
		// Build identity and detected tools travel with the very first frame,
		// so a node is correct in the UI without waiting for a health tick.
		"version": version,
		"tools":   snapshotTools(),
	})
	regMsg, _ := json.Marshal(AgentMessage{Action: "register", Payload: regPayload})
	connWriteMu.Lock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, regMsg)
	connWriteMu.Unlock()
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Heartbeat + health reporting — exits when done is closed
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				hb, _ := json.Marshal(AgentMessage{Action: "health", Payload: healthPayload()})
				connWriteMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, hb); err != nil {
					conn.Close() // wake the reader so all connection workers stop
					connWriteMu.Unlock()
					return
				}
				connWriteMu.Unlock()
			}
		}
	}()

	// Listen for commands
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// The refusal may arrive as a close frame only (the error frame can
			// be lost if the socket dies first), so the code is authoritative.
			if websocket.IsCloseError(err, wsCloseDuplicateName) {
				return errDuplicateName
			}
			return fmt.Errorf("read: %w", err)
		}
		// Reset read deadline on any successful read
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var envelope AgentMessage
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}

		switch envelope.Action {
		case "error":
			// Server-level refusal (not command output). A duplicate name is
			// terminal for this connection; anything else is logged with %q so
			// a hostile reason string cannot inject control characters into the
			// agent's log.
			var srvErr struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(envelope.Payload, &srvErr); err != nil {
				continue
			}
			if srvErr.Reason == duplicateNameReason {
				return errDuplicateName
			}
			log.Printf("Server reported an error: %q", srvErr.Reason)

		case "command":
			var cmd CommandRequest
			if err := json.Unmarshal(envelope.Payload, &cmd); err != nil {
				log.Printf("Invalid command payload: %v", err)
				continue
			}
			startRequest(cmd.ID, func(ctx context.Context) {
				executeCommandContext(ctx, conn, cmd)
			})

		case "shell_start":
			var req struct {
				ID   string `json:"id"`
				Cols uint16 `json:"cols,omitempty"`
				Rows uint16 `json:"rows,omitempty"`
			}
			if err := json.Unmarshal(envelope.Payload, &req); err != nil {
				continue
			}
			if !allowShell {
				sendOutput(conn, req.ID, "error", "Interactive shell is disabled on this node.")
				sendOutput(conn, req.ID, "done", "")
				continue
			}
			startRequest(req.ID, func(ctx context.Context) {
				startShellSessionSize(req.ID, req.Cols, req.Rows, func(id, typ, data string) {
					sendRequestOutput(ctx, conn, id, typ, data)
				}, ctx.Done())
			})

		case "shell_input":
			var req struct {
				ID    string     `json:"id"`
				Input ShellInput `json:"input"`
			}
			if err := json.Unmarshal(envelope.Payload, &req); err != nil {
				continue
			}
			handleShellInput(req.ID, req.Input)

		case "cancel":
			var cancel struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(envelope.Payload, &cancel); err != nil {
				continue
			}
			requestsMu.Lock()
			if request := requests[cancel.ID]; request != nil {
				request.cancel()
			}
			requestsMu.Unlock()
			runningCmdsMu.Lock()
			if proc, ok := runningCmds[cancel.ID]; ok {
				proc.cancel()
			}
			// A native probe (S7) is cancelled through its context while this
			// mutex is held: cancel only closes a channel, and the probe's
			// deferred unregister blocks until this section releases, so the
			// entry is removed by the probe itself.
			cancelNativeRunLocked(cancel.ID)
			runningCmdsMu.Unlock()
		}
	}
}

func sendOutput(conn *websocket.Conn, cmdID, outputType, data string) error {
	resp := CommandResponse{ID: cmdID, Type: outputType, Data: data}
	payload, _ := json.Marshal(resp)
	msg, _ := json.Marshal(AgentMessage{Action: "output", Payload: payload})
	// Re-check connClosed inside the mutex so a concurrent close cannot run
	// between the load and the WriteMessage call.
	connWriteMu.Lock()
	if connClosed.Load() {
		connWriteMu.Unlock()
		return fmt.Errorf("connection closed")
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := conn.WriteMessage(websocket.TextMessage, msg)
	if err != nil {
		// A failed write makes this WebSocket unusable; wake its reader rather
		// than leaving shell/probe workers alive while pings keep arriving.
		connClosed.Store(true)
		conn.Close()
	}
	connWriteMu.Unlock()
	return err
}

// Allowed commands whitelist — "shell" type uses the dedicated shell_start action
var allowedCommands = map[string]bool{
	"ping":       true,
	"traceroute": true,
	"mtr":        true,
	"nexttrace":  true,
	"iperf3":     true,
	"speedtest":  true,
	"dns":        true,
	"http":       true,
	// The four native probes (S7). They are mandatory here: the native branch
	// in executeCommand sits *after* this check, so a missing entry would
	// refuse the command before any probe ran. They deliberately have no
	// commandBuilders or commandBinaries entry — nativeProbes (agent/probes.go)
	// stands in for commandBuilders, and capabilities_test.go asserts every
	// allowed type has exactly one of the two.
	"tcp":      true,
	"tls":      true,
	"dnsbench": true,
	"download": true,
}

// Whitelisted iperf3 flags to prevent arbitrary flag injection
var allowedIperf3Flags = map[string]bool{
	"-p": true, "-t": true, "-P": true,
	"-R": true, "-u": true, "-b": true, "-n": true,
	"-4": true, "-6": true, "--reverse": true,
}

// maxIperf3Bandwidth caps the -b flag to prevent UDP flood abuse (100Mbit/s)
const maxIperf3Bandwidth = 100_000_000

// maxIperf3Bytes caps the -n flag to prevent excessive data transfer (1GB)
const maxIperf3Bytes = 1_000_000_000

// maxOutputBytes caps the total output one command may stream (10 MB). It is a
// package var, not a const, only so tests can shrink it before driving the
// output-cap abort path — the production value is unchanged.
var maxOutputBytes = 10 * 1024 * 1024

// ── Per-tool options: one grammar, one validation step ──
//
// Options arrive as a free-text string from the browser, and also from saved
// presets, saved history and /api/runs replays. Every builder used to scrape
// that string itself, which meant the grammar, the bounds and the injection
// guard were duplicated per tool. They now live in exactly one place:
// buildCommandArgs normalizes cmd.Options against optionSpecs before any
// builder sees it, and the builders only format argv.
//
// Two rules keep the restructure honest:
//
//   - Values reach argv only through strconv.Atoi + bounds or an enum lookup,
//     never as free text, so `count=5;rm -rf /` and `type=A;id` are not
//     representable rather than merely rejected (F2).
//   - The normalized string is re-read by the builders, so its encoding is
//     pinned: the IP version is re-emitted as the literal `-4`/`-6` token
//     (`ipv` is a parse-internal key that never reaches the wire) because
//     appendIPVersionFlags and buildDns detect it by scanning for those
//     literals. A "tidier" canonical `ipv=4` would silently drop the flag from
//     argv for all five grammar-governed builders.

// ipVersionKey is the reserved parse-internal key the `-4`/`-6` tokens are
// stored under. `ipv` is spellable as an ordinary key under the grammar, so
// parseOptions refuses the `ipv=…` form outright: otherwise a caller could put a
// value of its choosing into the reserved slot, and since the normalized string
// re-emits that slot as a bare token, `ipv=short` would inject the bare token
// `short` — which buildDns reads as +short. A value must never become a token.
const ipVersionKey = "ipv"

// maxOptionTokens bounds strings.Fields on the options string. Exceeding it
// rejects the whole command instead of truncating, so a flood of tokens can
// never be half-applied.
const maxOptionTokens = 16

// optionTokenRe is the whole-token option grammar. `-4`/`-6` are the two
// reserved literal forms; anything else is a 3..10 lowercase-letter key with an
// optional value. The value class excludes `-`, `;`, `/`, `+`, `,`, `:` and
// space, which is what makes `size=-1`, `count=5;rm -rf /` and `type=A;id`
// unrepresentable.
var optionTokenRe = regexp.MustCompile(`^(-4|-6|[a-z]{3,10}(=[A-Za-z0-9.]{1,16})?)$`)

// allowTCPTraceroute gates traceroute's `mode=tcp` (`-T`), which makes the agent
// open TCP connections to the target port — a scan primitive — so it is off
// unless the operator opted in with -allow-tcp-traceroute (F4). A package var
// rather than only a flag target so the gate is unit-testable, following
// maxOutputBytes and nativeProbeTypes.
var allowTCPTraceroute = false

// validDNSTypes is dig's record-type whitelist. Package-level because
// optionSpecs["dns"] derives its `type=` enum from it, so the accepted set lives
// in exactly one place.
var validDNSTypes = map[string]bool{
	"A": true, "AAAA": true, "MX": true, "CNAME": true,
	"NS": true, "TXT": true, "SOA": true, "SRV": true,
	"PTR": true, "ANY": true, "CAA": true, "DNSKEY": true,
}

// optionKind is how one option key is parsed and turned into argv.
type optionKind int

const (
	// optInt is a bounded integer: `key=N`, min..max inclusive.
	optInt optionKind = iota
	// optEnum is `key=value` against a literal table mapping each canonical
	// value to the argv token it produces. "" means "this is the tool's
	// default and adds nothing to argv"; for dns `type=` the mapping is the
	// identity, because the value itself is a positional argument.
	optEnum
	// optFlag is a bare key with no value, mapping to a single argv flag.
	optFlag
)

// optionDef describes one option key: its kind, its bounds or its literal
// value→flag table, and an optional gate.
type optionDef struct {
	kind     optionKind
	min, max int               // optInt bounds, inclusive
	enum     map[string]string // optEnum/optFlag: canonical value -> argv token
	// gate, when non-nil, runs after the enum lookup. A false return drops the
	// value with the returned reason instead of letting it reach argv.
	gate func(canon string) (bool, string)
}

// canon resolves a caller-supplied value to its canonical enum key: exact match
// first, then an upper- and a lower-cased retry. Every enum in optionSpecs is
// either all-upper (DNS record types) or all-lower (mode, method), so the
// retries can never select a different entry than an exact match would have.
func (d optionDef) canon(v string) (string, bool) {
	for _, candidate := range []string{v, strings.ToUpper(v), strings.ToLower(v)} {
		if _, ok := d.enum[candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

// values lists the accepted enum values, sorted, for a note line.
func (d optionDef) values() string {
	out := make([]string, 0, len(d.enum))
	for v := range d.enum {
		out = append(out, v)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// optionSpec is one command type's option grammar.
type optionSpec struct {
	// raw exempts the type from the grammar, the token cap and the notes:
	// cmd.Options reaches the builder byte-for-byte. iperf3 and speedtest are
	// raw because iperf3's shipped option strings (`-p 5201 -t 30 -P 4 -R`)
	// are already stored in users' presets, history and run records, the token
	// grammar cannot represent them, and buildIperf3 keeps its own flag
	// whitelist with space-separated values.
	raw bool
	// ipVersion allows the reserved `-4`/`-6` tokens.
	ipVersion bool
	// order fixes the normalized string's token order so the encoding is
	// deterministic and assertable. It must list exactly the keys of keys —
	// options_test.go proves it does.
	order []string
	keys  map[string]optionDef
}

// optionSpecs is the per-command-type option grammar.
//
// A command type with a builder but *no* entry here is treated as raw: options
// pass through untouched, with no grammar and no notes. That rule is
// load-bearing rather than a corner case — agent/summary_test.go registers
// throwaway types (testfail/testflood/testok) into allowedCommands and
// commandBuilders and never into this table, so a lookup miss must not be an
// error.
var optionSpecs = map[string]optionSpec{
	"ping": {
		ipVersion: true,
		order:     []string{"count", "size"},
		keys: map[string]optionDef{
			"count": {kind: optInt, min: 1, max: 100},
			"size":  {kind: optInt, min: 16, max: 1472},
		},
	},
	"traceroute": {
		ipVersion: true,
		order:     []string{"maxhops", "mode"},
		keys: map[string]optionDef{
			"maxhops": {kind: optInt, min: 1, max: 64},
			"mode": {
				kind: optEnum,
				// udp is traceroute's own default and adds no flag.
				enum: map[string]string{"icmp": "-I", "udp": "", "tcp": "-T"},
				gate: gateTracerouteMode,
			},
		},
	},
	"mtr": {
		ipVersion: true,
		order:     []string{"count"},
		keys: map[string]optionDef{
			"count": {kind: optInt, min: 1, max: 20},
		},
	},
	"http": {
		ipVersion: true,
		order:     []string{"method"},
		keys: map[string]optionDef{
			// No `follow` option exists, here or anywhere: a probe that
			// follows redirects can be walked onto a host the operator never
			// named (F3).
			"method": {kind: optEnum, enum: map[string]string{"get": "", "head": "--head"}},
		},
	},
	"dns": {
		ipVersion: true,
		order:     []string{"short", "trace", "dnssec", "type"},
		keys: map[string]optionDef{
			"short":  {kind: optFlag, enum: map[string]string{"": "+short"}},
			"trace":  {kind: optFlag, enum: map[string]string{"": "+trace"}},
			"dnssec": {kind: optFlag, enum: map[string]string{"": "+dnssec"}},
			"type":   {kind: optEnum, enum: dnsTypeEnum()},
		},
	},
	// nexttrace takes no options at all: its argv parser is custom and reads
	// `--` as an unknown argument, so there is no safe place to put a flag.
	// Governed (not raw) so every token is dropped with a note rather than
	// silently ignored.
	"nexttrace": {},
	"iperf3":    {raw: true},
	"speedtest": {raw: true},
	// The four native probes take no options at all (S7). Each entry is
	// `{}`-shaped and *not* raw: governed-with-no-keys means every token sent
	// to them is dropped with a note rather than silently ignored. They are
	// listed here because the whitelist-sync invariant requires an explicit
	// spec per allowed type, but the entries govern nothing in practice — the
	// native branch runs before buildCommandArgs, so normalizeOptions never
	// executes for these types and the probes never read cmd.Options. Taking
	// no options is the only shape that is both honest and safe here.
	"tcp":      {},
	"tls":      {},
	"dnsbench": {},
	"download": {},
}

// dnsTypeEnum turns validDNSTypes into an identity enum: the canonical value is
// the argv token, because dig takes the record type as a positional argument.
func dnsTypeEnum() map[string]string {
	m := make(map[string]string, len(validDNSTypes))
	for t := range validDNSTypes {
		m[t] = t
	}
	return m
}

// gateTracerouteMode refuses mode=tcp unless the agent was started with
// -allow-tcp-traceroute.
func gateTracerouteMode(canon string) (bool, string) {
	if canon == "tcp" && !allowTCPTraceroute {
		return false, "this node was not started with -allow-tcp-traceroute"
	}
	return true, ""
}

// parseOptions splits an options string into the tokens optionSpecs understands.
// Tokens that do not match the grammar go to dropped, where the caller turns
// each into a note; a duplicate key keeps the last grammar-valid occurrence.
// More than maxOptionTokens fields is an error that rejects the whole command.
//
// Validity here is purely the token grammar. Whether a key exists for this
// command type, and whether its value is in range, is normalizeOptions' job —
// so `count=5 count=999` keeps 999, which then fails the bounds check and is
// dropped with a note rather than quietly falling back to the 5.
func parseOptions(s string) (map[string]string, []string, error) {
	fields := strings.Fields(s)
	if len(fields) > maxOptionTokens {
		return nil, nil, fmt.Errorf("too many options: %d tokens (max %d)", len(fields), maxOptionTokens)
	}
	opts := make(map[string]string, len(fields))
	var dropped []string
	for _, f := range fields {
		if !optionTokenRe.MatchString(f) {
			dropped = append(dropped, f)
			continue
		}
		if f == "-4" || f == "-6" {
			opts[ipVersionKey] = f
			continue
		}
		key, value, _ := strings.Cut(f, "=")
		// The reserved slot is writable only by the two literal tokens above
		// (see ipVersionKey).
		if key == ipVersionKey {
			dropped = append(dropped, f)
			continue
		}
		opts[key] = value
	}
	return opts, dropped, nil
}

// normalizeOptions validates raw against one command type's spec and re-encodes
// the survivors into the pinned normalized form. It is the single authority on
// the grammar and the bounds; the builders' own checks are backstops.
//
// Every rejection produces a note, so the user sees why the option they asked
// for is not in argv. Nothing is ever silently clamped. An error return means
// the whole command is refused, and in that case the caller emits the error
// instead of the notes.
func normalizeOptions(cmdType string, spec optionSpec, raw string) (string, []string, error) {
	opts, dropped, err := parseOptions(raw)
	if err != nil {
		return "", nil, err
	}
	var notes []string
	note := func(format string, a ...any) {
		notes = append(notes, "[options] "+fmt.Sprintf(format, a...))
	}

	// Legacy bare DNS record type. The UI, saved presets, saved history and
	// /api/runs replays all carry the record type as a bare uppercase token
	// ("MX"), which the new grammar does not match, so it lands in dropped.
	// Rescue it — validated against validDNSTypes, so this is not the removed
	// "any leftover token becomes the type" fallthrough — but only when no
	// explicit type= was given.
	if _, explicit := opts["type"]; !explicit {
		if _, governs := spec.keys["type"]; governs {
			for i, tok := range dropped {
				upper := strings.ToUpper(tok)
				if !validDNSTypes[upper] {
					continue
				}
				opts["type"] = upper
				dropped = append(dropped[:i:i], dropped[i+1:]...)
				note("%q is a deprecated way to ask for a record type — use type=%s", tok, upper)
				break
			}
		}
	}

	// %q here is not cosmetic: it renders any non-printable byte as an escape,
	// so a token carrying e.g. an ESC cannot reach a terminal through a note.
	for _, tok := range dropped {
		note("dropped %q: not a valid option token", tok)
	}

	var tokens []string
	if v, ok := opts[ipVersionKey]; ok {
		delete(opts, ipVersionKey)
		switch {
		case v != "-4" && v != "-6":
			// Unreachable: parseOptions only ever writes the two literals. Kept
			// so a future edit to the parser cannot turn this slot into a
			// free-text token in the normalized string.
			note("dropped %q: not a valid IP-version flag", v)
		case !spec.ipVersion:
			note("dropped %q: %s takes no IP-version flag", v, cmdType)
		default:
			tokens = append(tokens, v)
		}
	}
	for _, key := range spec.order {
		value, present := opts[key]
		if !present {
			continue
		}
		delete(opts, key)
		def := spec.keys[key]
		switch def.kind {
		case optInt:
			n, convErr := strconv.Atoi(value)
			if convErr != nil || n < def.min || n > def.max {
				note("dropped %s=%s: must be a whole number between %d and %d", key, value, def.min, def.max)
				continue
			}
			tokens = append(tokens, key+"="+strconv.Itoa(n))
		case optEnum:
			canon, ok := def.canon(value)
			if !ok {
				note("dropped %s=%s: not one of %s", key, value, def.values())
				continue
			}
			if def.gate != nil {
				if allowed, why := def.gate(canon); !allowed {
					note("dropped %s=%s: %s", key, canon, why)
					continue
				}
			}
			tokens = append(tokens, key+"="+canon)
		case optFlag:
			if value != "" {
				note("dropped %s=%s: %s is a bare flag and takes no value", key, value, key)
				continue
			}
			tokens = append(tokens, key)
		}
	}
	// Whatever is left matched the token grammar but is not an option of this
	// command type.
	leftover := make([]string, 0, len(opts))
	for key := range opts {
		leftover = append(leftover, key)
	}
	slices.Sort(leftover)
	for _, key := range leftover {
		note("dropped %q: %s has no such option", key, cmdType)
	}
	return strings.Join(tokens, " "), notes, nil
}

// ── Normalized-options accessors, used by the builders ──
//
// The builders re-read the normalized string rather than receiving a parsed
// struct, because commandBuilder's signature is frozen. Each accessor
// re-applies the spec, so a builder called directly — by a test, or by some
// future path that skips normalization — still cannot put an out-of-range value
// or a gated flag into argv.

// normalizedField finds a whole token in a normalized options string, matching
// either `key` (a bare flag) or `key=value`. Whole-token matching for the same
// reason appendIPVersionFlags uses it: a substring match would read "count=-4"
// as the -4 flag. The last occurrence wins, mirroring the parser's duplicate
// rule; a normalized string never actually contains duplicates.
func normalizedField(options, key string) (string, bool) {
	value, found := "", false
	for _, f := range strings.Fields(options) {
		if f == key {
			value, found = "", true
			continue
		}
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

// optIntArg returns a bounded-int option's value. ok is false when the option is
// absent, unparsable, not an int option of this command type, or outside the
// spec's bounds — leaving the builder to apply its own default.
func optIntArg(cmdType, key, options string) (int, bool) {
	raw, present := normalizedField(options, key)
	if !present {
		return 0, false
	}
	def, known := optionSpecs[cmdType].keys[key]
	if !known || def.kind != optInt {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < def.min || n > def.max {
		return 0, false
	}
	return n, true
}

// optArgvToken returns the argv token an enum or bare-flag option maps to, or ""
// when the option is absent, not in the spec's table, or refused by its gate.
// For dns `type=` the token is the positional record type rather than a flag,
// because that is what dig takes.
func optArgvToken(cmdType, key, options string) string {
	raw, present := normalizedField(options, key)
	if !present {
		return ""
	}
	def, known := optionSpecs[cmdType].keys[key]
	if !known || def.kind == optInt {
		return ""
	}
	canon, ok := def.canon(raw)
	if !ok {
		return ""
	}
	if def.gate != nil {
		if allowed, _ := def.gate(canon); !allowed {
			return ""
		}
	}
	return def.enum[canon]
}

// ipVersionFlag returns the first literal `-4`/`-6` token in an options string,
// or "". First rather than last so a direct (non-normalized) caller sees exactly
// the behaviour appendIPVersionFlags has always had; a normalized string holds
// at most one of the two.
func ipVersionFlag(options string) string {
	for _, f := range strings.Fields(options) {
		if f == "-4" || f == "-6" {
			return f
		}
	}
	return ""
}

// ── Command builders (one per command type) ──

type commandBuilder func(cmd CommandRequest) (string, []string, error)

var commandBuilders = map[string]commandBuilder{
	"ping":       buildPing,
	"traceroute": buildTraceroute,
	"mtr":        buildMtr,
	"nexttrace":  buildNexttrace,
	"iperf3":     buildIperf3,
	"speedtest":  buildSpeedtest,
	"dns":        buildDns,
	"http":       buildHTTP,
}

// commandBinaries maps each allowedCommands key to the binary its builder
// actually execs. It is explicit rather than derived because the binary is not
// the command type for two of them: dns runs `dig` (buildDns) and http runs
// `curl` (buildHTTP). A test asserts every allowedCommands key has an entry, so
// adding a command type without declaring its binary fails the suite.
//
// These are the same bare names executeCommand passes to exec.Command, so a
// tool reported present here is a tool that will actually run. The
// healthBin/healthBinPaths table serves the *health stat* binaries only and is
// deliberately not reused.
var commandBinaries = map[string]string{
	"ping":       "ping",
	"traceroute": "traceroute",
	"mtr":        "mtr",
	"nexttrace":  "nexttrace",
	"iperf3":     "iperf3",
	"speedtest":  "speedtest",
	"dns":        "dig",
	"http":       "curl",
}

// nativeProbeTypes lists command types this agent implements in Go, with no
// external binary to look up. Members are reported available without a PATH
// lookup. It must describe the same set as the nativeProbes table in probes.go
// — the two are two sources of truth for it, and a test asserts they agree.
var nativeProbeTypes = []string{"tcp", "tls", "dnsbench", "download"}

// probeTools reports which probe tools this node can actually run, using the
// same bare-name PATH resolution executeCommand relies on. The result is
// advisory UI data: the server's dispatch gate is its own command whitelist and
// never consults this map.
func probeTools() map[string]bool {
	tools := make(map[string]bool, len(allowedCommands)+len(nativeProbeTypes))
	for cmdType := range allowedCommands {
		bin, ok := commandBinaries[cmdType]
		if !ok {
			// Unreachable in production (the coverage test forbids it); a type
			// with no declared binary is reported absent rather than guessed.
			tools[cmdType] = false
			continue
		}
		_, err := exec.LookPath(bin)
		tools[cmdType] = err == nil
	}
	// Native probes last so they always win over a same-named lookup.
	for _, cmdType := range nativeProbeTypes {
		tools[cmdType] = true
	}
	return tools
}

// appendIPVersionFlags prepends -4 or -6 when the options string carries one.
// It scans for the literal token (see ipVersionFlag) rather than a substring, so
// "count=-4" does not trip the detector — which is also why the normalized
// options string re-emits the IP version as that same literal token instead of a
// canonical key.
func appendIPVersionFlags(args []string, options string) []string {
	if f := ipVersionFlag(options); f != "" {
		return append([]string{f}, args...)
	}
	return args
}

// buildPing formats `[-4|-6] -c N [-s S] -- target`. Options are already
// validated by normalizeOptions; optIntArg re-checks the spec bounds as a
// backstop and the builder supplies the default count of 10.
func buildPing(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid ping target")
	}
	count := 10
	if n, ok := optIntArg("ping", "count", cmd.Options); ok {
		count = n
	}
	args := []string{"-c", strconv.Itoa(count)}
	if size, ok := optIntArg("ping", "size", cmd.Options); ok {
		args = append(args, "-s", strconv.Itoa(size))
	}
	// `--` separates flags from positional args so the target can never be
	// interpreted as a flag, even if a future code path bypasses the prefix check.
	args = append(args, "--", cmd.Target)
	return "ping", appendIPVersionFlags(args, cmd.Options), nil
}

// buildTraceroute formats `[-4|-6] [-m H] [-I|-T] -- target`.
func buildTraceroute(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid traceroute target")
	}
	var args []string
	if hops, ok := optIntArg("traceroute", "maxhops", cmd.Options); ok {
		args = append(args, "-m", strconv.Itoa(hops))
	}
	// mode=udp is traceroute's own default and maps to no flag at all;
	// mode=tcp (-T) comes back empty on an agent without
	// -allow-tcp-traceroute, because optArgvToken consults the spec's gate.
	if flag := optArgvToken("traceroute", "mode", cmd.Options); flag != "" {
		args = append(args, flag)
	}
	args = append(args, "--", cmd.Target)
	return "traceroute", appendIPVersionFlags(args, cmd.Options), nil
}

// buildMtr formats `[-4|-6] -r -w -c N -- target`, N defaulting to 5.
func buildMtr(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid mtr target")
	}
	count := 5
	if n, ok := optIntArg("mtr", "count", cmd.Options); ok {
		count = n
	}
	args := []string{"-r", "-w", "-c", strconv.Itoa(count), "--", cmd.Target}
	return "mtr", appendIPVersionFlags(args, cmd.Options), nil
}

func buildNexttrace(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid nexttrace target")
	}
	// nexttrace's argv parser is custom (not getopt) and treats `--` as an
	// unknown argument rather than a flag terminator, so we pass the target
	// bare. The HasPrefix("-") check above is the injection guard here.
	//
	// It also takes no options: with no flag terminator there is no position in
	// argv where a caller-influenced token is safe. optionSpecs["nexttrace"]
	// declares no keys, so normalizeOptions drops every token with a note
	// before this builder runs, and this builder ignores cmd.Options outright.
	return "nexttrace", []string{cmd.Target}, nil
}

func buildIperf3(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid iperf3 target")
	}
	if cmd.Options == "" {
		return "iperf3", []string{"-c", cmd.Target}, nil
	}
	// Always start with -c <target>, then append filtered options
	filtered := []string{"-c", cmd.Target}
	fields := strings.Fields(cmd.Options)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if allowedIperf3Flags[f] {
			// Validate port range for -p flag
			if f == "-p" {
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					port, err := strconv.Atoi(fields[i+1])
					if err != nil || port < 1024 || port > 65535 {
						i++ // skip invalid port value
						continue
					}
					filtered = append(filtered, f, fields[i+1])
					i++
				}
				continue
			}
			// Validate duration for -t flag (cap at 60s to prevent abuse)
			if f == "-t" {
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					dur, err := strconv.Atoi(fields[i+1])
					if err != nil || dur < 1 || dur > 60 {
						i++ // skip invalid duration
						continue
					}
					filtered = append(filtered, f, fields[i+1])
					i++
				}
				continue
			}
			// Validate parallel streams for -P flag (cap at 10)
			if f == "-P" {
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					streams, err := strconv.Atoi(fields[i+1])
					if err != nil || streams < 1 || streams > 10 {
						i++ // skip invalid value
						continue
					}
					filtered = append(filtered, f, fields[i+1])
					i++
				}
				continue
			}
			// Validate bandwidth for -b flag (cap at 100Mbit/s to prevent UDP flood)
			if f == "-b" {
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					bw, err := strconv.Atoi(fields[i+1])
					if err != nil || bw < 1 || bw > maxIperf3Bandwidth {
						i++ // skip invalid value
						continue
					}
					filtered = append(filtered, f, fields[i+1])
					i++
				}
				continue
			}
			// Validate byte count for -n flag (cap at 1GB)
			if f == "-n" {
				if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					n, err := strconv.Atoi(fields[i+1])
					if err != nil || n < 1 || n > maxIperf3Bytes {
						i++ // skip invalid value
						continue
					}
					filtered = append(filtered, f, fields[i+1])
					i++
				}
				continue
			}
			// Boolean flags (-R, -u, -4, -6, --reverse) — append without consuming a next argument
			filtered = append(filtered, f)
		}
	}
	return "iperf3", filtered, nil
}

func buildSpeedtest(cmd CommandRequest) (string, []string, error) {
	return "speedtest", []string{}, nil
}

// buildHTTP runs a single curl request with timing breakdown — DNS lookup,
// TCP connect, TLS handshake, time-to-first-byte, and total. Useful for
// catching slow CDNs, TLS issues, or upstream stalls that ICMP ping can't see.
//
// Single-shot by design: the cold-cache request is the most realistic. To
// get a warm-connection number, the user re-runs.
func buildHTTP(cmd CommandRequest) (string, []string, error) {
	target := strings.TrimSpace(cmd.Target)
	if target == "" {
		return "", nil, fmt.Errorf("HTTP probe requires a target URL or hostname")
	}
	// Reject obvious shell metacharacters and spaces — curl would refuse
	// most of these but it's cheaper to fail fast with a clear message.
	if strings.ContainsAny(target, " \t\n\r\"'`$|&;<>\\") {
		return "", nil, fmt.Errorf("invalid characters in HTTP target")
	}
	// Decide whether the user gave a bare hostname or a full URL. We reject
	// any explicit scheme that isn't http/https BEFORE prefixing — otherwise
	// "ftp://host" would silently become "https://ftp://host" which curl will
	// happily resolve to a confusing place. We also reject single-colon URI
	// schemes like javascript:, mailto:, data: even though they don't use ://.
	if i := strings.Index(target, "://"); i >= 0 {
		scheme := strings.ToLower(target[:i])
		if scheme != "http" && scheme != "https" {
			return "", nil, fmt.Errorf("HTTP probe scheme must be http or https")
		}
	} else {
		// No scheme at all. Reject anything containing a colon — `host:port`
		// is ambiguous with URI schemes like `javascript:1` and disabling
		// inline ports is safer than parsing them. For an explicit port,
		// users can write http://host:port.
		if strings.Contains(target, ":") {
			return "", nil, fmt.Errorf("HTTP probe target needs an explicit http:// or https:// when using a port or scheme")
		}
		target = "https://" + target
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return "", nil, fmt.Errorf("invalid HTTP target")
	}
	args := []string{
		"--globoff", "-sS", "-o", "/dev/null",
		"-w", "code=%{http_code}  dns=%{time_namelookup}s  connect=%{time_connect}s  tls=%{time_appconnect}s  ttfb=%{time_starttransfer}s  total=%{time_total}s  size=%{size_download}b\n",
		"--max-time", "15",
		"-A", "parallax-probe/1.0",
	}
	// method=get is curl's default and adds nothing; method=head adds --head.
	// There is no redirect-following option: --location would let a response
	// walk the probe onto a host the operator never named (F3).
	if flag := optArgvToken("http", "method", cmd.Options); flag != "" {
		args = append(args, flag)
	}
	args = append(args, "--", target)
	args = appendIPVersionFlags(args, cmd.Options)
	// -q must be the first argument: disable ~/.curlrc so local configuration
	// cannot silently enable redirects, extra URLs, uploads or output files.
	return "curl", append([]string{"-q"}, args...), nil
}

// buildDns formats `[+short] [+trace] [+dnssec] target [TYPE]` — or `-x ip` for
// a PTR of an IP literal — with the IP-version flag appended *last*, after the
// positional arguments, which is where dig has always taken it here.
//
// dig deliberately gets no `--`: its argument parser has no end-of-options
// marker and would read `--` as a query name. The injection guard is therefore
// the target-prefix rejection below, which covers every character dig treats as
// special in argument position: `-` (flag), `@` (server) and `+` (query option).
func buildDns(cmd CommandRequest) (string, []string, error) {
	if strings.HasPrefix(cmd.Target, "-") {
		return "", nil, fmt.Errorf("invalid dns target")
	}
	// Reject @server syntax to prevent querying attacker-controlled DNS servers
	if strings.HasPrefix(cmd.Target, "@") {
		return "", nil, fmt.Errorf("invalid dns target")
	}
	// Reject +query-option syntax for the same reason: with no `--` available,
	// a target starting with `+` would become a dig option (F6).
	if strings.HasPrefix(cmd.Target, "+") {
		return "", nil, fmt.Errorf("invalid dns target")
	}
	var args []string
	for _, key := range []string{"short", "trace", "dnssec"} {
		if flag := optArgvToken("dns", key, cmd.Options); flag != "" {
			args = append(args, flag)
		}
	}
	// The record type reaches argv only through optionSpecs["dns"]'s identity
	// enum over validDNSTypes. The old "any leftover token becomes the type"
	// fallthrough is gone (F6); a bare legacy token such as "MX" is rescued in
	// normalizeOptions, against the same table, and never here.
	recordType := optArgvToken("dns", "type", cmd.Options)
	// If the user asked for a PTR record and the target parses as an IP,
	// use `dig -x` so they don't have to construct the .in-addr.arpa name
	// by hand. Falls back to the literal target for non-IP PTR queries
	// (e.g. someone passing 1.0.0.10.in-addr.arpa explicitly).
	if recordType == "PTR" && net.ParseIP(cmd.Target) != nil {
		args = append(args, "-x", cmd.Target)
	} else if recordType != "" {
		args = append(args, cmd.Target, recordType)
	} else {
		args = append(args, cmd.Target)
	}
	if ipFlag := ipVersionFlag(cmd.Options); ipFlag != "" {
		args = append(args, ipFlag)
	}
	return "dig", args, nil
}

// buildCommandArgs turns one CommandRequest into the binary and argv to exec,
// plus any advisory note lines the caller must emit.
//
// It is internal — executeCommand is its only caller — which is why it can
// return notes without touching the frozen commandBuilder signature. The notes
// travel on the call stack: there is deliberately no package-level note map, so
// two commands running concurrently cannot see each other's notes.
//
// An error return means the command is refused; the caller emits the error and
// *not* the notes, because in that case the error is the message.
func buildCommandArgs(cmd CommandRequest) (string, []string, []string, error) {
	builder, ok := commandBuilders[cmd.Type]
	if !ok {
		return "", nil, nil, fmt.Errorf("unsupported command: %s", cmd.Type)
	}
	spec, governed := optionSpecs[cmd.Type]
	if !governed || spec.raw {
		// Raw: cmd.Options reaches the builder byte-for-byte — no grammar, no
		// token cap, no notes. iperf3 and speedtest are declared raw; a type
		// with a builder but no spec entry (see optionSpecs) lands here too.
		binary, args, err := builder(cmd)
		return binary, args, nil, err
	}
	normalized, notes, err := normalizeOptions(cmd.Type, spec, cmd.Options)
	if err != nil {
		return "", nil, nil, err
	}
	// The builder sees a copy carrying only validated options; cmd itself keeps
	// the caller's original string for logging.
	normCmd := cmd
	normCmd.Options = normalized
	binary, args, err := builder(normCmd)
	if err != nil {
		return "", nil, nil, err
	}
	return binary, args, notes, nil
}

// executeCommand runs one whitelisted probe and streams its output. Every
// return path ends in exactly one `done` message whose payload states whether
// the process exited cleanly (F35): exit_ok is true only when Wait() returned
// nil and the output cap was not hit. A structured `summary` message is sent
// immediately before `done`, and only when exit_ok is true.
func executeCommand(conn *websocket.Conn, cmd CommandRequest) {
	executeCommandContext(context.Background(), conn, cmd)
}

func executeCommandContext(parent context.Context, conn *websocket.Conn, cmd CommandRequest) {
	// Field-length caps (F5). backend/client_ws.go enforces exactly these three
	// before it dispatches, so this is defence in depth for an agent that would
	// otherwise trust whatever its server sends — including a compromised or
	// downgraded server. Checked before the type lookup and before any logging,
	// so an oversize field never reaches either.
	if len(cmd.Type) > 32 || len(cmd.Target) > 1024 || len(cmd.Options) > 512 {
		sendRequestOutput(parent, conn, cmd.ID, "error", "Command fields too long")
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	if !allowedCommands[cmd.Type] {
		sendRequestOutput(parent, conn, cmd.ID, "error", fmt.Sprintf("Unknown command type: %s", cmd.Type))
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	// Sanitize target: reject control characters (newlines, tabs, etc.) that could
	// pollute logs or confuse command-line tools
	if strings.ContainsAny(cmd.Target, "\n\r\t\x00") {
		sendRequestOutput(parent, conn, cmd.ID, "error", "Target contains invalid characters")
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	// Reject options with control characters for consistency
	if strings.ContainsAny(cmd.Options, "\n\r\t\x00") {
		sendRequestOutput(parent, conn, cmd.ID, "error", "Options contain invalid characters")
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	log.Printf("Executing command: %s %q (id=%q)", cmd.Type, cmd.Target, cmd.ID)

	// Validate target is non-empty for commands that require one
	if cmd.Type != "speedtest" && strings.TrimSpace(cmd.Target) == "" {
		sendRequestOutput(parent, conn, cmd.ID, "error", "Target is required")
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	// Native probes (S7) branch here: after the preamble above (field caps,
	// allowedCommands, control-char checks, target-required) and before
	// buildCommandArgs, which they have no builder for. They take no options,
	// so normalizeOptions deliberately never runs for them.
	if probe, native := nativeProbes[cmd.Type]; native {
		runNativeProbeContext(parent, conn, cmd, probe)
		return
	}

	binary, args, notes, err := buildCommandArgs(cmd)
	if err != nil {
		sendRequestOutput(parent, conn, cmd.ID, "error", err.Error())
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	// Advisory lines about options that were dropped or clamped out, emitted
	// before the process starts so they precede its output. These are per-call
	// values off the stack, never shared state, so concurrent commands cannot
	// cross-talk.
	for _, note := range notes {
		sendRequestOutput(parent, conn, cmd.ID, "output", note)
	}

	// Register cancellation, not exec.Cmd.Process: Start assigns Process and
	// races with cancel/disconnect if those handlers inspect it directly.
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	run, refusal := registerExecRun(cmd.ID, cancel)
	if refusal != "" {
		sendRequestOutput(parent, conn, cmd.ID, "error", refusal)
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}
	defer unregisterExecRun(cmd.ID, run)
	execCmd := exec.CommandContext(ctx, binary, args...)
	execCmd.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1")

	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		sendRequestOutput(parent, conn, cmd.ID, "error", fmt.Sprintf("Failed to create pipe: %v", err))
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}
	execCmd.Stderr = execCmd.Stdout
	// Kill the process group, including tool helpers inheriting stdout. Close
	// our pipe as well so cancellation always wakes a blocked scanner.
	execCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	execCmd.Cancel = func() error {
		stdout.Close()
		return syscall.Kill(-execCmd.Process.Pid, syscall.SIGKILL)
	}

	if err := execCmd.Start(); err != nil {
		sendRequestOutput(parent, conn, cmd.ID, "error", fmt.Sprintf("Failed to start command: %v", err))
		sendRequestOutput(parent, conn, cmd.ID, "done", doneData(false))
		return
	}

	// Bounded tail of the output, kept only for command types we can parse.
	// The full output is never retained — see agent/summary.go.
	var tail *tailBuffer
	if hasSummaryParser(cmd.Type) {
		tail = newTailBuffer()
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max line to handle iperf3/speedtest output
	totalOutputBytes := 0
	outputCapped := false
	for scanner.Scan() {
		line := ansiRegex.ReplaceAllString(scanner.Text(), "")
		line = strings.ReplaceAll(line, "\x1b", "")
		// Count raw bytes and line delimiters, including stripped escapes and
		// blank lines, so sanitization cannot bypass the output budget.
		totalOutputBytes += len(scanner.Bytes()) + 1
		if totalOutputBytes > maxOutputBytes {
			sendRequestOutput(parent, conn, cmd.ID, "error", "Output size limit exceeded, aborting command")
			outputCapped = true
			cancel()
			break
		}
		tail.add(line)
		if err := sendRequestOutput(parent, conn, cmd.ID, "output", line); err != nil {
			// Write failed (connection dead), kill the command to free resources
			cancel()
			break
		}
	}

	if err := scanner.Err(); err != nil {
		outputCapped = true // incomplete output must never produce a success summary
		cancel()
		sendRequestOutput(parent, conn, cmd.ID, "error", fmt.Sprintf("Failed to read command output: %v", err))
	}

	// Wait with a bounded timeout so an uninterruptible process (e.g. stuck in
	// a syscall on a hung filesystem) cannot leak a goroutine forever.
	exitOK := false
	waitCh := make(chan error, 1)
	go func() { waitCh <- execCmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			if !outputCapped {
				sendRequestOutput(parent, conn, cmd.ID, "error", fmt.Sprintf("Command exited with error: %v", err))
			}
		} else if !outputCapped {
			exitOK = true
		}
	case <-time.After(15 * time.Second):
		// Process didn't exit within 15s of EOF on its stdout. Force-kill once
		// more and abandon the wait — the OS will reap the zombie eventually.
		cancel()
		sendRequestOutput(parent, conn, cmd.ID, "error", "Command process did not exit cleanly; abandoned")
	}

	// A summary is only meaningful for a run that completed, and it must
	// arrive before `done` — the server tears the routing down on `done`.
	if exitOK {
		if blob := summaryJSON(cmd.Type, tail.tail()); blob != nil {
			sendRequestOutput(parent, conn, cmd.ID, "summary", string(blob))
		}
	}
	sendRequestOutput(parent, conn, cmd.ID, "done", doneData(exitOK))
}

type HealthInfo struct {
	Uptime    string  `json:"uptime"`
	LoadAvg   string  `json:"load_avg"`
	MemUsedPc float64 `json:"mem_used_pc"`
	CPUs      int     `json:"cpus"`
	OS        string  `json:"os"`
}

func gatherHealth() HealthInfo {
	h := HealthInfo{
		CPUs: runtime.NumCPU(),
		OS:   runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Uptime
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) > 0 {
			h.Uptime = parts[0] + "s"
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, healthBin("uptime")).Output()
		cancel()
		if err == nil {
			h.Uptime = strings.TrimSpace(string(out))
		}
	}

	// Load average
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			h.LoadAvg = strings.Join(parts[:3], " ")
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, healthBin("sysctl"), "-n", "vm.loadavg").Output()
		cancel()
		if err == nil {
			h.LoadAvg = strings.TrimSpace(strings.Trim(string(out), "{ }"))
		}
	}

	// Memory usage
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail int64
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "MemTotal:"):
				if _, e := fmt.Sscanf(line, "MemTotal: %d kB", &total); e != nil {
					log.Printf("Failed to parse MemTotal: %v", e)
				}
			case strings.HasPrefix(line, "MemAvailable:"):
				if _, e := fmt.Sscanf(line, "MemAvailable: %d kB", &avail); e != nil {
					log.Printf("Failed to parse MemAvailable: %v", e)
				}
			}
		}
		if total > 0 {
			h.MemUsedPc = float64(total-avail) / float64(total) * 100
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, healthBin("vm_stat")).Output()
		cancel()
		if err == nil {
			lines := strings.Split(string(out), "\n")
			var free, active, inactive, speculative, wired int64
			for _, l := range lines {
				l = strings.ReplaceAll(l, ".", "")
				switch {
				case strings.HasPrefix(l, "Pages free"):
					if _, e := fmt.Sscanf(l, "Pages free: %d", &free); e != nil {
						log.Printf("Failed to parse vm_stat Pages free: %v", e)
					}
				case strings.HasPrefix(l, "Pages active"):
					if _, e := fmt.Sscanf(l, "Pages active: %d", &active); e != nil {
						log.Printf("Failed to parse vm_stat Pages active: %v", e)
					}
				case strings.HasPrefix(l, "Pages inactive"):
					if _, e := fmt.Sscanf(l, "Pages inactive: %d", &inactive); e != nil {
						log.Printf("Failed to parse vm_stat Pages inactive: %v", e)
					}
				case strings.HasPrefix(l, "Pages speculative"):
					if _, e := fmt.Sscanf(l, "Pages speculative: %d", &speculative); e != nil {
						log.Printf("Failed to parse vm_stat Pages speculative: %v", e)
					}
				case strings.HasPrefix(l, "Pages wired"):
					if _, e := fmt.Sscanf(l, "Pages wired down: %d", &wired); e != nil {
						log.Printf("Failed to parse vm_stat Pages wired: %v", e)
					}
				}
			}
			total := free + active + inactive + speculative + wired
			if total > 0 {
				used := active + wired
				h.MemUsedPc = float64(used) / float64(total) * 100
			}
		}
	}

	return h
}
