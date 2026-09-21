package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// S7 — native Go probes: tcp, tls, dnsbench, download.
//
// These four command types need no external binary, so they never reach
// commandBuilders/exec: executeCommand branches into runNativeProbe after its
// existing preamble (field caps → allowedCommands → control-char checks →
// target-required) and before buildCommandArgs. They take no options at all
// (optionSpecs holds a `{}`-shaped non-raw entry for each, and nothing here
// ever reads cmd.Options), obey the same output caps and cancellation as the
// exec path, and end every return path in exactly one `done`.
//
// ── Why the call shapes below are pinned (F7/F8) ──
//
// A probe that resolves a name, validates the addresses, and then hands the
// *name* to a dialer has validated nothing: the second lookup can answer
// differently (DNS rebinding, TTL 0, round-robin), and Happy Eyeballs will
// cheerfully connect to whatever comes back. So:
//
//   - every probe resolves exactly once through resolveAndCheck and afterwards
//     dials only a validated IP *literal*;
//   - tls never calls tls.Dial (which re-resolves): it dials the literal with a
//     net.Dialer and wraps the conn with tls.Client;
//   - download's transport receives the hostname in DialContext and substitutes
//     the pinned literal before dialing;
//   - the three target-dialing probes (tcp, tls, download) also install
//     net.Dialer.Control, which sees the final socket address of *every*
//     attempt — fan-out included — and refuses any address the policy blocks.
//     That makes the property true by construction rather than by vigilance.
//
// dnsbench deliberately gets **no** Control hook: it dials only already
// validated resolver literals (from resolv.conf or the constant public set),
// never a resolved answer, and private resolvers are exactly what most of the
// fleet has. Installing the hook there would fail every system-resolver row on
// any host with an RFC1918 resolver.

// ── Budgets and caps ──
const (
	// Per-probe deadlines. Each probe applies its own on top of the caller's
	// context, so the effective deadline is always the earlier of the two.
	tcpProbeDeadline      = 5 * time.Second
	tlsProbeDeadline      = 10 * time.Second
	dnsbenchQueryDeadline = 3 * time.Second

	// nativeCommandTimeout mirrors the exec path's 10-minute parent timeout.
	// The native branch has to build its own: executeCommand's context is
	// created further down, after the branch point.
	nativeCommandTimeout = 10 * time.Minute

	// maxNativeLineBytes matches the exec path's 1 MB scanner line cap.
	maxNativeLineBytes = 1024 * 1024

	// nativeFieldRunes rune-truncates every remote-controlled field *before*
	// it is interpolated into a line: certificate subject/issuer/SAN,
	// verification error text, DNS answers, HTTP error text. The 1 MB cap
	// above applies to the finished line; this one to the untrusted pieces
	// inside it. It equals the server's summaryMaxStringRunes so a summary
	// string is never silently truncated a second time.
	nativeFieldRunes = 256

	// maxConcurrentDownloads caps in-flight download probes per agent. The
	// server's per-client command cap counts distinct client-chosen command
	// IDs, so a client reusing one ID keeps that count at 1 while the agent
	// starts N goroutines (F37, server-side, not fixed here); without this cap
	// one repeated 200-byte frame pulls N × 100 MB from a URL of the
	// requester's choosing.
	maxConcurrentDownloads = 2

	// maxSystemResolvers matches glibc's MAXNS: resolv.conf entries past the
	// third are ignored by the system resolver, so benchmarking them would be
	// misleading as well as unbounded.
	maxSystemResolvers = 3

	// tlsDefaultPort is used when a tls target carries no port.
	tlsDefaultPort = "443"

	// maxDNSName is the DNS wire limit for a name, minus the root label.
	maxDNSName  = 253
	maxDNSLabel = 63

	// maxResolvConfBytes bounds the read of resolvConfPath.
	maxResolvConfBytes = 64 * 1024

	// maxReportedSANs bounds the SAN list one tls probe prints.
	maxReportedSANs = 8
)

var (
	// downloadDeadline covers dial, response headers *and* the body read. A
	// dial or header timeout alone would let a slowloris server trickle one
	// byte per second for the full 10-minute command timeout. It is a package
	// var only so the slowloris test does not have to take 20 seconds — the
	// production value is asserted by TestS7DownloadBudgetsAreTheContractValues
	// (same precedent as maxOutputBytes and alertTimeout).
	downloadDeadline = 20 * time.Second

	// maxDownloadBytes caps one download probe's body read. A var for the same
	// testability reason as downloadDeadline; the production value is pinned by
	// the same test.
	maxDownloadBytes int64 = 100 << 20

	// probeAllowPrivate is read once at startup from PROBE_ALLOW_PRIVATE (see
	// initProbeEnv) and never again, so a later environment change cannot widen
	// a running agent's reach. It relaxes only the private, loopback,
	// link-local-unicast, CGNAT and on-link classes — never unspecified,
	// multicast, broadcast, 0/8 or 240/4, none of which is ever diagnostic.
	//
	// An atomic.Bool rather than a plain bool because isBlockedAddr is called
	// from net.Dialer.Control, which runs on goroutines the http transport owns:
	// a plain bool would be a data race the moment anything but startup wrote it.
	probeAllowPrivate atomic.Bool

	// dnsbenchPublicResolvers is the compile-time constant public resolver set
	// (F11). It is deliberately not configurable: an env-supplied resolver
	// would be an attacker-chosen UDP destination.
	dnsbenchPublicResolvers = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}
)

// ── The three test seams ──
//
// Package vars with production defaults, following the maxOutputBytes /
// nativeProbeTypes precedent. Without them the P0 rebinding test, the on-link
// GUA test and the missing-resolv.conf test are unwritable: the cgo resolver
// cannot be made to answer public-then-loopback, and a build host without
// native IPv6 cannot produce an on-link GUA.
var (
	// probeLookupIPs is the single resolution point for every probe.
	probeLookupIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}

	// localPrefixSource enumerates this host's on-link prefixes.
	localPrefixSource = interfaceLocalPrefixes

	// resolvConfPath is where dnsbench reads the system resolvers from.
	resolvConfPath = "/etc/resolv.conf"
)

// initProbeEnv reads the one probe environment variable, once, at startup.
func initProbeEnv() {
	probeAllowPrivate.Store(os.Getenv("PROBE_ALLOW_PRIVATE") == "1")
	if probeAllowPrivate.Load() {
		log.Println("WARNING: PROBE_ALLOW_PRIVATE=1 — native probes (tcp/tls/download) may target private, loopback, link-local, CGNAT and on-link addresses on this node. Unset it unless this node exists to diagnose an internal network.")
	}
}

// ── Address policy ──

// neverAllowedPrefixes are blocked in every configuration, including with
// PROBE_ALLOW_PRIVATE=1: documentation, benchmarking, relay and translation
// space, plus the two v4 ranges (0/8, 240/4) that are never a diagnostic
// target. The relaxable classes live in isRelaxableAddr instead.
var neverAllowedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("192.88.99.0/24"),     // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking
	netip.MustParsePrefix("192.0.2.0/24"),       // TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"),    // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),     // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved
	netip.MustParsePrefix("255.255.255.255/32"), // broadcast
	netip.MustParsePrefix("::/128"),             // unspecified
	netip.MustParsePrefix("::/96"),              // deprecated IPv4-compatible
	netip.MustParsePrefix("::ffff:0:0:0/96"),    // IPv4-translated
	netip.MustParsePrefix("64:ff9b::/96"),       // NAT64 well-known
	netip.MustParsePrefix("64:ff9b:1::/48"),     // RFC 8215 local-use translation
	netip.MustParsePrefix("2001::/32"),          // Teredo
	netip.MustParsePrefix("2001:10::/28"),       // ORCHID
	netip.MustParsePrefix("2001:20::/28"),       // ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),      // documentation
	netip.MustParsePrefix("3fff::/20"),          // documentation
	netip.MustParsePrefix("2002::/16"),          // 6to4
}

var (
	// cgnatPrefix is 100.64/10 (RFC 6598). Carrier-grade NAT space is not
	// public but it is a real network an operator may want to diagnose, so it
	// is relaxable.
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
	// siteLocalV6Prefix is fec0::/10, the deprecated IPv6 site-local range.
	// It is *not* inside fc00::/7 and no netip helper covers it, so it needs
	// its own entry; it is site-private, hence relaxable.
	siteLocalV6Prefix = netip.MustParsePrefix("fec0::/10")
)

// isRelaxableAddr reports whether addr is in one of the classes
// PROBE_ALLOW_PRIVATE may unblock.
func isRelaxableAddr(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	return addr.IsLoopback() ||
		addr.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		addr.IsLinkLocalUnicast() ||
		cgnatPrefix.Contains(addr) ||
		siteLocalV6Prefix.Contains(addr)
}

// isBlockedAddr is the one address-policy function every probe and every dialer
// Control hook goes through. Canonicalize only for policy checks: Prefix.Contains
// refuses zoned IPv6 addresses, while the original address must retain its zone
// for dialing scoped targets when private probing is allowed.
func isBlockedAddr(addr netip.Addr) bool {
	a := addr.Unmap().WithZone("")
	if !a.IsValid() {
		return true
	}
	// Never diagnostic, never relaxable.
	if a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	// PROBE_ALLOW_PRIVATE relaxes its classes *before* the never-allowed
	// prefixes are consulted, because one address is in both lists: ::1 sits
	// inside the deprecated IPv4-compatible ::/96. An operator who opted into
	// loopback probing means ::1 too, and the exemption cannot reach anything
	// else — it covers exactly loopback, RFC1918/fc00::/7, link-local unicast,
	// CGNAT and fec0::/10.
	if probeAllowPrivate.Load() && isRelaxableAddr(a) {
		return false
	}
	for _, p := range neverAllowedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	// 6to4, IPv4-compatible and Teredo carry an IPv4 address inside them. The
	// enclosing prefixes above already refuse all three; re-checking the
	// embedded address keeps the hole closed if one of those prefixes is ever
	// narrowed.
	for _, embedded := range embeddedIPv4s(a) {
		if isBlockedAddr(embedded) {
			return true
		}
	}
	if !probeAllowPrivate.Load() {
		if isRelaxableAddr(a) {
			return true
		}
		// The hole a static prefix list cannot close: on a LAN with native
		// IPv6 every host holds a globally routable 2000::/3 address, the
		// agent sits inside that LAN, and the prefix is not secret — the agent
		// detects and reports its own public IPv6 and the server shows it to
		// every browser. A hostile name resolving to the router's GUA passes
		// every static check above, so anything on-link is refused too.
		if addrInLocalPrefix(a) {
			return true
		}
	}
	return false
}

// embeddedIPv4s returns the IPv4 address(es) encoded inside a v6 address for
// the three encodings that carry one, or nil.
func embeddedIPv4s(a netip.Addr) []netip.Addr {
	if !a.Is6() || a.Is4In6() {
		return nil
	}
	b := a.As16()
	switch {
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16 — 6to4: bytes 2..5
		return []netip.Addr{netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})}
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0: // 2001::/32 — Teredo
		// Server IPv4 in bytes 4..7, client IPv4 obfuscated (one's complement)
		// in bytes 12..15.
		return []netip.Addr{
			netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]}),
			netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}),
		}
	case isAllZero(b[:12]): // ::/96 — deprecated IPv4-compatible
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}
	case b[0] == 0 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && isAllZero(b[4:12]): // 64:ff9b::/96
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}
	}
	return nil
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// ── On-link prefix set: shared mutable state, replace-only ──
//
// Synchronized exactly like the S5 tool cache: the refresh builds a *fresh*
// slice and assigns it under localPrefixesMu. Readers hold that lock or use
// snapshotLocalPrefixes; the published slice is never edited in place.
// Refreshed at startup and on the tools-refresh ticker in main(), which always
// runs — not the IP-detection ticker, which only runs under -auto-ip and would
// leave -auto-ip=false nodes with a set frozen at boot.
var (
	localPrefixes            []netip.Prefix
	localPrefixesUnavailable bool
	localPrefixesMu          sync.RWMutex
)

func storeLocalPrefixes(prefixes []netip.Prefix) {
	localPrefixesMu.Lock()
	localPrefixes = prefixes
	localPrefixesUnavailable = false
	localPrefixesMu.Unlock()
}

// snapshotLocalPrefixes returns a copy. Every reader goes through it: the copy
// is what makes it safe to iterate outside the lock while the refresher
// replaces the published slice.
func snapshotLocalPrefixes() []netip.Prefix {
	localPrefixesMu.RLock()
	defer localPrefixesMu.RUnlock()
	out := make([]netip.Prefix, len(localPrefixes))
	copy(out, localPrefixes)
	return out
}

// refreshLocalPrefixes re-enumerates the host's interfaces and publishes a
// fresh set. Failed or partial enumeration blocks target connections until
// recovery; it must not silently discard the on-link boundary.
func refreshLocalPrefixes() {
	prefixes, err := localPrefixSource()
	if err != nil {
		localPrefixesMu.Lock()
		localPrefixesUnavailable = true
		localPrefixesMu.Unlock()
		log.Println("Cannot enumerate on-link prefixes; native target probes are blocked unless PROBE_ALLOW_PRIVATE=1")
		return
	}
	storeLocalPrefixes(prefixes)
}

func addrInLocalPrefix(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	localPrefixesMu.RLock()
	defer localPrefixesMu.RUnlock()
	if localPrefixesUnavailable {
		return true
	}
	for _, p := range localPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// interfaceLocalPrefixes lists every on-link prefix of every local interface,
// plus each local address itself as a host-length prefix (a point-to-point or
// /128 address has no wider prefix to hide behind). This covers on-link GUAs;
// off-link destinations translated by custom network-specific NAT64 prefixes
// still require an egress firewall enforcing the operator's network policy.
func interfaceLocalPrefixes() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, 8)
	seen := make(map[netip.Prefix]bool, 8)
	add := func(p netip.Prefix) {
		p = p.Masked()
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			add(netip.PrefixFrom(ip, ip.BitLen()))
			ones, bits := ipnet.Mask.Size()
			if bits == ip.BitLen() && ones > 0 {
				if p, err := ip.Prefix(ones); err == nil {
					add(p)
				}
			}
		}
	}
	return out, nil
}

// ── Resolution ──

// errBlockedAddress is the sentinel behind every address-policy refusal.
var errBlockedAddress = errors.New("address refused by the agent's probe address policy")

func blockedAddrError(what string, a netip.Addr) error {
	return fmt.Errorf("%s %s is not a valid probe target: %w", what, a, errBlockedAddress)
}

// resolveAndCheck resolves host exactly once and validates every answer.
//
// Three rules, all load-bearing:
//   - the lookup runs inside the probe's own deadline (a cgo resolver can burn
//     tens of seconds and make a 5-second budget a lie);
//   - zero addresses is a refusal;
//   - if *any* address is blocked the whole probe is refused, never filtered to
//     the survivors — a name resolving to one public and one loopback address
//     is an attack, not a partial success.
//
// The returned addresses are unmapped literals. Nothing downstream may resolve
// host again.
func resolveAndCheck(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("no host to resolve")
	}
	if len(host) > maxDNSName {
		return nil, fmt.Errorf("host is too long (%d bytes, max %d)", len(host), maxDNSName)
	}
	// An IP literal is already an address: no lookup, same policy.
	if literal, err := netip.ParseAddr(host); err == nil {
		a := literal.Unmap()
		if isBlockedAddr(a) {
			return nil, blockedAddrError("address", a)
		}
		return []netip.Addr{a}, nil
	}
	addrs, err := probeLookupIPs(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %s", clampProbeField(host), clampProbeField(err.Error()))
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolved to no addresses", clampProbeField(host))
	}
	out := make([]netip.Addr, 0, len(addrs))
	seen := make(map[netip.Addr]bool, len(addrs))
	for _, raw := range addrs {
		a := raw.Unmap()
		if isBlockedAddr(a) {
			return nil, blockedAddrError(clampProbeField(host)+" resolves to", a)
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, nil
}

// probeDialControl is net.Dialer.Control for the three target-dialing probes.
// It runs on the final socket address of every connection attempt — including
// every leg of Happy Eyeballs fan-out — which is what makes "a probe never
// touches a blocked address" a property of the code rather than a promise.
func probeDialControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial %s: unparsable socket address", clampProbeField(address))
	}
	a := ap.Addr().Unmap()
	if isBlockedAddr(a) {
		return blockedAddrError("dial address", a)
	}
	return nil
}

// probeDialer is the only dialer constructor tcp, tls and download use, so the
// Control hook cannot be forgotten on one of them.
func probeDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: probeDialControl}
}

// ── Output: the emitter ──

var (
	// errNativeOutputCapped and errNativeEmitFailed are the two probe errors
	// whose `error` frame is already sent (or pointless because the socket is
	// gone), so runNativeProbe emits only the `done` for them.
	errNativeOutputCapped = errors.New("output size limit exceeded")
	errNativeEmitFailed   = errors.New("send failed")
)

// nativeEmitter is one probe's output channel. It is owned by the single
// goroutine running that probe — never shared — which is why it needs no lock.
//
// It enforces the same caps as the exec path (1 MB per line, maxOutputBytes in
// total), sanitizes every line, and aborts the probe when sendOutput fails, the
// way the exec path kills its process on a failed write: otherwise a closed
// browser could leave a download running to its deadline.
type nativeEmitter struct {
	// send is the one way a line leaves this emitter. It is a field, not a
	// direct sendOutput call, so a test can drive a probe without a live
	// socket and assert what it emitted; sendOutput itself is unchanged.
	send func(outputType, data string) error

	total   int
	capped  bool
	summary map[string]any
}

// newNativeEmitter wires an emitter to one command on one connection.
func newNativeEmitter(ctx context.Context, conn *websocket.Conn, cmdID string) *nativeEmitter {
	return &nativeEmitter{send: func(outputType, data string) error {
		return sendRequestOutput(ctx, conn, cmdID, outputType, data)
	}}
}

func (e *nativeEmitter) emitLine(format string, args ...any) error {
	line := sanitizeProbeText(fmt.Sprintf(format, args...))
	if len(line) > maxNativeLineBytes {
		line = strings.ToValidUTF8(line[:maxNativeLineBytes], "")
	}
	e.total += len(line)
	if e.total > maxOutputBytes {
		e.capped = true
		e.send("error", "Output size limit exceeded, aborting command")
		return errNativeOutputCapped
	}
	if err := e.send("output", line); err != nil {
		return fmt.Errorf("%w: %v", errNativeEmitFailed, err)
	}
	return nil
}

// setSummary hands the dispatcher the flat map to send as the `summary` message
// just before `done`. The interface keeps run()'s signature at
// (ctx, cmd, emit) error; the summary rides out on the emitter.
func (e *nativeEmitter) setSummary(m map[string]any) {
	e.summary = m
}

// sanitizeProbeText strips ANSI sequences and a stray ESC exactly as the exec
// path does, then strips every C0 control character and DEL (a superset of the
// \r\n\t\x00 the contract names — a certificate CN is remote-controlled text
// and has no business carrying any of them). Nothing is dropped: only the
// offending bytes are removed, never the line.
func sanitizeProbeText(s string) string {
	s = ansiRegex.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x1b", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// clampProbeField sanitizes and rune-truncates one remote-controlled field
// before it is interpolated into a line or a summary value.
func clampProbeField(s string) string {
	return truncateProbeRunes(sanitizeProbeText(s), nativeFieldRunes)
}

func truncateProbeRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// ── Dispatch, registration and cancellation ──

// nativeProbe is one native command type's implementation.
type nativeProbe interface {
	run(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error
}

type probeFunc func(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error

func (f probeFunc) run(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
	return f(ctx, cmd, emit)
}

// nativeProbes is the native counterpart of commandBuilders: it is what
// satisfies the whitelist-sync invariant for types that have no binary. A test
// asserts it describes the same set as nativeProbeTypes, and that no type has
// both a builder and a native probe.
var nativeProbes = map[string]nativeProbe{
	"tcp":      probeFunc(probeTCP),
	"tls":      probeFunc(probeTLS),
	"dnsbench": probeFunc(probeDNSBench),
	"download": probeFunc(probeDownload),
}

// nativeRun is one in-flight native probe. cmdType is carried so the download
// cap can be counted from this one map instead of a second structure, and token
// so a probe only ever deletes its *own* registration (a connection teardown
// cancels and clears every entry, and a probe from the dead connection must not
// then delete a same-ID entry belonging to the new one).
type nativeRun struct {
	cmdType string
	cancel  context.CancelFunc
	token   uint64
}

// runningNative lives under the existing runningCmdsMu — the single mutex for
// both running maps — so the duplicate-ID check spans both.
var (
	runningNative    = make(map[string]nativeRun)
	nativeRunCounter atomic.Uint64
)

// registerNativeRun does the duplicate-ID check, the download count and the
// insert in one runningCmdsMu critical section: check-and-insert atomically,
// never check-then-insert. It returns the refusal message, or "" on success.
func registerNativeRun(cmd CommandRequest, token uint64, cancel context.CancelFunc) string {
	runningCmdsMu.Lock()
	defer runningCmdsMu.Unlock()
	if _, busy := runningCmds[cmd.ID]; busy {
		return "A command with this id is already running on this node."
	}
	if _, busy := runningNative[cmd.ID]; busy {
		return "A command with this id is already running on this node."
	}
	if cmd.Type == "download" {
		inFlight := 0
		for _, r := range runningNative {
			if r.cmdType == "download" {
				inFlight++
			}
		}
		if inFlight >= maxConcurrentDownloads {
			return fmt.Sprintf("Too many concurrent download probes on this node (limit %d). Try again when one finishes.", maxConcurrentDownloads)
		}
	}
	runningNative[cmd.ID] = nativeRun{cmdType: cmd.Type, cancel: cancel, token: token}
	return ""
}

// unregisterNativeRun removes this probe's own registration only.
func unregisterNativeRun(id string, token uint64) {
	runningCmdsMu.Lock()
	if r, ok := runningNative[id]; ok && r.token == token {
		delete(runningNative, id)
	}
	runningCmdsMu.Unlock()
}

// cancelNativeRunLocked cancels one native probe. The caller must hold
// runningCmdsMu — cancel only closes a channel, and the probe's deferred
// unregister blocks until the caller releases the lock, so calling it here
// cannot deadlock. Pinned: "fixing" this into a lock-free call would introduce
// the check-then-act race it avoids.
func cancelNativeRunLocked(id string) bool {
	r, ok := runningNative[id]
	if !ok {
		return false
	}
	r.cancel()
	return true
}

// cancelAllNativeRunsLocked is the connection-teardown path: cancel every
// in-flight native probe. The caller must hold runningCmdsMu.
//
// It deliberately does *not* delete the entries: each probe's deferred
// unregister (which blocks until this section releases the mutex) removes its
// own, so an empty runningNative really does mean every probe goroutine has
// finished. Clearing the map here would report "all stopped" while the probes
// were still winding down, and could delete a same-ID entry that already
// belongs to the *next* connection.
func cancelAllNativeRunsLocked() {
	for _, r := range runningNative {
		r.cancel()
	}
}

// runningNativeCount reports how many native probes are in flight, for tests.
func runningNativeCount() int {
	runningCmdsMu.Lock()
	defer runningCmdsMu.Unlock()
	return len(runningNative)
}

// failNative is the single refusal path: the `error` frame and then exactly one
// `done` carrying exit_ok:false, exactly as the exec path does on each of its
// returns. Returning silently instead would hold one of the client's 20 command
// slots for the life of the connection, leave the browser showing a running
// command, and strand a scheduled probe at `running` until the watchdog.
func failNative(ctx context.Context, conn *websocket.Conn, cmd CommandRequest, msg string) {
	sendRequestOutput(ctx, conn, cmd.ID, "error", sanitizeProbeText(msg))
	sendRequestOutput(ctx, conn, cmd.ID, "done", doneData(false))
}

// runNativeProbe is executeCommand's native branch.
func runNativeProbe(conn *websocket.Conn, cmd CommandRequest, probe nativeProbe) {
	runNativeProbeContext(context.Background(), conn, cmd, probe)
}

func runNativeProbeContext(parent context.Context, conn *websocket.Conn, cmd CommandRequest, probe nativeProbe) {
	// The native branch builds its own parent context: executeCommand's is
	// created below the branch point, so the 10-minute timeout is not
	// inherited.
	ctx, cancel := context.WithTimeout(parent, nativeCommandTimeout)
	defer cancel()

	token := nativeRunCounter.Add(1)
	if refusal := registerNativeRun(cmd, token, cancel); refusal != "" {
		failNative(ctx, conn, cmd, refusal)
		return
	}
	defer unregisterNativeRun(cmd.ID, token)

	emit := newNativeEmitter(ctx, conn, cmd.ID)
	err := probe.run(ctx, cmd, emit)
	switch {
	case err == nil:
		// A summary is only meaningful for a run that completed, and it must
		// arrive before `done` — the server tears the routing down on `done`.
		if blob := nativeSummaryJSON(emit.summary); blob != nil {
			sendRequestOutput(ctx, conn, cmd.ID, "summary", string(blob))
		}
		sendRequestOutput(ctx, conn, cmd.ID, "done", doneData(true))
	case errors.Is(err, errNativeOutputCapped), errors.Is(err, errNativeEmitFailed):
		// emitLine already sent the cap error, or the socket is gone and an
		// error frame would go nowhere. Either way: exactly one `done`.
		sendRequestOutput(ctx, conn, cmd.ID, "done", doneData(false))
	default:
		failNative(ctx, conn, cmd, err.Error())
	}
}

// nativeSummaryJSON marshals a probe's summary map after re-applying the
// server-side grammar the probe must stay inside (≤ summaryMaxKeys keys, flat
// values, strings ≤ nativeFieldRunes runes), because the server drops an
// off-grammar or oversize summary *wholesale*.
func nativeSummaryJSON(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	clean := make(map[string]any, len(m))
	for k, v := range m {
		if len(clean) >= 32 {
			break
		}
		key := clampProbeField(k)
		if key == "" {
			continue
		}
		switch tv := v.(type) {
		case string:
			clean[key] = clampProbeField(tv)
		case bool, int, int64, float64:
			clean[key] = tv
		default:
			// Anything else would be off-grammar; dropping the field is always
			// better than losing the whole summary.
		}
	}
	if len(clean) == 0 {
		return nil
	}
	blob, err := json.Marshal(clean)
	if err != nil || len(blob) > 16384 {
		return nil
	}
	return blob
}

// ── tcp ──

// probeTCP times one TCP connect to exactly one port.
//
// It needs no -allow-tcp-traceroute-style operator opt-in even though it is a
// connect primitive: unlike traceroute's -T, which has no address policy at
// all, every address tcp touches goes through isBlockedAddr, and its scan rate
// is bounded by the per-agent concurrency cap. The asymmetry with
// allowTCPTraceroute is deliberate.
func probeTCP(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
	host, port, err := splitSingleHostPort(cmd.Target)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, tcpProbeDeadline)
	defer cancel()

	if err := emit.emitLine("TCP connect to %s:%s (deadline %s)", clampProbeField(host), port, tcpProbeDeadline); err != nil {
		return err
	}
	addrs, err := resolveAndCheck(ctx, host)
	if err != nil {
		return err
	}
	if err := emit.emitLine("resolved to %s", joinAddrs(addrs)); err != nil {
		return err
	}

	dialer := probeDialer(0) // the context carries the deadline
	var lastErr error
	for _, a := range addrs {
		// The validated literal, never the hostname again.
		target := net.JoinHostPort(a.String(), port)
		start := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", target)
		elapsed := time.Since(start)
		if err != nil {
			lastErr = err
			if emitErr := emit.emitLine("%s — failed after %s: %s", target, round3ms(elapsed), clampProbeField(err.Error())); emitErr != nil {
				return emitErr
			}
			continue
		}
		conn.Close()
		ms := round3(float64(elapsed.Microseconds()) / 1000)
		if err := emit.emitLine("%s — connected in %g ms", target, ms); err != nil {
			return err
		}
		emit.setSummary(map[string]any{"connect_ms": ms})
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("no address to connect to")
	}
	return fmt.Errorf("could not connect to %s:%s: %s", clampProbeField(host), port, clampProbeField(lastErr.Error()))
}

// splitSingleHostPort accepts exactly one host and exactly one numeric port. A
// comma- or space-separated port list is rejected, never truncated to its first
// element — silently probing one port of a list the user meant as a scan would
// be a surprising success.
func splitSingleHostPort(target string) (string, string, error) {
	target = strings.TrimSpace(target)
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", "", fmt.Errorf("target must be host:port with exactly one port (%s)", clampProbeField(err.Error()))
	}
	if strings.TrimSpace(host) == "" {
		return "", "", errors.New("target must be host:port — the host is empty")
	}
	if !isAllDigits(port) || len(port) > 5 {
		return "", "", fmt.Errorf("port %q must be a single number between 1 and 65535", clampProbeField(port))
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("port %q must be a single number between 1 and 65535", clampProbeField(port))
	}
	return host, strconv.Itoa(n), nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func joinAddrs(addrs []netip.Addr) string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return strings.Join(out, ", ")
}

func round3ms(d time.Duration) string {
	return strconv.FormatFloat(round3(float64(d.Microseconds())/1000), 'g', -1, 64) + " ms"
}

// ── tls ──

// probeTLS inspects the certificate one endpoint presents.
//
// Two pinned decisions:
//
//   - It never calls tls.Dial, which would resolve the name a second time. It
//     dials the validated literal with probeDialer and wraps the conn with
//     tls.Client.
//   - InsecureSkipVerify is on *and* the chain is verified by hand. With
//     verification on, an expired certificate is a transport error — which is
//     precisely the case this probe exists to report. With InsecureSkipVerify
//     and no manual verify, it would report chain_ok:true for an expired,
//     self-signed or mismatched certificate: a clean bill of health from a tool
//     whose only job is that verdict. This config never leaves this function.
//
// Go's client MinVersion is TLS 1.2, so a legacy TLS 1.0/1.1 endpoint — one of
// the more interesting things to find — appears as a handshake failure rather
// than a reported weak version. Accepted for this slice.
func probeTLS(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
	host, port, isIP, err := splitTLSTarget(cmd.Target)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, tlsProbeDeadline)
	defer cancel()

	if err := emit.emitLine("TLS handshake with %s:%s (deadline %s)", clampProbeField(host), port, tlsProbeDeadline); err != nil {
		return err
	}
	addrs, err := resolveAndCheck(ctx, host)
	if err != nil {
		return err
	}
	if err := emit.emitLine("resolved to %s", joinAddrs(addrs)); err != nil {
		return err
	}

	target := net.JoinHostPort(addrs[0].String(), port)
	rawConn, err := probeDialer(0).DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("could not connect to %s: %s", target, clampProbeField(err.Error()))
	}
	defer rawConn.Close()

	cfg := &tls.Config{
		// See the doc comment: verification is done by hand below so an
		// expired or untrusted certificate is a *reported finding* instead of
		// a transport error.
		InsecureSkipVerify: true, //nolint:gosec // deliberate; manual x509 verification follows
		MinVersion:         tls.VersionTLS12,
	}
	if isIP {
		// No SNI for an IP literal: sending one would be a lie, and many
		// servers answer with a default certificate anyway.
		cfg.ServerName = ""
	} else {
		cfg.ServerName = host
	}

	start := time.Now()
	tlsConn := tls.Client(rawConn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake with %s failed: %s", target, clampProbeField(err.Error()))
	}
	handshake := time.Since(start)
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("%s presented no certificate", target)
	}
	leaf := state.PeerCertificates[0]

	if err := emit.emitLine("handshake: %s, %s, %s", tlsVersionName(state.Version), tls.CipherSuiteName(state.CipherSuite), round3ms(handshake)); err != nil {
		return err
	}
	if isIP {
		if err := emit.emitLine("note: the target is an IP literal, so no SNI was sent and the certificate is checked against its IP SANs"); err != nil {
			return err
		}
	}
	if err := emit.emitLine("subject: %s", clampProbeField(leaf.Subject.String())); err != nil {
		return err
	}
	issuer := clampProbeField(leaf.Issuer.String())
	if err := emit.emitLine("issuer: %s", issuer); err != nil {
		return err
	}
	days := int(math.Floor(time.Until(leaf.NotAfter).Hours() / 24))
	if err := emit.emitLine("valid: %s → %s (%d day(s) remaining)",
		leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339), days); err != nil {
		return err
	}
	if sans := reportedSANs(leaf); sans != "" {
		if err := emit.emitLine("SAN: %s", sans); err != nil {
			return err
		}
	}
	if len(state.PeerCertificates) > 1 {
		if err := emit.emitLine("chain: %d certificate(s) presented", len(state.PeerCertificates)); err != nil {
			return err
		}
	}

	chainOK, verifyErr := verifyPeerChain(leaf, state.PeerCertificates[1:], host)
	if chainOK {
		if err := emit.emitLine("chain: ok — verified against the system roots"); err != nil {
			return err
		}
	} else {
		if err := emit.emitLine("chain: NOT ok — %s", clampProbeField(verifyErr)); err != nil {
			return err
		}
	}

	summary := map[string]any{
		"days_remaining": days,
		"issuer":         issuer,
		"chain_ok":       chainOK,
	}
	if !chainOK {
		summary["verify_error"] = clampProbeField(verifyErr)
	}
	emit.setSummary(summary)
	// A failed verification is the probe's *finding*, not its failure: exit_ok
	// stays true so the summary and its badge reach the browser.
	return nil
}

// verifyPeerChain does the verification tls.Config was told to skip: system
// roots, the presented intermediates, the current time and the target name
// (x509 checks an IP literal against IPAddresses, a name against DNSNames).
func verifyPeerChain(leaf *x509.Certificate, rest []*x509.Certificate, host string) (bool, string) {
	intermediates := x509.NewCertPool()
	for _, c := range rest {
		intermediates.AddCert(c)
	}
	opts := x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		Roots:         nil, // system roots
		CurrentTime:   time.Now(),
	}
	if _, err := leaf.Verify(opts); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func reportedSANs(leaf *x509.Certificate) string {
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	for _, n := range leaf.DNSNames {
		names = append(names, clampProbeField(n))
	}
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return ""
	}
	extra := 0
	if len(names) > maxReportedSANs {
		extra = len(names) - maxReportedSANs
		names = names[:maxReportedSANs]
	}
	out := strings.Join(names, ", ")
	if extra > 0 {
		out += fmt.Sprintf(" (+%d more)", extra)
	}
	return out
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	default:
		return fmt.Sprintf("TLS version 0x%04x", v)
	}
}

// splitTLSTarget accepts `host`, `host:port`, an IP literal or `[v6]:port`.
func splitTLSTarget(target string) (host, port string, isIP bool, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", false, errors.New("target is required")
	}
	// A bare IP literal (including a bare IPv6 address, which is full of
	// colons) must be recognised before any host:port split.
	if a, perr := netip.ParseAddr(target); perr == nil {
		return a.Unmap().String(), tlsDefaultPort, true, nil
	}
	if h, p, serr := net.SplitHostPort(target); serr == nil {
		if !isAllDigits(p) || len(p) > 5 {
			return "", "", false, fmt.Errorf("port %q must be a single number between 1 and 65535", clampProbeField(p))
		}
		n, cerr := strconv.Atoi(p)
		if cerr != nil || n < 1 || n > 65535 {
			return "", "", false, fmt.Errorf("port %q must be a single number between 1 and 65535", clampProbeField(p))
		}
		if strings.TrimSpace(h) == "" {
			return "", "", false, errors.New("target host is empty")
		}
		if a, perr := netip.ParseAddr(h); perr == nil {
			return a.Unmap().String(), strconv.Itoa(n), true, nil
		}
		return h, strconv.Itoa(n), false, nil
	}
	if strings.Contains(target, ":") {
		return "", "", false, errors.New("target must be host or host:port with exactly one port")
	}
	return target, tlsDefaultPort, false, nil
}

// ── dnsbench ──

// benchResolver is one resolver row: a validated IP literal with port 53
// pinned, plus where it came from.
type benchResolver struct {
	addr   netip.AddrPort
	source string // "system" or "public"
}

// probeDNSBench queries one name against the system resolvers and a constant
// public set, sequentially, and reports each resolver's latency.
//
// Two residuals accepted on the record: the results disclose the internal
// resolver's address and behaviour to whoever ran the probe, and a wildcard
// attacker-controlled name makes that resolver emit an outbound query revealing
// its egress IP.
func probeDNSBench(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
	return probeDNSBenchResolvers(ctx, cmd, emit, dnsbenchResolvers())
}

func probeDNSBenchResolvers(ctx context.Context, cmd CommandRequest, emit *nativeEmitter, resolvers []benchResolver) error {
	fqdn, err := validateDNSBenchName(cmd.Target)
	if err != nil {
		return err
	}
	if len(resolvers) == 0 {
		return errors.New("no resolvers to query")
	}
	if err := emit.emitLine("dnsbench %s — %d resolver(s), %s each, queried sequentially",
		fqdn, len(resolvers), dnsbenchQueryDeadline); err != nil {
		return err
	}

	fastest := math.Inf(1)
	fastestAddr := ""
	answered := 0
	for _, r := range resolvers {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("dnsbench cancelled: %s", err)
		}
		answers, elapsed, qerr := dnsbenchQuery(ctx, r, fqdn)
		ms := round3(float64(elapsed.Microseconds()) / 1000)
		if qerr != nil {
			if err := emit.emitLine("%-24s %-7s %8g ms  error: %s", r.addr.String(), r.source, ms, clampProbeField(qerr.Error())); err != nil {
				return err
			}
			continue
		}
		answered++
		if ms < fastest {
			fastest, fastestAddr = ms, r.addr.Addr().String()
		}
		if err := emit.emitLine("%-24s %-7s %8g ms  %d answer(s)  %s",
			r.addr.String(), r.source, ms, len(answers), clampProbeField(strings.Join(answers, ", "))); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if answered == 0 {
		return fmt.Errorf("no resolver answered for %s", fqdn)
	}
	if err := emit.emitLine("fastest: %s at %g ms", fastestAddr, fastest); err != nil {
		return err
	}
	emit.setSummary(map[string]any{
		"fastest_ms":     fastest,
		"resolver_count": len(resolvers),
	})
	return nil
}

// validateDNSBenchName checks the name before it is used (F11) and returns it
// as an FQDN. At least one dot is required (W5): a single-label name goes
// through the resolver's search list and turns the probe into an internal-name
// oracle.
func validateDNSBenchName(target string) (string, error) {
	name := strings.TrimSpace(target)
	if name == "" {
		return "", errors.New("target is required")
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return "", errors.New("target is required")
	}
	if len(name) > maxDNSName {
		return "", fmt.Errorf("name is %d bytes, longer than the %d-byte DNS limit", len(name), maxDNSName)
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return "", errors.New("dnsbench takes a domain name, not an IP literal")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", errors.New("dnsbench needs a fully qualified name with at least one dot — a single label would be expanded through this node's search list")
	}
	for _, label := range labels {
		if label == "" {
			return "", errors.New("name has an empty label")
		}
		if len(label) > maxDNSLabel {
			return "", fmt.Errorf("label %q is longer than %d bytes", clampProbeField(label), maxDNSLabel)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("label %q starts or ends with a hyphen", clampProbeField(label))
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return "", fmt.Errorf("label %q has a character outside letters, digits and hyphen", clampProbeField(label))
			}
		}
	}
	// Query as an FQDN so the resolver's search list is never consulted.
	return name + ".", nil
}

// dnsbenchResolvers assembles the resolver list: up to maxSystemResolvers IP
// literals from resolvConfPath, then the constant public set, deduplicated with
// the system entry winning so a resolver is never benchmarked twice.
//
// System resolvers are deliberately exempt from isBlockedAddr: they are
// legitimately private, this probe never dials the *answer*, and sending a name
// to a resolver is what a resolver is for.
func dnsbenchResolvers() []benchResolver {
	out := make([]benchResolver, 0, maxSystemResolvers+len(dnsbenchPublicResolvers))
	seen := make(map[netip.Addr]bool, maxSystemResolvers+len(dnsbenchPublicResolvers))
	add := func(a netip.Addr, source string) {
		a = a.Unmap()
		if !a.IsValid() || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, benchResolver{addr: netip.AddrPortFrom(a, 53), source: source})
	}
	for _, a := range systemResolvers() {
		add(a, "system")
	}
	for _, s := range dnsbenchPublicResolvers {
		if a, err := netip.ParseAddr(s); err == nil {
			add(a, "public")
		}
	}
	return out
}

// systemResolvers reads up to maxSystemResolvers nameserver literals from
// resolvConfPath. A missing, unreadable or empty file yields none — the public
// set still gives the probe something to compare against. Only IP literals are
// accepted: a nameserver line naming a host would have to be resolved by the
// resolver we are trying to measure.
func systemResolvers() []netip.Addr {
	file, err := os.Open(resolvConfPath)
	if err != nil {
		return nil
	}
	defer file.Close()
	data := make([]byte, maxResolvConfBytes)
	n, err := io.ReadFull(file, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil
	}
	data = data[:n]
	var out []netip.Addr
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		// Preserve IPv6 scope zones: a link-local resolver cannot be reached
		// without its interface, and AddrPort carries zones through dialing.
		a, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}
		out = append(out, a.Unmap())
		if len(out) >= maxSystemResolvers {
			break
		}
	}
	return out
}

// dnsbenchResolverDialer is the dnsbench-only dialer. It carries **no** Control
// hook, deliberately: see the file header. It dials exactly the pinned resolver
// literal; callers cannot substitute a different destination.
func dnsbenchResolverDialer(r benchResolver) func(context.Context, string, string) (net.Conn, error) {
	pinned := r.addr.String()
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := &net.Dialer{}
		return d.DialContext(ctx, network, pinned)
	}
}

// dnsbenchQuery asks one resolver for one name and times it.
func dnsbenchQuery(ctx context.Context, r benchResolver, fqdn string) ([]string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsbenchQueryDeadline)
	defer cancel()
	start := time.Now()
	answers, err := queryDNSResolver(ctx, r, fqdn)
	return answers, time.Since(start), err
}

// ── download ──

// probeDownload measures throughput from one URL.
//
// The transport is constructed field by field and never cloned from
// http.DefaultTransport, which carries ProxyFromEnvironment: with a proxy the
// *proxy* resolves the URL host and the whole address policy is bypassed, and
// HTTP_PROXY/HTTPS_PROXY are reachable in production through the agent's env
// file. See newDownloadTransport for the other two pinned fields.
func probeDownload(ctx context.Context, cmd CommandRequest, emit *nativeEmitter) error {
	raw := strings.TrimSpace(cmd.Target)
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %s", clampProbeField(err.Error()))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("download takes an http:// or https:// URL, not %q", clampProbeField(u.Scheme))
	}
	hostname := u.Hostname()
	if hostname == "" {
		return errors.New("download URL has no host")
	}
	port := u.Port()
	if port != "" && (!isAllDigits(port) || len(port) > 5) {
		return fmt.Errorf("port %q must be a single number between 1 and 65535", clampProbeField(port))
	}

	ctx, cancel := context.WithTimeout(ctx, downloadDeadline)
	defer cancel()

	if err := emit.emitLine("download %s (limit %d MB, deadline %s covering headers and body)",
		clampProbeField(raw), maxDownloadBytes>>20, downloadDeadline); err != nil {
		return err
	}
	addrs, err := resolveAndCheck(ctx, hostname)
	if err != nil {
		return err
	}
	pinned := addrs[0]
	if err := emit.emitLine("resolved to %s — pinning %s for this probe", joinAddrs(addrs), pinned); err != nil {
		return err
	}

	client := &http.Client{
		Transport: newDownloadTransport(pinned),
		// No redirect is ever followed, so no second request can be aimed at a
		// host the operator never named.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("refusing to follow a redirect")
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fmt.Errorf("could not build the request: %s", clampProbeField(err.Error()))
	}
	req.Header.Set("User-Agent", "parallax-agent")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %s", clampProbeField(err.Error()))
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)
	if err := emit.emitLine("HTTP %d in %s", resp.StatusCode, round3ms(ttfb)); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("server answered HTTP %d, which is not a usable throughput sample", resp.StatusCode)
	}

	readStart := time.Now()
	// io.Copy into io.Discard through a LimitReader, never io.ReadAll: reading
	// the body into memory would hold up to maxDownloadBytes resident per
	// probe. DisableCompression (see the transport) is what makes these wire
	// bytes rather than decompressed ones.
	n, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxDownloadBytes))
	elapsed := time.Since(readStart)
	if copyErr != nil {
		return fmt.Errorf("read failed after %d byte(s): %s", n, clampProbeField(copyErr.Error()))
	}
	secs := elapsed.Seconds()
	mbits := 0.0
	if secs > 0 {
		mbits = round3(float64(n) * 8 / secs / 1e6)
	}
	if err := emit.emitLine("read %d wire byte(s) in %s → %g Mbit/s", n, round3ms(elapsed), mbits); err != nil {
		return err
	}
	if n >= maxDownloadBytes {
		if err := emit.emitLine("note: stopped at the %d MB limit; the throughput above is for the bytes actually read", maxDownloadBytes>>20); err != nil {
			return err
		}
	}
	emit.setSummary(map[string]any{"mbits": mbits, "bytes": n})
	return nil
}

// newDownloadTransport builds the one transport a download probe uses.
//
// Every field here is load-bearing:
//   - Proxy is nil, explicitly. http.DefaultTransport.Clone() would carry
//     ProxyFromEnvironment and a proxy bypasses the address policy wholesale.
//   - DialContext receives the *hostname* and substitutes the pinned literal.
//     Handing the received address to net.Dialer.DialContext would resolve a
//     second time and Happy-Eyeballs-dial whatever came back — a TTL-0
//     rebinding answer gives the checker a public address and the dialer
//     127.0.0.1.
//   - DisableCompression: Go otherwise adds Accept-Encoding: gzip and
//     decompresses transparently, so the byte cap would count *decompressed*
//     bytes and a 100 KB gzip bomb would become 100 MB of CPU and a fictional
//     Mbit/s.
//   - DisableKeepAlives: no socket is ever pooled across probes or users.
func newDownloadTransport(pinned netip.Addr) *http.Transport {
	dialer := probeDialer(0)
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("refusing to dial %s: unparsable address", clampProbeField(addr))
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(pinned.String(), port))
		},
		DisableCompression:  true,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        1,
		TLSHandshakeTimeout: downloadDeadline,
	}
}

// logLocalPrefixes names the on-link prefixes once at startup, so an operator
// can see why a probe to a neighbour was refused without turning on debug
// logging. Same shape as logToolProbe.
func logLocalPrefixes() {
	localPrefixesMu.RLock()
	unavailable := localPrefixesUnavailable
	localPrefixesMu.RUnlock()
	if unavailable {
		return // refreshLocalPrefixes already logged the failure.
	}
	prefixes := snapshotLocalPrefixes()
	if len(prefixes) == 0 {
		log.Println("On-link prefix set is empty — native probes will fall back to the static address policy only")
		return
	}
	log.Printf("Native probes: %d on-link prefix(es) refused as probe targets (%s)",
		len(prefixes), prefixesString(prefixes))
}

func prefixesString(prefixes []netip.Prefix) string {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
