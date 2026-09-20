package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// S10/F29 + F30. sanitizeString no longer HTML-escapes: escaping is a
// render-layer concern and happens where HTML is actually built. These tests
// pin the new contract (strip control characters, then rune-truncate) and the
// one-time, gated migration of the escaped `node_name` values already on disk.

// ---- sanitizeString ---------------------------------------------------------

func TestSanitizeStringDoesNotEscapeHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		// The headline case: a real provider name rendered literally.
		{"AT&T", "AT&T"},
		// It no longer escapes, and that is the point — React escapes this on
		// render, and the single HTML sink (GeoMap.escapeHtml) escapes its own.
		{"<script>", "<script>"},
		{"O'Brien", "O'Brien"},
		{`say "hi"`, `say "hi"`},
		{"a < b && b > c", "a < b && b > c"},
		// Already-escaped input is left exactly as it is: sanitizeString neither
		// escapes nor unescapes, so nothing compounds on repeated application.
		{"&amp;", "&amp;"},
		{"🇭🇰 Hong Kong", "🇭🇰 Hong Kong"},
	}
	for _, tc := range cases {
		if got := sanitizeString(tc.in, 64); got != tc.want {
			t.Errorf("sanitizeString(%q, 64) = %q, want %q", tc.in, got, tc.want)
		}
		// Idempotent: storing a stored value changes nothing.
		if got := sanitizeString(sanitizeString(tc.in, 64), 64); got != tc.want {
			t.Errorf("sanitizeString applied twice to %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeStringStripsControlCharacters(t *testing.T) {
	in := "ok\x00nul\x1besc\x7fdel\nnewline\ttab\rcr"
	want := "oknulescdelnewlinetabcr"
	got := sanitizeString(in, 128)
	if got != want {
		t.Fatalf("sanitizeString(%q) = %q, want %q", in, got, want)
	}
	if strings.ContainsAny(got, "\x00\x1b\x7f\n\t\r") {
		t.Fatalf("control characters survived: %q", got)
	}
}

// The strip-then-truncate order is observable, and it matters: sanitizeHealthInfo
// is the one caller whose own call sites do *not* pre-apply stripControlChars,
// so this function is those three fields' sole control-character defence. If it
// truncated first, control bytes would consume the rune budget and legitimate
// characters would be dropped in their place.
func TestSanitizeStringStripsBeforeTruncating(t *testing.T) {
	in := "\x00\x01\x02abcdef"
	if got, want := sanitizeString(in, 3), "abc"; got != want {
		t.Fatalf("sanitizeString(%q, 3) = %q, want %q (strip must happen before truncate)", in, got, want)
	}
	// Same property through the health path, which does not pre-strip.
	h := sanitizeHealthInfo(&HealthInfo{Uptime: "\x00\x00up 3 days", LoadAvg: "0.1\n0.2", OS: "linux\x1b[31m"})
	if h.Uptime != "up 3 days" {
		t.Errorf("health uptime = %q, want %q", h.Uptime, "up 3 days")
	}
	if h.LoadAvg != "0.10.2" {
		t.Errorf("health load = %q, want %q", h.LoadAvg, "0.10.2")
	}
	if h.OS != "linux[31m" {
		t.Errorf("health os = %q, want %q", h.OS, "linux[31m")
	}
}

func TestSanitizeStringTruncatesByRunes(t *testing.T) {
	// Multi-byte input: the cut is on a rune boundary and the result is still
	// valid UTF-8 — and there is no longer an HTML entity that a cut could
	// split in half, because none is produced.
	in := "héllo🌍wörld"
	got := sanitizeString(in, 6)
	if want := "héllo🌍"; got != want {
		t.Fatalf("sanitizeString(%q, 6) = %q, want %q", in, got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 6 {
		t.Fatalf("got %d runes, want 6", n)
	}
	if got := sanitizeString("short", 64); got != "short" {
		t.Fatalf("under-cap input was altered: %q", got)
	}
}

// Before this slice, 64 ampersands became 320 bytes of `&amp;` — and a cap
// expressed in runes stopped describing what was stored. Now the rune count is
// the byte count for ASCII, and nothing expands.
func TestSanitizeStringDoesNotExpandInput(t *testing.T) {
	in := strings.Repeat("&", 64)
	got := sanitizeString(in, 64)
	if got != in {
		t.Fatalf("sanitizeString expanded or altered the input: %d bytes, want %d", len(got), len(in))
	}
	if n := utf8.RuneCountInString(got); n != 64 {
		t.Fatalf("got %d runes, want 64", n)
	}
}

// ---- F30: the one-time node_name migration ---------------------------------

// sanitizeTestServer returns a bare Server wired to the given schedules file.
// Separate from newSchedulerTestServer because these tests need several
// successive servers over the *same* file, the way a restart does.
func sanitizeTestServer(t *testing.T, path string) *Server {
	t.Helper()
	t.Setenv("AGENT_API_KEY", "")
	t.Setenv("CLIENT_API_KEY", "")
	t.Setenv("ALLOWED_ORIGINS", "")
	t.Setenv("SCHEDULES_FILE", path)
	return NewServer()
}

func legacyScheduleEntry(id, nodeName string) map[string]any {
	return map[string]any{
		// No "schema_version": this is what a pre-S10 binary wrote.
		"id": id, "node_id": "node-legacy", "node_name": nodeName,
		"command": "ping", "target": "192.0.2.1", "interval_sec": 60,
		"enabled": true, "created_at": "2026-09-01T12:00:00Z",
		"last_status": "ok",
	}
}

func readSchedulesFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// parseAsTopLevelArray is the shape assertion the rollback story depends on:
// schedules.json must stay a top-level JSON array, so a reverted binary keeps
// unmarshalling it into []*Schedule instead of logging "starting empty" and
// rewriting the file.
func parseAsTopLevelArray(t *testing.T, data []byte) []scheduleView {
	t.Helper()
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '[' {
		t.Fatalf("schedules file is not a top-level array: %s", data)
	}
	var list []scheduleView
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("parse as []scheduleView: %v\n%s", err, data)
	}
	// An older binary reads the same bytes as []*Schedule and ignores the
	// unknown schema_version field.
	var legacy []*Schedule
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatalf("a pre-slice binary could not parse the file: %v\n%s", err, data)
	}
	if len(legacy) != len(list) {
		t.Fatalf("pre-slice parse produced %d schedules, want %d", len(legacy), len(list))
	}
	return list
}

func nodeNameOf(t *testing.T, srv *Server, id string) (string, int) {
	t.Helper()
	srv.schedulesMu.RLock()
	defer srv.schedulesMu.RUnlock()
	sc, ok := srv.schedules[id]
	if !ok {
		t.Fatalf("schedule %q missing after load", id)
	}
	return sc.NodeName, sc.SchemaVersion
}

func TestLoadSchedulesUnescapesNodeNameOnceAndStampsMarker(t *testing.T) {
	path := testSchedulesPath(t)
	writeSchedulesFile(t, path, []map[string]any{legacyScheduleEntry("sched-esc", "A&amp;T")})

	// Cycle 1: the migrating load. It unescapes, stamps, and writes the marker
	// synchronously — before the server would begin serving.
	srv1 := sanitizeTestServer(t, path)
	srv1.loadSchedules()
	if name, ver := nodeNameOf(t, srv1, "sched-esc"); name != "A&T" || ver != scheduleSchemaVersion {
		t.Fatalf("after migrating load: node_name = %q, schema_version = %d; want %q, %d",
			name, ver, "A&T", scheduleSchemaVersion)
	}
	first := readSchedulesFile(t, path)
	view := parseAsTopLevelArray(t, first)
	if len(view) != 1 || view[0].NodeName != "A&T" || view[0].SchemaVersion != scheduleSchemaVersion {
		t.Fatalf("migrating load did not persist the marker: %s", first)
	}

	// Cycles 2 and 3: a restart must not migrate again, and the file must be
	// byte-identical — a second unescape would silently corrupt a node
	// legitimately named "A&amp;T".
	for cycle := 2; cycle <= 3; cycle++ {
		srv := sanitizeTestServer(t, path)
		srv.loadSchedules()
		if name, ver := nodeNameOf(t, srv, "sched-esc"); name != "A&T" || ver != scheduleSchemaVersion {
			t.Fatalf("cycle %d: node_name = %q, schema_version = %d; want %q, %d",
				cycle, name, ver, "A&T", scheduleSchemaVersion)
		}
		srv.saveSchedules()
		got := readSchedulesFile(t, path)
		if !bytes.Equal(got, first) {
			t.Fatalf("cycle %d rewrote the file:\n--- want ---\n%s\n--- got ---\n%s", cycle, first, got)
		}
		parseAsTopLevelArray(t, got)
	}
}

func TestLoadSchedulesDoubleEscapedLosesExactlyOneEntityLevel(t *testing.T) {
	path := testSchedulesPath(t)
	writeSchedulesFile(t, path, []map[string]any{legacyScheduleEntry("sched-double", "A&amp;amp;T")})

	srv1 := sanitizeTestServer(t, path)
	srv1.loadSchedules()
	name, ver := nodeNameOf(t, srv1, "sched-double")
	if name != "A&amp;T" {
		t.Fatalf("first load: node_name = %q, want %q (exactly one entity level)", name, "A&amp;T")
	}
	if ver != scheduleSchemaVersion {
		t.Fatalf("first load: schema_version = %d, want %d", ver, scheduleSchemaVersion)
	}

	// And never again, on any later restart.
	for cycle := 2; cycle <= 3; cycle++ {
		srv := sanitizeTestServer(t, path)
		srv.loadSchedules()
		if name, _ := nodeNameOf(t, srv, "sched-double"); name != "A&amp;T" {
			t.Fatalf("cycle %d: node_name = %q, want %q — the migration is not one-time", cycle, name, "A&amp;T")
		}
		srv.saveSchedules()
	}
}

// Unescaping can *synthesise* control characters, and metricsLabelValue escapes
// backslash, quote and newline — but not a carriage return. A hand-edited
// "&#13;" must therefore not reach storage, or it would split one /metrics
// exposition line in two.
func TestMigrationStripsSynthesizedControlCharacters(t *testing.T) {
	path := testSchedulesPath(t)
	writeSchedulesFile(t, path, []map[string]any{
		legacyScheduleEntry("sched-cr", "Edge&#13;lookingglass_probe_status{node=\"pwned\"} 0"),
		legacyScheduleEntry("sched-nl", "Line&#10;break"),
	})

	srv := sanitizeTestServer(t, path)
	srv.loadSchedules()

	for _, id := range []string{"sched-cr", "sched-nl"} {
		name, ver := nodeNameOf(t, srv, id)
		if strings.ContainsAny(name, "\r\n\x00\x1b\x7f") {
			t.Fatalf("%s: stored node_name carries a control character: %q", id, name)
		}
		if ver != scheduleSchemaVersion {
			t.Fatalf("%s: schema_version = %d, want %d", id, ver, scheduleSchemaVersion)
		}
	}
	// The persisted value carries none either.
	for _, v := range parseAsTopLevelArray(t, readSchedulesFile(t, path)) {
		if strings.ContainsAny(v.NodeName, "\r\n") {
			t.Fatalf("persisted node_name carries a line break: %q", v.NodeName)
		}
	}

	// Every exposition line is well formed: three metric lines' worth of
	// grammar, no bare CR, no injected series.
	rec := httptest.NewRecorder()
	srv.writeScheduleMetrics(rec)
	body := rec.Body.String()
	if strings.Contains(body, "\r") {
		t.Fatalf("/metrics output contains a carriage return:\n%q", body)
	}
	series := 0
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series++
		if !strings.HasPrefix(line, "lookingglass_probe_") {
			t.Fatalf("malformed exposition line (no metric name): %q", line)
		}
		open := strings.Index(line, "{")
		closeAt := strings.LastIndex(line, "} ")
		if open < 0 || closeAt < open {
			t.Fatalf("malformed exposition line (labels): %q", line)
		}
		if _, err := fmt.Sscanf(line[closeAt+2:], "%f", new(float64)); err != nil {
			t.Fatalf("malformed exposition line (value %q): %q", line[closeAt+2:], line)
		}
	}
	// Two schedules, status only (no history → no rtt/loss gauges).
	if series != 2 {
		t.Fatalf("emitted %d series, want 2 — a label value broke a line", series)
	}
}

// The marker is written even when no NodeName actually changed. Without that,
// nothing would distinguish "already migrated" from "legacy" on the next start.
func TestMigrationStampsEvenWhenNothingChanged(t *testing.T) {
	path := testSchedulesPath(t)
	writeSchedulesFile(t, path, []map[string]any{legacyScheduleEntry("sched-plain", "Plain Name")})

	srv := sanitizeTestServer(t, path)
	srv.loadSchedules()
	if name, ver := nodeNameOf(t, srv, "sched-plain"); name != "Plain Name" || ver != scheduleSchemaVersion {
		t.Fatalf("node_name = %q, schema_version = %d; want %q, %d", name, ver, "Plain Name", scheduleSchemaVersion)
	}
	view := parseAsTopLevelArray(t, readSchedulesFile(t, path))
	if len(view) != 1 || view[0].SchemaVersion != scheduleSchemaVersion {
		t.Fatalf("marker was not written for an unchanged node_name: %+v", view)
	}
}

// Unescaping can also *lengthen* nothing but shorten a lot; what it must not do
// is leave a value longer than the 64-rune cap the registration path applies.
func TestMigrationReTruncatesToNodeNameCap(t *testing.T) {
	path := testSchedulesPath(t)
	long := strings.Repeat("&amp;", 40) // 200 runes escaped, 40 after unescaping
	writeSchedulesFile(t, path, []map[string]any{
		legacyScheduleEntry("sched-long", long),
		legacyScheduleEntry("sched-verylong", strings.Repeat("ü", 200)),
	})

	srv := sanitizeTestServer(t, path)
	srv.loadSchedules()
	if name, _ := nodeNameOf(t, srv, "sched-long"); name != strings.Repeat("&", 40) {
		t.Fatalf("node_name = %q, want 40 ampersands", name)
	}
	name, _ := nodeNameOf(t, srv, "sched-verylong")
	if n := utf8.RuneCountInString(name); n != scheduleNodeNameMax {
		t.Fatalf("node_name is %d runes, want %d", n, scheduleNodeNameMax)
	}
	if !utf8.ValidString(name) {
		t.Fatalf("truncation produced invalid UTF-8: %q", name)
	}
}

// A schedule created after the migration must be stamped at construction, or it
// would be re-classified as legacy on the next restart and have its already-raw
// node_name unescaped once more — the compounding defect, reintroduced for
// exactly the newest data.
func TestScheduleCreateStampsSchemaVersion(t *testing.T) {
	srv, ts, path := newSchedulerTestServer(t)
	agent := dialFakeAgent(t, ts, "Kit&Caboodle")
	defer agent.close()
	nodeID := waitForNodeID(t, srv, "Kit&Caboodle")

	body := fmt.Sprintf(`{"node_id":%q,"command":"ping","target":"192.0.2.1","interval_sec":60}`, nodeID)
	resp, err := ts.Client().Post(ts.URL+"/api/schedules", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/schedules: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created scheduleView
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.SchemaVersion != scheduleSchemaVersion {
		t.Fatalf("create response schema_version = %d, want %d", created.SchemaVersion, scheduleSchemaVersion)
	}
	// The node name is stored — and now returned — unescaped.
	if created.NodeName != "Kit&Caboodle" {
		t.Fatalf("create response node_name = %q, want %q", created.NodeName, "Kit&Caboodle")
	}

	persisted := parseAsTopLevelArray(t, readSchedulesFile(t, path))
	if len(persisted) != 1 || persisted[0].SchemaVersion != scheduleSchemaVersion {
		t.Fatalf("persisted schedule is unstamped: %+v", persisted)
	}

	// Reloading it changes nothing: no migration, no unescape.
	srv2 := sanitizeTestServer(t, path)
	srv2.loadSchedules()
	if name, ver := nodeNameOf(t, srv2, created.ID); name != "Kit&Caboodle" || ver != scheduleSchemaVersion {
		t.Fatalf("after restart: node_name = %q, schema_version = %d; want %q, %d",
			name, ver, "Kit&Caboodle", scheduleSchemaVersion)
	}
}

// A file that is already migrated must not be rewritten at all by a load: the
// marker write is the migration's own, not a per-start rewrite.
func TestLoadOfMigratedFileDoesNotRewriteIt(t *testing.T) {
	path := testSchedulesPath(t)
	entry := legacyScheduleEntry("sched-stamped", "Already Raw & Fine")
	entry["schema_version"] = scheduleSchemaVersion
	writeSchedulesFile(t, path, []map[string]any{entry})
	before := readSchedulesFile(t, path)

	srv := sanitizeTestServer(t, path)
	srv.loadSchedules()
	if name, ver := nodeNameOf(t, srv, "sched-stamped"); name != "Already Raw & Fine" || ver != scheduleSchemaVersion {
		t.Fatalf("node_name = %q, schema_version = %d", name, ver)
	}
	if got := readSchedulesFile(t, path); !bytes.Equal(got, before) {
		t.Fatalf("an already-migrated file was rewritten on load:\n--- before ---\n%s\n--- after ---\n%s", before, got)
	}
}

// Sanity on the HTTP surface: an agent-supplied provider name reaches the
// browser literally, not as `AT&amp;T`. json.Encoder escapes nothing here
// beyond <, > and & in \u form, so decode and compare the value, not the bytes.
func TestNodeAPIReturnsUnescapedAgentStrings(t *testing.T) {
	// The same calls agent_ws.go's registration path makes, with the caps it
	// uses. (Its own struct is anonymous, so the values are inlined here.)
	got := struct{ name, location, flag, provider string }{
		sanitizeString(stripControlChars("A&B Networks"), 64),
		sanitizeString(stripControlChars("Hong Kong <HK>"), 128),
		sanitizeString(stripControlChars("<b>\U0001F1ED\U0001F1F0</b>"), 32),
		sanitizeString(stripControlChars("AT&T"), 128),
	}
	want := struct{ name, location, flag, provider string }{
		"A&B Networks", "Hong Kong <HK>", "<b>\U0001F1ED\U0001F1F0</b>", "AT&T",
	}
	if got != want {
		t.Fatalf("registration sanitization = %+v, want %+v", got, want)
	}
	// And the JSON body a browser receives decodes back to the same value.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]string{"provider": got.provider}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back["provider"] != "AT&T" {
		t.Fatalf("round-tripped provider = %q, want %q", back["provider"], "AT&T")
	}
}
