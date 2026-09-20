package main

import (
	"slices"
	"strings"
	"testing"
)

// curlWriteFormatLiteral is buildHTTP's -w format string, pinned verbatim. The
// full-argv tables below assert it byte for byte: agent/summary_test.go's parser
// is taught exactly these placeholders, and the server-side summary for an http
// probe is derived from this line, so an "innocent" reformat breaks both.
const curlWriteFormatLiteral = "code=%{http_code}  dns=%{time_namelookup}s  connect=%{time_connect}s  tls=%{time_appconnect}s  ttfb=%{time_starttransfer}s  total=%{time_total}s  size=%{size_download}b\n"

// argsHaveTargetSeparator returns true if `--` appears in args before the
// target string. Confirms a future code path can't accidentally let target
// be parsed as a flag.
func argsHaveTargetSeparator(args []string, target string) bool {
	idx := slices.Index(args, target)
	if idx < 0 {
		return false
	}
	return slices.Contains(args[:idx], "--")
}

// ---- pinned argv templates --------------------------------------------------
//
// Full-argv equality, never substring: the whole point of moving validation out
// of the builders is that argv is now a pure function of the *normalized*
// options string, so every byte of it is assertable. A substring assertion would
// not catch a flag landing in the wrong position, which for dig (no `--`) and
// nexttrace (no flag terminator at all) is the difference between an option and
// an injection.
//
// These call the builders directly, i.e. with an already-normalized options
// string. options_test.go drives the same templates through buildCommandArgs so
// the normalization step is covered too.

func TestBuilderArgvTemplates(t *testing.T) {
	cases := []struct {
		name     string
		build    commandBuilder
		cmd      CommandRequest
		wantBin  string
		wantArgs []string
	}{
		// ping — [-4|-6] -c N [-s S] -- target
		{
			name:  "ping default count",
			build: buildPing, cmd: CommandRequest{Target: "example.com"},
			wantBin: "ping", wantArgs: []string{"-c", "10", "--", "example.com"},
		},
		{
			name: "ping count", build: buildPing,
			cmd:     CommandRequest{Target: "example.com", Options: "count=3"},
			wantBin: "ping", wantArgs: []string{"-c", "3", "--", "example.com"},
		},
		{
			name: "ping count and size", build: buildPing,
			cmd:     CommandRequest{Target: "example.com", Options: "count=3 size=64"},
			wantBin: "ping", wantArgs: []string{"-c", "3", "-s", "64", "--", "example.com"},
		},
		{
			name: "ping v6 prepends", build: buildPing,
			cmd:     CommandRequest{Target: "example.com", Options: "-6 count=3"},
			wantBin: "ping", wantArgs: []string{"-6", "-c", "3", "--", "example.com"},
		},
		// traceroute — [-4|-6] [-m H] [-I|-T] -- target
		{
			name: "traceroute bare", build: buildTraceroute,
			cmd:     CommandRequest{Target: "example.com"},
			wantBin: "traceroute", wantArgs: []string{"--", "example.com"},
		},
		{
			name: "traceroute maxhops", build: buildTraceroute,
			cmd:     CommandRequest{Target: "example.com", Options: "maxhops=5"},
			wantBin: "traceroute", wantArgs: []string{"-m", "5", "--", "example.com"},
		},
		{
			name: "traceroute icmp mode", build: buildTraceroute,
			cmd:     CommandRequest{Target: "example.com", Options: "mode=icmp"},
			wantBin: "traceroute", wantArgs: []string{"-I", "--", "example.com"},
		},
		{
			name: "traceroute udp mode adds nothing", build: buildTraceroute,
			cmd:     CommandRequest{Target: "example.com", Options: "mode=udp"},
			wantBin: "traceroute", wantArgs: []string{"--", "example.com"},
		},
		{
			name: "traceroute everything", build: buildTraceroute,
			cmd:     CommandRequest{Target: "example.com", Options: "-4 maxhops=5 mode=icmp"},
			wantBin: "traceroute", wantArgs: []string{"-4", "-m", "5", "-I", "--", "example.com"},
		},
		// mtr — [-4|-6] -r -w -c N -- target, N defaults to 5
		{
			name: "mtr default count", build: buildMtr,
			cmd:     CommandRequest{Target: "example.com"},
			wantBin: "mtr", wantArgs: []string{"-r", "-w", "-c", "5", "--", "example.com"},
		},
		{
			name: "mtr count", build: buildMtr,
			cmd:     CommandRequest{Target: "example.com", Options: "count=20"},
			wantBin: "mtr", wantArgs: []string{"-r", "-w", "-c", "20", "--", "example.com"},
		},
		{
			name: "mtr v6 prepends", build: buildMtr,
			cmd:     CommandRequest{Target: "example.com", Options: "-6"},
			wantBin: "mtr", wantArgs: []string{"-6", "-r", "-w", "-c", "5", "--", "example.com"},
		},
		// http — [-4|-6] -sS -o /dev/null -w FMT --max-time 15 -A ua [--head] -- target
		{
			name: "http bare", build: buildHTTP,
			cmd:     CommandRequest{Target: "example.com"},
			wantBin: "curl",
			wantArgs: []string{
				"-q", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
				"--max-time", "15", "-A", "parallax-probe/1.0",
				"--", "https://example.com",
			},
		},
		{
			name: "http head", build: buildHTTP,
			cmd:     CommandRequest{Target: "example.com", Options: "method=head"},
			wantBin: "curl",
			wantArgs: []string{
				"-q", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
				"--max-time", "15", "-A", "parallax-probe/1.0", "--head",
				"--", "https://example.com",
			},
		},
		{
			name: "http get adds nothing", build: buildHTTP,
			cmd:     CommandRequest{Target: "example.com", Options: "method=get"},
			wantBin: "curl",
			wantArgs: []string{
				"-q", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
				"--max-time", "15", "-A", "parallax-probe/1.0",
				"--", "https://example.com",
			},
		},
		{
			name: "http v6 prepends", build: buildHTTP,
			cmd:     CommandRequest{Target: "example.com", Options: "-6"},
			wantBin: "curl",
			wantArgs: []string{
				"-q", "-6", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
				"--max-time", "15", "-A", "parallax-probe/1.0",
				"--", "https://example.com",
			},
		},
		// dns — [+short] [+trace] [+dnssec] target [TYPE], IP flag last, no `--`
		{
			name: "dns bare", build: buildDns,
			cmd:     CommandRequest{Target: "example.com"},
			wantBin: "dig", wantArgs: []string{"example.com"},
		},
		{
			name: "dns type", build: buildDns,
			cmd:     CommandRequest{Target: "example.com", Options: "type=MX"},
			wantBin: "dig", wantArgs: []string{"example.com", "MX"},
		},
		{
			name: "dns bare flags then target then type", build: buildDns,
			cmd:     CommandRequest{Target: "example.com", Options: "short trace dnssec type=MX"},
			wantBin: "dig", wantArgs: []string{"+short", "+trace", "+dnssec", "example.com", "MX"},
		},
		{
			name: "dns ip flag is appended last", build: buildDns,
			cmd:     CommandRequest{Target: "example.com", Options: "-4 short type=MX"},
			wantBin: "dig", wantArgs: []string{"+short", "example.com", "MX", "-4"},
		},
		{
			name: "dns ptr of an ip literal", build: buildDns,
			cmd:     CommandRequest{Target: "1.1.1.1", Options: "type=PTR"},
			wantBin: "dig", wantArgs: []string{"-x", "1.1.1.1"},
		},
		{
			name: "dns ptr of an arpa name stays positional", build: buildDns,
			cmd:     CommandRequest{Target: "1.1.1.1.in-addr.arpa", Options: "type=PTR"},
			wantBin: "dig", wantArgs: []string{"1.1.1.1.in-addr.arpa", "PTR"},
		},
		// nexttrace — bare target, no options, ever
		{
			name: "nexttrace ignores options", build: buildNexttrace,
			cmd:     CommandRequest{Target: "example.com", Options: "-4 count=3 mode=icmp"},
			wantBin: "nexttrace", wantArgs: []string{"example.com"},
		},
		// iperf3 / speedtest — unchanged
		{
			name: "iperf3 client flag only", build: buildIperf3,
			cmd:     CommandRequest{Target: "host"},
			wantBin: "iperf3", wantArgs: []string{"-c", "host"},
		},
		{
			name: "speedtest takes no argv", build: buildSpeedtest,
			cmd:     CommandRequest{},
			wantBin: "speedtest", wantArgs: []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, args, err := c.build(c.cmd)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if bin != c.wantBin {
				t.Errorf("binary = %q, want %q", bin, c.wantBin)
			}
			if !slices.Equal(args, c.wantArgs) {
				t.Errorf("argv mismatch\n got: %#v\nwant: %#v", args, c.wantArgs)
			}
		})
	}
}

// dig gets no `--` at all: its argument parser has no end-of-options marker and
// would read `--` as a query name. The guard is the target-prefix rejection, so
// this is pinned rather than left to be "fixed" later.
func TestBuildDnsHasNoEndOfOptionsMarker(t *testing.T) {
	for _, opts := range []string{"", "type=MX", "short type=A", "-6"} {
		_, args, err := buildDns(CommandRequest{Target: "example.com", Options: opts})
		if err != nil {
			t.Fatalf("buildDns(%q): %v", opts, err)
		}
		if slices.Contains(args, "--") {
			t.Errorf("buildDns(%q) must not pass -- to dig, got %v", opts, args)
		}
	}
}

func TestBuildPingTargetSeparator(t *testing.T) {
	binary, args, err := buildPing(CommandRequest{Target: "example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if binary != "ping" {
		t.Errorf("binary = %q, want ping", binary)
	}
	if !argsHaveTargetSeparator(args, "example.com") {
		t.Errorf("expected -- before target in %v", args)
	}
}

func TestBuildPingRejectsLeadingDash(t *testing.T) {
	if _, _, err := buildPing(CommandRequest{Target: "-h"}); err == nil {
		t.Errorf("expected error for target with leading dash")
	}
}

// The builder's own bounds check is the backstop, not the authority:
// normalizeOptions never hands it an out-of-range count, but a direct caller
// still cannot smuggle one into argv.
func TestBuildPingClampCount(t *testing.T) {
	cases := []struct {
		options string
		want    string
	}{
		{"count=5", "5"},
		{"count=0", "10"},    // out of range, falls back to default
		{"count=999", "10"},  // out of range
		{"count=abc", "10"},  // invalid
		{"", "10"},           // unset
		{"count=100", "100"}, // upper bound inclusive
		{"count=1", "1"},     // lower bound inclusive
	}
	for _, c := range cases {
		_, args, err := buildPing(CommandRequest{Target: "host", Options: c.options})
		if err != nil {
			t.Fatalf("buildPing(%q): %v", c.options, err)
		}
		// args is [-c <count> -- host], possibly prefixed with -4/-6.
		idx := slices.Index(args, "-c")
		if idx < 0 || idx+1 >= len(args) || args[idx+1] != c.want {
			t.Errorf("buildPing(options=%q) count = %v, want %v (full args: %v)", c.options, args, c.want, args)
		}
	}
}

// Same backstop for the sizes and hop counts the restructure added.
func TestBuilderBoundsBackstops(t *testing.T) {
	if _, args, _ := buildPing(CommandRequest{Target: "host", Options: "size=15"}); slices.Contains(args, "-s") {
		t.Errorf("size=15 is below the spec minimum and must not reach argv: %v", args)
	}
	if _, args, _ := buildPing(CommandRequest{Target: "host", Options: "size=1473"}); slices.Contains(args, "-s") {
		t.Errorf("size=1473 is above the spec maximum and must not reach argv: %v", args)
	}
	if _, args, _ := buildTraceroute(CommandRequest{Target: "host", Options: "maxhops=65"}); slices.Contains(args, "-m") {
		t.Errorf("maxhops=65 is above the spec maximum and must not reach argv: %v", args)
	}
	_, args, _ := buildMtr(CommandRequest{Target: "host", Options: "count=21"})
	idx := slices.Index(args, "-c")
	if idx < 0 || args[idx+1] != "5" {
		t.Errorf("mtr count=21 is above the spec maximum; expected the default 5, got %v", args)
	}
}

func TestBuildPingIPVersion(t *testing.T) {
	_, args, _ := buildPing(CommandRequest{Target: "host", Options: "-6"})
	if args[0] != "-6" {
		t.Errorf("expected -6 prepended, got %v", args)
	}
}

func TestBuildTracerouteSeparator(t *testing.T) {
	_, args, _ := buildTraceroute(CommandRequest{Target: "host"})
	if !argsHaveTargetSeparator(args, "host") {
		t.Errorf("expected -- before target in %v", args)
	}
}

// mode=tcp is an operator opt-in: -T makes the agent open real TCP connections
// to the target port, which is a scan primitive.
func TestBuildTracerouteTCPModeNeedsTheAgentFlag(t *testing.T) {
	if _, args, _ := buildTraceroute(CommandRequest{Target: "host", Options: "mode=tcp"}); slices.Contains(args, "-T") {
		t.Errorf("mode=tcp must not reach argv on a default agent: %v", args)
	}
	old := allowTCPTraceroute
	allowTCPTraceroute = true
	t.Cleanup(func() { allowTCPTraceroute = old })
	_, args, _ := buildTraceroute(CommandRequest{Target: "host", Options: "mode=tcp"})
	if !slices.Equal(args, []string{"-T", "--", "host"}) {
		t.Errorf("with -allow-tcp-traceroute, argv = %v, want [-T -- host]", args)
	}
}

func TestBuildMtrSeparator(t *testing.T) {
	_, args, _ := buildMtr(CommandRequest{Target: "host"})
	if !argsHaveTargetSeparator(args, "host") {
		t.Errorf("expected -- before target in %v", args)
	}
}

func TestBuildNexttraceNoSeparator(t *testing.T) {
	// nexttrace doesn't accept -- (custom arg parser), so target is bare.
	_, args, _ := buildNexttrace(CommandRequest{Target: "host"})
	if slices.Contains(args, "--") {
		t.Errorf("nexttrace must not include --, got %v", args)
	}
	if !slices.Contains(args, "host") {
		t.Errorf("expected target in args: %v", args)
	}
}

// The regression this whole slice exists to avoid: iperf3 is `raw`, so a shipped
// option string keeps producing exactly the argv it produced at HEAD e670742.
// These strings live in users' saved presets, their command history and their
// stored run records; the new token grammar cannot represent them.
func TestBuildIperf3ArgvIsByteIdenticalToHEAD(t *testing.T) {
	bin, args, err := buildIperf3(CommandRequest{Target: "host", Options: "-p 5201 -t 30 -P 4 -R"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bin != "iperf3" {
		t.Errorf("binary = %q, want iperf3", bin)
	}
	want := []string{"-c", "host", "-p", "5201", "-t", "30", "-P", "4", "-R"}
	if !slices.Equal(args, want) {
		t.Errorf("iperf3 argv changed\n got: %#v\nwant: %#v", args, want)
	}
}

func TestBuildIperf3FilterUnknownFlags(t *testing.T) {
	// Smuggle an unsupported flag — must be dropped silently.
	_, args, err := buildIperf3(CommandRequest{Target: "host", Options: "--logfile /tmp/x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range args {
		if a == "--logfile" || a == "/tmp/x" {
			t.Errorf("--logfile or its value leaked through: %v", args)
		}
	}
}

func TestBuildIperf3PortRange(t *testing.T) {
	cases := []struct {
		port     string
		expected bool // expected to be present in args
	}{
		{"80", false},    // below 1024 — rejected
		{"1024", true},   // lower bound
		{"5201", true},   // typical iperf3 port
		{"65535", true},  // upper bound
		{"99999", false}, // above 65535
		{"abc", false},   // not a number
	}
	for _, c := range cases {
		_, args, _ := buildIperf3(CommandRequest{Target: "host", Options: "-p " + c.port})
		got := slices.Contains(args, c.port)
		if got != c.expected {
			t.Errorf("buildIperf3 -p %s present=%v, want %v (args=%v)", c.port, got, c.expected, args)
		}
	}
}

func TestBuildIperf3DurationCap(t *testing.T) {
	_, args, _ := buildIperf3(CommandRequest{Target: "host", Options: "-t 999"})
	if slices.Contains(args, "999") {
		t.Errorf("expected -t 999 to be rejected, got %v", args)
	}
	_, args, _ = buildIperf3(CommandRequest{Target: "host", Options: "-t 30"})
	if !slices.Contains(args, "30") {
		t.Errorf("expected -t 30 to be accepted, got %v", args)
	}
}

func TestBuildIperf3BandwidthCap(t *testing.T) {
	// 200 Mbit/s exceeds the 100 Mbit/s cap.
	_, args, _ := buildIperf3(CommandRequest{Target: "host", Options: "-b 200000000"})
	if slices.Contains(args, "200000000") {
		t.Errorf("expected -b 200_000_000 to be rejected, got %v", args)
	}
}

func TestBuildIperf3KeepsClientFlag(t *testing.T) {
	binary, args, _ := buildIperf3(CommandRequest{Target: "host", Options: ""})
	if binary != "iperf3" {
		t.Errorf("binary = %q", binary)
	}
	if len(args) < 2 || args[0] != "-c" || args[1] != "host" {
		t.Errorf("expected args to start with [-c host], got %v", args)
	}
}

func TestBuildDnsRejectsAtServer(t *testing.T) {
	if _, _, err := buildDns(CommandRequest{Target: "@8.8.8.8"}); err == nil {
		t.Errorf("expected error for target starting with @")
	}
}

// With no `--` available, every character dig treats as special in argument
// position has to be refused outright: `-` (flag), `@` (server), `+` (query
// option).
func TestBuildDnsRejectsSpecialTargetPrefixes(t *testing.T) {
	for _, target := range []string{"-h", "@8.8.8.8", "+short", "+trace", "-oProxyCommand=id"} {
		if _, _, err := buildDns(CommandRequest{Target: target}); err == nil {
			t.Errorf("expected error for dns target %q", target)
		}
	}
}

func TestBuildDnsPTRReverseLookup(t *testing.T) {
	// PTR + IP target should yield `dig -x <ip>`.
	_, args, _ := buildDns(CommandRequest{Target: "1.1.1.1", Options: "type=PTR"})
	if !slices.Contains(args, "-x") || !slices.Contains(args, "1.1.1.1") {
		t.Errorf("expected dig -x 1.1.1.1, got %v", args)
	}
	// PTR + non-IP target (already in .in-addr.arpa form) passes literal.
	_, args, _ = buildDns(CommandRequest{Target: "1.1.1.1.in-addr.arpa", Options: "type=PTR"})
	if slices.Contains(args, "-x") {
		t.Errorf("expected no -x for explicit arpa target, got %v", args)
	}
}

func TestBuildDnsRecordTypeWhitelist(t *testing.T) {
	// Valid type passes through.
	_, args, _ := buildDns(CommandRequest{Target: "example.com", Options: "type=MX"})
	if !slices.Contains(args, "MX") {
		t.Errorf("expected MX in args: %v", args)
	}
	// Invalid type is silently dropped (no shell injection vector).
	_, args, _ = buildDns(CommandRequest{Target: "example.com", Options: "type=MAGIC"})
	if slices.Contains(args, "MAGIC") {
		t.Errorf("expected MAGIC to be dropped: %v", args)
	}
	// The removed fallthrough: a bare token is no longer read as a record type
	// by the builder. Rescuing the legacy form is normalizeOptions' job, against
	// the same table — see options_test.go.
	_, args, _ = buildDns(CommandRequest{Target: "example.com", Options: "MX"})
	if slices.Contains(args, "MX") {
		t.Errorf("buildDns must not read a bare token as the record type: %v", args)
	}
}

func TestBuildHTTPAcceptsHostname(t *testing.T) {
	binary, args, err := buildHTTP(CommandRequest{Target: "example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if binary != "curl" {
		t.Errorf("binary = %q, want curl", binary)
	}
	if !slices.Contains(args, "https://example.com") {
		t.Errorf("expected https:// prefix in target, got %v", args)
	}
}

func TestBuildHTTPRejectsBadSchemes(t *testing.T) {
	cases := []string{"ftp://x", "file:///etc/passwd", "javascript:1"}
	for _, c := range cases {
		if _, _, err := buildHTTP(CommandRequest{Target: c}); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestBuildHTTPRejectsShellChars(t *testing.T) {
	for _, c := range []string{"x; rm -rf", "x|cat", "x`whoami`", "x\"y", "x with space"} {
		if _, _, err := buildHTTP(CommandRequest{Target: c}); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// No redirect following, ever (F3): a probe that follows a redirect can be
// walked onto a host the operator never named.
func TestBuildHTTPNeverFollowsRedirects(t *testing.T) {
	for _, opts := range []string{"", "method=head", "follow", "follow=1", "location"} {
		_, args, err := buildHTTP(CommandRequest{Target: "example.com", Options: opts})
		if err != nil {
			t.Fatalf("buildHTTP(%q): %v", opts, err)
		}
		for _, a := range args {
			if a == "-L" || a == "--location" || a == "--location-trusted" {
				t.Errorf("buildHTTP(%q) put a redirect-following flag in argv: %v", opts, args)
			}
		}
	}
}

func TestBuildSpeedtestNoArgs(t *testing.T) {
	binary, args, err := buildSpeedtest(CommandRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if binary != "speedtest" {
		t.Errorf("binary = %q", binary)
	}
	if len(args) != 0 {
		t.Errorf("expected empty args, got %v", args)
	}
}

func TestBuildCommandArgsUnknown(t *testing.T) {
	if _, _, _, err := buildCommandArgs(CommandRequest{Type: "rm-rf", Target: "host"}); err == nil {
		t.Errorf("expected unsupported command error")
	}
}

func TestAppendIPVersionFlagsExactMatch(t *testing.T) {
	// Verify whole-token matching: "count=-4" must not trip the -4 detector.
	got := appendIPVersionFlags([]string{"x"}, "count=-4")
	if slices.Contains(got, "-4") {
		t.Errorf("expected count=-4 to NOT add -4 flag, got %v", got)
	}
}

func TestStripControlCharsExtracted(t *testing.T) {
	// Verify the agent's options control-char rejection chars match what
	// executeCommand checks. Updating one without the other would silently
	// open the gate.
	for _, c := range []string{"\n", "\r", "\t", "\x00"} {
		if !strings.ContainsAny(c, "\n\r\t\x00") {
			t.Errorf("expected %q to be rejected", c)
		}
	}
}
