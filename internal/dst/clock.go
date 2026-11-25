// Package dst provides the deterministic-simulation-testing foundation (RFC-199 Tier 0):
// a Clock seam, a seeded randomness source, and FoundationDB-style Buggify fault points.
//
// The production defaults are drop-in and cost nothing measurable: RealClock reads the
// wall clock, CryptoRandomness delegates to crypto/rand, and a disabled Buggifier never
// fires. A simulation swaps in a SimClock advanced by the driver, a seeded PCG randomness
// source, and an enabled Buggifier over the same seed — so a single seed reproduces the
// run's time, randomness, and injected faults exactly.
//
// This is the shared foundation for both Track A (record + relational, via SimFDB) and
// Track B (client transport). It depends only on the standard library so every layer can
// import it without a cycle.
package dst

import (
	"sync"
	"time"
)

// Clock abstracts the passage of time so simulation code advances a logical clock
// deterministically instead of reading the wall clock. It mirrors FoundationDB's
// INetwork::now() seam (fdbrpc/sim2.actor.cpp), where in simulation time only advances
// when the ready queue drains.
//
// Only Now() is required for the record/relational (Track A) persisted-byte sites; the
// full timer surface (After/NewTimer/Sleep) belongs to Track B and is intentionally left
// off this interface until that work lands.
type Clock interface {
	// Now returns the current time. Callers that persist the result (store headers,
	// heartbeats, lease TTLs, SQL CURRENT_*) must route through here rather than
	// time.Now so a simulation run is byte-reproducible.
	Now() time.Time
}

// RealClock is the production Clock: it reads the wall clock via time.Now.
type RealClock struct{}

// Now returns the wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// SimClock is a deterministic Clock whose time only moves when the driver advances it —
// the analog of FDB Sim2's logical clock. It is safe for concurrent reads (the record
// layer's goroutine fan-out may read the clock from several goroutines); Advance/Set are
// driver-controlled and serialized by the same lock.
type SimClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewSimClock returns a SimClock pinned at start. A fixed, non-zero start (e.g. a known
// UTC instant) keeps persisted timestamps stable across runs.
func NewSimClock(start time.Time) *SimClock {
	return &SimClock{now: start}
}

// Now returns the current logical time.
func (c *SimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the logical clock forward by d and returns the new time. d must be
// non-negative; a negative delta would let persisted timestamps go backwards and is
// treated as a no-op.
func (c *SimClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
	}
	return c.now
}

// Set pins the logical clock to t. Used by the driver to jump to a chosen instant.
func (c *SimClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Since returns the time elapsed since t according to the clock — the seam-aware analog
// of time.Since. Kept as a free function so it works against any Clock.
func Since(c Clock, t time.Time) time.Duration { return c.Now().Sub(t) }

// Epoch is a stable, arbitrary non-zero start instant for simulation clocks: 2020-01-01
// UTC. Chosen to be recent enough that any code asserting "timestamp is after some
// sensible past date" holds, and fixed so persisted bytes are identical across runs.
var Epoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
