package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	rdapCacheTTL  = 6 * time.Hour
	rdapCacheMax  = 5000
	rdapBootstrap = "https://rdap.org"
)

// handleRDAP proxies a query to rdap.org and caches the JSON response.
// Two query types are supported:
//   - IP address  → /ip/<ip>     (returns AS handle, abuse contact, etc.)
//   - Domain      → /domain/<d>  (returns registrar, NS, registrant, etc.)
//
// Rate limit shares the geoip token bucket because both go to external
// services and the practical concern is total outbound burst, not per-API.
func (s *Server) handleRDAP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	// Same gate as geoip: an authenticated deployment doesn't hand out an
	// anonymous RDAP proxy.
	if !s.requireClientAuth(w, r) {
		return
	}
	query := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/rdap/"))
	if query == "" {
		writeJSONError(w, "missing query", 400)
		return
	}
	if len(query) > 253 {
		writeJSONError(w, "query too long", 400)
		return
	}

	// Decide IP vs. domain. We refuse anything else — RDAP also has
	// /entity/, /nameserver/, /autnum/ endpoints but they're rarely useful
	// from a network-diagnostics UI and broadening the surface invites
	// abuse-as-proxy concerns.
	var kind string
	if net.ParseIP(query) != nil {
		kind = "ip"
	} else if isPlausibleDomain(query) {
		kind = "domain"
	} else {
		writeJSONError(w, "invalid query (expected IP or domain)", 400)
		return
	}

	// Cache hit?
	s.rdapCacheMu.Lock()
	if e, ok := s.rdapCache[query]; ok && time.Now().Before(e.expiresAt) {
		data := e.data
		s.rdapCacheMu.Unlock()
		w.Write(data)
		return
	}
	s.rdapCacheMu.Unlock()

	// Cache miss — share the geoip rate-limit budget for outbound lookups.
	if !s.allowGeoIP(clientRateKey(r)) {
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, "rate limit exceeded", 429)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/%s/%s", rdapBootstrap, kind, query)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		writeJSONError(w, "RDAP lookup failed", 502)
		return
	}
	req.Header.Set("Accept", "application/rdap+json")
	req.Header.Set("User-Agent", "parallax/1.0")

	resp, err := s.geoClient.Do(req)
	if err != nil {
		log.Printf("RDAP lookup failed for %s: %v", query, err)
		writeJSONError(w, "RDAP lookup failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeJSONError(w, fmt.Sprintf("RDAP upstream returned %d", resp.StatusCode), 502)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		writeJSONError(w, "RDAP lookup failed", 502)
		return
	}
	if !json.Valid(body) {
		writeJSONError(w, "invalid RDAP response", 502)
		return
	}

	// Insert into cache. Eviction is coarse — RDAP records are larger and
	// less frequently consulted than geoip, so an LRU isn't worth the code.
	s.rdapCacheMu.Lock()
	if len(s.rdapCache) >= rdapCacheMax {
		// Pass 1: drop expired
		now := time.Now()
		for k, e := range s.rdapCache {
			if now.After(e.expiresAt) {
				delete(s.rdapCache, k)
			}
		}
		// Pass 2: still over → drop arbitrary entry. We don't track LRU here
		// because RDAP queries are sparse compared to geoip.
		if len(s.rdapCache) >= rdapCacheMax {
			for k := range s.rdapCache {
				delete(s.rdapCache, k)
				break
			}
		}
	}
	s.rdapCache[query] = &rdapEntry{data: body, expiresAt: time.Now().Add(rdapCacheTTL)}
	s.rdapCacheMu.Unlock()

	w.Write(body)
}

// isPlausibleDomain checks that the query looks enough like a hostname to
// send to rdap.org. We don't try to be authoritative — the registry will
// reject malformed names. The check exists to keep weird input (URLs, paths,
// shell-special characters) out of the upstream URL.
func isPlausibleDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	if !strings.Contains(s, ".") {
		return false
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
