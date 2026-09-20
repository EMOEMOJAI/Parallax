package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- fixture helpers --------------------------------------------------------

func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

// throughTail pushes every line through the real bounded tail buffer, so the
// parser tests see exactly what the agent would hand them at process exit.
func throughTail(lines []string) []string {
	tb := newTailBuffer()
	for _, l := range lines {
		tb.add(l)
	}
	return tb.tail()
}

func parseFixture(t *testing.T, cmdType, fixture string) map[string]any {
	t.Helper()
	blob := summaryJSON(cmdType, throughTail(fixtureLines(t, fixture)))
	if blob == nil {
		t.Fatalf("%s fixture %s produced no summary", cmdType, fixture)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("summary is not valid JSON: %v", err)
	}
	return out
}

func wantNum(t *testing.T, m map[string]any, key string, want float64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("summary is missing %q (have %v)", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%q = %#v, want a number", key, v)
	}
	if f != want {
		t.Errorf("%q = %v, want %v", key, f, want)
	}
}

func wantStr(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("summary is missing %q (have %v)", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%q = %#v, want a string", key, v)
	}
	if s != want {
		t.Errorf("%q = %q, want %q", key, s, want)
	}
}

// ---- tail buffer ------------------------------------------------------------

func TestTailBufferKeepsOnlyTheLastLines(t *testing.T) {
	tb := newTailBuffer()
	for i := 0; i < summaryTailMaxLines*4; i++ {
		tb.add(fmt.Sprintf("line-%d", i))
	}
	got := tb.tail()
	if len(got) != summaryTailMaxLines {
		t.Fatalf("kept %d lines, want %d", len(got), summaryTailMaxLines)
	}
	if want := fmt.Sprintf("line-%d", summaryTailMaxLines*4-1); got[len(got)-1] != want {
		t.Errorf("last line = %q, want %q", got[len(got)-1], want)
	}
}

func TestTailBufferRespectsByteCap(t *testing.T) {
	tb := newTailBuffer()
	big := strings.Repeat("x", 8*1024)
	for i := 0; i < 64; i++ { // 512 KB pushed through a 64 KB window
		tb.add(big)
	}
	if tb.bytes > summaryTailMaxBytes {
		t.Fatalf("buffer holds %d bytes, cap is %d", tb.bytes, summaryTailMaxBytes)
	}
	if len(tb.tail()) == 0 {
		t.Fatal("byte cap evicted everything")
	}
}

func TestTailBufferTruncatesAnOversizeLine(t *testing.T) {
	tb := newTailBuffer()
	tb.add(strings.Repeat("y", summaryTailMaxBytes*2))
	if tb.bytes > summaryTailMaxBytes {
		t.Fatalf("buffer holds %d bytes, cap is %d", tb.bytes, summaryTailMaxBytes)
	}
}

// ---- ping -------------------------------------------------------------------

func TestParsePingLinux(t *testing.T) {
	m := parseFixture(t, "ping", "ping_linux.txt")
	wantNum(t, m, "sent", 5)
	wantNum(t, m, "received", 5)
	wantNum(t, m, "loss_pct", 0)
	wantNum(t, m, "min_ms", 11.9)
	wantNum(t, m, "avg_ms", 12.5)
	wantNum(t, m, "max_ms", 13.2)
	wantNum(t, m, "mdev_ms", 0.462)
}

func TestParsePingLinuxWithLoss(t *testing.T) {
	m := parseFixture(t, "ping", "ping_linux_loss.txt")
	wantNum(t, m, "sent", 10)
	wantNum(t, m, "received", 6)
	wantNum(t, m, "loss_pct", 40)
	wantNum(t, m, "avg_ms", 24.8)
}

func TestParsePingMacOS(t *testing.T) {
	m := parseFixture(t, "ping", "ping_macos.txt")
	wantNum(t, m, "sent", 5)
	wantNum(t, m, "received", 5)
	wantNum(t, m, "loss_pct", 0)
	wantNum(t, m, "min_ms", 11.914)
	wantNum(t, m, "avg_ms", 12.506)
	wantNum(t, m, "max_ms", 13.204)
	wantNum(t, m, "mdev_ms", 0.462)
}

// A 100 %-loss run has no round-trip line at all: the stats are still a
// complete summary, so it must parse (and it must not invent rtt fields).
func TestParsePingMacOSTotalLoss(t *testing.T) {
	m := parseFixture(t, "ping", "ping_macos_timeout.txt")
	wantNum(t, m, "sent", 5)
	wantNum(t, m, "received", 0)
	wantNum(t, m, "loss_pct", 100)
	if _, ok := m["avg_ms"]; ok {
		t.Errorf("avg_ms present for a 100%% loss run: %v", m)
	}
}

func TestParsePingGarbageProducesNoSummary(t *testing.T) {
	if blob := summaryJSON("ping", []string{"ping: cannot resolve nope.invalid: Unknown host"}); blob != nil {
		t.Fatalf("expected no summary, got %s", blob)
	}
}

// ---- http -------------------------------------------------------------------

// curlWriteFormat extracts the literal -w format string out of buildHTTP, so
// the parser is exercised against the builder's own format and the two cannot
// drift apart silently.
func curlWriteFormat(t *testing.T) string {
	t.Helper()
	_, args, err := buildHTTP(CommandRequest{Target: "example.com"})
	if err != nil {
		t.Fatalf("buildHTTP: %v", err)
	}
	for i, a := range args {
		if a == "-w" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatal("buildHTTP no longer passes a -w format string")
	return ""
}

func TestParseHTTPUsesBuildHTTPFormat(t *testing.T) {
	format := curlWriteFormat(t)
	line := format
	for placeholder, value := range map[string]string{
		"%{http_code}":          "200",
		"%{time_namelookup}":    "0.004231",
		"%{time_connect}":       "0.010972",
		"%{time_appconnect}":    "0.031544",
		"%{time_starttransfer}": "0.062118",
		"%{time_total}":         "0.062512",
		"%{size_download}":      "1256",
	} {
		line = strings.ReplaceAll(line, placeholder, value)
	}
	if strings.Contains(line, "%{") {
		t.Fatalf("buildHTTP's -w format has an unknown placeholder the parser was never taught: %q", line)
	}
	line = strings.TrimSuffix(line, "\n")

	blob := summaryJSON("http", throughTail([]string{line}))
	if blob == nil {
		t.Fatalf("rendered curl -w line did not parse: %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("summary is not valid JSON: %v", err)
	}
	wantNum(t, m, "http_code", 200)
	wantNum(t, m, "dns_ms", 4.231)
	wantNum(t, m, "connect_ms", 10.972)
	wantNum(t, m, "tls_ms", 31.544)
	wantNum(t, m, "ttfb_ms", 62.118)
	wantNum(t, m, "total_ms", 62.512)
	wantNum(t, m, "size_bytes", 1256)
}

func TestParseHTTPFixture(t *testing.T) {
	m := parseFixture(t, "http", "curl_w.txt")
	wantNum(t, m, "http_code", 200)
	wantNum(t, m, "total_ms", 62.512)
	wantNum(t, m, "size_bytes", 1256)
}

func TestParseHTTPPartialLineProducesNoSummary(t *testing.T) {
	if blob := summaryJSON("http", []string{"code=200  total=0.5s"}); blob != nil {
		t.Fatalf("expected no summary for a partial -w line, got %s", blob)
	}
}

// ---- dns --------------------------------------------------------------------

func TestParseDNSDig(t *testing.T) {
	m := parseFixture(t, "dns", "dig_a.txt")
	wantStr(t, m, "status", "NOERROR")
	wantNum(t, m, "answer_count", 2)
	wantNum(t, m, "query_time_ms", 23)
	if _, ok := m["loss_pct"]; ok {
		t.Errorf("dns summary must not carry loss_pct: %v", m)
	}
}

func TestParseDNSNXDomain(t *testing.T) {
	m := parseFixture(t, "dns", "dig_nxdomain.txt")
	wantStr(t, m, "status", "NXDOMAIN")
	wantNum(t, m, "answer_count", 0)
	wantNum(t, m, "query_time_ms", 41)
}

func TestParseDNSTruncatedProducesNoSummary(t *testing.T) {
	lines := []string{";; ->>HEADER<<- opcode: QUERY, status: NOERROR, id: 4242"}
	if blob := summaryJSON("dns", lines); blob != nil {
		t.Fatalf("expected no summary for partial dig output, got %s", blob)
	}
}

// ---- mtr --------------------------------------------------------------------

func TestParseMtrReport(t *testing.T) {
	m := parseFixture(t, "mtr", "mtr_r.txt")
	wantNum(t, m, "hop_count", 4)
	wantNum(t, m, "loss_pct", 0) // last hop is the target
	hops, ok := m["hops"].([]any)
	if !ok || len(hops) != 4 {
		t.Fatalf("hops = %#v, want 4 entries", m["hops"])
	}
	third, ok := hops[2].(map[string]any)
	if !ok {
		t.Fatalf("hop entry is not an object: %#v", hops[2])
	}
	if third["host"] != "???" {
		t.Errorf("hop 3 host = %v, want ???", third["host"])
	}
	if third["loss_pct"] != 100.0 {
		t.Errorf("hop 3 loss_pct = %v, want 100", third["loss_pct"])
	}
	last, _ := hops[3].(map[string]any)
	if len(last) > 8 {
		t.Errorf("hop object has %d keys, the server grammar allows 8", len(last))
	}
}

// The tail buffer must be big enough that the *last* hop of a full-length mtr
// survives even when the run also printed a few hundred noisy lines first —
// that hop is what supplies the top-level loss_pct.
func TestParseMtr64HopsSurvivesTailBuffer(t *testing.T) {
	noise := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		noise = append(noise, fmt.Sprintf("noise line %d", i))
	}
	lines := append(noise, fixtureLines(t, "mtr_64hop.txt")...)

	blob := summaryJSON("mtr", throughTail(lines))
	if blob == nil {
		t.Fatal("64-hop mtr produced no summary")
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("summary is not valid JSON: %v", err)
	}
	wantNum(t, m, "hop_count", 64)
	wantNum(t, m, "loss_pct", 40) // loss of hop 64, the target
	hops := m["hops"].([]any)
	last := hops[63].(map[string]any)
	if got := last["hop"]; got != float64(64) {
		t.Errorf("last hop index = %v, want 64", got)
	}
	// Still small enough for the server's 16 KB pre-parse cap.
	if len(blob) > 16384 {
		t.Errorf("64-hop summary is %d bytes, server drops anything over 16384", len(blob))
	}
	// …but larger than what a schedule will persist (4 KB), which is the
	// behaviour backend/summary_test.go asserts on.
	if len(blob) <= 4096 {
		t.Errorf("64-hop summary is only %d bytes; the persistence test needs > 4096", len(blob))
	}
}

func TestParseMtrHeaderOnlyProducesNoSummary(t *testing.T) {
	lines := []string{"Start: 2026-09-19T12:00:00+0000", "mtr: unable to resolve nope.invalid"}
	if blob := summaryJSON("mtr", lines); blob != nil {
		t.Fatalf("expected no summary, got %s", blob)
	}
}

func TestNoParserForTracerouteOrIperf(t *testing.T) {
	for _, typ := range []string{"traceroute", "nexttrace", "iperf3", "speedtest", "shell"} {
		if hasSummaryParser(typ) {
			t.Errorf("%s unexpectedly has a summary parser", typ)
		}
		if blob := summaryJSON(typ, []string{"anything"}); blob != nil {
			t.Errorf("%s produced a summary: %s", typ, blob)
		}
	}
}

// ---- done payload -----------------------------------------------------------

func TestDoneDataShape(t *testing.T) {
	if got := doneData(true); got != `{"exit_ok":true}` {
		t.Errorf("doneData(true) = %s", got)
	}
	if got := doneData(false); got != `{"exit_ok":false}` {
		t.Errorf("doneData(false) = %s", got)
	}
}

// ---- executeCommand end to end ---------------------------------------------
//
// These drive the real executeCommand over a real WebSocket and collect every
// frame it emits, so the ordering guarantee (summary strictly before done) and
// the exit_ok enumeration are asserted on the wire, not on internals.

type agentFrames struct {
	frames []CommandResponse
}

func (a *agentFrames) types() []string {
	out := make([]string, 0, len(a.frames))
	for _, f := range a.frames {
		out = append(out, f.Type)
	}
	return out
}

func (a *agentFrames) first(typ string) (CommandResponse, bool) {
	for _, f := range a.frames {
		if f.Type == typ {
			return f, true
		}
	}
	return CommandResponse{}, false
}

// runExecuteCommand connects executeCommand to a throwaway WebSocket server
// and returns every frame the agent wrote before `done`.
func runExecuteCommand(t *testing.T, cmd CommandRequest) *agentFrames {
	t.Helper()
	collected := make(chan []CommandResponse, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var frames []CommandResponse
		for {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
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
			frames = append(frames, resp)
			if resp.Type == "done" {
				break
			}
		}
		collected <- frames
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	connClosed.Store(false)

	executeCommand(conn, cmd)

	select {
	case frames := <-collected:
		return &agentFrames{frames: frames}
	case <-time.After(60 * time.Second):
		t.Fatal("collector never saw a done frame")
		return nil
	}
}

// withTestCommand registers a throwaway command type backed by /bin/sh for the
// duration of one test. It lets the failure-class cases (non-zero exit, output
// cap) run without depending on ping/dig/mtr/curl being installed.
func withTestCommand(t *testing.T, name string, build commandBuilder) {
	t.Helper()
	allowedCommands[name] = true
	commandBuilders[name] = build
	t.Cleanup(func() {
		delete(allowedCommands, name)
		delete(commandBuilders, name)
	})
}

func assertNoSummary(t *testing.T, fr *agentFrames) {
	t.Helper()
	if _, ok := fr.first("summary"); ok {
		t.Errorf("a summary was emitted for a failed run: %v", fr.types())
	}
}

func assertExitOK(t *testing.T, fr *agentFrames, want bool) {
	t.Helper()
	done, ok := fr.first("done")
	if !ok {
		t.Fatalf("no done frame: %v", fr.types())
	}
	var payload struct {
		ExitOK bool `json:"exit_ok"`
	}
	if err := json.Unmarshal([]byte(done.Data), &payload); err != nil {
		t.Fatalf("done payload %q is not JSON: %v", done.Data, err)
	}
	if payload.ExitOK != want {
		t.Errorf("exit_ok = %v, want %v (frames %v)", payload.ExitOK, want, fr.types())
	}
}

func TestExecuteCommandUnknownTypeIsNotOK(t *testing.T) {
	fr := runExecuteCommand(t, CommandRequest{ID: "c1", Type: "definitely-not-a-probe", Target: "example.com"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
}

func TestExecuteCommandInvalidTargetIsNotOK(t *testing.T) {
	fr := runExecuteCommand(t, CommandRequest{ID: "c2", Type: "ping", Target: "example.com\nrm -rf /"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
}

func TestExecuteCommandInvalidOptionsIsNotOK(t *testing.T) {
	fr := runExecuteCommand(t, CommandRequest{ID: "c3", Type: "ping", Target: "example.com", Options: "count=5\n-4"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
}

func TestExecuteCommandEmptyTargetIsNotOK(t *testing.T) {
	fr := runExecuteCommand(t, CommandRequest{ID: "c4", Type: "ping", Target: "   "})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
}

func TestExecuteCommandBuildErrorIsNotOK(t *testing.T) {
	// buildPing refuses a target that looks like a flag.
	fr := runExecuteCommand(t, CommandRequest{ID: "c5", Type: "ping", Target: "-c 100000"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
}

func TestExecuteCommandNonZeroExitIsNotOK(t *testing.T) {
	withTestCommand(t, "testfail", func(cmd CommandRequest) (string, []string, error) {
		return "sh", []string{"-c", "echo hello; exit 3"}, nil
	})
	fr := runExecuteCommand(t, CommandRequest{ID: "c6", Type: "testfail", Target: "x"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
	if _, ok := fr.first("error"); !ok {
		t.Errorf("expected an error frame for a non-zero exit: %v", fr.types())
	}
}

func TestExecuteCommandOutputCapAbortIsNotOK(t *testing.T) {
	// Shrink the cap instead of streaming 10 MB through the socket; the abort
	// path under test is the same one.
	old := maxOutputBytes
	maxOutputBytes = 4096
	t.Cleanup(func() { maxOutputBytes = old })

	withTestCommand(t, "testflood", func(cmd CommandRequest) (string, []string, error) {
		return "sh", []string{"-c", `i=0; while [ $i -lt 400 ]; do echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; i=$((i+1)); done`}, nil
	})
	fr := runExecuteCommand(t, CommandRequest{ID: "c7", Type: "testflood", Target: "x"})
	assertExitOK(t, fr, false)
	assertNoSummary(t, fr)
	found := false
	for _, f := range fr.frames {
		if f.Type == "error" && strings.Contains(f.Data, "Output size limit exceeded") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the output-cap error frame: %v", fr.types())
	}
}

// A clean run of a parsable probe emits exactly one summary, immediately
// before done, with exit_ok true. The fixture is replayed through `cat` so the
// test does not need ping installed.
func TestExecuteCommandEmitsSummaryBeforeDone(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fixture := filepath.Join(wd, "testdata", "ping_linux.txt")
	oldBuilder := commandBuilders["ping"]
	commandBuilders["ping"] = func(cmd CommandRequest) (string, []string, error) {
		return "cat", []string{"--", fixture}, nil
	}
	t.Cleanup(func() { commandBuilders["ping"] = oldBuilder })

	fr := runExecuteCommand(t, CommandRequest{ID: "c8", Type: "ping", Target: "1.1.1.1"})
	assertExitOK(t, fr, true)

	types := fr.types()
	summaryIdx, doneIdx := -1, -1
	summaries := 0
	for i, typ := range types {
		switch typ {
		case "summary":
			summaries++
			if summaryIdx < 0 {
				summaryIdx = i
			}
		case "done":
			doneIdx = i
		}
	}
	if summaries != 1 {
		t.Fatalf("got %d summary frames, want exactly 1: %v", summaries, types)
	}
	if summaryIdx > doneIdx {
		t.Fatalf("summary came after done: %v", types)
	}
	if doneIdx != summaryIdx+1 {
		t.Errorf("summary is not immediately before done: %v", types)
	}
	summary, _ := fr.first("summary")
	var m map[string]any
	if err := json.Unmarshal([]byte(summary.Data), &m); err != nil {
		t.Fatalf("summary data %q is not JSON: %v", summary.Data, err)
	}
	wantNum(t, m, "sent", 5)
	wantNum(t, m, "loss_pct", 0)
	wantNum(t, m, "avg_ms", 12.5)
}

// A command type with no parser never produces a summary even on a clean exit.
func TestExecuteCommandCleanExitWithoutParserHasNoSummary(t *testing.T) {
	withTestCommand(t, "testok", func(cmd CommandRequest) (string, []string, error) {
		return "sh", []string{"-c", "echo fine"}, nil
	})
	fr := runExecuteCommand(t, CommandRequest{ID: "c9", Type: "testok", Target: "x"})
	assertExitOK(t, fr, true)
	assertNoSummary(t, fr)
}
