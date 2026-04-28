package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"

// FixpointApply fires each rule in `rules` against `ref` repeatedly
// until no rule yields a new (non-duplicate) member. Returns the
// number of rule fires that actually grew the Reference's member
// set — useful for tests that assert the optimiser made progress.
//
// This is a SEED-LEVEL rule engine. Phase 4.6's CascadesPlanner is the
// real task-stack-based driver (EXPLORE / OPTIMIZE / sub-tree
// memoisation, cost-driven extraction). FixpointApply lets the seed
// chain rules into multi-step rewrites without that machinery — handy
// for end-to-end rule tests that need (FilterMerge → NoOpFilter)
// composition or (UnionMerge → DistinctMerge) collapses.
//
// Termination is guaranteed by Reference.Insert's children-aware
// dedup contract: a rule that yields an expression matching an
// existing member's EqualsWithoutChildren AND sharing the same
// child Reference pointers gets absorbed without growing the set.
// The seed rules are deterministic and have a finite output for
// any given input expression shape, so after some bounded number
// of iterations every rule yields only duplicates and the loop
// exits.
//
// `maxIters` caps the loop — defaults to 100 if zero. Hard cap at
// 10_000 (sanity). Returning the cap-hit case as `(progress, false)`
// signals the caller that the loop exited via cap, not convergence.
func FixpointApply(rules []ExpressionRule, ref *expressions.Reference, maxIters int) (int, bool) {
	if maxIters <= 0 {
		maxIters = 100
	}
	if maxIters > 10000 {
		maxIters = 10000
	}
	progress := 0
	for iter := 0; iter < maxIters; iter++ {
		grewThisPass := false
		for _, rule := range rules {
			before := len(ref.Members())
			_ = FireExpressionRule(rule, ref)
			after := len(ref.Members())
			if after > before {
				grewThisPass = true
				progress += after - before
			}
		}
		if !grewThisPass {
			return progress, true // converged
		}
	}
	return progress, false // hit maxIters cap
}
