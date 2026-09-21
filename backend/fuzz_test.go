package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func FuzzValidateSummary(f *testing.F) {
	for _, seed := range []string{`{}`, `{"loss_pct":20,"name":"a\u0000b"}`, `{"hops":[{"ip":"192.0.2.1","avg_ms":1.2}]}`, `{"a":null}`, `{} {}`, `{"a":1,"a\u0000":2}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		canonical, ok := validateSummary(input)
		if !ok {
			return
		}
		if len(canonical) > summaryMaxBytes || !json.Valid(canonical) || !utf8.Valid(canonical) {
			t.Fatal("accepted summary violates output bounds or encoding")
		}
		again, valid := validateSummary(string(canonical))
		if !valid || !bytes.Equal(canonical, again) {
			t.Fatal("canonical summary is not stable under validation")
		}
	})
}
