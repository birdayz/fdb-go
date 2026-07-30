package dst

import (
	"hash/fnv"
	mrand "math/rand/v2"
	"sync"
)

// FDB Buggify default probabilities (flow/flow.cpp:351-352:
// P_BUGGIFIED_SECTION_ACTIVATED / P_BUGGIFIED_SECTION_FIRES, both 0.25).
const (
	defaultActivationProb = 0.25
	defaultFireProb       = 0.25
	buggifyStream         = uint64(2) // PCG stream base, distinct from randStream
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
// Each site draws from its OWN generator, derived from (seed, site label) — the rand.go
// pattern of giving each consumer its own PCG stream off one seed, applied per site. A single
// shared generator would make the fault schedule an artifact of the ORDER and COUNT of site
// hits across the whole run: adding a fault point anywhere, or hitting an existing one one
// extra time, re-phases every later draw and silently changes what a seed injects. Then a
// "reproducer for seed N" stops reproducing the moment an unrelated site is added, which is
// the one property the whole seam exists to provide.
//
// Production uses DisabledBuggifier so a BUGGIFY point costs only a nil/bool check and never
// fires.
//
// FDB identifies a site by (__FILE__, __LINE__). Go has no such macros, so a site is
// identified by a caller-supplied stable string label (e.g. "simfdb.commit.conflict"); the
// label plays the exact role of the file:line pair. Use a distinct constant per site.
type Buggifier struct {
	mu             sync.Mutex
	seed           uint64
	enabled        bool
	activationProb float64
	fireProb       float64
	sites          map[string]*buggifySite
	fired          int // total fault firings this run — observability for DST hunters
}

// buggifySite is one call site's private state: its own generator plus the cached activation
// decision drawn from it.
type buggifySite struct {
	rng       *mrand.Rand
	activated bool
}

// NewBuggifier returns a Buggifier seeded by seed. When enabled is false it behaves like
// DisabledBuggifier for fault points (never fires) but still remembers the seed, so a run can
// be started disabled and enabled later without reseeding — and so Coin, which is not a fault
// gate, stays deterministic either way.
func NewBuggifier(seed uint64, enabled bool) *Buggifier {
	return &Buggifier{
		seed:           seed,
		enabled:        enabled,
		activationProb: defaultActivationProb,
		fireProb:       defaultFireProb,
		sites:          make(map[string]*buggifySite),
	}
}

// DisabledBuggifier returns a Buggifier that never fires faults — the production default. A
// nil *Buggifier behaves identically, so callers may hold a nil field.
func DisabledBuggifier() *Buggifier { return NewBuggifier(0, false) }

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
	s := b.siteLocked("fault:" + site)
	if !s.activated {
		return false
	}
	if s.rng.Float64() < prob {
		b.fired++
		return true
	}
	return false
}

// Coin returns a deterministic per-site coin flip (fair, 50/50).
//
// It is NOT a fault gate and deliberately ignores the enabled flag: a coin resolves a
// MODELLING choice the simulator has to make either way — which of two equally-real
// behaviours the simulated system exhibits this time — rather than injecting a failure that
// would otherwise not happen. The archetype is commit_unknown_result: the mutations either
// landed or did not, both outcomes are real FDB, and the sim must pick one and be consistent
// about it for the rest of the run.
//
// Like a fault site, a coin site draws from its own (seed, label)-derived generator, so
// coins at one site never re-phase another site's schedule. A nil Buggifier has no seed and
// so cannot flip: it returns false. Callers that need both branches without a seeded Env must
// choose the branch explicitly rather than rely on that value.
func (b *Buggifier) Coin(site string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.siteLocked("coin:"+site).rng.Float64() < 0.5
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

// siteLocked returns site's private state, creating it (and drawing its one-time activation
// decision) on first hit — getSBVar. Caller holds b.mu.
func (b *Buggifier) siteLocked(site string) *buggifySite {
	s, ok := b.sites[site]
	if ok {
		return s
	}
	s = &buggifySite{rng: mrand.New(mrand.NewPCG(b.seed, buggifyStream^siteHash(site)))}
	s.activated = s.rng.Float64() < b.activationProb
	if b.sites == nil {
		b.sites = make(map[string]*buggifySite)
	}
	b.sites[site] = s
	return s
}

// siteHash maps a site label to a PCG stream selector. FNV-1a 64: stable across runs and
// builds (unlike Go's map hash, which is per-process randomized), which is what makes "seed N
// reproduces" hold across processes.
func siteHash(site string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(site))
	return h.Sum64()
}

// SetProbabilities overrides the activation and firing probabilities (both in [0,1]). Used
// by a driver that wants faults denser or sparser than FDB's 0.25/0.25 default. A value
// outside [0,1] is clamped.
//
// Sites already hit keep the activation decision they were given; the new activation
// probability governs sites first hit afterwards. Call it before the run starts.
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
