package main

import (
	"encoding/json"
	"io"
	"strings"
)

// Structured probe summaries — server-side validator (S2, F21/F23).
//
// An agent may send one `summary` message per command, carrying a small JSON
// object it parsed out of the probe's own output. That object is attacker
// controlled in exactly the same way every other agent-supplied string is, so
// it is never forwarded or persisted as received:
//
//  1. anything larger than summaryMaxBytes is dropped before it is parsed;
//  2. it must match a *total* grammar (below) — anything else drops the whole
//     summary, never a partial one;
//  3. every string at any depth is control-stripped and rune-truncated;
//  4. the result is re-marshalled once, and that canonical blob is what the
//     browser receives and what a schedule persists.
//
// Accepted grammar:
//
//	top level     object, ≤ summaryMaxKeys keys
//	  value       string | number | bool
//	            | array of ≤ summaryMaxArrayLen string|number|bool
//	            | array of ≤ summaryMaxArrayLen flat objects of
//	              ≤ summaryMaxObjectKeys string|number|bool values
//
// Nothing nests deeper: an object at top level, an object inside an object
// inside an array, an array of arrays, and JSON null are all rejected.
const (
	// summaryMaxBytes is checked before parsing — a 64-hop mtr summary is
	// ~12 KB, so this leaves headroom without inviting a parser DoS.
	summaryMaxBytes = 16384
	// summaryMaxKeys bounds the top-level object.
	summaryMaxKeys = 32
	// summaryMaxArrayLen bounds any array (mtr's hops is the only one today).
	summaryMaxArrayLen = 64
	// summaryMaxObjectKeys bounds one flat object inside an array.
	summaryMaxObjectKeys = 8
	// summaryMaxStringRunes truncates (never rejects) every string value.
	summaryMaxStringRunes = 256
)

// truncateRunes cuts s to at most max runes. Rune-based so a multi-byte
// sequence is never split.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// sanitizeSummaryString applies the envelope invariant for agent-supplied
// strings: strip control characters, then rune-truncate. It deliberately does
// *not* HTML-escape — this value is JSON data the browser renders as text, and
// escaping here would corrupt it (rendering is the render layer's job).
func sanitizeSummaryString(s string) string {
	return truncateRunes(stripControlChars(s), summaryMaxStringRunes)
}

// validateSummary checks one agent-supplied summary against the grammar and
// returns the canonical, sanitized re-marshalled blob. ok is false whenever
// anything at all is off — the caller then drops the message entirely.
func validateSummary(data string) (canonical []byte, ok bool) {
	if len(data) > summaryMaxBytes {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(data))
	// UseNumber keeps every number as its literal text: no float rounding, no
	// exponent surprises, and the re-marshalled blob matches what was sent.
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, false
	}
	// Reject trailing content ("{} {}" or "{} garbage").
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	if top == nil || len(top) > summaryMaxKeys {
		return nil, false
	}

	out := make(map[string]any, len(top))
	for key, value := range top {
		clean, valueOK := sanitizeSummaryValue(value)
		if !valueOK {
			return nil, false
		}
		out[sanitizeSummaryString(key)] = clean
	}
	canonical, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	// A canonical form that grew past the cap (it cannot, but the check is
	// cheap and keeps the stored/forwarded bound absolute) is dropped.
	if len(canonical) > summaryMaxBytes {
		return nil, false
	}
	return canonical, true
}

// sanitizeSummaryValue handles one top-level value.
func sanitizeSummaryValue(value any) (any, bool) {
	if scalar, ok := sanitizeSummaryScalar(value); ok {
		return scalar, true
	}
	arr, isArray := value.([]any)
	if !isArray {
		return nil, false // object at top level, null, or anything unknown
	}
	if len(arr) > summaryMaxArrayLen {
		return nil, false
	}
	out := make([]any, 0, len(arr))
	// An array is either all scalars or all flat objects — never a mix, and
	// never an array of arrays.
	objects := false
	for i, elem := range arr {
		if obj, isObj := elem.(map[string]any); isObj {
			if i == 0 {
				objects = true
			} else if !objects {
				return nil, false
			}
			flat, flatOK := sanitizeSummaryFlatObject(obj)
			if !flatOK {
				return nil, false
			}
			out = append(out, flat)
			continue
		}
		if objects {
			return nil, false
		}
		scalar, scalarOK := sanitizeSummaryScalar(elem)
		if !scalarOK {
			return nil, false
		}
		out = append(out, scalar)
	}
	return out, true
}

// sanitizeSummaryFlatObject handles one object inside an array (mtr's hops).
func sanitizeSummaryFlatObject(obj map[string]any) (map[string]any, bool) {
	if len(obj) > summaryMaxObjectKeys {
		return nil, false
	}
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		scalar, ok := sanitizeSummaryScalar(value)
		if !ok {
			return nil, false // nested object/array inside a hop
		}
		out[sanitizeSummaryString(key)] = scalar
	}
	return out, true
}

// sanitizeSummaryScalar accepts exactly string, number and bool. JSON null and
// every composite value are rejected here.
func sanitizeSummaryScalar(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return sanitizeSummaryString(v), true
	case json.Number:
		return v, true
	case bool:
		return v, true
	default:
		return nil, false
	}
}

// parseExitOK reads the structured `done` payload (F35). Older agents send an
// empty string, which means "no information" and is treated as a clean exit so
// a mixed-version fleet does not suddenly report every run as failed.
func parseExitOK(data string) bool {
	if strings.TrimSpace(data) == "" {
		return true
	}
	var payload struct {
		ExitOK *bool `json:"exit_ok"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil || payload.ExitOK == nil {
		return true
	}
	return *payload.ExitOK
}

// summaryLossPct extracts a numeric top-level loss_pct from an already
// validated summary. ok is false when the summary is absent or carries no
// numeric loss_pct (http, dns).
func summaryLossPct(summary []byte) (float64, bool) {
	if len(summary) == 0 {
		return 0, false
	}
	dec := json.NewDecoder(strings.NewReader(string(summary)))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return 0, false
	}
	num, ok := top["loss_pct"].(json.Number)
	if !ok {
		return 0, false
	}
	f, err := num.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

// acceptSummary validates an agent-supplied summary and enforces "at most one
// per command id". The dedup set is bounded by the number of in-flight
// commands: handleAgentOutput only reaches here for a cmdID that is currently
// registered in cmdNodes, and every teardown path deletes the entry
// (cleanupCommand, clearRunRegistration, the agent-disconnect loop).
//
// summarySeenMu is taken alone, with no other server mutex held.
func (s *Server) acceptSummary(cmdID, data string) ([]byte, bool) {
	canonical, ok := validateSummary(data)
	if !ok {
		return nil, false
	}
	s.summarySeenMu.Lock()
	defer s.summarySeenMu.Unlock()
	if _, dup := s.summarySeen[cmdID]; dup {
		return nil, false
	}
	s.summarySeen[cmdID] = struct{}{}
	return canonical, true
}

// forgetSummary drops the dedup entry for a command. Safe to call for a cmdID
// that never had one. Cleanup may hold cmdOwnersMu before taking
// summarySeenMu; the reverse order is never permitted.
func (s *Server) forgetSummary(cmdID string) {
	s.summarySeenMu.Lock()
	delete(s.summarySeen, cmdID)
	s.summarySeenMu.Unlock()
}
