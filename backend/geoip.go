package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleGeoIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Geo answers describe addresses the operator looked up; never let a shared
	// cache hold them, and never serve one to a request that has no key (F20).
	w.Header().Set("Cache-Control", "no-store")

	// With a client key configured and public mode off, geo lookups are an
	// authenticated outbound proxy: unauthenticated callers can't use them.
	if !s.requireClientAuth(w, r) {
		return
	}

	// Bucket per /32 (v4) or /64 (v6) rather than per exact address, so a
	// client with a routed v6 prefix can't mint a fresh bucket per request.
	source := clientRateKey(r)

	if r.Method == "POST" {
		// Rate limit: charge one token per IP requested in the batch, not per call,
		// so callers can't sneak around the limit by batching 50 IPs at once.
		var req struct {
			IPs []string `json:"ips"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "bad request", 400)
			return
		}
		if len(req.IPs) > maxGeoIPBatchSize {
			writeJSONError(w, fmt.Sprintf("batch size exceeds maximum of %d", maxGeoIPBatchSize), 400)
			return
		}
		results := make([]json.RawMessage, 0, len(req.IPs))
		for _, ip := range req.IPs {
			ip = strings.TrimSpace(ip)
			if ip == "" || ip == "*" {
				continue
			}
			if net.ParseIP(ip) == nil {
				continue
			}
			// Cache hits don't consume a token (no upstream call), so check the
			// rate limit only when we're actually about to call ip-api.com.
			s.geoCacheMu.Lock()
			elem, cached := s.geoCache[ip]
			s.geoCacheMu.Unlock()
			if !cached || time.Now().After(elem.Value.(*geoCacheEntry).expiresAt) {
				if !s.allowGeoIP(source) {
					w.Header().Set("Retry-After", "1")
					writeJSONError(w, "rate limit exceeded", 429)
					return
				}
			}
			data, err := s.lookupGeoIP(ip)
			if err != nil {
				continue
			}
			results = append(results, data)
		}
		json.NewEncoder(w).Encode(results)
		return
	}

	if r.Method != "GET" {
		writeJSONError(w, "method not allowed", 405)
		return
	}

	// Single IP lookup: /api/geoip/1.1.1.1
	ip := strings.TrimPrefix(r.URL.Path, "/api/geoip/")
	ip = strings.TrimSpace(ip)
	if ip == "" || net.ParseIP(ip) == nil {
		writeJSONError(w, "invalid IP", 400)
		return
	}

	// Same cache-aware rate-limit check.
	s.geoCacheMu.Lock()
	elem, cached := s.geoCache[ip]
	s.geoCacheMu.Unlock()
	if !cached || time.Now().After(elem.Value.(*geoCacheEntry).expiresAt) {
		if !s.allowGeoIP(source) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, "rate limit exceeded", 429)
			return
		}
	}

	data, err := s.lookupGeoIP(ip)
	if err != nil {
		log.Printf("GeoIP lookup failed for %s: %v", ip, err)
		writeJSONError(w, "GeoIP lookup failed", 502)
		return
	}
	w.Write(data)
}

func (s *Server) lookupGeoIP(ip string) ([]byte, error) {
	s.geoLookupsTotal.Add(1)
	// Check cache (with TTL). On hit, promote to MRU position.
	s.geoCacheMu.Lock()
	if elem, ok := s.geoCache[ip]; ok {
		entry := elem.Value.(*geoCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			s.geoCacheList.MoveToFront(elem)
			data := entry.data
			s.geoCacheMu.Unlock()
			s.geoCacheHitsTotal.Add(1)
			return data, nil
		}
		// Expired: drop now so the slot is available.
		s.geoCacheList.Remove(elem)
		delete(s.geoCache, ip)
	}
	s.geoCacheMu.Unlock()

	resp, err := s.geoClient.Get(fmt.Sprintf("http://ip-api.com/json/%s?fields=status,message,country,countryCode,region,city,lat,lon,isp,org,as,query", ip))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Limit response body size
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}

	// Validate and sanitize: parse the JSON, ensure it's valid, re-serialize with whitelisted fields only
	var geoData map[string]any
	if err := json.Unmarshal(body, &geoData); err != nil {
		return nil, fmt.Errorf("invalid GeoIP response")
	}
	// Don't cache error responses from ip-api.com (e.g., rate limit, invalid IP)
	if status, ok := geoData["status"].(string); ok && status != "success" {
		msg, _ := geoData["message"].(string)
		return nil, fmt.Errorf("GeoIP lookup failed: %s", msg)
	}
	// Only pass through known safe fields to prevent forwarding unexpected data
	allowedFields := map[string]bool{
		"status": true, "country": true, "countryCode": true, "region": true,
		"city": true, "lat": true, "lon": true, "isp": true, "org": true,
		"as": true, "query": true,
	}
	filtered := make(map[string]any, len(allowedFields))
	for k, v := range geoData {
		if allowedFields[k] {
			filtered[k] = v
		}
	}
	sanitizedBody, err := json.Marshal(filtered)
	if err != nil {
		return nil, fmt.Errorf("failed to re-serialize GeoIP data")
	}

	// Insert at MRU front; evict LRU back if over capacity. O(1) regardless of pressure.
	s.geoCacheMu.Lock()
	// Another goroutine may have raced and inserted while we were doing the HTTP call.
	if elem, ok := s.geoCache[ip]; ok {
		s.geoCacheList.Remove(elem)
		delete(s.geoCache, ip)
	}
	entry := &geoCacheEntry{ip: ip, data: sanitizedBody, expiresAt: time.Now().Add(geoCacheTTL)}
	s.geoCache[ip] = s.geoCacheList.PushFront(entry)
	for len(s.geoCache) > geoCacheMaxSize {
		oldest := s.geoCacheList.Back()
		if oldest == nil {
			break
		}
		s.geoCacheList.Remove(oldest)
		delete(s.geoCache, oldest.Value.(*geoCacheEntry).ip)
	}
	s.geoCacheMu.Unlock()

	return sanitizedBody, nil
}

// allowGeoIP charges one token from the per-source-IP bucket and reports
// whether the request should be permitted. Buckets refill continuously and
// are garbage-collected by geoCacheCleanup.
func (s *Server) allowGeoIP(sourceIP string) bool {
	s.geoRateMu.Lock()
	defer s.geoRateMu.Unlock()
	now := time.Now()
	b, ok := s.geoRateBuckets[sourceIP]
	if !ok {
		// Bound attacker-controlled source identities without evicting active
		// buckets (which would reset their rate limit). Idle entries are reaped
		// by geoCacheCleanup.
		if len(s.geoRateBuckets) >= rateEntryMaxCount {
			s.geoRateLimitedTotal.Add(1)
			return false
		}
		// New bucket starts full so first-time requests aren't penalised.
		b = &ipRateBucket{tokens: geoRateBurst, lastFill: now}
		s.geoRateBuckets[sourceIP] = b
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * geoRateRefillPerSec
		if b.tokens > geoRateBurst {
			b.tokens = geoRateBurst
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		s.geoRateLimitedTotal.Add(1)
		return false
	}
	b.tokens--
	return true
}

// geoCacheCleanup periodically removes expired cache entries and stale rate
// buckets. Stops when the provided channel is closed.
func (s *Server) geoCacheCleanup(done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			now := time.Now()
			s.geoCacheMu.Lock()
			// Hits promote entries without refreshing their expiry, so LRU
			// order is not expiry order. Scan the bounded cache completely.
			for ip, elem := range s.geoCache {
				if !now.Before(elem.Value.(*geoCacheEntry).expiresAt) {
					s.geoCacheList.Remove(elem)
					delete(s.geoCache, ip)
				}
			}
			s.geoCacheMu.Unlock()

			// Drop rate buckets that have been idle long enough to be at full
			// burst — keeping them adds no value and just leaks memory.
			s.geoRateMu.Lock()
			for ip, b := range s.geoRateBuckets {
				if now.Sub(b.lastFill) > 10*time.Minute {
					delete(s.geoRateBuckets, ip)
				}
			}
			s.geoRateMu.Unlock()

			// Same idle rule for the public-mode limiter, but entries holding
			// live connections are never dropped (see ratelimit.go). rateMu is
			// taken here with no other lock held.
			s.rateGC(now)
		}
	}
}
