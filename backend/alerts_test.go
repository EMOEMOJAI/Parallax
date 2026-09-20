package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// S3 — webhook alerts (F24–F26). The webhook URL is treated as a credential
// throughout: these tests pin the "never logged, never persisted, never
// returned" invariant as hard as the transition logic.

// alertSink is a webhook endpoint that records what it receives.
type alertSink struct {
	ts   *httptest.Server
	mu   sync.Mutex
	got  [][]byte
	code int
	hang time.Duration
}

func newAlertSink(t *testing.T) *alertSink {
	t.Helper()
	sink := &alertSink{code: 200}
	sink.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		sink.mu.Lock()
		hang, code := sink.hang, sink.code
		sink.got = append(sink.got, body)
		sink.mu.Unlock()
		if hang > 0 {
			time.Sleep(hang)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(sink.ts.Close)
	return sink
}

func (s *alertSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *alertSink) bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.got...)
}

// setAlertTimings shortens the cooldown / request timeout for a test.
func setAlertTimings(t *testing.T, cooldown, timeout time.Duration) {
	t.Helper()
	oldCooldown, oldTimeout := alertCooldown, alertTimeout
	alertCooldown, alertTimeout = cooldown, timeout
	t.Cleanup(func() { alertCooldown, alertTimeout = oldCooldown, oldTimeout })
}

// alertServer builds a server whose webhook points at url and starts the
// single dispatcher goroutine, stopping it when the test ends.
func alertServer(t *testing.T, url string) (*Server, *httptest.Server, string) {
	t.Helper()
	srv, ts, path := histServer(t, map[string]string{"ALERT_WEBHOOK_URL": url})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.alerts.run(done)
	}()
	t.Cleanup(func() {
		close(done)
		wg.Wait()
	})
	return srv, ts, path
}

// waitForAlerts waits until the sink has seen n POSTs.
func waitForAlerts(t *testing.T, sink *alertSink, n int) {
	t.Helper()
	waitFor(t, "webhook deliveries", 5*time.Second, func() bool { return sink.count() >= n })
}

// ---- transition logic -------------------------------------------------------

func TestAlertFiresOnlyAfterTwoConsecutiveRunsAtTheNewStatus(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	sink := newAlertSink(t)
	srv, _, _ := alertServer(t, sink.ts.URL)
	sc := addSchedule(srv, "node-a", "ping", "1.1.1.1")

	// First run after process start: always silent, whatever the status.
	srv.finalizeSchedule(sc, "ok", "run 1")
	// A single bad run is a blip.
	srv.finalizeSchedule(sc, "degraded", "run 2")
	time.Sleep(100 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("%d alerts fired before the transition was confirmed", n)
	}

	// Confirmed by the second consecutive degraded run.
	srv.finalizeSchedule(sc, "degraded", "run 3")
	waitForAlerts(t, sink, 1)

	var ev alertEvent
	if err := json.Unmarshal(sink.bodies()[0], &ev); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	// from is the status the operator was last told about, not the previous
	// (unconfirmed) run's.
	if ev.From != "ok" || ev.To != "degraded" {
		t.Errorf("payload from/to = %q/%q, want ok/degraded", ev.From, ev.To)
	}
	if ev.ScheduleID != sc.ID || ev.Command != "ping" || ev.Target != "1.1.1.1" || ev.Node != "Testville" {
		t.Errorf("payload identity fields = %+v", ev)
	}
	if ev.At.IsZero() {
		t.Error("payload has no timestamp")
	}

	// Staying degraded does not re-alert.
	srv.finalizeSchedule(sc, "degraded", "run 4")
	time.Sleep(100 * time.Millisecond)
	if n := sink.count(); n != 1 {
		t.Errorf("a repeated status re-alerted (%d deliveries)", n)
	}
}

func TestAlertFlappingInsideTheCooldownIsSuppressed(t *testing.T) {
	setAlertTimings(t, 10*time.Second, 5*time.Second)
	sink := newAlertSink(t)
	srv, _, _ := alertServer(t, sink.ts.URL)
	sc := addSchedule(srv, "node-b", "ping", "1.1.1.1")

	srv.finalizeSchedule(sc, "ok", "baseline") // suppressed: first run
	srv.finalizeSchedule(sc, "error", "1")
	srv.finalizeSchedule(sc, "error", "2") // confirmed → fires
	waitForAlerts(t, sink, 1)

	// Flap back inside the cooldown: confirmed by two runs, but too soon.
	srv.finalizeSchedule(sc, "ok", "3")
	srv.finalizeSchedule(sc, "ok", "4")
	time.Sleep(150 * time.Millisecond)
	if n := sink.count(); n != 1 {
		t.Fatalf("flapping inside the cooldown fired %d alerts, want 1", n)
	}
}

func TestAlertFirstRunAfterStartIsSuppressedForEverySchedule(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	sink := newAlertSink(t)
	srv, _, _ := alertServer(t, sink.ts.URL)

	for _, status := range []string{"ok", "degraded", "error", "agent_offline"} {
		sc := addSchedule(srv, "node-c", "ping", "1.1.1.1")
		srv.finalizeSchedule(sc, status, "first run after start")
	}
	time.Sleep(150 * time.Millisecond)
	if n := sink.count(); n != 0 {
		t.Fatalf("%d alerts fired for first runs after start", n)
	}
}

func TestAlertPayloadCarriesTheCanonicalSummary(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	sink := newAlertSink(t)
	srv, _, _ := alertServer(t, sink.ts.URL)
	sc := addSchedule(srv, "node-p", "ping", "1.1.1.1")
	summary := histSummary(t, map[string]any{"loss_pct": 40, "avg_ms": 31.5, "sent": 10, "received": 6})

	srv.finalizeScheduleWithSummary(sc, "ok", "baseline", nil)
	srv.finalizeScheduleWithSummary(sc, "degraded", "1", summary)
	srv.finalizeScheduleWithSummary(sc, "degraded", "2", summary)
	waitForAlerts(t, sink, 1)

	var ev alertEvent
	if err := json.Unmarshal(sink.bodies()[0], &ev); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Summary, &got); err != nil {
		t.Fatalf("payload summary is not JSON: %v", err)
	}
	if got["loss_pct"] != 40.0 || got["avg_ms"] != 31.5 {
		t.Errorf("payload summary = %v", got)
	}
	// The wire contract, key by key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(sink.bodies()[0], &raw); err != nil {
		t.Fatalf("payload is not an object: %v", err)
	}
	for _, key := range []string{"schedule_id", "node", "command", "target", "from", "to", "summary", "at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("payload is missing %q: %s", key, sink.bodies()[0])
		}
	}
}

// ---- webhook client ---------------------------------------------------------

func TestAlertWebhookDoesNotFollowRedirects(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	final := newAlertSink(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.ts.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	srv, _, _ := alertServer(t, redirector.URL)
	srv.alerts.post(alertEvent{ScheduleID: "s1", To: "error", At: time.Now()})

	if n := final.count(); n != 0 {
		t.Fatalf("the webhook followed a 302 to another host (%d deliveries)", n)
	}
	if got := srv.alerts.sent.Load(); got != 0 {
		t.Errorf("a redirect was counted as a successful delivery (%d)", got)
	}
}

func TestAlertWebhookTimesOut(t *testing.T) {
	setAlertTimings(t, time.Hour, 200*time.Millisecond)
	sink := newAlertSink(t)
	sink.hang = 3 * time.Second
	srv, _, _ := alertServer(t, sink.ts.URL)

	start := time.Now()
	srv.alerts.post(alertEvent{ScheduleID: "s1", To: "error", At: time.Now()})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("post blocked for %s — the client timeout is not enforced", elapsed)
	}
	if got := srv.alerts.client.Timeout; got != 200*time.Millisecond {
		t.Errorf("client timeout = %s, want the configured value", got)
	}
}

// The production defaults are part of the contract (F24/F26).
func TestAlertDefaultsAreFiveMinutesAndFiveSeconds(t *testing.T) {
	if alertCooldown != 5*time.Minute {
		t.Errorf("alertCooldown = %s, want 5m", alertCooldown)
	}
	if alertTimeout != 5*time.Second {
		t.Errorf("alertTimeout = %s, want 5s", alertTimeout)
	}
	if alertQueueSize != 32 {
		t.Errorf("alertQueueSize = %d, want 32", alertQueueSize)
	}
}

func TestAlertInvalidWebhookURLDisablesAlerting(t *testing.T) {
	for _, raw := range []string{"ftp://example.com/hook", "not-a-url", "https://"} {
		t.Setenv("ALERT_WEBHOOK_URL", raw)
		a := newAlertManager()
		if a.enabled {
			t.Errorf("%q was accepted as a webhook URL", raw)
		}
	}
	t.Setenv("ALERT_WEBHOOK_URL", "https://hooks.example.com/services/T000/B000/xoxb-secret")
	if a := newAlertManager(); !a.enabled {
		t.Error("a valid https URL was rejected")
	}
}

// The URL is a credential: it must not reach the log, the API or the file.
func TestAlertWebhookURLIsNeverLoggedOrExposed(t *testing.T) {
	setAlertTimings(t, time.Hour, 200*time.Millisecond)
	const secret = "xoxb-super-secret-token"
	sink := newAlertSink(t)
	secretURL := sink.ts.URL + "/services/" + secret + "?auth=" + secret

	var srv *Server
	var ts *httptest.Server
	var path string
	logged := captureLog(t, func() {
		srv, ts, path = alertServer(t, secretURL)
		sc := addSchedule(srv, "node-s", "ping", "1.1.1.1")
		srv.finalizeSchedule(sc, "ok", "baseline")
		srv.finalizeSchedule(sc, "error", "1")
		srv.finalizeSchedule(sc, "error", "2")
		waitForAlerts(t, sink, 1)
		// …and a failing delivery, whose error string embeds the URL.
		srv.alerts.post(alertEvent{ScheduleID: "dead", To: "error", At: time.Now()})
		srv.flushSchedules()
	})

	if strings.Contains(logged, secret) {
		t.Errorf("the webhook credential reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "Webhook alerts enabled") {
		t.Errorf("the startup line is missing:\n%s", logged)
	}
	host := strings.TrimPrefix(sink.ts.URL, "http://")
	if !strings.Contains(logged, "http://"+host) {
		t.Errorf("the startup line should name scheme+host:\n%s", logged)
	}

	// No handler returns it…
	for _, p := range []string{"/api/schedules", "/metrics"} {
		_, body, _ := histGet(t, ts, p, "")
		if strings.Contains(body, secret) || strings.Contains(body, "ALERT_WEBHOOK_URL") {
			t.Errorf("%s exposed the webhook URL: %s", p, body)
		}
	}
	// …and it is never persisted.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schedules file: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("the webhook URL was persisted: %s", data)
	}
}

// ---- dispatcher -------------------------------------------------------------

func TestAlertsDroppedWhenTheQueueIsFull(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	sink := newAlertSink(t)
	// No dispatcher goroutine here: the queue can only fill up.
	srv, ts, _ := histServer(t, map[string]string{"ALERT_WEBHOOK_URL": sink.ts.URL})
	if !srv.alerts.enabled {
		t.Fatal("alerting is not enabled")
	}
	for i := 0; i < alertQueueSize; i++ {
		srv.alerts.ch <- alertEvent{ScheduleID: "filler", To: "error"}
	}

	sc := addSchedule(srv, "node-d", "ping", "1.1.1.1")
	srv.finalizeSchedule(sc, "ok", "baseline")
	srv.finalizeSchedule(sc, "error", "1")
	srv.finalizeSchedule(sc, "error", "2") // would fire, but the queue is full

	if got := srv.alerts.droppedTotal(); got != 1 {
		t.Fatalf("dropped counter = %d, want 1", got)
	}
	code, body, _ := histGet(t, ts, "/metrics", "")
	if code != 200 {
		t.Fatalf("/metrics status %d", code)
	}
	if !strings.Contains(body, "lookingglass_alerts_dropped_total 1") {
		t.Errorf("the drop counter is not exposed:\n%s", body)
	}
}

func TestAlertDispatcherStopsOnShutdown(t *testing.T) {
	sink := newAlertSink(t)
	srv, _, _ := histServer(t, map[string]string{"ALERT_WEBHOOK_URL": sink.ts.URL})

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		srv.alerts.run(done)
		close(stopped)
	}()
	close(done)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatcher goroutine outlived the shutdown signal")
	}
}

// Deleting a schedule drops its alert bookkeeping, so the map cannot grow past
// the schedules that exist.
func TestAlertStateIsForgottenOnDelete(t *testing.T) {
	setAlertTimings(t, time.Hour, 5*time.Second)
	sink := newAlertSink(t)
	srv, ts, _ := alertServer(t, sink.ts.URL)
	sc := addSchedule(srv, "node-x", "ping", "1.1.1.1")
	srv.finalizeSchedule(sc, "ok", "baseline")

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/schedules/"+sc.ID, nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()

	srv.alerts.mu.Lock()
	_, present := srv.alerts.states[sc.ID]
	srv.alerts.mu.Unlock()
	if present {
		t.Error("alert state survived the delete")
	}
}
