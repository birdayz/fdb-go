package dst

import (
	mrand "math/rand/v2"
	"sync"
)

// FDB Buggify default probabilities (flow/flow.cpp:351-352:
// P_BUGGIFIED_SECTION_ACTIVATED / P_BUGGIFIED_SECTION_FIRES, both 0.25).
const (
	defaultActivationProb = 0.25
	defaultFireProb       = 0.25
	buggifyStream         = uint64(2) // PCG stream, distinct from randStream
)

// Buggifier ports FoundationDB's getSBVar plus the BUGGIFY firing gate (flow/flow.cpp:356).
// Every call site has two independent seeded gates:
//
//   - activation: decided ONCE per site the first time it is hit, then cached — the site is
//     "active" with probability ActivationProb (FDB default 0.25). An inactive site never
//     fires for the rest of the run.
//   - firing: re-rolled on every hit of an active site — fires with probability FireProb
//     (FDB default 0.25) or a per-call probability.
//
// Both rolls draw from one seeded generator, so a given seed injects the same faults at the
// same hits every run. Production uses DisabledBuggifier so a BUGGIFY point costs only a
// nil/bool check and never fires.
//
// FDB identifies a site by (__FILE__, __LINE__). Go has no such macros, so a site is
// identified by a caller-supplied stable string label (e.g. "simfdb.commit.conflict"); the
// label plays the exact role of the file:line pair. Use a distinct constant per site.
type Buggifier struct {
	mu             sync.Mutex
	rng            *mrand.Rand
	enabled        bool
	activationProb float64
	fireProb       float64
	activated      map[string]bool
	fired          int // total fault firings this run — observability for DST hunters
}

// NewBuggifier returns a Buggifier seeded by seed. When enabled is false it behaves like
// DisabledBuggifier (never fires) but still remembers the seed, so a run can be started
// disabled and enabled later without reseeding.
func NewBuggifier(seed uint64, enabled bool) *Buggifier {
	return &Buggifier{
		rng:            mrand.New(mrand.NewPCG(seed, buggifyStream)),
		enabled:        enabled,
		activationProb: defaultActivationProb,
		fireProb:       defaultFireProb,
		activated:      make(map[string]bool),
	}
}

// DisabledBuggifier returns a Buggifier that never fires — the production default. A nil
// *Buggifier behaves identically, so callers may hold a nil field.
func DisabledBuggifier() *Buggifier { return &Buggifier{enabled: false} }

// Enabled reports whether this Buggifier can fire. A nil Buggifier is disabled.
func (b *Buggifier) Enabled() bool { return b != nil && b.enabled }

// Buggify reports whether the fault point identified by site should fire on this hit, using
// the default fire probability. Direct port of the BUGGIFY macro.
func (b *Buggifier) Buggify(site string) bool {
	if b == nil {
		return false
	}
	return b.BuggifyWithProb(site, b.fireProb)
}

// BuggifyWithProb is BUGGIFY_WITH_PROB: the cached activation gate AND a per-call fire roll
// at probability prob. Returns false immediately when disabled (no RNG draw), so disabled
// Buggify points are free and never perturb the seeded sequence.
func (b *Buggifier) BuggifyWithProb(site string, prob float64) bool {
	if b == nil || !b.enabled {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.activatedLocked(site) {
		return false
	}
	if b.rng.Float64() < prob {
		b.fired++
		return true
	}
	return false
}

// Fired returns how many fault points have fired this run. A brute-force hunter reads it to
// confirm a seed actually injected faults (a run with zero firings exercises only the happy
// path). Deterministic for a given seed. A nil or disabled Buggifier reports 0.
func (b *Buggifier) Fired() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fired
}

// activatedLocked returns whether site is active, deciding-and-caching on first hit
// (getSBVar). Caller holds b.mu.
func (b *Buggifier) activatedLocked(site string) bool {
	active, seen := b.activated[site]
	if !seen {
		active = b.rng.Float64() < b.activationProb
		b.activated[site] = active
	}
	return active
}

// SetProbabilities overrides the activation and firing probabilities (both in [0,1]). Used
// by a driver that wants faults denser or sparser than FDB's 0.25/0.25 default. A value
// outside [0,1] is clamped.
func (b *Buggifier) SetProbabilities(activation, fire float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activationProb = clamp01(activation)
	b.fireProb = clamp01(fire)
}

func clamp01(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}
