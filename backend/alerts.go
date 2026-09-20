package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Webhook alerts for scheduled probes (S3, F24–F26).
//
// One optional webhook URL (ALERT_WEBHOOK_URL) receives a JSON POST when a
// schedule's status changes in a way an operator should see. The URL is a
// credential in practice — it usually embeds a Slack/Discord token — so it is
// validated once at startup, never logged beyond scheme+host, never persisted
// to schedules.json and never returned by any handler.
//
// Noise control (F26): a transition fires only after two consecutive runs at
// the new status, at most once per alertCooldown per schedule, and never for
// the first run of a schedule after process start (otherwise a restart would
// alert for every schedule it re-runs).

// alertCooldown and alertTimeout are package vars so tests need neither
// multi-minute sleeps nor a five-second hang. Production values are the ones
// below.
var (
	alertCooldown = 5 * time.Minute
	alertTimeout  = 5 * time.Second
)

// alertQueueSize bounds the dispatcher's backlog. A full queue drops the
// alert (and counts it) rather than blocking a finalize path.
const alertQueueSize = 32

// alertEvent is one finalized run, as handed to the alert layer. It is also
// the webhook payload: the json tags are the wire contract.
type alertEvent struct {
	ScheduleID string          `json:"schedule_id"`
	Node       string          `json:"node"`
	Command    string          `json:"command"`
	Target     string          `json:"target"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Summary    json.RawMessage `json:"summary,omitempty"`
	At         time.Time       `json:"at"`
}

// alertState is the per-schedule transition bookkeeping.
type alertState struct {
	// seen is false until the first finalize after process start.
	seen bool
	// streakStatus / streakCount count consecutive runs at the same status.
	streakStatus string
	streakCount  int
	// notified is the status the operator was last told about (seeded with
	// the first observed status so a restart is silent).
	notified string
	lastFire time.Time
}

// alertManager owns the webhook client, the dispatcher queue and the
// per-schedule state. mu guards states only and is taken with no other mutex
// held, exactly like ratelimit.go's.
type alertManager struct {
	enabled bool
	url     string
	client  *http.Client
	ch      chan alertEvent

	mu     sync.Mutex
	states map[string]*alertState

	dropped atomic.Int64
	sent    atomic.Int64
}

// newAlertManager reads and validates ALERT_WEBHOOK_URL. An unset or unusable
// value disables alerting; the process never refuses to start over it.
func newAlertManager() *alertManager {
	a := &alertManager{
		ch:     make(chan alertEvent, alertQueueSize),
		states: make(map[string]*alertState),
		client: &http.Client{
			Timeout: alertTimeout,
			// Never follow a redirect: a 302 from a compromised or
			// misconfigured endpoint would re-send the payload (and the
			// webhook path) somewhere the operator never configured.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// Default transport: TLS verification stays on.
		},
	}
	raw := os.Getenv("ALERT_WEBHOOK_URL")
	if raw == "" {
		return a
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Nothing of the URL is safe to echo here — not even the host, since
		// the parse failed.
		log.Println("ALERT_WEBHOOK_URL could not be parsed — webhook alerts are disabled.")
		return a
	}
	scheme, host := logSafe(u.Scheme), logSafe(u.Host)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Printf("ALERT_WEBHOOK_URL is not a usable http(s) URL (scheme=%q host=%q) — webhook alerts are disabled.", scheme, host)
		return a
	}
	a.enabled = true
	a.url = raw
	// Scheme + host only: the path and query are where webhook tokens live.
	log.Printf("Webhook alerts enabled for scheduled probes (endpoint %s://%s).", scheme, host)
	return a
}

// logSafe makes an operator-supplied fragment safe to put in a log line.
func logSafe(s string) string {
	return truncateRunes(stripControlChars(s), 128)
}

// run is the single dispatcher goroutine. It exits when done is closed.
func (a *alertManager) run(done <-chan struct{}) {
	if a == nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case ev := <-a.ch:
			a.post(ev)
		}
	}
}

// observe records one finalized run and queues a webhook when the transition
// clears the confirmation, dedup and cooldown rules. Callers must hold no
// server mutex — this takes its own and then does a non-blocking send.
func (a *alertManager) observe(ev alertEvent) {
	if a == nil || !a.enabled {
		return
	}
	if !a.shouldFire(&ev) {
		return
	}
	select {
	case a.ch <- ev:
	default:
		// Queue full: the dispatcher is stuck on a slow endpoint. Dropping is
		// the only option that cannot stall the scheduler.
		a.dropped.Add(1)
	}
}

// shouldFire applies the F26 rules and, when it returns true, records the
// alert as fired and rewrites ev.From to the status the operator was last
// told about — "ok → degraded" is the transition worth paging on, not
// "degraded → degraded" (the immediately preceding run, which was the first,
// unconfirmed one).
func (a *alertManager) shouldFire(ev *alertEvent) bool {
	now := ev.At
	if now.IsZero() {
		now = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[ev.ScheduleID]
	if st == nil {
		st = &alertState{}
		a.states[ev.ScheduleID] = st
	}
	if !st.seen {
		// First run of this schedule since the process started: adopt the
		// status as the baseline and stay quiet.
		st.seen = true
		st.streakStatus = ev.To
		st.streakCount = 1
		st.notified = ev.To
		return false
	}
	if ev.To == st.streakStatus {
		st.streakCount++
	} else {
		st.streakStatus = ev.To
		st.streakCount = 1
	}
	// Two consecutive runs at the new status: one bad run is a blip.
	if st.streakCount < 2 {
		return false
	}
	if ev.To == st.notified {
		return false
	}
	if !st.lastFire.IsZero() && now.Sub(st.lastFire) < alertCooldown {
		// Flapping: keep the notified status as it was so the next transition
		// after the cooldown still fires.
		return false
	}
	ev.From = st.notified
	st.notified = ev.To
	st.lastFire = now
	return true
}

// forgetSchedule drops the alert state of a schedule (deleted by the
// operator), so a recreated id starts from a clean baseline.
func (a *alertManager) forgetSchedule(id string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.states, id)
	a.mu.Unlock()
}

// post delivers one event. Failures are logged without the URL.
func (a *alertManager) post(ev alertEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", a.url, bytes.NewReader(body))
	if err != nil {
		log.Println("Alert webhook request could not be built — check ALERT_WEBHOOK_URL.")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// err.Error() embeds the full URL — never log it.
		log.Printf("Alert webhook delivery failed for schedule %s.", logSafe(ev.ScheduleID))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		log.Printf("Alert webhook returned HTTP %d for schedule %s.", resp.StatusCode, logSafe(ev.ScheduleID))
		return
	}
	a.sent.Add(1)
}

// droppedTotal is the /metrics accessor; nil-safe so a hand-built Server in a
// test never panics.
func (a *alertManager) droppedTotal() int64 {
	if a == nil {
		return 0
	}
	return a.dropped.Load()
}
