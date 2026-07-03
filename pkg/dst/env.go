package dst

import "time"

// Env bundles the three Tier-0 seams — Clock, Randomness, and Buggifier — for injection
// through the record and relational layers. Threading one Env keeps the seam surface small:
// a store, indexer, or session holds an *Env and reaches Clock/Random/Buggify off it.
//
// Production() is the drop-in default (wall clock, crypto/rand, buggify off) and is what
// every non-simulation code path uses. NewSim(seed) builds a fully deterministic environment
// for a simulation run. A nil *Env means "production" — use Env.orProduction accessors so a
// nil field never panics.
type Env struct {
	Clock   Clock
	Random  Randomness
	Buggify *Buggifier
}

// Production returns the drop-in default environment: wall clock, crypto/rand, buggify off.
func Production() *Env {
	return &Env{
		Clock:   RealClock{},
		Random:  CryptoRandomness{},
		Buggify: DisabledBuggifier(),
	}
}

// NewSim returns a fully deterministic environment seeded by seed, with its logical clock
// pinned at Epoch. All three seams share the one seed (via distinct PCG streams), so the
// run is reproducible from seed alone.
func NewSim(seed uint64) *Env {
	return &Env{
		Clock:   NewSimClock(Epoch),
		Random:  NewSeededRandomness(seed),
		Buggify: NewBuggifier(seed, true),
	}
}

// Now returns the environment's current time, treating a nil Env or nil Clock as production
// (wall clock). Convenience for call sites that hold a possibly-nil *Env.
func (e *Env) Now() time.Time {
	if e == nil || e.Clock == nil {
		return time.Now()
	}
	return e.Clock.Now()
}

// Read fills p from the environment's randomness source, treating a nil Env or nil Random as
// production (crypto/rand).
func (e *Env) Read(p []byte) (int, error) {
	if e == nil || e.Random == nil {
		return CryptoRandomness{}.Read(p)
	}
	return e.Random.Read(p)
}

// Fault reports whether the fault point site should fire, treating a nil Env or nil Buggify
// as production (never fires).
func (e *Env) Fault(site string) bool {
	if e == nil {
		return false
	}
	return e.Buggify.Buggify(site)
}
