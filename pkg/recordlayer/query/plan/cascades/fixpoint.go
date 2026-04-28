package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"

// FixpointApply fires each rule in `rules` against `ref` AND every
// Reference reachable through `ref` repeatedly until no rule yields
// a new (non-duplicate) member anywhere in the tree. Returns the
// total number of rule fires that grew any Reference's member set
// — useful for tests that assert the optimiser made progress.
//
// This is a SEED-LEVEL rule engine. Phase 4.6's CascadesPlanner is the
// real task-stack-based driver (EXPLORE / OPTIMIZE / sub-tree
// memoisation, cost-driven extraction). FixpointApply lets the seed
// chain rules into multi-step rewrites without that machinery — handy
// for end-to-end rule tests that need (FilterMerge → NoOpFilter)
// composition or (UnionMerge → DistinctMerge) collapses across
// the whole expression tree, not just the top-level Reference.
//
// Termination is guaranteed by Reference.Insert's dedup contract
// (pointer-identity fast path + SemanticEquals fallback): a rule
// that yields an expression matching an existing member gets
// absorbed without growing the set. The seed rules are deterministic
// and have a finite output for any given input expression shape, so
// after some bounded number of iterations every rule yields only
// duplicates and the loop exits.
//
// Sub-Reference traversal: each iteration walks the tree from `ref`
// down through every member's Quantifiers' Reference and fires every
// rule on every reachable Reference. This lets a rule chain
// FilterMerge → NoOpFilter compose across a Quantifier boundary
// (e.g. inside a Sort's inner sub-tree) the way Java's task-stack
// planner does. Without this descent, rules only fire at the top
// level, and nested-operator chains don't fully optimise.
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
		// Collect all reachable References (including the root)
		// before firing rules — firing a rule can add members that
		// hold new sub-References, but we don't want to recurse into
		// THOSE on the same pass (avoids infinite descent during a
		// single iteration). Next iteration will pick them up.
		refs := collectReferences(ref)
		for _, r := range refs {
			for _, rule := range rules {
				before := len(r.Members())
				_ = FireExpressionRule(rule, r)
				after := len(r.Members())
				if after > before {
					grewThisPass = true
					progress += after - before
				}
			}
		}
		if !grewThisPass {
			return progress, true // converged
		}
	}
	return progress, false // hit maxIters cap
}

// collectReferences returns every Reference reachable from `root`,
// including `root` itself. Visits each Reference at most once per
// pointer (so a shared sub-Reference visited via two paths is
// returned once). Order is unspecified.
func collectReferences(root *expressions.Reference) []*expressions.Reference {
	if root == nil {
		return nil
	}
	visited := map[*expressions.Reference]struct{}{}
	var out []*expressions.Reference
	var walk func(r *expressions.Reference)
	walk = func(r *expressions.Reference) {
		if r == nil {
			return
		}
		if _, seen := visited[r]; seen {
			return
		}
		visited[r] = struct{}{}
		out = append(out, r)
		// Snapshot members at entry — the walk doesn't mutate, but
		// downstream rule fires on this Reference might add members.
		// Iterating over the snapshot avoids descending into newly-
		// added members during this collection.
		members := r.Members()
		for _, m := range members {
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver())
			}
		}
	}
	walk(root)
	return out
}
