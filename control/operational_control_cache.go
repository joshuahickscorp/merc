package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// Operational-control pause cache.
//
// Invariant that used to force a synchronous DB read on every request:
//
//	"the operator kill-switch must take effect before the next admission."
//
// That invariant is real for same-process admin mutations, and inherited
// (stronger than necessary) for multi-instance lag. The kill-switch is a
// coarse operator tool, not a per-credential security boundary: a few tens
// of milliseconds of additional admissions after pause is operationally
// fine and does not create a billing or auth hole.
//
// Propagation:
//  1. Same process: AdminSetOperationalControl invalidates the entry
//     immediately after a successful commit. Next read refetches.
//  2. Multi-instance: max lag is operationalControlCacheTTL (50 ms).
//  3. Missing / errored DB rows are not cached as "active"; fail-closed
//     paths still hit the database.
//
// Fail-closed: a cache miss or expired entry re-reads the database. A
// paused=true observation is cached the same as paused=false — the TTL
// bounds how long a *resume* also takes to propagate, which is symmetric
// and acceptable for an operator control.
const operationalControlCacheTTL = 50 * time.Millisecond

type operationalControlCache struct {
	mu      sync.Mutex
	entries map[string]operationalControlCacheEntry
	// generation bumps on any invalidation so concurrent readers that
	// loaded a pre-invalidation pointer still treat it as stale if we
	// ever store generation on the entry. Currently invalidation just
	// deletes; generation is retained for tests and future LISTEN/NOTIFY.
	generation atomic.Uint64
}

type operationalControlCacheEntry struct {
	paused    bool
	expiresAt time.Time
}

func (c *operationalControlCache) get(name string) (paused bool, ok bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return false, false
	}
	e, hit := c.entries[name]
	if !hit {
		return false, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, name)
		return false, false
	}
	return e.paused, true
}

func (c *operationalControlCache) put(name string, paused bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]operationalControlCacheEntry, 4)
	}
	c.entries[name] = operationalControlCacheEntry{
		paused:    paused,
		expiresAt: time.Now().Add(operationalControlCacheTTL),
	}
}

func (c *operationalControlCache) invalidate(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.entries != nil {
		delete(c.entries, name)
	}
	c.mu.Unlock()
	c.generation.Add(1)
}
