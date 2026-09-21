package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeoIPRejectsMalformedProviderData(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"valid", `{"status":"success","query":"1.1.1.1","city":"A\nB","lat":20,"lon":-80}`, 200, true},
		{"object-text", `{"status":"success","city":{}}`, 200, false},
		{"object-status", `{"status":{}}`, 200, false},
		{"null", `null`, 200, false},
		{"missing-status", `{}`, 200, false},
		{"coordinate-type", `{"status":"success","lat":"20"}`, 200, false},
		{"coordinate-range", `{"status":"success","lon":181}`, 200, false},
		{"wrong-query", `{"status":"success","query":"8.8.8.8"}`, 200, false},
		{"http-error", `{"status":"success"}`, 500, false},
		{"oversized", `{"status":"success"}` + strings.Repeat(" ", 64*1024), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := s11NewServer(t)
			srv.geoClient = &http.Client{Transport: metadataRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			body, err := srv.lookupGeoIP(context.Background(), "1.1.1.1")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid && len(srv.geoCache) != 0 {
				t.Fatal("invalid response cached")
			}
			if tc.valid {
				var record map[string]any
				if err := json.Unmarshal(body, &record); err != nil {
					t.Fatal(err)
				}
				if record["city"] != "AB" {
					t.Fatalf("unsanitized city: %v", record)
				}
			}
		})
	}
}

func TestRDAPRejectsNonObjectsAndOversizedBodies(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `"text"`, `{}` + strings.Repeat(" ", 512*1024)} {
		srv, _ := s11NewServer(t)
		srv.geoClient = &http.Client{Transport: metadataRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		rec := httptest.NewRecorder()
		srv.handleRDAP(rec, httptest.NewRequest("GET", "/api/rdap/example.com", nil))
		if rec.Code != 502 || len(srv.rdapCache) != 0 {
			t.Fatalf("status=%d cached=%d", rec.Code, len(srv.rdapCache))
		}
	}
}

func TestSummaryRejectsSanitizedKeyCollisions(t *testing.T) {
	for _, body := range []string{`{"avg_ms":1,"avg_\nms":2}`, `{"hops":[{"host":"a","ho\nst":"b"}]}`} {
		for i := 0; i < 20; i++ {
			if _, ok := validateSummary(body); ok {
				t.Fatalf("accepted ambiguous summary: %s", body)
			}
		}
	}
}
