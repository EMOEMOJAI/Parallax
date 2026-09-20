package main

import (
	"container/list"
	"sync"
	"testing"
	"time"
)

// newTestServer constructs a Server with only the geoCache fields populated —
// enough to exercise lookupGeoIP-style cache operations without touching the
// HTTP/WebSocket layers.
func newTestServer() *Server {
	return &Server{
		geoCache:     make(map[string]*list.Element),
		geoCacheList: list.New(),
	}
}

// insertEntry mirrors the cache-write block from lookupGeoIP. We don't call
// lookupGeoIP directly because that requires an HTTP round trip to ip-api.com.
func insertEntry(s *Server, ip string, ttl time.Duration) {
	s.geoCacheMu.Lock()
	defer s.geoCacheMu.Unlock()
	if elem, ok := s.geoCache[ip]; ok {
		s.geoCacheList.Remove(elem)
		delete(s.geoCache, ip)
	}
	entry := &geoCacheEntry{ip: ip, data: []byte(ip), expiresAt: time.Now().Add(ttl)}
	s.geoCache[ip] = s.geoCacheList.PushFront(entry)
	for len(s.geoCache) > geoCacheMaxSize {
		oldest := s.geoCacheList.Back()
		if oldest == nil {
			break
		}
		s.geoCacheList.Remove(oldest)
		delete(s.geoCache, oldest.Value.(*geoCacheEntry).ip)
	}
}

func TestGeoCacheLRUEviction(t *testing.T) {
	s := newTestServer()
	// Fill to capacity.
	for i := 0; i < geoCacheMaxSize; i++ {
		insertEntry(s, formatIP(i), time.Hour)
	}
	if got := len(s.geoCache); got != geoCacheMaxSize {
		t.Fatalf("expected cache full at %d, got %d", geoCacheMaxSize, got)
	}

	// Inserting one more should evict the oldest (IP 0), not anything else.
	insertEntry(s, "extra", time.Hour)
	if _, ok := s.geoCache[formatIP(0)]; ok {
		t.Errorf("expected oldest entry %q to be evicted", formatIP(0))
	}
	if _, ok := s.geoCache["extra"]; !ok {
		t.Errorf("expected new entry %q to be present", "extra")
	}
	if got := len(s.geoCache); got != geoCacheMaxSize {
		t.Errorf("expected cache size to remain at %d, got %d", geoCacheMaxSize, got)
	}
}

func TestGeoCacheLRUPromotion(t *testing.T) {
	s := newTestServer()
	// Insert 3 entries: oldest=A, then B, then C (most recent).
	insertEntry(s, "A", time.Hour)
	insertEntry(s, "B", time.Hour)
	insertEntry(s, "C", time.Hour)

	// Promote A by re-inserting (mirrors lookupGeoIP's cache-hit promotion via
	// MoveToFront). Use the actual MoveToFront call to test the same code path.
	s.geoCacheMu.Lock()
	s.geoCacheList.MoveToFront(s.geoCache["A"])
	s.geoCacheMu.Unlock()

	// B should now be the LRU entry. Check by inspecting the back of the list.
	back := s.geoCacheList.Back().Value.(*geoCacheEntry).ip
	if back != "B" {
		t.Errorf("expected LRU to be B after promoting A, got %q", back)
	}
}

func TestGeoCacheConcurrentInsert(t *testing.T) {
	s := newTestServer()
	var wg sync.WaitGroup
	// 50 goroutines each insert 100 entries. With map+list under one mutex,
	// the result must be a consistent state with no panics.
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				insertEntry(s, formatIP(start*100+i), time.Hour)
			}
		}(g)
	}
	wg.Wait()
	if got := len(s.geoCache); got > geoCacheMaxSize {
		t.Errorf("cache exceeded max size: %d > %d", got, geoCacheMaxSize)
	}
	if s.geoCacheList.Len() != len(s.geoCache) {
		t.Errorf("list/map size mismatch: list=%d map=%d", s.geoCacheList.Len(), len(s.geoCache))
	}
}

func TestRateBucketRefill(t *testing.T) {
	s := &Server{geoRateBuckets: make(map[string]*ipRateBucket)}
	src := "1.2.3.4"
	// Drain the burst.
	for i := 0; i < int(geoRateBurst); i++ {
		if !s.allowGeoIP(src) {
			t.Fatalf("expected initial token %d to be granted", i)
		}
	}
	// Next call must be denied.
	if s.allowGeoIP(src) {
		t.Errorf("expected request beyond burst to be denied")
	}
	// Force refill by rewinding lastFill.
	s.geoRateMu.Lock()
	s.geoRateBuckets[src].lastFill = time.Now().Add(-1 * time.Second)
	s.geoRateMu.Unlock()
	if !s.allowGeoIP(src) {
		t.Errorf("expected request to be granted after 1s of refill")
	}
}

func formatIP(i int) string {
	// Compact unique key generator — not a real IP, just distinct strings.
	return "k-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
