package recordlayer

import (
	crand "crypto/rand"
	"io"
	"time"
)

// Clock supplies the current time for persisted timestamps and scan/build
// time budgets. A nil Clock uses the wall clock (time.Now).
type Clock interface {
	Now() time.Time
}

// Env bundles optional deterministic time and randomness seams for simulation.
//
// Production is a nil *Env. Every accessor (Now, Since, Read) treats a nil
// *Env — and a nil field within an Env — as wall clock (time.Now) and
// crypto/rand, so an unset env is byte-identical to production. Asymmetric
// simulation-only sites test `env != nil`.
type Env struct {
	Clock  Clock
	Random io.Reader
}

// Now returns the environment's current time, treating a nil Env or nil Clock
// as production (wall clock).
func (e *Env) Now() time.Time {
	if e == nil || e.Clock == nil {
		return time.Now()
	}
	return e.Clock.Now()
}

// Since returns the time elapsed since t on the environment's clock.
func (e *Env) Since(t time.Time) time.Duration {
	return e.Now().Sub(t)
}

// Read fills p from the environment's randomness source, treating a nil Env or
// nil Random as production (crypto/rand).
func (e *Env) Read(p []byte) (int, error) {
	if e == nil || e.Random == nil {
		return crand.Read(p)
	}
	return e.Random.Read(p)
}
