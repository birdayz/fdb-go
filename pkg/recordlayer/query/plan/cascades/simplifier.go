package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// Simplifier — seed Phase 4.6 driver.
//
// Tiny fixed-point driver that applies a list of rules to a
// QueryPredicate until no rule yields. Not a real planner (no memo,
// no cost model, no task stack) — the Phase 4.6 CascadesPlanner
// replaces this with a memo-based driver. Seed exists so the
// simplification rules in DefaultSimplifyRules have a working
// end-to-end composition point, proving the full rule-driver loop
// works.

// Simplify iterates the rules on `pred` until no rule produces a
// rewrite, then returns the final form. Each iteration applies every
// rule in order; if ANY rule yields, the result replaces `pred` and
// the loop restarts from the top (fixpoint convergence). Descends
// into child predicates after the top-level stabilises.
//
// Termination invariants for the rule sets this driver runs:
//
//   - DefaultSimplifyRules — each rule strictly reduces the tree
//     (folds a constant or drops an identity / absorbed child), and
//     there's a finite number of constants / identities to collapse.
//   - NormalizationRules — adds DeMorganRule, which strictly
//     INCREASES the node count (NOT distributed across N children
//     adds N-1 NOTs). DeMorganRule terminates via NOT-depth monotone
//     decrease: each application moves every NOT one level closer to
//     leaves, leaves are finitely deep, leaves eventually hit
//     NotComparisonRewriteRule and disappear (or stop matching).
//
// Not safe against cyclic-rewrite rule sets — real Cascades uses a
// memo to detect cycles. The seed rule sets are termination-proven
// per above so no cycle is possible.
func Simplify(pred predicates.QueryPredicate, rules []CascadesRule) predicates.QueryPredicate {
	if pred == nil || len(rules) == 0 {
		return pred
	}
	// Top-level fixpoint.
	for {
		next := applyRulesOnce(pred, rules)
		if next == pred {
			break
		}
		pred = next
	}
	// Recurse into children. After a stable top-level, rewrite
	// sub-predicates and then re-simplify the top (child
	// simplifications may expose new top-level opportunities).
	switch p := pred.(type) {
	case *predicates.AndPredicate:
		rewritten := false
		simpler := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sp := range p.SubPredicates {
			simpler[i] = Simplify(sp, rules)
			if simpler[i] != sp {
				rewritten = true
			}
		}
		if rewritten {
			return Simplify(&predicates.AndPredicate{SubPredicates: simpler}, rules)
		}
	case *predicates.OrPredicate:
		rewritten := false
		simpler := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sp := range p.SubPredicates {
			simpler[i] = Simplify(sp, rules)
			if simpler[i] != sp {
				rewritten = true
			}
		}
		if rewritten {
			return Simplify(&predicates.OrPredicate{SubPredicates: simpler}, rules)
		}
	case *predicates.NotPredicate:
		if inner := Simplify(p.Child, rules); inner != p.Child {
			return Simplify(&predicates.NotPredicate{Child: inner}, rules)
		}
	}
	return pred
}

// applyRulesOnce fires each rule against pred exactly once, returning
// the first yielded replacement. When no rule fires, returns pred
// unchanged (the caller's fixpoint test uses pointer-equality).
func applyRulesOnce(pred predicates.QueryPredicate, rules []CascadesRule) predicates.QueryPredicate {
	for _, rule := range rules {
		matches := rule.Matcher().BindMatches(matching.NewBindings(), pred)
		for _, b := range matches {
			call := &RuleCall{Bindings: b}
			rule.OnMatch(call)
			if ys := call.Yielded(); len(ys) > 0 {
				// First yield wins — rules are ordered by priority.
				if qp, ok := ys[0].(predicates.QueryPredicate); ok {
					return qp
				}
			}
		}
	}
	return pred
}

// DefaultSimplifyRules returns the canonical simplification rule
// set this shift ships. Callers pass this to Simplify for a typical
// "flatten + constant-fold + identity-drop" pass. Order matters:
// flattens run first so the constant-fold rules see a flat operand
// list; then Comparison constants fold; then Not resolves; then the
// And/Or identity-drop + absorbing-element rules.
//
// Rules NOT included (intentional):
//   - De Morgan NOT-distribution (`NOT(AND(a,b))` → `OR(NOT a, NOT b)`).
//     Kleene-safe but NODE-INCREASING (N-ary AND becomes N-element
//     OR plus N NOTs), so it doesn't fit the strict-reduction
//     termination invariant DefaultSimplifyRules guarantees. Java
//     applies De Morgan as a separate `BooleanNormalizer` pre-CNF
//     pass; we mirror this via the explicit `NormalizationRules()`
//     rule set (which prepends `NewDeMorganRule()` before the
//     default set).
//   - Tautology / contradiction folds that require NOT-NULL
//     metadata (`x = x` → TRUE iff x is NOT NULL). Waits on Type
//     nullability tracking.
func DefaultSimplifyRules() []CascadesRule {
	return []CascadesRule{
		NewAndFlattenRule(),
		NewOrFlattenRule(),
		NewComparisonConstantSimplifyRule(),
		NewNotConstantSimplifyRule(),
		NewAndConstantSimplifyRule(),
		NewOrConstantSimplifyRule(),
		NewAndDedupRule(),
		NewOrDedupRule(),
		NewAndAbsorbOrRule(),
		NewOrAbsorbAndRule(),
		NewNotComparisonRewriteRule(),
		NewValuePredicateConstantFoldRule(),
	}
}
