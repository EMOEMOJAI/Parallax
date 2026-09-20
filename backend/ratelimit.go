package main

import (
	"math"
	"time"
)

// Per-rate-key limits for anonymous ("public mode") traffic, plus the
// run-create quota that applies to public deployments.
//
// LOCKING: every piece of per-IP state lives in Server.rateEntries and is
// guarded by Server.rateMu. rateMu is always taken on its own — no other mutex
// (nodesMu, clientsMu, cmdOwnersMu, cmdNodesMu, geoRateMu, geoCacheMu,
// schedulesMu, …) may be held while it is locked, and nothing inside these
// critical sections takes another lock. That keeps it out of the server's lock
// ordering graph entirely, so it can never participate in a deadlock cycle.
//
// CAPACITY: the map is hard-capped at rateEntryMaxCount. Both the cap eviction
// and the idle GC refuse to drop an entry that still holds live connections —
// evicting one would hand a connected client's slot back and let a single
// source exceed maxPublicConnsPerKey by churning keys. Depleted buckets are
// retained until their tokens recover, so eviction cannot reset a quota.

const (
	// Concurrent /ws/client connections a single rate key may hold in public
	// mode. Authenticated sessions are unaffected.
	maxPublicConnsPerKey = 3

	// Command token bucket for public sessions: 10 per minute, burst 10.
	publicCmdBurst        = 10.0
	publicCmdRefillPerSec = 10.0 / 60.0

	// Run-create quota, applied only when PUBLIC_MODE=1: 10 per hour, burst 10.
	runCreateBurst        = 10.0
	runCreateRefillPerSec = 10.0 / 3600.0

	// Hard cap on tracked keys, and the idle age at which an entry is dropped.
	rateEntryMaxCount = 50000
	rateEntryIdleTTL  = 10 * time.Minute
)

// rateEntry is the per-rate-key state. lastFill doubles as the idle marker for
// the GC: it is bumped on every touch, so "no refill in rateEntryIdleTTL" means
// "no request in rateEntryIdleTTL".
type rateEntry struct {
	conns     int // live public WebSocket connections holding a slot
	cmdTokens float64
	runTokens float64
	lastFill  time.Time
}

// refillLocked tops both buckets up for the elapsed time. Caller holds rateMu.
func refillLocked(e *rateEntry, now time.Time) {
	elapsed := now.Sub(e.lastFill).Seconds()
	if elapsed <= 0 {
		return
	}
	e.cmdTokens = math.Min(publicCmdBurst, e.cmdTokens+elapsed*publicCmdRefillPerSec)
	e.runTokens = math.Min(runCreateBurst, e.runTokens+elapsed*runCreateRefillPerSec)
	e.lastFill = now
}

// rateEntryLocked returns the entry for key, creating it (full buckets) if it
// does not exist. Returns nil when the map is at capacity and every entry holds
// a live connection or an unfilled quota. Callers must fail closed rather than
// let an untracked request through. Caller holds rateMu.
func (s *Server) rateEntryLocked(key string, now time.Time) *rateEntry {
	if s.rateEntries == nil {
		s.rateEntries = make(map[string]*rateEntry)
	}
	if e, ok := s.rateEntries[key]; ok {
		refillLocked(e, now)
		return e
	}
	if len(s.rateEntries) >= rateEntryMaxCount && !s.evictOldestRateEntryLocked() {
		return nil
	}
	e := &rateEntry{cmdTokens: publicCmdBurst, runTokens: runCreateBurst, lastFill: now}
	s.rateEntries[key] = e
	return e
}

// rateEntryRecovered reports whether recreating this entry with full buckets
// would preserve its quotas. Do not refill in place: lastFill also tracks idle
// time, and inspecting an entry must not keep it alive. Caller holds rateMu.
func rateEntryRecovered(e *rateEntry, now time.Time) bool {
	elapsed := math.Max(0, now.Sub(e.lastFill).Seconds())
	return e.cmdTokens+elapsed*publicCmdRefillPerSec >= publicCmdBurst &&
		e.runTokens+elapsed*runCreateRefillPerSec >= runCreateBurst
}

// evictOldestRateEntryLocked drops the least recently used fully replenished
// entry without a live connection. Reports whether anything was removed.
// Caller holds rateMu.
func (s *Server) evictOldestRateEntryLocked() bool {
	var (
		oldestKey string
		oldest    time.Time
		found     bool
	)
	now := time.Now()
	for k, e := range s.rateEntries {
		if e.conns > 0 || !rateEntryRecovered(e, now) {
			continue // never reset an active slot or a depleted quota
		}
		if !found || e.lastFill.Before(oldest) {
			oldestKey, oldest, found = k, e.lastFill, true
		}
	}
	if !found {
		return false
	}
	delete(s.rateEntries, oldestKey)
	return true
}

// rateAcquireConn takes one public connection slot for key. Reports false when
// the key is already at maxPublicConnsPerKey (caller answers HTTP 429) or when
// no entry could be created. Must be called before the WebSocket upgrade.
func (s *Server) rateAcquireConn(key string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	e := s.rateEntryLocked(key, time.Now())
	if e == nil || e.conns >= maxPublicConnsPerKey {
		return false
	}
	e.conns++
	return true
}

// rateReleaseConn returns one slot. A missing entry is a no-op and the counter
// never goes negative, so double release (or release after a GC pass that
// somehow dropped the entry) is harmless.
func (s *Server) rateReleaseConn(key string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	e, ok := s.rateEntries[key]
	if !ok {
		return
	}
	if e.conns > 0 {
		e.conns--
	}
}

// rateAllowPublicCommand charges one token from the public command bucket.
func (s *Server) rateAllowPublicCommand(key string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	e := s.rateEntryLocked(key, time.Now())
	if e == nil || e.cmdTokens < 1 {
		return false
	}
	e.cmdTokens--
	return true
}

// rateAllowRunCreate charges one token from the run-create quota. Callers apply
// it only when publicModeEnabled(); a private deployment has no quota at all.
func (s *Server) rateAllowRunCreate(key string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	e := s.rateEntryLocked(key, time.Now())
	if e == nil || e.runTokens < 1 {
		return false
	}
	e.runTokens--
	return true
}

// rateGC drops idle entries only when their buckets have fully replenished.
// The hourly run quota can outlive the ten-minute idle threshold. Entries
// with live connections are always kept. Called outside every other lock.
func (s *Server) rateGC(now time.Time) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	for k, e := range s.rateEntries {
		if e.conns > 0 {
			continue
		}
		if now.Sub(e.lastFill) > rateEntryIdleTTL && rateEntryRecovered(e, now) {
			delete(s.rateEntries, k)
		}
	}
}

// rateEntryCount reports the tracked key count for the /metrics gauge.
func (s *Server) rateEntryCount() int {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	return len(s.rateEntries)
}

// rateConnCount reports the live public connection count for one key.
func (s *Server) rateConnCount(key string) int {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	e, ok := s.rateEntries[key]
	if !ok {
		return 0
	}
	return e.conns
}
