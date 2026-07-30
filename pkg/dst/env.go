package dst

import "time"

// Env bundles the three Tier-0 seams — Clock, Randomness, and Buggifier — for injection
// through the record and relational layers. Threading one Env keeps the seam surface small:
// a store, indexer, or session holds an *Env and reaches Clock/Random/Buggify off it.
//
// PRODUCTION IS A NIL *Env. There is deliberately no constructor for it. Every accessor
// (Now/Read/Fault/Coin) treats a nil Env — and a nil field within an Env — as wall clock,
// crypto/rand, and never-fire, so an unset env is byte-identical to the code before the seam.
// NewSim(seed) builds a fully deterministic environment for a simulation run.
//
// A "production Env" constructor existed and was dead, and its existence was actively harmful:
// several seam sites are deliberately ASYMMETRIC — they divert only when an env is present,
// because routing them through the nil-default would have CHANGED production bytes (the SPFresh
// builder token used math/rand, not crypto/rand; the SPFresh process nonce was minted once per
// process, not per call). Those sites test `env != nil`, so installing a hand-built
// "production" Env would have flipped them onto the simulation path while claiming to be
// production. With nil as the only spelling of production, `env != nil` means "a simulation
// environment is installed", which is exactly what those guards intend.
type Env struct {
	Clock   Clock
	Random  Randomness
	Buggify *Buggifier
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

// Coin flips the deterministic per-site coin (Buggifier.Coin) — the seam for a modelling
// choice between two equally-real behaviours, as opposed to Fault, which injects a failure.
// A nil Env has no seed and returns false; callers that must reach both branches without a
// seeded Env choose the branch explicitly instead.
func (e *Env) Coin(site string) bool {
	if e == nil {
		return false
	}
	return e.Buggify.Coin(site)
}
