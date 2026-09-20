package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapTileSecurityHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	securityHeaders(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' https://tile.openstreetmap.org data:;") {
		t.Fatalf("map tile origin missing from restricted image policy: %s", csp)
	}
	if strings.Contains(csp, "carto") {
		t.Fatal("retired map providers must not remain in the image allowlist")
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Fatalf("tile requests need an origin referrer without URL paths or queries: %s", got)
	}
}
