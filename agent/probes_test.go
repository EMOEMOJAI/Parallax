package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// S7 — the native probes: the address policy, the pinned call shapes, the
// execution plumbing and the four probes themselves. Helpers carry an s7 prefix
// so they cannot collide with the existing builder/summary/capability suites.

// s7PublicLiteral is a routable address outside every blocked prefix. It is the
// stub resolver's answer in the rebinding tests: a probe that honours it can
// only fail to connect, while a probe that re-resolves the *name* reaches the
// trap listener on loopback.
const s7PublicLiteral = "93.184.216.34"

// ── seams and fixtures ────────────────────────────────────────────────────────

// s7Seams snapshots every package-level seam and cap a test may touch and
// restores them afterwards. These tests mutate package state, so none of them
// calls t.Parallel.
func s7Seams(t *testing.T) {
	t.Helper()
	oldLookup := probeLookupIPs
	oldSource := localPrefixSource
	oldResolv := resolvConfPath
	oldAllow := probeAllowPrivate.Load()
	oldPrefixes := snapshotLocalPrefixes()
	oldDeadline := downloadDeadline
	oldMaxDownload := maxDownloadBytes
	oldMaxOutput := maxOutputBytes
	t.Cleanup(func() {
		probeLookupIPs = oldLookup
		localPrefixSource = oldSource
		resolvConfPath = oldResolv
		probeAllowPrivate.Store(oldAllow)
		storeLocalPrefixes(oldPrefixes)
		downloadDeadline = oldDeadline
		maxDownloadBytes = oldMaxDownload
		maxOutputBytes = oldMaxOutput
	})
}

// s7StubLookup makes probeLookupIPs answer with fixed literals.
func s7StubLookup(t *testing.T, answers ...string) {
	t.Helper()
	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		parsed, err := netip.ParseAddr(a)
		if err != nil {
			t.Fatalf("bad stub answer %q: %v", a, err)
		}
		addrs = append(addrs, parsed)
	}
	probeLookupIPs = func(context.Context, string) ([]netip.Addr, error) {
		return addrs, nil
	}
}

// s7Recorder is a nativeEmitter sink. failAt > 0 makes the failAt-th send fail,
// which is how the "abort the probe when the send fails" path is driven.
type s7Recorder struct {
	mu     sync.Mutex
	frames []string
	types  []string
	sends  int
	failAt int
}

func (r *s7Recorder) emitter() *nativeEmitter {
	return &nativeEmitter{send: func(outputType, data string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sends++
		if r.failAt > 0 && r.sends == r.failAt {
			return errors.New("connection closed")
		}
		r.types = append(r.types, outputType)
		r.frames = append(r.frames, data)
		return nil
	}}
}

func (r *s7Recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.frames, "\n")
}

func (r *s7Recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// s7TrapListener is a loopback listener that counts accepted connections. Any
// count above zero in a rebinding test means the probe dialed a name instead of
// the validated literal.
func s7TrapListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("trap listen: %v", err)
	}
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("trap addr: %v", err)
	}
	return port, &accepted
}

// s7WaitForAccept polls the accept counter: a dial returns as soon as the
// kernel completes the handshake, which can be before Accept() runs.
func s7WaitForAccept(t *testing.T, accepted *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for accepted.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := accepted.Load(); got != want {
		t.Errorf("listener accepted %d connection(s), want %d", got, want)
	}
}

// s7AssertTrapReachableByName proves the rebinding tests are not vacuous: the
// *system* resolver really does map this name to the trap listener, so a probe
// that re-resolves would connect to it.
func s7AssertTrapReachableByName(t *testing.T, name, port string, accepted *atomic.Int64) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(name, port), 3*time.Second)
	if err != nil {
		t.Skipf("this host's resolver does not map %q to the loopback trap (%v) — the rebinding assertion would be vacuous", name, err)
	}
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Fatalf("the trap listener did not record the control connection — the fixture is broken")
	}
	accepted.Store(0)
}

// s7WaitFor polls a condition, so a test never depends on a server goroutine
// having been scheduled yet.
func s7WaitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// s7WaitForNoNativeRuns waits until every native probe goroutine has passed its
// deferred unregister. Both teardown paths leave the deletion to the probe
// itself, so an empty map is a real "everything has stopped" signal.
func s7WaitForNoNativeRuns(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runningNativeCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d native probe(s) still registered", runningNativeCount())
}

func s7ShortCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// ── the address policy table ─────────────────────────────────────────────────

func TestS7IsBlockedAddrCoversEveryListedPrefix(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)

	blocked := []string{
		// v4: loopback, private, link-local, CGNAT
		"127.0.0.1", "127.255.255.254",
		"10.0.0.1", "172.16.5.4", "192.168.1.1",
		"169.254.169.254",
		"100.64.0.1", "100.127.255.255",
		// v4: never relaxable
		"0.0.0.0", "0.1.2.3",
		"192.0.0.1", "192.88.99.1", "198.18.0.1", "198.19.255.255",
		"192.0.2.1", "198.51.100.1", "203.0.113.1",
		"240.0.0.1", "255.255.255.255",
		"224.0.0.1", "239.255.255.255",
		// v6
		"::1", "::", "fe80::1", "ff02::1", "ff01::1", "ff0e::1",
		"fc00::1", "fd12:3456::1", "fec0::1", "feff::1",
		"::7f00:1", "::ffff:0:1.2.3.4", "64:ff9b::7f00:1",
		"2001::1", "2001:10::1", "2001:20::1", "2001:db8::1",
		"3fff::1", "2002:7f00:1::", "2002:0a00:0001::",
		// v4-mapped forms must be blocked through .Unmap()
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.0.1",
	}
	for _, s := range blocked {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", s, err)
		}
		if !isBlockedAddr(a.Unmap()) {
			t.Errorf("isBlockedAddr(%s) = false, want blocked", s)
		}
	}

	// A Teredo address whose embedded server IPv4 is private and whose
	// obfuscated client IPv4 is loopback. Blocked by the 2001::/32 prefix and,
	// independently, by the embedded-IPv4 re-check.
	teredo := netip.AddrFrom16([16]byte{
		0x20, 0x01, 0x00, 0x00, // 2001:0000::/32
		10, 0, 0, 1, // server IPv4 10.0.0.1
		0x80, 0x00, 0x12, 0x34, // flags + obfuscated port
		^byte(127), ^byte(0), ^byte(0), ^byte(1), // client IPv4 127.0.0.1
	})
	if !isBlockedAddr(teredo) {
		t.Errorf("isBlockedAddr(%s) = false, want blocked (Teredo with embedded private v4)", teredo)
	}
	if got := embeddedIPv4s(teredo); len(got) != 2 || got[0].String() != "10.0.0.1" || got[1].String() != "127.0.0.1" {
		t.Errorf("embeddedIPv4s(teredo) = %v, want [10.0.0.1 127.0.0.1]", got)
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", s7PublicLiteral, "2606:4700:4700::1111", "2000::1"}
	for _, s := range allowed {
		a := netip.MustParseAddr(s)
		if isBlockedAddr(a.Unmap()) {
			t.Errorf("isBlockedAddr(%s) = true, want allowed", s)
		}
	}
}

func TestS7ProbeAllowPrivateRelaxesOnlyItsStatedClasses(t *testing.T) {
	s7Seams(t)
	storeLocalPrefixes([]netip.Prefix{netip.MustParsePrefix("2001:470:abcd:1234::/64")})
	probeAllowPrivate.Store(true)

	relaxed := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "::1", "fe80::1", "fc00::1", "fec0::1",
		"2001:470:abcd:1234::5", // on-link
	}
	for _, s := range relaxed {
		a := netip.MustParseAddr(s)
		if isBlockedAddr(a.Unmap()) {
			t.Errorf("PROBE_ALLOW_PRIVATE=1: isBlockedAddr(%s) = true, want relaxed", s)
		}
	}

	stillBlocked := []string{
		"0.0.0.0", "::", "0.1.2.3", "240.0.0.1", "255.255.255.255",
		"224.0.0.1", "ff02::1", "ff01::1", "239.1.2.3",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.0.1",
		"192.0.0.1", "192.88.99.1", "2001:db8::1", "2002:7f00:1::",
		"64:ff9b::7f00:1", "::7f00:1", "3fff::1",
	}
	for _, s := range stillBlocked {
		a := netip.MustParseAddr(s)
		if !isBlockedAddr(a.Unmap()) {
			t.Errorf("PROBE_ALLOW_PRIVATE=1 must never unblock %s", s)
		}
	}
}

func TestS7OnLinkPrefixIsBlockedEvenWhenGloballyRoutable(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	// A build host need not have native IPv6, so the on-link set comes from the
	// seam rather than from real interfaces.
	localPrefixSource = func() []netip.Prefix {
		return []netip.Prefix{
			netip.MustParsePrefix("2001:470:abcd:1234::/64"),
			netip.MustParsePrefix("203.0.55.0/24"),
		}
	}
	refreshLocalPrefixes()

	for _, s := range []string{"2001:470:abcd:1234::1", "2001:470:abcd:1234:ffff::9", "203.0.55.7"} {
		if !isBlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("on-link address %s was not blocked", s)
		}
	}
	// A GUA outside every on-link prefix stays reachable.
	if isBlockedAddr(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Errorf("an off-link GUA was blocked")
	}

	// Replace-only discipline: a refresh publishes a fresh slice, and the
	// previously returned snapshot is unaffected.
	before := snapshotLocalPrefixes()
	localPrefixSource = func() []netip.Prefix { return nil }
	refreshLocalPrefixes()
	if len(before) != 2 {
		t.Errorf("an earlier snapshot changed under the caller: %v", before)
	}
	if len(snapshotLocalPrefixes()) != 0 {
		t.Errorf("refresh did not publish the fresh (empty) set")
	}
}

func TestS7InterfaceLocalPrefixesIncludesTheHostAddressesThemselves(t *testing.T) {
	prefixes := interfaceLocalPrefixes()
	if len(prefixes) == 0 {
		t.Skip("no interface addresses on this host")
	}
	// Every real host has loopback, so 127.0.0.1/32 (or ::1/128) must be in the
	// set — proving the host address itself is covered, not only its subnet.
	found := false
	for _, p := range prefixes {
		if p.Bits() == p.Addr().BitLen() && p.Addr().IsLoopback() {
			found = true
		}
	}
	if !found {
		t.Errorf("interfaceLocalPrefixes did not include a host-length loopback prefix: %v", prefixes)
	}
}

// ── resolveAndCheck ──────────────────────────────────────────────────────────

func TestS7ResolveAndCheckRefusesTheWholeProbeOnOneBlockedAnswer(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	s7StubLookup(t, s7PublicLiteral, "127.0.0.1")

	if _, err := resolveAndCheck(context.Background(), "mixed.example.com"); err == nil {
		t.Fatalf("a name resolving to one public and one loopback address was accepted")
	} else if !errors.Is(err, errBlockedAddress) {
		t.Fatalf("error = %v, want an address-policy refusal", err)
	}
}

func TestS7ResolveAndCheckRefusesZeroAddresses(t *testing.T) {
	s7Seams(t)
	probeLookupIPs = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
	if _, err := resolveAndCheck(context.Background(), "empty.example.com"); err == nil {
		t.Fatalf("zero addresses was not a refusal")
	}
}

func TestS7ResolveAndCheckPassesPublicAnswersThroughAndDedupes(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	s7StubLookup(t, s7PublicLiteral, s7PublicLiteral, "2606:4700:4700::1111")
	addrs, err := resolveAndCheck(context.Background(), "ok.example.com")
	if err != nil {
		t.Fatalf("resolveAndCheck: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("addrs = %v, want the two distinct answers", addrs)
	}
}

func TestS7ResolveAndCheckUsesNoLookupForAnIPLiteral(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	probeLookupIPs = func(context.Context, string) ([]netip.Addr, error) {
		t.Errorf("an IP literal target went through the resolver")
		return nil, errors.New("must not be called")
	}
	if _, err := resolveAndCheck(context.Background(), s7PublicLiteral); err != nil {
		t.Fatalf("public literal refused: %v", err)
	}
	if _, err := resolveAndCheck(context.Background(), "127.0.0.1"); err == nil {
		t.Fatalf("loopback literal accepted")
	}
}

// ── the P0: dial the validated literal, never the name again ─────────────────

func TestS7TCPDialsTheValidatedLiteralAndNeverReResolves(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	port, accepted := s7TrapListener(t)
	s7AssertTrapReachableByName(t, "localhost", port, accepted)
	s7StubLookup(t, s7PublicLiteral)

	rec := &s7Recorder{}
	err := probeTCP(s7ShortCtx(t, 1200*time.Millisecond), CommandRequest{ID: "p0-tcp", Type: "tcp", Target: "localhost:" + port}, rec.emitter())
	if err == nil {
		t.Errorf("probeTCP succeeded — it must have dialed something other than the validated literal")
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback trap accepted %d connection(s): tcp re-resolved the hostname", n)
	}
	if !strings.Contains(rec.text()+err.Error(), s7PublicLiteral) {
		t.Errorf("neither the output nor the error names the validated literal:\n%s\n%v", rec.text(), err)
	}
}

func TestS7TLSDialsTheValidatedLiteralAndNeverReResolves(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	port, accepted := s7TrapListener(t)
	s7AssertTrapReachableByName(t, "localhost", port, accepted)
	s7StubLookup(t, s7PublicLiteral)

	rec := &s7Recorder{}
	err := probeTLS(s7ShortCtx(t, 1200*time.Millisecond), CommandRequest{ID: "p0-tls", Type: "tls", Target: "localhost:" + port}, rec.emitter())
	if err == nil {
		t.Errorf("probeTLS succeeded against a plain-TCP trap — it cannot have dialed the validated literal")
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback trap accepted %d connection(s): tls re-resolved the hostname (tls.Dial?)", n)
	}
	if !strings.Contains(err.Error(), s7PublicLiteral) {
		t.Errorf("the failure does not name the validated literal: %v", err)
	}
}

func TestS7DownloadTransportSubstitutesTheValidatedLiteral(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	downloadDeadline = 1200 * time.Millisecond
	port, accepted := s7TrapListener(t)
	s7AssertTrapReachableByName(t, "localhost", port, accepted)
	s7StubLookup(t, s7PublicLiteral)

	rec := &s7Recorder{}
	err := probeDownload(context.Background(), CommandRequest{ID: "p0-dl", Type: "download", Target: "http://localhost:" + port + "/big"}, rec.emitter())
	if err == nil {
		t.Errorf("probeDownload succeeded — the transport cannot have dialed the validated literal")
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback trap accepted %d connection(s): the transport handed the hostname to the dialer", n)
	}
	if !strings.Contains(rec.text(), "pinning "+s7PublicLiteral) {
		t.Errorf("the probe did not pin the validated literal:\n%s", rec.text())
	}
}

// TestS7DownloadDialContextSubstitutesThePinnedLiteral is the other half of the
// P0 for download: the transport's DialContext receives the *hostname* and must
// replace it with the validated literal, keeping the port. Here the address
// handed to DialContext is one nothing is listening on, and the pinned literal
// is the fixture listener — so a connection at all, to that listener, is proof
// of the substitution.
func TestS7DownloadDialContextSubstitutesThePinnedLiteral(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	port, accepted := s7TrapListener(t)

	tr := newDownloadTransport(netip.MustParseAddr("127.0.0.1"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := tr.DialContext(ctx, "tcp", "some.hostname.invalid:"+port)
	if err != nil {
		t.Fatalf("DialContext did not substitute the pinned literal: %v", err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+port {
		t.Errorf("connected to %s, want the pinned literal with the requested port", got)
	}
	s7WaitForAccept(t, accepted, 1)
}

// ── the Control hook ─────────────────────────────────────────────────────────

func TestS7ProbeDialControlRefusesBlockedSocketAddresses(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)

	for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "10.0.0.1:22", "169.254.169.254:80", "[ff02::1]:53", "0.0.0.0:80"} {
		if err := probeDialControl("tcp", addr, nil); err == nil {
			t.Errorf("probeDialControl accepted %s", addr)
		}
	}
	if err := probeDialControl("tcp", s7PublicLiteral+":443", nil); err != nil {
		t.Errorf("probeDialControl refused a public address: %v", err)
	}
	if err := probeDialControl("tcp", "not-an-address", nil); err == nil {
		t.Errorf("probeDialControl accepted an unparsable socket address")
	}
}

// TestS7EveryTargetDialingDialerCarriesTheControlHook reads probes.go: the
// three target-dialing probes must build their dialers through probeDialer (the
// only constructor that installs Control), and the only dialer literal without
// the hook must be the dnsbench resolver dialer, which is exempt by design.
func TestS7EveryTargetDialingDialerCarriesTheControlHook(t *testing.T) {
	if probeDialer(0).Control == nil {
		t.Fatalf("probeDialer returned a dialer with no Control hook")
	}
	src, err := os.ReadFile("probes.go")
	if err != nil {
		t.Fatalf("read probes.go: %v", err)
	}
	// Every &net.Dialer{...} literal in the file, with the function it sits in.
	var current string
	literals := map[string]string{}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "func ") {
			current = strings.TrimSpace(strings.TrimPrefix(line, "func "))
			if i := strings.IndexByte(current, '('); i > 0 {
				current = current[:i]
			}
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "&net.Dialer{") {
			literals[current] = trimmed
		}
	}
	allowed := map[string]bool{
		"probeDialer":            true, // installs Control — used by tcp, tls, download
		"dnsbenchResolverDialer": true, // deliberately hookless; dials pinned resolver literals only
	}
	for fn, line := range literals {
		if !allowed[fn] {
			t.Errorf("probes.go builds a net.Dialer in %s (%s) — target-dialing probes must go through probeDialer", fn, line)
		}
	}
	for fn := range allowed {
		if _, ok := literals[fn]; !ok {
			t.Errorf("expected a net.Dialer literal in %s; the file changed shape", fn)
		}
	}
	// And the three target-dialing probes must not construct one themselves.
	for _, fn := range []string{"probeTCP", "probeTLS", "probeDownload", "newDownloadTransport"} {
		if line, ok := literals[fn]; ok {
			t.Errorf("%s builds its own dialer (%s) instead of calling probeDialer", fn, line)
		}
	}
}

// ── download ─────────────────────────────────────────────────────────────────

// s7AllowLoopback relaxes the address policy for the local fixture servers the
// download and tls tests need. It is the PROBE_ALLOW_PRIVATE path, exercised
// deliberately.
func s7AllowLoopback(t *testing.T) {
	t.Helper()
	probeAllowPrivate.Store(true)
	storeLocalPrefixes(nil)
}

func s7RunDownload(t *testing.T, rawURL string) (*s7Recorder, error) {
	t.Helper()
	rec := &s7Recorder{}
	err := probeDownload(context.Background(), CommandRequest{ID: "dl", Type: "download", Target: rawURL}, rec.emitter())
	return rec, err
}

func TestS7DownloadBudgetsAreTheContractValues(t *testing.T) {
	if downloadDeadline != 20*time.Second {
		t.Errorf("downloadDeadline = %s, want 20s", downloadDeadline)
	}
	if maxDownloadBytes != 100<<20 {
		t.Errorf("maxDownloadBytes = %d, want %d", maxDownloadBytes, int64(100<<20))
	}
	if tcpProbeDeadline != 5*time.Second {
		t.Errorf("tcpProbeDeadline = %s, want 5s", tcpProbeDeadline)
	}
	if tlsProbeDeadline != 10*time.Second {
		t.Errorf("tlsProbeDeadline = %s, want 10s", tlsProbeDeadline)
	}
	if dnsbenchQueryDeadline != 3*time.Second {
		t.Errorf("dnsbenchQueryDeadline = %s, want 3s", dnsbenchQueryDeadline)
	}
	if nativeCommandTimeout != 10*time.Minute {
		t.Errorf("nativeCommandTimeout = %s, want 10m", nativeCommandTimeout)
	}
	if maxConcurrentDownloads != 2 {
		t.Errorf("maxConcurrentDownloads = %d, want 2", maxConcurrentDownloads)
	}
}

func TestS7DownloadTransportIsBuiltFieldByField(t *testing.T) {
	tr := newDownloadTransport(netip.MustParseAddr("1.1.1.1"))
	if tr.Proxy != nil {
		t.Errorf("transport carries a Proxy — a proxy resolves the URL host itself and bypasses the address policy")
	}
	if !tr.DisableCompression {
		t.Errorf("DisableCompression is off — the byte cap would count decompressed bytes")
	}
	if !tr.DisableKeepAlives {
		t.Errorf("DisableKeepAlives is off — a socket could be pooled across probes")
	}
	if tr.DialContext == nil {
		t.Errorf("transport has no DialContext, so it would resolve the URL host itself")
	}
	src, err := os.ReadFile("probes.go")
	if err != nil {
		t.Fatalf("read probes.go: %v", err)
	}
	for _, banned := range []string{"DefaultTransport", "ProxyFromEnvironment", "tls.Dial(", "io.ReadAll"} {
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(trimmed, banned) {
				t.Errorf("probes.go uses %s: %s", banned, trimmed)
			}
		}
	}
}

func TestS7DownloadIgnoresProxyEnvironment(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	// A sink no proxy is listening on: if the transport honoured it, the probe
	// could not succeed.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")

	body := bytes.Repeat([]byte("x"), 64*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	rec, err := s7RunDownload(t, srv.URL+"/file")
	if err != nil {
		t.Fatalf("download failed with a proxy in the environment: %v\n%s", err, rec.text())
	}
	if got := rec.emitter().summary; got != nil {
		_ = got
	}
}

func TestS7DownloadReportsWireBytesNotDecompressedBytes(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	if _, err := zw.Write(bytes.Repeat([]byte{0}, 8<<20)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	zw.Close()
	wire := gzipped.Len()
	if wire > 1<<20 {
		t.Fatalf("fixture is not a bomb: %d compressed bytes", wire)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); strings.Contains(ae, "gzip") {
			t.Errorf("the probe advertised Accept-Encoding: %q — Go would then decompress transparently", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gzipped.Bytes())
	}))
	defer srv.Close()

	rec := &s7Recorder{}
	emit := rec.emitter()
	if err := probeDownload(context.Background(), CommandRequest{ID: "bomb", Type: "download", Target: srv.URL + "/bomb"}, emit); err != nil {
		t.Fatalf("download: %v\n%s", err, rec.text())
	}
	got, ok := emit.summary["bytes"].(int64)
	if !ok {
		t.Fatalf("summary bytes = %#v", emit.summary["bytes"])
	}
	if int(got) != wire {
		t.Errorf("reported %d bytes, want the %d wire bytes (decompressed would be %d)", got, wire, 8<<20)
	}
}

func TestS7DownloadStopsAtTheByteLimitWithoutBufferingIt(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	// The cap is shrunk from 100 MB so the test streams 40 MB rather than
	// 104 MB; the production value is pinned by
	// TestS7DownloadBudgetsAreTheContractValues. The memory assertion is what
	// distinguishes io.Copy(io.Discard, …) from io.ReadAll: ReadAll would hold
	// the whole capped body resident.
	maxDownloadBytes = 32 << 20
	chunk := bytes.Repeat([]byte("y"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 40; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	rec := &s7Recorder{}
	emit := rec.emitter()
	if err := probeDownload(context.Background(), CommandRequest{ID: "cap", Type: "download", Target: srv.URL + "/big"}, emit); err != nil {
		t.Fatalf("download: %v\n%s", err, rec.text())
	}
	runtime.ReadMemStats(&after)

	got, _ := emit.summary["bytes"].(int64)
	if got != maxDownloadBytes {
		t.Errorf("read %d bytes, want the %d-byte limit", got, maxDownloadBytes)
	}
	if !strings.Contains(rec.text(), "limit") {
		t.Errorf("the output does not say the limit stopped the read:\n%s", rec.text())
	}
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > 8<<20 {
		t.Errorf("heap grew by %d bytes reading a %d-byte body — the body is being buffered", growth, maxDownloadBytes)
	}
}

func TestS7DownloadRefusesRedirects(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	_, err := s7RunDownload(t, srv.URL+"/start")
	if err == nil {
		t.Fatalf("a redirect was followed")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want a redirect refusal", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want exactly 1 (no second request)", n)
	}
}

func TestS7DownloadEndsAtItsOwnDeadlineNotTheCommandTimeout(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	// Shrunk so the test does not take 20 seconds; the production value is
	// pinned elsewhere. What is under test is that the deadline covers the
	// *body read*: a dial/header timeout alone would let this server trickle
	// for the full 10-minute command timeout.
	downloadDeadline = 1200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			if _, err := w.Write([]byte("z")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, err := s7RunDownload(t, srv.URL+"/slow")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a one-byte-per-100ms server produced a successful throughput sample")
	}
	if elapsed > 8*time.Second {
		t.Errorf("the probe ran for %s — the deadline does not cover the body read", elapsed)
	}
	if elapsed < downloadDeadline/2 {
		t.Errorf("the probe ended after %s, before its deadline — something else failed: %v", elapsed, err)
	}
}

func TestS7DownloadRefusesNonHTTPSchemesAndBlockedHosts(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)

	for _, target := range []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"gopher://example.com",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/",
		"http://[::1]:8080/",
		"http://10.0.0.1/",
		"https://[fd00::1]/",
		"http:///nohost",
	} {
		rec := &s7Recorder{}
		if err := probeDownload(s7ShortCtx(t, time.Second), CommandRequest{ID: "x", Type: "download", Target: target}, rec.emitter()); err == nil {
			t.Errorf("probeDownload accepted %q", target)
		}
	}
}

// ── tls ──────────────────────────────────────────────────────────────────────

// s7SelfSignedCert builds a self-signed certificate for 127.0.0.1 with the
// given validity window.
func s7SelfSignedCert(t *testing.T, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "s7-fixture"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func TestS7TLSExpiredCertificateIsAFindingNotATransportError(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	cert := s7SelfSignedCert(t, time.Now().Add(-72*time.Hour), time.Now().Add(-24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}

	rec := &s7Recorder{}
	emit := rec.emitter()
	// An IP-literal target: no SNI, and the certificate is checked against its
	// IP SANs.
	if err := probeTLS(context.Background(), CommandRequest{ID: "tls", Type: "tls", Target: u.Host}, emit); err != nil {
		t.Fatalf("an expired certificate became a probe failure instead of a finding: %v\n%s", err, rec.text())
	}
	if ok, _ := emit.summary["chain_ok"].(bool); ok {
		t.Errorf("chain_ok = true for an expired self-signed certificate — that is the false clean bill of health this probe exists to avoid")
	}
	verr, _ := emit.summary["verify_error"].(string)
	if verr == "" {
		t.Errorf("no verification error was reported: %#v", emit.summary)
	}
	if days, ok := emit.summary["days_remaining"].(int); !ok || days >= 0 {
		t.Errorf("days_remaining = %#v, want a negative number for an expired certificate", emit.summary["days_remaining"])
	}
	text := rec.text()
	if !strings.Contains(text, "no SNI") {
		t.Errorf("the probe did not report that no SNI was sent:\n%s", text)
	}
	if !strings.Contains(text, "chain: NOT ok") {
		t.Errorf("the probe did not report the chain verdict:\n%s", text)
	}
	if !strings.Contains(text, "day(s) remaining") || !strings.Contains(text, "-") {
		t.Errorf("the expiry was not reported in the output:\n%s", text)
	}

	// The control that makes the InsecureSkipVerify+manual-verify shape
	// necessary: with verification left on, this endpoint is a *transport*
	// error, so the probe could report nothing at all.
	raw, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial fixture: %v", err)
	}
	defer raw.Close()
	verifying := tls.Client(raw, &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err := verifying.Handshake(); err == nil {
		t.Errorf("the fixture certificate verifies against the system roots — the control is vacuous")
	}
}

func TestS7TLSSendsSNIForANameAndNoneForALiteral(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	var mu sync.Mutex
	var seen []string
	cert := s7SelfSignedCert(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// GetConfigForClient rather than GetCertificate: Go's server skips
	// GetCertificate entirely when the ClientHello carries no SNI and the
	// config already holds a certificate, which is exactly the case under test.
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			seen = append(seen, hello.ServerName)
			mu.Unlock()
			return nil, nil
		},
	}
	srv.StartTLS()
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)

	rec := &s7Recorder{}
	if err := probeTLS(context.Background(), CommandRequest{ID: "tls-ip", Type: "tls", Target: u.Host}, rec.emitter()); err != nil {
		t.Fatalf("literal target: %v\n%s", err, rec.text())
	}
	s7WaitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 1 }, "the server did not record the first handshake")
	// A name target reaches the same listener through the stub resolver.
	s7StubLookup(t, "127.0.0.1")
	rec2 := &s7Recorder{}
	if err := probeTLS(context.Background(), CommandRequest{ID: "tls-name", Type: "tls", Target: "localhost:" + port}, rec2.emitter()); err != nil {
		t.Fatalf("name target: %v\n%s", err, rec2.text())
	}
	s7WaitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 2 }, "the server did not record the second handshake")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d handshakes: %v", len(seen), seen)
	}
	if seen[0] != "" {
		t.Errorf("SNI %q was sent for an IP-literal target", seen[0])
	}
	if seen[1] != "localhost" {
		t.Errorf("SNI for a name target = %q, want localhost", seen[1])
	}
	if strings.Contains(rec2.text(), "no SNI") {
		t.Errorf("the name-target run claimed no SNI was sent:\n%s", rec2.text())
	}
}

func TestS7SplitTLSTarget(t *testing.T) {
	cases := []struct {
		in      string
		host    string
		port    string
		isIP    bool
		wantErr bool
	}{
		{in: "example.com", host: "example.com", port: "443"},
		{in: "example.com:8443", host: "example.com", port: "8443"},
		{in: "1.1.1.1", host: "1.1.1.1", port: "443", isIP: true},
		{in: "1.1.1.1:853", host: "1.1.1.1", port: "853", isIP: true},
		{in: "2606:4700:4700::1111", host: "2606:4700:4700::1111", port: "443", isIP: true},
		{in: "[2606:4700:4700::1111]:853", host: "2606:4700:4700::1111", port: "853", isIP: true},
		{in: "::ffff:1.1.1.1", host: "1.1.1.1", port: "443", isIP: true},
		{in: "example.com:0", wantErr: true},
		{in: "example.com:65536", wantErr: true},
		{in: "example.com:443,8443", wantErr: true},
		{in: "example.com:443 8443", wantErr: true},
		{in: "example.com:+443", wantErr: true},
		{in: "example.com:https", wantErr: true},
		{in: ":443", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		host, port, isIP, err := splitTLSTarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("splitTLSTarget(%q) = (%q, %q, %v, nil), want an error", c.in, host, port, isIP)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitTLSTarget(%q): %v", c.in, err)
			continue
		}
		if host != c.host || port != c.port || isIP != c.isIP {
			t.Errorf("splitTLSTarget(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, host, port, isIP, c.host, c.port, c.isIP)
		}
	}
}

// ── tcp ──────────────────────────────────────────────────────────────────────

func TestS7TCPTargetRequiresExactlyOnePort(t *testing.T) {
	bad := []string{
		"example.com", "example.com:", "example.com:0", "example.com:65536",
		"example.com:80,443", "example.com:80 443", "example.com:http",
		"1.2.3.4:80:90", ":80", "example.com:-80", "example.com:+80",
		"example.com:080443", "2606:4700:4700::1111", "",
	}
	for _, target := range bad {
		if host, port, err := splitSingleHostPort(target); err == nil {
			t.Errorf("splitSingleHostPort(%q) = (%q, %q), want an error", target, host, port)
		}
	}
	good := map[string][2]string{
		"example.com:443":            {"example.com", "443"},
		"1.1.1.1:53":                 {"1.1.1.1", "53"},
		"[2606:4700:4700::1111]:853": {"2606:4700:4700::1111", "853"},
		"example.com:1":              {"example.com", "1"},
		"example.com:65535":          {"example.com", "65535"},
	}
	for target, want := range good {
		host, port, err := splitSingleHostPort(target)
		if err != nil {
			t.Errorf("splitSingleHostPort(%q): %v", target, err)
			continue
		}
		if host != want[0] || port != want[1] {
			t.Errorf("splitSingleHostPort(%q) = (%q, %q), want %v", target, host, port, want)
		}
	}
}

func TestS7TCPReportsConnectTimeAgainstALocalListener(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	port, accepted := s7TrapListener(t)

	rec := &s7Recorder{}
	emit := rec.emitter()
	if err := probeTCP(context.Background(), CommandRequest{ID: "tcp-ok", Type: "tcp", Target: "127.0.0.1:" + port}, emit); err != nil {
		t.Fatalf("probeTCP: %v\n%s", err, rec.text())
	}
	s7WaitForAccept(t, accepted, 1)
	if _, ok := emit.summary["connect_ms"].(float64); !ok {
		t.Errorf("summary connect_ms = %#v", emit.summary["connect_ms"])
	}
	if !strings.Contains(rec.text(), "connected in") {
		t.Errorf("output does not report the connect time:\n%s", rec.text())
	}
}

// ── dnsbench ─────────────────────────────────────────────────────────────────

func TestS7DNSBenchNameValidation(t *testing.T) {
	label64 := strings.Repeat("a", 64)
	name254 := strings.Repeat("a.", 126) + "bcd" // 255 bytes
	name253 := strings.Repeat("aaaaaaaaa.", 25) + "aaa"
	if len(name253) != 253 {
		t.Fatalf("fixture name253 is %d bytes", len(name253))
	}
	bad := []string{
		"", ".", "single", "localhost", label64 + ".com", name254,
		"-lead.example.com", "trail-.example.com", "under_score.example.com",
		"1.1.1.1", "2606:4700:4700::1111", "a..b", "space name.com",
		"ex$ample.com", "exam;ple.com", "+short.example.com",
	}
	for _, in := range bad {
		if got, err := validateDNSBenchName(in); err == nil {
			t.Errorf("validateDNSBenchName(%q) = %q, want an error", in, got)
		}
	}
	good := map[string]string{
		"example.com":       "example.com.",
		"example.com.":      "example.com.",
		"a.b":               "a.b.",
		"WWW.Example.COM":   "WWW.Example.COM.",
		"xn--bcher-kva.ch":  "xn--bcher-kva.ch.",
		"a-b.example-1.com": "a-b.example-1.com.",
		name253:             name253 + ".",
	}
	for in, want := range good {
		got, err := validateDNSBenchName(in)
		if err != nil {
			t.Errorf("validateDNSBenchName(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("validateDNSBenchName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestS7SystemResolversFromResolvConf(t *testing.T) {
	s7Seams(t)
	dir := t.TempDir()

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	// Capped at three (glibc MAXNS), non-literal lines skipped, comments
	// ignored, options ignored.
	resolvConfPath = write("full", strings.Join([]string{
		"# a comment",
		"search lan",
		"options edns0 trust-ad",
		"nameserver 192.168.1.1",
		"nameserver resolver.example.com",
		"nameserver 9.9.9.9 # inline comment",
		"nameserver fd00::1%en0",
		"nameserver 1.0.0.1",
		"nameserver 8.8.4.4",
		"",
	}, "\n"))
	got := systemResolvers()
	want := []string{"192.168.1.1", "9.9.9.9", "fd00::1%en0"}
	if len(got) != len(want) {
		t.Fatalf("systemResolvers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("systemResolvers()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if len(got) > maxSystemResolvers {
		t.Errorf("systemResolvers returned %d entries, above the %d cap", len(got), maxSystemResolvers)
	}

	// Missing file.
	resolvConfPath = filepath.Join(dir, "does-not-exist")
	if got := systemResolvers(); len(got) != 0 {
		t.Errorf("a missing resolv.conf yielded %v", got)
	}
	// Empty file.
	resolvConfPath = write("empty", "")
	if got := systemResolvers(); len(got) != 0 {
		t.Errorf("an empty resolv.conf yielded %v", got)
	}
	// A file with nothing usable.
	resolvConfPath = write("junk", "nameserver\nnameserver not-an-ip\nsearch example.com\n")
	if got := systemResolvers(); len(got) != 0 {
		t.Errorf("an unusable resolv.conf yielded %v", got)
	}
}

func TestS7DNSBenchResolverSetPinsPort53AndDedupes(t *testing.T) {
	s7Seams(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "resolv.conf")
	// 1.1.1.1 is also in the constant public set: it must appear once, as the
	// system entry.
	if err := os.WriteFile(p, []byte("nameserver 192.168.1.1\nnameserver 1.1.1.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvConfPath = p

	resolvers := dnsbenchResolvers()
	if len(resolvers) != 4 {
		t.Fatalf("resolvers = %v, want 2 system + 2 remaining public", resolvers)
	}
	seen := map[string]string{}
	for _, r := range resolvers {
		if r.addr.Port() != 53 {
			t.Errorf("resolver %s does not pin port 53", r.addr)
		}
		key := r.addr.Addr().String()
		if prev, dup := seen[key]; dup {
			t.Errorf("resolver %s appears twice (%s and %s)", key, prev, r.source)
		}
		seen[key] = r.source
	}
	if seen["192.168.1.1"] != "system" || seen["1.1.1.1"] != "system" {
		t.Errorf("system entries mislabelled: %v", seen)
	}
	if seen["8.8.8.8"] != "public" || seen["9.9.9.9"] != "public" {
		t.Errorf("public entries missing: %v", seen)
	}
	// The public set is a compile-time constant, not configuration.
	t.Setenv("DNSBENCH_RESOLVERS", "6.6.6.6")
	t.Setenv("PROBE_RESOLVERS", "6.6.6.6")
	for _, r := range dnsbenchResolvers() {
		if r.addr.Addr().String() == "6.6.6.6" {
			t.Errorf("an environment variable injected a resolver")
		}
	}
}

// s7DNSResponder is a minimal UDP DNS server that answers A queries with one
// record. It listens on loopback — a *private* address — which is exactly the
// point: dnsbench's resolver dialer carries no Control hook, so a private
// system resolver still works.
func s7DNSResponder(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("dns listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if resp, ok := s7DNSAnswer(buf[:n]); ok {
				pc.WriteToUDP(resp, addr)
			}
		}
	}()
	ap := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func s7DNSAnswer(q []byte) ([]byte, bool) {
	if len(q) < 17 {
		return nil, false
	}
	i := 12
	for i < len(q) {
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l >= 0xc0 || i+l+1 > len(q) {
			return nil, false
		}
		i += l + 1
	}
	if i+4 > len(q) {
		return nil, false
	}
	qtype := int(q[i])<<8 | int(q[i+1])
	question := q[12 : i+4]

	resp := make([]byte, 0, len(q)+16)
	resp = append(resp, q[0], q[1], 0x81, 0x80, 0x00, 0x01)
	if qtype == 1 { // A
		resp = append(resp, 0x00, 0x01)
	} else {
		resp = append(resp, 0x00, 0x00)
	}
	resp = append(resp, 0x00, 0x00, 0x00, 0x00)
	resp = append(resp, question...)
	if qtype == 1 {
		resp = append(resp, 0xc0, 0x0c, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x3c, 0x00, 0x04, 93, 184, 216, 34)
	}
	return resp, true
}

func TestS7DNSBenchQueryReachesAPrivateResolverWithNoControlHook(t *testing.T) {
	s7Seams(t)
	// The Control hook would refuse this loopback resolver at the socket layer,
	// which is why dnsbench's dialer must not have one.
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	server := s7DNSResponder(t)

	answers, elapsed, err := dnsbenchQuery(context.Background(), benchResolver{addr: server, source: "system"}, "example.com.")
	if err != nil {
		t.Fatalf("query against a private resolver failed: %v", err)
	}
	if len(answers) != 1 || answers[0] != "93.184.216.34" {
		t.Errorf("answers = %v, want [93.184.216.34]", answers)
	}
	if elapsed <= 0 || elapsed > 3*time.Second {
		t.Errorf("elapsed = %s", elapsed)
	}
}

func TestS7DNSBenchRunReportsOneRowPerResolver(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	resolvers := []benchResolver{
		{addr: s7DNSResponder(t), source: "system"},
		{addr: s7DNSResponder(t), source: "public"},
	}
	rec := &s7Recorder{}
	emit := rec.emitter()
	err := probeDNSBenchResolvers(s7ShortCtx(t, 2*time.Second), CommandRequest{ID: "bench", Type: "dnsbench", Target: "example.com"}, emit, resolvers)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(rec.text(), "\n")
	rows := 0
	for _, line := range lines {
		for _, r := range resolvers {
			if strings.HasPrefix(line, r.addr.String()) {
				rows++
			}
		}
	}

	if rows != len(resolvers) {
		t.Errorf("reported %d rows for %d resolvers:\n%s", rows, len(resolvers), rec.text())
	}
	if _, ok := emit.summary["fastest_ms"].(float64); !ok {
		t.Errorf("summary fastest_ms = %#v", emit.summary["fastest_ms"])
	}
	if got, _ := emit.summary["resolver_count"].(int); got != len(resolvers) {
		t.Errorf("summary resolver_count = %#v, want %d", emit.summary["resolver_count"], len(resolvers))
	}
}

// ── the native table and the option decision ─────────────────────────────────

func TestS7NativeProbesAndNativeProbeTypesDescribeTheSameSet(t *testing.T) {
	if len(nativeProbes) != len(nativeProbeTypes) {
		t.Fatalf("nativeProbes has %d entries, nativeProbeTypes %d", len(nativeProbes), len(nativeProbeTypes))
	}
	for _, cmdType := range nativeProbeTypes {
		if _, ok := nativeProbes[cmdType]; !ok {
			t.Errorf("nativeProbeTypes lists %q with no nativeProbes entry", cmdType)
		}
		if !allowedCommands[cmdType] {
			t.Errorf("native probe type %q is not in allowedCommands — the native branch sits after that check", cmdType)
		}
		if _, ok := commandBuilders[cmdType]; ok {
			t.Errorf("native probe type %q also has a commandBuilders entry", cmdType)
		}
		if _, ok := commandBinaries[cmdType]; ok {
			t.Errorf("native probe type %q also has a commandBinaries entry", cmdType)
		}
	}
	for cmdType := range nativeProbes {
		found := false
		for _, t2 := range nativeProbeTypes {
			if t2 == cmdType {
				found = true
			}
		}
		if !found {
			t.Errorf("nativeProbes has %q, which nativeProbeTypes does not list", cmdType)
		}
	}
}

func TestS7NativeProbesTakeNoOptions(t *testing.T) {
	for _, cmdType := range nativeProbeTypes {
		spec, ok := optionSpecs[cmdType]
		if !ok {
			t.Errorf("optionSpecs has no entry for native type %q", cmdType)
			continue
		}
		if spec.raw {
			t.Errorf("optionSpecs[%q] is raw — options would reach the probe unvalidated", cmdType)
		}
		if len(spec.keys) != 0 || len(spec.order) != 0 || spec.ipVersion {
			t.Errorf("optionSpecs[%q] declares options: %+v", cmdType, spec)
		}
	}
	// The probes never read cmd.Options at all, which is what makes any option
	// token inert rather than merely validated.
	src, err := os.ReadFile("probes.go")
	if err != nil {
		t.Fatalf("read probes.go: %v", err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "cmd.Options") || strings.Contains(trimmed, ".Options") {
			t.Errorf("probes.go:%d reads the options string: %s", i+1, trimmed)
		}
	}
}

func TestS7HostileOptionsStringChangesNothing(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	port, _ := s7TrapListener(t)

	hostile := "count=99 -4 maxhops=9 type=+short ;rm -rf / $(id) `id` 0000000000000000"
	plain := &s7Recorder{}
	plainEmit := plain.emitter()
	if err := probeTCP(context.Background(), CommandRequest{ID: "a", Type: "tcp", Target: "127.0.0.1:" + port}, plainEmit); err != nil {
		t.Fatalf("plain run: %v", err)
	}
	withOpts := &s7Recorder{}
	optsEmit := withOpts.emitter()
	if err := probeTCP(context.Background(), CommandRequest{ID: "b", Type: "tcp", Target: "127.0.0.1:" + port, Options: hostile}, optsEmit); err != nil {
		t.Fatalf("hostile-options run: %v", err)
	}
	if plain.count() != withOpts.count() {
		t.Errorf("the options string changed the number of emitted lines: %d vs %d", plain.count(), withOpts.count())
	}
	if strings.Contains(withOpts.text(), "rm -rf") || strings.Contains(withOpts.text(), "count=99") {
		t.Errorf("an option token reached the output:\n%s", withOpts.text())
	}
	if len(plainEmit.summary) != len(optsEmit.summary) {
		t.Errorf("the options string changed the summary shape: %#v vs %#v", plainEmit.summary, optsEmit.summary)
	}
}

// ── emitLine: caps, sanitizing, abort on send failure ────────────────────────

func TestS7EmitLineStripsANSIAndControlCharacters(t *testing.T) {
	rec := &s7Recorder{}
	emit := rec.emitter()
	// A certificate CN carrying ANSI, a stray ESC, CR/LF/TAB and a NUL.
	cn := "CN=\x1b[31mevil\x1b[0m\x1b]0;title\x07\r\nbad\ttab\x00"
	if err := emit.emitLine("subject: %s", clampProbeField(cn)); err != nil {
		t.Fatalf("emitLine: %v", err)
	}
	got := rec.text()
	for _, bad := range []string{"\x1b", "\r", "\n", "\t", "\x00", "[31m"} {
		if strings.Contains(got, bad) {
			t.Errorf("emitted line still contains %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "evil") || !strings.Contains(got, "badtab") {
		t.Errorf("sanitizing dropped the payload instead of the control bytes: %q", got)
	}
}

func TestS7ClampProbeFieldTruncatesAt256Runes(t *testing.T) {
	long := strings.Repeat("é", 400)
	got := clampProbeField(long)
	if n := len([]rune(got)); n != nativeFieldRunes {
		t.Errorf("clampProbeField kept %d runes, want %d", n, nativeFieldRunes)
	}
	if !strings.HasPrefix(long, got) {
		t.Errorf("clampProbeField split a multi-byte rune")
	}
}

func TestS7EmitLineEnforcesTheLineAndTotalCaps(t *testing.T) {
	s7Seams(t)
	rec := &s7Recorder{}
	emit := rec.emitter()
	if err := emit.emitLine("%s", strings.Repeat("a", 2*maxNativeLineBytes)); err != nil {
		t.Fatalf("emitLine: %v", err)
	}
	if got := len(rec.frames[0]); got != maxNativeLineBytes {
		t.Errorf("emitted line is %d bytes, want the %d cap", got, maxNativeLineBytes)
	}

	maxOutputBytes = 4096
	rec2 := &s7Recorder{}
	emit2 := rec2.emitter()
	var err error
	for i := 0; i < 10; i++ {
		if err = emit2.emitLine("%s", strings.Repeat("b", 1000)); err != nil {
			break
		}
	}
	if !errors.Is(err, errNativeOutputCapped) {
		t.Fatalf("error = %v, want errNativeOutputCapped", err)
	}
	if !emit2.capped {
		t.Errorf("the emitter did not record the cap")
	}
	last := rec2.types[len(rec2.types)-1]
	if last != "error" {
		t.Errorf("the cap did not emit an error frame (last type %q)", last)
	}
	if !strings.Contains(rec2.text(), "Output size limit exceeded") {
		t.Errorf("the cap message does not match the exec path's:\n%s", rec2.text())
	}
}

func TestS7ProbeAbortsWhenTheSendFails(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	port, _ := s7TrapListener(t)

	// The second send fails: the probe must stop instead of running to its
	// deadline against a browser that is gone.
	rec := &s7Recorder{failAt: 2}
	err := probeTCP(context.Background(), CommandRequest{ID: "gone", Type: "tcp", Target: "127.0.0.1:" + port}, rec.emitter())
	if !errors.Is(err, errNativeEmitFailed) {
		t.Fatalf("error = %v, want errNativeEmitFailed", err)
	}
}

// ── summaries stay inside the S2 grammar ─────────────────────────────────────

// s7AssertSummaryGrammar re-applies the server-side grammar (backend/summary.go)
// to a blob: ≤16 KB, an object of ≤32 flat keys, strings ≤256 runes.
func s7AssertSummaryGrammar(t *testing.T, label string, blob []byte) {
	t.Helper()
	if len(blob) > 16384 {
		t.Errorf("%s summary is %d bytes, over the server's 16 KB pre-parse cap", label, len(blob))
	}
	var top map[string]any
	if err := json.Unmarshal(blob, &top); err != nil {
		t.Fatalf("%s summary does not parse: %v", label, err)
	}
	if len(top) > 32 {
		t.Errorf("%s summary has %d keys, over the 32-key cap", label, len(top))
	}
	for k, v := range top {
		switch tv := v.(type) {
		case string:
			if n := len([]rune(tv)); n > 256 {
				t.Errorf("%s summary[%q] is %d runes, over the 256-rune cap", label, k, n)
			}
		case float64, bool:
		default:
			t.Errorf("%s summary[%q] is not a flat value: %#v", label, k, v)
		}
	}
}

func TestS7SummariesStayInsideTheServerGrammar(t *testing.T) {
	cases := map[string]map[string]any{
		"tcp":      {"connect_ms": 12.345},
		"tls":      {"days_remaining": -3, "issuer": strings.Repeat("é", 400), "chain_ok": false, "verify_error": strings.Repeat("x", 900)},
		"dnsbench": {"fastest_ms": 9.5, "resolver_count": 6},
		"download": {"mbits": 942.113, "bytes": int64(100 << 20)},
	}
	for label, m := range cases {
		blob := nativeSummaryJSON(m)
		if blob == nil {
			t.Errorf("%s summary was dropped", label)
			continue
		}
		s7AssertSummaryGrammar(t, label, blob)
	}

	// Hostile shapes: a nested value is dropped, an overlong key list is
	// bounded, and an empty result yields no summary at all.
	hostile := map[string]any{
		"nested": map[string]any{"a": 1},
		"array":  []any{1, 2, 3},
		"ok":     1.5,
	}
	blob := nativeSummaryJSON(hostile)
	s7AssertSummaryGrammar(t, "hostile", blob)
	var top map[string]any
	json.Unmarshal(blob, &top)
	if _, present := top["nested"]; present {
		t.Errorf("a nested value survived: %v", top)
	}
	if _, present := top["array"]; present {
		t.Errorf("an array survived: %v", top)
	}
	big := map[string]any{}
	for i := 0; i < 80; i++ {
		big[fmt.Sprintf("k%d", i)] = i
	}
	s7AssertSummaryGrammar(t, "wide", nativeSummaryJSON(big))
	if nativeSummaryJSON(nil) != nil || nativeSummaryJSON(map[string]any{}) != nil {
		t.Errorf("an empty summary produced a blob")
	}
}

// ── dispatch: registration, refusals, the done pairing, cancellation ─────────

// s7DriveCommands runs the real run() against a throwaway server, sends the
// given command frames, and collects every `output` action until each command
// has produced its `done`.
func s7DriveCommands(t *testing.T, cmds ...CommandRequest) []CommandResponse {
	t.Helper()
	collected := make(chan []CommandResponse, 1)
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil { // register
			collected <- nil
			return
		}
		for _, cmd := range cmds {
			payload, _ := json.Marshal(cmd)
			msg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				collected <- nil
				return
			}
		}
		var out []CommandResponse
		dones := 0
		for dones < len(cmds) {
			conn.SetReadDeadline(time.Now().Add(25 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var env AgentMessage
			if json.Unmarshal(data, &env) != nil || env.Action != "output" {
				continue
			}
			var resp CommandResponse
			if json.Unmarshal(env.Payload, &resp) != nil {
				continue
			}
			out = append(out, resp)
			if resp.Type == "done" {
				dones++
			}
		}
		collected <- out
	})
	select {
	case out := <-collected:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("the server handler never reported its frames")
		return nil
	}
}

func s7FramesOfType(frames []CommandResponse, id, typ string) []CommandResponse {
	var out []CommandResponse
	for _, f := range frames {
		if f.ID == id && f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func s7AssertOneFailedDone(t *testing.T, frames []CommandResponse, id string) {
	t.Helper()
	dones := s7FramesOfType(frames, id, "done")
	if len(dones) != 1 {
		t.Fatalf("%s produced %d done frames, want exactly 1 (the server frees cmdOwners/cmdNodes/summarySeen on done)", id, len(dones))
	}
	var payload struct {
		ExitOK bool `json:"exit_ok"`
	}
	if err := json.Unmarshal([]byte(dones[0].Data), &payload); err != nil {
		t.Fatalf("%s done payload %q: %v", id, dones[0].Data, err)
	}
	if payload.ExitOK {
		t.Errorf("%s reported exit_ok:true on a refusal", id)
	}
	if len(s7FramesOfType(frames, id, "error")) == 0 {
		t.Errorf("%s emitted no error frame before done", id)
	}
	if n := len(s7FramesOfType(frames, id, "summary")); n != 0 {
		t.Errorf("%s emitted %d summary frames on a refusal", id, n)
	}
	// done must be last for this command.
	lastIdx := -1
	doneIdx := -1
	for i, f := range frames {
		if f.ID != id {
			continue
		}
		lastIdx = i
		if f.Type == "done" {
			doneIdx = i
		}
	}
	if doneIdx != lastIdx {
		t.Errorf("%s sent frames after done", id)
	}
}

func TestS7EveryRefusalEmitsTheErrorThenExactlyOneFailedDone(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	storeLocalPrefixes(nil)
	downloadDeadline = time.Second

	cmds := []CommandRequest{
		{ID: "blocked-tcp", Type: "tcp", Target: "127.0.0.1:22"},
		{ID: "badport-tcp", Type: "tcp", Target: "example.com:80,443"},
		{ID: "blocked-tls", Type: "tls", Target: "10.0.0.1"},
		{ID: "badname-bench", Type: "dnsbench", Target: "single"},
		{ID: "badscheme-dl", Type: "download", Target: "ftp://example.com/x"},
		{ID: "blocked-dl", Type: "download", Target: "http://169.254.169.254/latest/meta-data/"},
	}
	frames := s7DriveCommands(t, cmds...)
	for _, cmd := range cmds {
		s7AssertOneFailedDone(t, frames, cmd.ID)
	}
	s7WaitForNoNativeRuns(t)
}

func TestS7DuplicateCommandIDIsRefused(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	// Occupy the ID deterministically, exactly as an in-flight probe would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if reason := registerNativeRun(CommandRequest{ID: "dup", Type: "tcp"}, 1, cancel); reason != "" {
		t.Fatalf("fixture registration refused: %s", reason)
	}
	t.Cleanup(func() { unregisterNativeRun("dup", 1) })
	_ = ctx

	port, accepted := s7TrapListener(t)
	frames := s7DriveCommands(t, CommandRequest{ID: "dup", Type: "tcp", Target: "127.0.0.1:" + port})
	s7AssertOneFailedDone(t, frames, "dup")
	if !strings.Contains(strings.Join(s7ErrorTexts(frames, "dup"), " "), "already running") {
		t.Errorf("the refusal does not name the duplicate id: %v", s7ErrorTexts(frames, "dup"))
	}
	if accepted.Load() != 0 {
		t.Errorf("the refused probe still dialed")
	}
}

func s7ErrorTexts(frames []CommandResponse, id string) []string {
	var out []string
	for _, f := range s7FramesOfType(frames, id, "error") {
		out = append(out, f.Data)
	}
	return out
}

func TestS7ThirdConcurrentDownloadIsRefused(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	_, cancelA := context.WithCancel(context.Background())
	_, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	for i, id := range []string{"dl-a", "dl-b"} {
		if reason := registerNativeRun(CommandRequest{ID: id, Type: "download"}, uint64(100+i), func() {}); reason != "" {
			t.Fatalf("fixture download %s refused: %s", id, reason)
		}
		defer unregisterNativeRun(id, uint64(100+i))
	}
	// A third download is refused …
	if reason := registerNativeRun(CommandRequest{ID: "dl-c", Type: "download"}, 200, func() {}); reason == "" {
		t.Fatalf("a third concurrent download was accepted")
	} else if !strings.Contains(reason, "concurrent download") {
		t.Errorf("refusal = %q", reason)
	}
	// … while a non-download native probe is not.
	if reason := registerNativeRun(CommandRequest{ID: "tcp-c", Type: "tcp"}, 201, func() {}); reason != "" {
		t.Fatalf("the download cap refused a tcp probe: %s", reason)
	}
	unregisterNativeRun("tcp-c", 201)

	// End to end, through the real dispatch path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	frames := s7DriveCommands(t, CommandRequest{ID: "dl-third", Type: "download", Target: srv.URL})
	s7AssertOneFailedDone(t, frames, "dl-third")
	if !strings.Contains(strings.Join(s7ErrorTexts(frames, "dl-third"), " "), "concurrent download") {
		t.Errorf("refusal texts = %v", s7ErrorTexts(frames, "dl-third"))
	}
}

// s7SlowServer trickles bytes forever so a probe has to be stopped from outside.
func s7SlowServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000000")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			if _, err := w.Write([]byte("z")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestS7CancelStopsANativeProbeAndClearsTheMap(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	downloadDeadline = 30 * time.Second // only a cancel can end this run quickly
	srv := s7SlowServer(t)

	type outcome struct {
		frames  []CommandResponse
		elapsed time.Duration
	}
	got := make(chan outcome, 1)
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			got <- outcome{}
			return
		}
		cmd := CommandRequest{ID: "cancel-me", Type: "download", Target: srv.URL + "/slow"}
		payload, _ := json.Marshal(cmd)
		msg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})
		conn.WriteMessage(websocket.TextMessage, msg)

		start := time.Now()
		var frames []CommandResponse
		sentCancel := false
		for {
			conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var env AgentMessage
			if json.Unmarshal(data, &env) != nil || env.Action != "output" {
				continue
			}
			var resp CommandResponse
			json.Unmarshal(env.Payload, &resp)
			frames = append(frames, resp)
			if !sentCancel && resp.Type == "output" && strings.Contains(resp.Data, "HTTP 200") {
				sentCancel = true
				cp, _ := json.Marshal(map[string]string{"id": cmd.ID})
				cm, _ := json.Marshal(AgentMessage{Action: "cancel", Payload: cp})
				conn.WriteMessage(websocket.TextMessage, cm)
			}
			if resp.Type == "done" {
				break
			}
		}
		got <- outcome{frames: frames, elapsed: time.Since(start)}
	})
	out := <-got
	if len(out.frames) == 0 {
		t.Fatal("no frames")
	}
	s7AssertOneFailedDone(t, out.frames, "cancel-me")
	if out.elapsed > 15*time.Second {
		t.Errorf("the probe took %s to stop — the cancel did not reach it", out.elapsed)
	}
	s7WaitForNoNativeRuns(t)
}

func TestS7DroppedConnectionCancelsEveryNativeProbe(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)
	downloadDeadline = 30 * time.Second
	srv := s7SlowServer(t)

	started := make(chan struct{}, 1)
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		if _, _, err := s5ReadEnvelope(conn); err != nil {
			started <- struct{}{}
			return
		}
		for _, id := range []string{"drop-a", "drop-b"} {
			payload, _ := json.Marshal(CommandRequest{ID: id, Type: "download", Target: srv.URL + "/slow"})
			msg, _ := json.Marshal(AgentMessage{Action: "command", Payload: payload})
			conn.WriteMessage(websocket.TextMessage, msg)
		}
		// Wait until both probes are registered, then drop the socket by
		// returning (the harness closes it).
		deadline := time.Now().Add(10 * time.Second)
		for runningNativeCount() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		started <- struct{}{}
	})
	<-started

	// Every probe must have been cancelled and unregistered itself. With a
	// 30-second download deadline, nothing but the teardown's cancel could have
	// ended them this quickly.
	s7WaitForNoNativeRuns(t)
}

func TestS7UnregisterOnlyRemovesItsOwnRegistration(t *testing.T) {
	s7Seams(t)
	if reason := registerNativeRun(CommandRequest{ID: "shared", Type: "tcp"}, 7, func() {}); reason != "" {
		t.Fatalf("register: %s", reason)
	}
	// A stale probe from a previous connection must not delete the live entry.
	unregisterNativeRun("shared", 6)
	if runningNativeCount() != 1 {
		t.Errorf("a stale token deleted a live registration")
	}
	unregisterNativeRun("shared", 7)
	if runningNativeCount() != 0 {
		t.Errorf("the owner could not delete its own registration")
	}
}

// ── -race: the shared state ──────────────────────────────────────────────────

func TestS7RaceOnLinkRefreshAgainstBlockedChecksAndTheDownloadCap(t *testing.T) {
	s7Seams(t)
	probeAllowPrivate.Store(false)
	localPrefixSource = func() []netip.Prefix {
		return []netip.Prefix{
			netip.MustParsePrefix("2001:470:abcd:1234::/64"),
			netip.MustParsePrefix("192.168.7.0/24"),
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// The refresher replaces the published set as fast as it can.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				refreshLocalPrefixes()
			}
		}
	}()
	// Readers hammer the policy.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addrs := []netip.Addr{
				netip.MustParseAddr("2001:470:abcd:1234::9"),
				netip.MustParseAddr("2606:4700:4700::1111"),
				netip.MustParseAddr("192.168.7.9"),
				netip.MustParseAddr(s7PublicLiteral),
			}
			for {
				select {
				case <-stop:
					return
				default:
					for _, a := range addrs {
						isBlockedAddr(a)
					}
					probeDialControl("tcp", net.JoinHostPort(addrs[i%len(addrs)].String(), "443"), nil)
				}
			}
		}(i)
	}
	// Two writers contend for the download cap.
	var accepted atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("race-%d-%d", i, n)
				token := nativeRunCounter.Add(1)
				if reason := registerNativeRun(CommandRequest{ID: id, Type: "download"}, token, func() {}); reason == "" {
					accepted.Add(1)
					unregisterNativeRun(id, token)
				}
			}
		}(i)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if accepted.Load() == 0 {
		t.Errorf("the download cap refused every registration")
	}
	s7WaitForNoNativeRuns(t)
}

// TestS7AnExpiredCertificateReachesTheBrowserAsASuccessfulFinding closes the
// loop the tls probe exists for: not just that probeTLS returns nil, but that
// the dispatch path then emits a `summary` frame and a `done` carrying
// exit_ok:true — which is what makes the chain_ok badge render at all.
func TestS7AnExpiredCertificateReachesTheBrowserAsASuccessfulFinding(t *testing.T) {
	s7Seams(t)
	s7AllowLoopback(t)

	cert := s7SelfSignedCert(t, time.Now().Add(-72*time.Hour), time.Now().Add(-24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}

	frames := s7DriveCommands(t, CommandRequest{ID: "tls-finding", Type: "tls", Target: u.Host})
	dones := s7FramesOfType(frames, "tls-finding", "done")
	if len(dones) != 1 {
		t.Fatalf("got %d done frames, want 1", len(dones))
	}
	var done struct {
		ExitOK bool `json:"exit_ok"`
	}
	if err := json.Unmarshal([]byte(dones[0].Data), &done); err != nil {
		t.Fatalf("done payload %q: %v", dones[0].Data, err)
	}
	if !done.ExitOK {
		t.Errorf("exit_ok:false — an untrusted certificate became a failed run instead of a finding")
	}
	summaries := s7FramesOfType(frames, "tls-finding", "summary")
	if len(summaries) != 1 {
		t.Fatalf("got %d summary frames, want 1", len(summaries))
	}
	s7AssertSummaryGrammar(t, "tls-live", []byte(summaries[0].Data))
	var summary map[string]any
	if err := json.Unmarshal([]byte(summaries[0].Data), &summary); err != nil {
		t.Fatalf("summary %q: %v", summaries[0].Data, err)
	}
	if ok, _ := summary["chain_ok"].(bool); ok {
		t.Errorf("chain_ok = true in the forwarded summary: %v", summary)
	}
	if days, _ := summary["days_remaining"].(float64); days >= 0 {
		t.Errorf("days_remaining = %v, want negative", summary["days_remaining"])
	}
	s7WaitForNoNativeRuns(t)
}
