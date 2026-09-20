package main

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// These cover the normalization step that now owns the option grammar: the token
// parser, the per-tool bounds, the notes, and the two exemptions (raw types and
// a type with a builder but no spec). The argv templates themselves are pinned in
// builders_test.go; the IP-version rows below re-assert them *through*
// buildCommandArgs, because a canonically re-encoded normalized string would
// drop the flag silently and the optional-flag templates alone would not notice.

// buildVia runs the whole pipeline: normalization, then the builder.
func buildVia(t *testing.T, cmd CommandRequest) (string, []string, []string) {
	t.Helper()
	bin, args, notes, err := buildCommandArgs(cmd)
	if err != nil {
		t.Fatalf("buildCommandArgs(%+v): unexpected error %v", cmd, err)
	}
	return bin, args, notes
}

func notesMention(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// ---- the spec table itself --------------------------------------------------

// Whitelist-sync invariant, agent side: every command type the agent will run
// must have an explicit optionSpecs entry, so adding a probe forces a deliberate
// decision about its options instead of silently inheriting `raw`.
func TestOptionSpecsCoverEveryAllowedCommand(t *testing.T) {
	for cmdType := range allowedCommands {
		if _, ok := optionSpecs[cmdType]; !ok {
			t.Errorf("allowedCommands[%q] has no optionSpecs entry — its options would pass through unvalidated", cmdType)
		}
	}
}

// order drives the normalized encoding, so a key missing from it would be parsed,
// validated and then thrown away.
func TestOptionSpecOrderCoversEveryKey(t *testing.T) {
	for cmdType, spec := range optionSpecs {
		if spec.raw {
			if len(spec.keys) > 0 || len(spec.order) > 0 {
				t.Errorf("optionSpecs[%q] is raw but declares keys/order", cmdType)
			}
			continue
		}
		if len(spec.order) != len(spec.keys) {
			t.Errorf("optionSpecs[%q]: order has %d entries, keys has %d", cmdType, len(spec.order), len(spec.keys))
		}
		for _, key := range spec.order {
			if _, ok := spec.keys[key]; !ok {
				t.Errorf("optionSpecs[%q].order names %q, which is not a key", cmdType, key)
			}
		}
		for key, def := range spec.keys {
			if !slices.Contains(spec.order, key) {
				t.Errorf("optionSpecs[%q].keys has %q but order does not — it would never be encoded", cmdType, key)
			}
			if def.kind == optInt && def.min > def.max {
				t.Errorf("optionSpecs[%q].keys[%q] has min > max", cmdType, key)
			}
			if def.kind != optInt && len(def.enum) == 0 {
				t.Errorf("optionSpecs[%q].keys[%q] is an enum/flag with no values", cmdType, key)
			}
		}
	}
}

// ---- the token grammar ------------------------------------------------------

func TestParseOptionsTokenGrammar(t *testing.T) {
	opts, dropped, err := parseOptions("-6 count=10 size=64 type=A short BOGUS c=1 waytoolongakey=1 has_underscore=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		ipVersionKey: "-6",
		"count":      "10",
		"size":       "64",
		"type":       "A",
		"short":      "",
	}
	if len(opts) != len(want) {
		t.Errorf("parsed %v, want %v", opts, want)
	}
	for k, v := range want {
		if opts[k] != v {
			t.Errorf("opts[%q] = %q, want %q", k, opts[k], v)
		}
	}
	for _, tok := range []string{"BOGUS", "c=1", "waytoolongakey=1", "has_underscore=1"} {
		if !slices.Contains(dropped, tok) {
			t.Errorf("expected %q to be dropped, dropped = %v", tok, dropped)
		}
	}
}

// The value class excludes `-`, `;`, `/`, `+`, `,`, `:` and space, so these are
// not merely rejected — they are unrepresentable (F2).
func TestParseOptionsValueClassExcludesMetacharacters(t *testing.T) {
	for _, tok := range []string{
		"count=5;rm", "size=-1", "type=A;id", "count=5/6", "type=A,B",
		"mode=a:b", "count=+5", "count=$(id)", "count=`id`", "count=5|6",
		"type=A&B", "count=5>x", "mode=a*b", `count="5"`, "count='5'",
	} {
		opts, dropped, err := parseOptions(tok)
		if err != nil {
			t.Fatalf("parseOptions(%q): %v", tok, err)
		}
		if len(opts) != 0 {
			t.Errorf("parseOptions(%q) accepted %v — the value class must reject it", tok, opts)
		}
		if !slices.Contains(dropped, tok) {
			t.Errorf("parseOptions(%q) dropped = %v, want the token itself", tok, dropped)
		}
	}
}

// 17 tokens rejects the whole command rather than truncating. Named on `ping`
// explicitly: iperf3 and speedtest are raw and exempt from the cap, and their
// shipped option strings must stay untouched.
func TestSeventeenTokensOnPingRejectsTheWholeCommand(t *testing.T) {
	sixteen := strings.TrimSpace(strings.Repeat("count=5 ", 16))
	if _, _, _, err := buildCommandArgs(CommandRequest{Type: "ping", Target: "host", Options: sixteen}); err != nil {
		t.Fatalf("16 tokens must be accepted: %v", err)
	}
	seventeen := strings.TrimSpace(strings.Repeat("count=5 ", 17))
	_, args, notes, err := buildCommandArgs(CommandRequest{Type: "ping", Target: "host", Options: seventeen})
	if err == nil {
		t.Fatalf("17 tokens must reject the whole command, got argv %v", args)
	}
	if args != nil || notes != nil {
		t.Errorf("a rejected command must return no argv and no notes, got %v / %v", args, notes)
	}
	if !strings.Contains(err.Error(), "too many options") {
		t.Errorf("error = %v, want a too-many-options error", err)
	}
}

// iperf3 and speedtest are exempt from the grammar, the token cap and the notes.
func TestRawTypesAreExemptFromGrammarAndTokenCap(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("-R ", 40))
	bin, args, notes, err := buildCommandArgs(CommandRequest{Type: "iperf3", Target: "host", Options: long})
	if err != nil {
		t.Fatalf("iperf3 is raw and must not hit the token cap: %v", err)
	}
	if bin != "iperf3" || len(notes) != 0 {
		t.Errorf("iperf3 bin=%q notes=%v, want iperf3 and no notes", bin, notes)
	}
	if !slices.Contains(args, "-R") {
		t.Errorf("iperf3 argv lost its whitelisted flags: %v", args)
	}
	// The shipped option string, end to end through the pipeline.
	_, args, notes, err = buildCommandArgs(CommandRequest{Type: "iperf3", Target: "host", Options: "-p 5201 -t 30 -P 4 -R"})
	if err != nil {
		t.Fatalf("shipped iperf3 option string was rejected: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("shipped iperf3 option string produced notes: %v", notes)
	}
	if want := []string{"-c", "host", "-p", "5201", "-t", "30", "-P", "4", "-R"}; !slices.Equal(args, want) {
		t.Errorf("iperf3 argv through buildCommandArgs\n got: %#v\nwant: %#v", args, want)
	}
	_, args, notes, err = buildCommandArgs(CommandRequest{Type: "speedtest", Options: "anything at all -4"})
	if err != nil || len(args) != 0 || len(notes) != 0 {
		t.Errorf("speedtest = (%v, %v, %v), want empty argv, no notes, no error", args, notes, err)
	}
}

// A command type with a builder but no optionSpecs entry is raw too. This is not
// hypothetical: agent/summary_test.go registers throwaway types exactly that way.
func TestTypeWithBuilderButNoSpecPassesOptionsThrough(t *testing.T) {
	const weird = "COUNT=5 --logfile /tmp/x +short"
	var seen string
	withTestCommand(t, "testnospec", func(cmd CommandRequest) (string, []string, error) {
		seen = cmd.Options
		return "sh", []string{"-c", "true"}, nil
	})
	if _, ok := optionSpecs["testnospec"]; ok {
		t.Fatal("testnospec must not have a spec for this test to mean anything")
	}
	_, _, notes, err := buildCommandArgs(CommandRequest{Type: "testnospec", Target: "x", Options: weird})
	if err != nil {
		t.Fatalf("a spec-less type must not be an error: %v", err)
	}
	if seen != weird {
		t.Errorf("builder saw options %q, want them untouched (%q)", seen, weird)
	}
	if len(notes) != 0 {
		t.Errorf("a spec-less type must produce no notes, got %v", notes)
	}
}

// ---- bounds, at the limits and past them -----------------------------------

func TestBoundedValuesAtTheirLimits(t *testing.T) {
	cases := []struct {
		cmdType string
		options string
		want    []string // argv, target "host"
	}{
		{"ping", "count=1", []string{"-c", "1", "--", "host"}},
		{"ping", "count=100", []string{"-c", "100", "--", "host"}},
		{"ping", "size=16", []string{"-c", "10", "-s", "16", "--", "host"}},
		{"ping", "size=1472", []string{"-c", "10", "-s", "1472", "--", "host"}},
		{"traceroute", "maxhops=1", []string{"-m", "1", "--", "host"}},
		{"traceroute", "maxhops=64", []string{"-m", "64", "--", "host"}},
		{"mtr", "count=1", []string{"-r", "-w", "-c", "1", "--", "host"}},
		{"mtr", "count=20", []string{"-r", "-w", "-c", "20", "--", "host"}},
	}
	for _, c := range cases {
		_, args, notes := buildVia(t, CommandRequest{Type: c.cmdType, Target: "host", Options: c.options})
		if !slices.Equal(args, c.want) {
			t.Errorf("%s %q argv\n got: %#v\nwant: %#v", c.cmdType, c.options, args, c.want)
		}
		if len(notes) != 0 {
			t.Errorf("%s %q is in range and must produce no notes, got %v", c.cmdType, c.options, notes)
		}
	}
}

// Out of range is dropped with a note, never clamped: a silently clamped value
// makes a probe lie about what it measured.
func TestOutOfRangeValuesAreDroppedWithANote(t *testing.T) {
	cases := []struct {
		cmdType  string
		options  string
		want     []string
		noteWord string
	}{
		{"ping", "count=0", []string{"-c", "10", "--", "host"}, "count=0"},
		{"ping", "count=101", []string{"-c", "10", "--", "host"}, "count=101"},
		{"ping", "size=15", []string{"-c", "10", "--", "host"}, "size=15"},
		{"ping", "size=1473", []string{"-c", "10", "--", "host"}, "size=1473"},
		{"traceroute", "maxhops=65", []string{"--", "host"}, "maxhops=65"},
		{"traceroute", "maxhops=0", []string{"--", "host"}, "maxhops=0"},
		{"mtr", "count=21", []string{"-r", "-w", "-c", "5", "--", "host"}, "count=21"},
	}
	for _, c := range cases {
		_, args, notes := buildVia(t, CommandRequest{Type: c.cmdType, Target: "host", Options: c.options})
		if !slices.Equal(args, c.want) {
			t.Errorf("%s %q argv\n got: %#v\nwant: %#v", c.cmdType, c.options, args, c.want)
		}
		if !notesMention(notes, c.noteWord) {
			t.Errorf("%s %q: expected a note naming %s, got %v", c.cmdType, c.options, c.noteWord, notes)
		}
	}
}

// ---- injection attempts -----------------------------------------------------

func TestOptionInjectionAttemptsNeverReachArgv(t *testing.T) {
	cases := []struct {
		cmdType string
		options string
		want    []string
	}{
		{"ping", "count=5 -f", []string{"-c", "5", "--", "host"}},
		{"ping", "count=5;rm -rf /", []string{"-c", "10", "--", "host"}},
		{"ping", "size=-1", []string{"-c", "10", "--", "host"}},
		{"ping", "count=5 --logfile=/tmp/x", []string{"-c", "5", "--", "host"}},
		{"ping", "-4 -f -s", []string{"-4", "-c", "10", "--", "host"}},
		{"traceroute", "mode=icmp;id", []string{"--", "host"}},
		{"mtr", "count=5 -O /tmp/x", []string{"-r", "-w", "-c", "5", "--", "host"}},
		{"http", "method=head;id", []string{
			"-q", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
			"--max-time", "15", "-A", "parallax-probe/1.0",
			"--", "https://host",
		}},
	}
	for _, c := range cases {
		_, args, notes := buildVia(t, CommandRequest{Type: c.cmdType, Target: "host", Options: c.options})
		if !slices.Equal(args, c.want) {
			t.Errorf("%s %q argv\n got: %#v\nwant: %#v", c.cmdType, c.options, args, c.want)
		}
		if len(notes) == 0 {
			t.Errorf("%s %q dropped tokens but produced no note", c.cmdType, c.options)
		}
	}
	// dns separately: `type=A;id` must not reach dig, and the type must not be
	// rescued from the malformed token either.
	_, args, notes := buildVia(t, CommandRequest{Type: "dns", Target: "example.com", Options: "type=A;id"})
	if !slices.Equal(args, []string{"example.com"}) {
		t.Errorf("dns type=A;id argv = %#v, want just the target", args)
	}
	if len(notes) == 0 {
		t.Errorf("dns type=A;id produced no note")
	}
}

// The reserved IP-version slot is writable only by the literal `-4`/`-6` tokens.
// `ipv=short` must not put the bare token `short` into the normalized string,
// where buildDns would read it as +short — a value must never become a token.
func TestReservedIPVersionKeyCannotBeWrittenAsAValue(t *testing.T) {
	opts, dropped, err := parseOptions("ipv=short ipv=4 ipv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := opts[ipVersionKey]; ok {
		t.Errorf("the reserved key was written from a key=value token: %v", opts)
	}
	for _, tok := range []string{"ipv=short", "ipv=4"} {
		if !slices.Contains(dropped, tok) {
			t.Errorf("expected %q to be dropped, got %v", tok, dropped)
		}
	}
	normalized, _, err := normalizeOptions("dns", optionSpecs["dns"], "ipv=short")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if normalized != "" {
		t.Errorf("normalized = %q, want empty", normalized)
	}
	_, args, _ := buildVia(t, CommandRequest{Type: "dns", Target: "example.com", Options: "ipv=short"})
	if slices.Contains(args, "+short") {
		t.Errorf("ipv=short injected +short into argv: %v", args)
	}
	// And the bare `ipv` key, which the grammar does accept as a key, is simply
	// an unknown option — not the reserved slot.
	_, args, notes := buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "ipv"})
	if want := []string{"-c", "10", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if !notesMention(notes, "ipv") {
		t.Errorf("expected a note for the unknown key, got %v", notes)
	}
}

// A target is never readable as a flag, an option or a server name.
func TestHostileTargetsAreRejected(t *testing.T) {
	cases := []struct {
		cmdType string
		target  string
	}{
		{"ping", "-h"},
		{"ping", "-oProxyCommand=id"},
		{"traceroute", "-h"},
		{"mtr", "-h"},
		{"nexttrace", "-h"},
		{"iperf3", "-h"},
		{"dns", "-h"},
		{"dns", "@8.8.8.8"},
		{"dns", "+short"},
		{"dns", "-oProxyCommand=id"},
		{"http", "x; rm -rf"},
	}
	for _, c := range cases {
		_, args, _, err := buildCommandArgs(CommandRequest{Type: c.cmdType, Target: c.target})
		if err == nil {
			t.Errorf("%s target %q was accepted, argv = %v", c.cmdType, c.target, args)
		}
	}
}

// ---- duplicates, unknown keys, nexttrace -----------------------------------

// parseOptions' rule is "the last grammar-valid occurrence wins"; the bounds then
// run once, on that value. So a later out-of-range duplicate is dropped with a
// note and the builder's default applies — it does not fall back to an earlier
// in-range value, because only one value per key ever reaches validation.
func TestDuplicateKeyTakesTheLastValue(t *testing.T) {
	_, args, notes := buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "count=5 count=7"})
	if want := []string{"-c", "7", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if len(notes) != 0 {
		t.Errorf("both duplicates are valid, so there should be no note: %v", notes)
	}
	_, args, notes = buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "count=5 count=999"})
	if want := []string{"-c", "10", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want the default (%#v)", args, want)
	}
	if !notesMention(notes, "count=999") {
		t.Errorf("expected a note naming the rejected duplicate, got %v", notes)
	}
	// Two IP-version tokens collapse to one reserved key, last wins.
	_, args, _ = buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "-4 -6"})
	if want := []string{"-6", "-c", "10", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
}

func TestUnknownOptionKeyIsDroppedWithANote(t *testing.T) {
	_, args, notes := buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "maxhops=5 method=head"})
	if want := []string{"-c", "10", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if !notesMention(notes, "maxhops") || !notesMention(notes, "method") {
		t.Errorf("expected notes for both foreign keys, got %v", notes)
	}
}

// nexttrace's argv parser has no flag terminator, so it takes no options at all.
func TestNexttraceIgnoresEveryOption(t *testing.T) {
	bin, args, notes := buildVia(t, CommandRequest{
		Type: "nexttrace", Target: "example.com",
		Options: "-4 count=3 maxhops=5 mode=icmp short",
	})
	if bin != "nexttrace" {
		t.Errorf("binary = %q", bin)
	}
	if want := []string{"example.com"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if len(notes) == 0 {
		t.Errorf("expected notes explaining that nexttrace takes no options")
	}
}

// ---- traceroute mode=tcp gate (F4) -----------------------------------------

func TestTracerouteTCPModeIsGatedOnTheAgentFlag(t *testing.T) {
	if allowTCPTraceroute {
		t.Fatal("allowTCPTraceroute must default to false")
	}
	_, args, notes := buildVia(t, CommandRequest{Type: "traceroute", Target: "host", Options: "mode=tcp"})
	if want := []string{"--", "host"}; !slices.Equal(args, want) {
		t.Errorf("default agent argv = %#v, want %#v", args, want)
	}
	if !notesMention(notes, "-allow-tcp-traceroute") {
		t.Errorf("expected a note naming the agent flag, got %v", notes)
	}

	old := allowTCPTraceroute
	allowTCPTraceroute = true
	t.Cleanup(func() { allowTCPTraceroute = old })
	_, args, notes = buildVia(t, CommandRequest{Type: "traceroute", Target: "host", Options: "mode=tcp"})
	if want := []string{"-T", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("opted-in agent argv = %#v, want %#v", args, want)
	}
	if len(notes) != 0 {
		t.Errorf("an honoured option must produce no note, got %v", notes)
	}
}

// ---- legacy bare DNS record type -------------------------------------------

// The UI, saved presets, saved history and /api/runs replays all carry the DNS
// record type as a bare uppercase token. It is rescued against validDNSTypes —
// an explicit table-validated allowance, not the removed "any token becomes the
// type" fallthrough.
func TestLegacyBareDNSTypeIsRescuedWithADeprecationNote(t *testing.T) {
	bin, args, notes := buildVia(t, CommandRequest{Type: "dns", Target: "example.com", Options: "MX"})
	if bin != "dig" {
		t.Errorf("binary = %q", bin)
	}
	if want := []string{"example.com", "MX"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if !notesMention(notes, "deprecated") || !notesMention(notes, "type=MX") {
		t.Errorf("expected a deprecation note pointing at type=MX, got %v", notes)
	}
	// A bare non-type token is not rescued.
	_, args, notes = buildVia(t, CommandRequest{Type: "dns", Target: "example.com", Options: "MAGIC"})
	if want := []string{"example.com"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if !notesMention(notes, "MAGIC") {
		t.Errorf("expected a drop note for MAGIC, got %v", notes)
	}
	// An explicit type= wins; the bare token is then just a dropped token.
	_, args, notes = buildVia(t, CommandRequest{Type: "dns", Target: "example.com", Options: "type=A MX"})
	if want := []string{"example.com", "A"}; !slices.Equal(args, want) {
		t.Errorf("argv = %#v, want %#v", args, want)
	}
	if notesMention(notes, "deprecated") {
		t.Errorf("no deprecation note is due when type= was given explicitly: %v", notes)
	}
	if !notesMention(notes, `"MX"`) {
		t.Errorf("expected the bare MX to be reported as dropped, got %v", notes)
	}
	// The rescue is dns-only: ping has no `type` key, so a bare MX is not
	// reinterpreted there.
	_, args, _ = buildVia(t, CommandRequest{Type: "ping", Target: "host", Options: "MX"})
	if want := []string{"-c", "10", "--", "host"}; !slices.Equal(args, want) {
		t.Errorf("ping argv = %#v, want %#v", args, want)
	}
}

// ---- notes ------------------------------------------------------------------

func TestEveryDropProducesANote(t *testing.T) {
	cases := []struct {
		cmdType string
		options string
		drops   int // how many tokens must be reported
	}{
		{"ping", "count=101", 1},
		{"ping", "count=101 size=1473", 2},
		{"ping", "BOGUS count=5;x maxhops=1", 3},
		{"traceroute", "mode=tcp", 1},
		{"dns", "short=1", 1},
		{"http", "method=post", 1},
	}
	for _, c := range cases {
		_, _, notes := buildVia(t, CommandRequest{Type: c.cmdType, Target: "host", Options: c.options})
		if len(notes) != c.drops {
			t.Errorf("%s %q produced %d notes, want %d: %v", c.cmdType, c.options, len(notes), c.drops, notes)
		}
		for _, n := range notes {
			if !strings.HasPrefix(n, "[options] ") {
				t.Errorf("note %q is missing the [options] prefix", n)
			}
		}
	}
}

// Notes travel on the call stack, so concurrent commands cannot see each other's.
// A package-level note map would show up here under -race, and as cross-talk even
// without it.
func TestConcurrentCommandsDoNotShareNotes(t *testing.T) {
	const rounds = 40
	var wg sync.WaitGroup
	fail := make(chan string, rounds*2)
	for i := 0; i < rounds; i++ {
		for _, c := range []struct{ options, mine, theirs string }{
			{"count=101", "count=101", "size=1473"},
			{"size=1473", "size=1473", "count=101"},
		} {
			wg.Add(1)
			go func(options, mine, theirs string) {
				defer wg.Done()
				_, _, notes, err := buildCommandArgs(CommandRequest{Type: "ping", Target: "host", Options: options})
				if err != nil {
					fail <- "unexpected error: " + err.Error()
					return
				}
				if len(notes) != 1 || !strings.Contains(notes[0], mine) {
					fail <- "notes for " + options + " = " + strings.Join(notes, " | ")
					return
				}
				if notesMention(notes, theirs) {
					fail <- "cross-talk: " + options + " saw " + theirs
				}
			}(c.options, c.mine, c.theirs)
		}
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Error(msg)
	}
}

// ---- the pinned normalized encoding ----------------------------------------

// The normalized string is re-read by the builders, so its encoding is part of
// the contract. The IP version must come back as the literal `-4`/`-6` token: a
// canonical `ipv=4` would leave appendIPVersionFlags and buildDns scanning for a
// token that is no longer there, silently dropping the flag from argv for all
// five grammar-governed builders.
func TestNormalizedStringReEmitsTheLiteralIPVersionToken(t *testing.T) {
	cases := []struct {
		cmdType string
		options string
		want    string
	}{
		{"ping", "-4 count=3", "-4 count=3"},
		{"ping", "count=3 -6 size=64", "-6 count=3 size=64"},
		{"traceroute", "-4 mode=icmp maxhops=9", "-4 maxhops=9 mode=icmp"},
		{"mtr", "-6 count=7", "-6 count=7"},
		{"http", "-4 method=head", "-4 method=head"},
		{"dns", "-4 MX", "-4 type=MX"},
		{"dns", "-6 type=a short", "-6 short type=A"},
	}
	for _, c := range cases {
		got, _, err := normalizeOptions(c.cmdType, optionSpecs[c.cmdType], c.options)
		if err != nil {
			t.Fatalf("normalizeOptions(%s, %q): %v", c.cmdType, c.options, err)
		}
		if got != c.want {
			t.Errorf("normalizeOptions(%s, %q) = %q, want %q", c.cmdType, c.options, got, c.want)
		}
		if strings.Contains(got, ipVersionKey+"=") {
			t.Errorf("normalizeOptions(%s, %q) emitted a canonical %s= key: %q", c.cmdType, c.options, ipVersionKey, got)
		}
	}
	// nexttrace is governed but allows no IP version: the token is dropped.
	got, notes, err := normalizeOptions("nexttrace", optionSpecs["nexttrace"], "-4")
	if err != nil {
		t.Fatalf("normalizeOptions(nexttrace): %v", err)
	}
	if got != "" || len(notes) != 1 {
		t.Errorf("nexttrace -4 = (%q, %v), want dropped with one note", got, notes)
	}
}

// An IP-version row for every grammar-governed builder, asserted on full argv
// through the whole pipeline — the check the optional-flag templates alone would
// not make.
func TestIPVersionReachesArgvForEveryGrammarGovernedBuilder(t *testing.T) {
	cases := []struct {
		cmdType string
		target  string
		options string
		wantBin string
		want    []string
	}{
		{"ping", "host", "-6", "ping", []string{"-6", "-c", "10", "--", "host"}},
		{"traceroute", "host", "-6", "traceroute", []string{"-6", "--", "host"}},
		{"mtr", "host", "-6", "mtr", []string{"-6", "-r", "-w", "-c", "5", "--", "host"}},
		{"http", "host", "-6", "curl", []string{
			"-q", "-6", "--globoff", "-sS", "-o", "/dev/null", "-w", curlWriteFormatLiteral,
			"--max-time", "15", "-A", "parallax-probe/1.0",
			"--", "https://host",
		}},
		// The legacy bare type *and* the IP flag together — the exact shape a
		// pre-S6 saved preset replays as.
		{"dns", "example.com", "-4 MX", "dig", []string{"example.com", "MX", "-4"}},
		{"dns", "example.com", "-4 type=MX", "dig", []string{"example.com", "MX", "-4"}},
	}
	for _, c := range cases {
		bin, args, _ := buildVia(t, CommandRequest{Type: c.cmdType, Target: c.target, Options: c.options})
		if bin != c.wantBin {
			t.Errorf("%s binary = %q, want %q", c.cmdType, bin, c.wantBin)
		}
		if !slices.Equal(args, c.want) {
			t.Errorf("%s %q argv\n got: %#v\nwant: %#v", c.cmdType, c.options, args, c.want)
		}
	}
}

// ---- executeCommand: notes on the wire, and the field-length caps ----------

// The notes reach the browser as `output` lines, before the process starts, so
// they precede its output.
func TestExecuteCommandEmitsOptionNotesBeforeProcessOutput(t *testing.T) {
	// Swap ping's builder for a shell echo so the test needs no ping binary and
	// no network; normalization still runs, because optionSpecs["ping"] governs.
	old := commandBuilders["ping"]
	commandBuilders["ping"] = func(cmd CommandRequest) (string, []string, error) {
		return "sh", []string{"-c", "echo probe-output"}, nil
	}
	t.Cleanup(func() { commandBuilders["ping"] = old })

	fr := runExecuteCommand(t, CommandRequest{ID: "n1", Type: "ping", Target: "host", Options: "count=101 BOGUS"})
	assertExitOK(t, fr, true)

	var outputs []string
	for _, f := range fr.frames {
		if f.Type == "output" {
			outputs = append(outputs, f.Data)
		}
	}
	if len(outputs) < 3 {
		t.Fatalf("expected two notes and the process output, got %v", outputs)
	}
	if !strings.HasPrefix(outputs[0], "[options] ") || !strings.HasPrefix(outputs[1], "[options] ") {
		t.Errorf("the first output lines should be the notes, got %v", outputs)
	}
	if outputs[len(outputs)-1] != "probe-output" {
		t.Errorf("last output line = %q, want the process output", outputs[len(outputs)-1])
	}
}

// A rejected command reports the error and no notes: the error is the message.
func TestExecuteCommandRejectedOptionsEmitErrorAndNoNotes(t *testing.T) {
	fr := runExecuteCommand(t, CommandRequest{
		ID: "n2", Type: "ping", Target: "host",
		Options: strings.TrimSpace(strings.Repeat("count=5 ", 17)),
	})
	assertExitOK(t, fr, false)
	errFrame, ok := fr.first("error")
	if !ok {
		t.Fatalf("expected an error frame: %v", fr.types())
	}
	if !strings.Contains(errFrame.Data, "too many options") {
		t.Errorf("error frame = %q", errFrame.Data)
	}
	for _, f := range fr.frames {
		if f.Type == "output" && strings.HasPrefix(f.Data, "[options] ") {
			t.Errorf("a rejected command must emit no notes, got %q", f.Data)
		}
	}
}

// Field-length caps (F5). backend/client_ws.go already enforces exactly these
// three before dispatch; the agent repeats them so it does not depend on its
// server being the one it was built against.
func TestExecuteCommandRejectsOversizeFieldsAsDefenceInDepthForTheBackendCaps(t *testing.T) {
	cases := []struct {
		name string
		cmd  CommandRequest
	}{
		{"type over 32", CommandRequest{ID: "l1", Type: strings.Repeat("p", 33), Target: "host"}},
		{"target over 1024", CommandRequest{ID: "l2", Type: "ping", Target: strings.Repeat("h", 1025)}},
		{"options over 512", CommandRequest{ID: "l3", Type: "ping", Target: "host", Options: strings.Repeat("a", 513)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := runExecuteCommand(t, c.cmd)
			assertExitOK(t, fr, false)
			errFrame, ok := fr.first("error")
			if !ok {
				t.Fatalf("expected an error frame: %v", fr.types())
			}
			if !strings.Contains(errFrame.Data, "too long") {
				t.Errorf("error frame = %q, want a too-long rejection", errFrame.Data)
			}
		})
	}
	// The limits themselves are inclusive: exactly 512 option bytes is fine.
	// 512 bytes of grammar-valid tokens would exceed the 16-token cap, so use
	// one long (dropped) token — the point is the length gate, not the grammar.
	// ping's builder is swapped out so this needs neither the binary nor DNS.
	old := commandBuilders["ping"]
	commandBuilders["ping"] = func(cmd CommandRequest) (string, []string, error) {
		return "sh", []string{"-c", "true"}, nil
	}
	t.Cleanup(func() { commandBuilders["ping"] = old })
	fr := runExecuteCommand(t, CommandRequest{
		ID: "l4", Type: "ping", Target: "host", Options: strings.Repeat("a", 512),
	})
	if f, ok := fr.first("error"); ok && strings.Contains(f.Data, "too long") {
		t.Errorf("512 option bytes is at the limit and must not be refused: %q", f.Data)
	}
}
