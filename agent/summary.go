package main

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Structured probe summaries (S2).
//
// executeCommand streams every output line to the server exactly as before.
// In parallel it feeds the lines of the probes we can parse into a *bounded*
// tail buffer (last 256 lines, ≤ 64 KB) and, if and only if the process
// exited cleanly, parses that tail into a small JSON object that is sent as
// one extra `summary` message *before* `done`.
//
// Two rules the parsers must never break:
//   - the full output is never retained — only the tail buffer is;
//   - an output we cannot parse produces *no* summary, never a partial one.

const (
	// summaryTailMaxLines / summaryTailMaxBytes size the per-command tail
	// buffer. 256 lines is enough for a 64-hop `mtr -r` plus its headers, so
	// the last hop — which supplies the top-level loss_pct — is never evicted.
	summaryTailMaxLines = 256
	summaryTailMaxBytes = 64 * 1024

	// summaryMaxHostLen clamps a hostname taken from mtr output. mtr -w prints
	// un-truncated names; 64 of them at full DNS length would push the summary
	// past the server's 16 KB pre-parse cap and the whole summary would be
	// dropped. 128 keeps a 64-hop trace comfortably inside that budget.
	summaryMaxHostLen = 128
)

// tailBuffer keeps the last summaryTailMaxLines lines, never exceeding
// summaryTailMaxBytes in total. Not safe for concurrent use: it is owned by
// the single goroutine running one command's scanner loop.
type tailBuffer struct {
	lines []string
	bytes int
}

func newTailBuffer() *tailBuffer {
	return &tailBuffer{lines: make([]string, 0, 64)}
}

// add appends one line, evicting from the front until both caps hold. A single
// line longer than the byte cap is truncated rather than allowed to blow the
// budget (an unparsable tail simply yields no summary).
func (t *tailBuffer) add(line string) {
	if t == nil {
		return
	}
	if len(line) > summaryTailMaxBytes {
		line = line[:summaryTailMaxBytes]
	}
	t.lines = append(t.lines, line)
	t.bytes += len(line)
	for len(t.lines) > summaryTailMaxLines || (t.bytes > summaryTailMaxBytes && len(t.lines) > 1) {
		t.bytes -= len(t.lines[0])
		t.lines[0] = "" // drop the reference so the string can be collected
		t.lines = t.lines[1:]
	}
}

// tail returns the retained lines, oldest first.
func (t *tailBuffer) tail() []string {
	if t == nil {
		return nil
	}
	return t.lines
}

// summaryParser turns the retained tail of one probe's output into a flat map.
// Returning nil means "not parsable" — the caller then emits no summary.
type summaryParser func(lines []string) map[string]any

// summaryParsers is keyed by command type. A type that is absent here simply
// never produces a summary (traceroute, nexttrace, iperf3, speedtest today)
// and its output is not buffered at all.
var summaryParsers = map[string]summaryParser{
	"ping": parsePingSummary,
	"http": parseHTTPSummary,
	"dns":  parseDNSSummary,
	"mtr":  parseMtrSummary,
}

// hasSummaryParser reports whether this command type is worth tail-buffering.
func hasSummaryParser(cmdType string) bool {
	_, ok := summaryParsers[cmdType]
	return ok
}

// summaryJSON parses the tail and returns the JSON blob to send, or nil when
// there is no parser for this command type or the output did not parse.
func summaryJSON(cmdType string, lines []string) []byte {
	parser, ok := summaryParsers[cmdType]
	if !ok {
		return nil
	}
	m := parser(lines)
	if len(m) == 0 {
		return nil
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return blob
}

// doneData is the structured payload of the `done` message (F35). Older
// servers ignore it; this server reads exit_ok from it and falls back to
// "true" for the empty string older agents send.
func doneData(exitOK bool) string {
	blob, err := json.Marshal(struct {
		ExitOK bool `json:"exit_ok"`
	}{ExitOK: exitOK})
	if err != nil {
		return ""
	}
	return string(blob)
}

// round3 keeps derived millisecond values readable without inventing
// precision the source never had.
func round3(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1000) / 1000
}

// ── ping ─────────────────────────────────────────────────────────────────────
//
// Linux (iputils):
//
//	10 packets transmitted, 10 received, 0% packet loss, time 9012ms
//	rtt min/avg/max/mdev = 11.9/12.5/13.2/0.4 ms
//
// macOS / BSD:
//
//	10 packets transmitted, 10 packets received, 0.0% packet loss
//	round-trip min/avg/max/stddev = 11.9/12.5/13.2/0.4 ms
//
// The stats line is mandatory (it carries sent/received/loss_pct); the rtt
// line is optional because a 100 %-loss run legitimately has none.
var (
	pingStatsRe = regexp.MustCompile(`(\d+)\s+packets transmitted,\s+(\d+)\s+(?:packets\s+)?received.*?([0-9]+(?:\.[0-9]+)?)%\s+packet loss`)
	pingRTTRe   = regexp.MustCompile(`(?:rtt|round-trip)\s+min/avg/max/(?:mdev|stddev)\s*=\s*([0-9.]+)/([0-9.]+)/([0-9.]+)/([0-9.]+)\s*ms`)
)

func parsePingSummary(lines []string) map[string]any {
	out := make(map[string]any, 7)
	for _, line := range lines {
		if m := pingStatsRe.FindStringSubmatch(line); m != nil {
			sent, err1 := strconv.Atoi(m[1])
			recv, err2 := strconv.Atoi(m[2])
			loss, err3 := strconv.ParseFloat(m[3], 64)
			if err1 != nil || err2 != nil || err3 != nil {
				return nil
			}
			out["sent"] = sent
			out["received"] = recv
			out["loss_pct"] = round3(loss)
			continue
		}
		if m := pingRTTRe.FindStringSubmatch(line); m != nil {
			vals := make([]float64, 4)
			ok := true
			for i := 0; i < 4; i++ {
				v, err := strconv.ParseFloat(m[i+1], 64)
				if err != nil {
					ok = false
					break
				}
				vals[i] = v
			}
			if !ok {
				continue
			}
			out["min_ms"] = round3(vals[0])
			out["avg_ms"] = round3(vals[1])
			out["max_ms"] = round3(vals[2])
			out["mdev_ms"] = round3(vals[3])
		}
	}
	if _, ok := out["sent"]; !ok {
		return nil
	}
	return out
}

// ── http ─────────────────────────────────────────────────────────────────────
//
// buildHTTP asks curl for exactly one line:
//
//	code=%{http_code}  dns=%{time_namelookup}s  connect=%{time_connect}s \
//	tls=%{time_appconnect}s  ttfb=%{time_starttransfer}s \
//	total=%{time_total}s  size=%{size_download}b
//
// The parser therefore keys off those exact labels; summary_test.go renders
// the format string taken straight out of buildHTTP so the two cannot drift.
var httpFieldRe = regexp.MustCompile(`(?:^|\s)(code|dns|connect|tls|ttfb|total|size)=([0-9]+(?:\.[0-9]+)?)[sb]?(?:\s|$)`)

func parseHTTPSummary(lines []string) map[string]any {
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, "code=") || !strings.Contains(line, "total=") {
			continue
		}
		fields := make(map[string]float64, 7)
		for _, m := range httpFieldRe.FindAllStringSubmatch(line, -1) {
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				continue
			}
			fields[m[1]] = v
		}
		for _, want := range []string{"code", "dns", "connect", "tls", "ttfb", "total", "size"} {
			if _, ok := fields[want]; !ok {
				return nil
			}
		}
		return map[string]any{
			"http_code":  int(fields["code"]),
			"dns_ms":     round3(fields["dns"] * 1000),
			"connect_ms": round3(fields["connect"] * 1000),
			"tls_ms":     round3(fields["tls"] * 1000),
			"ttfb_ms":    round3(fields["ttfb"] * 1000),
			"total_ms":   round3(fields["total"] * 1000),
			"size_bytes": int(fields["size"]),
		}
	}
	return nil
}

// ── dns ──────────────────────────────────────────────────────────────────────
//
// dig's default output carries everything we need in three lines:
//
//	;; ->>HEADER<<- opcode: QUERY, status: NOERROR, id: 4242
//	;; flags: qr rd ra; QUERY: 1, ANSWER: 2, AUTHORITY: 0, ADDITIONAL: 1
//	;; Query time: 23 msec
var (
	digStatusRe    = regexp.MustCompile(`status:\s*([A-Za-z]+)`)
	digAnswerRe    = regexp.MustCompile(`ANSWER:\s*(\d+)`)
	digQueryTimeRe = regexp.MustCompile(`Query time:\s*(\d+)\s*msec`)
)

func parseDNSSummary(lines []string) map[string]any {
	out := make(map[string]any, 3)
	for _, line := range lines {
		if _, seen := out["status"]; !seen {
			if m := digStatusRe.FindStringSubmatch(line); m != nil {
				out["status"] = strings.ToUpper(m[1])
			}
		}
		if _, seen := out["answer_count"]; !seen {
			if m := digAnswerRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					out["answer_count"] = n
				}
			}
		}
		if _, seen := out["query_time_ms"]; !seen {
			if m := digQueryTimeRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					out["query_time_ms"] = n
				}
			}
		}
	}
	if len(out) != 3 {
		return nil // partial dig output is not a summary
	}
	return out
}

// ── mtr ──────────────────────────────────────────────────────────────────────
//
// buildMtr runs `mtr -r -w -c 5`, whose report looks like:
//
//	Start: 2026-09-19T12:00:00+0000
//	HOST: probe-01                    Loss%   Snt   Last   Avg  Best  Wrst StDev
//	  1.|-- 192.168.1.1                0.0%     5    0.4   0.5   0.4   0.7   0.1
//	  2.|-- ???                      100.0%     5    0.0   0.0   0.0   0.0   0.0
//
// The top-level loss_pct is the loss of the *last* hop — that is the target,
// which is what a monitoring status should key off.
var mtrHopRe = regexp.MustCompile(`^\s*(\d+)\.\|--\s+(\S+)\s+([0-9.]+)%\s+(\d+)\s+([0-9.]+)\s+([0-9.]+)\s+([0-9.]+)\s+([0-9.]+)\s+([0-9.]+)\s*$`)

func parseMtrSummary(lines []string) map[string]any {
	hops := make([]any, 0, 16)
	var lastLoss float64
	for _, line := range lines {
		m := mtrHopRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err0 := strconv.Atoi(m[1])
		loss, err1 := strconv.ParseFloat(m[3], 64)
		avg, err2 := strconv.ParseFloat(m[6], 64)
		best, err3 := strconv.ParseFloat(m[7], 64)
		worst, err4 := strconv.ParseFloat(m[8], 64)
		if err0 != nil || err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		host := m[2]
		if len(host) > summaryMaxHostLen {
			host = host[:summaryMaxHostLen]
		}
		hops = append(hops, map[string]any{
			"hop":      idx,
			"host":     host,
			"loss_pct": round3(loss),
			"avg":      round3(avg),
			"best":     round3(best),
			"worst":    round3(worst),
		})
		lastLoss = round3(loss)
	}
	if len(hops) == 0 {
		return nil
	}
	return map[string]any{
		"hops":      hops,
		"hop_count": len(hops),
		"loss_pct":  lastLoss,
	}
}
