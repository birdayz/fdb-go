package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// TypeFilterRedundantOverScanRule eliminates a LogicalTypeFilter
// whose record-type allow-set is a SUPERSET of (or equal to) the
// inner FullUnorderedScan's record-type set — the filter rejects
// nothing.
//
//	TypeFilter([A, B, C], Scan(A, B))   → Scan(A, B)
//	TypeFilter([A], Scan(A))            → Scan(A)
//	TypeFilter([B], Scan(A, B))         → no change (B is a strict subset)
//
// Why this matters in the seed: convertScan emits a single-record-type
// Scan, and an upstream `TypeFilter` is sometimes layered on by callers
// that don't know the scan was already narrowed to that type. Without
// this rule the planner would carry a redundant operator through to
// the physical phase, where the cost model would prefer the bare scan
// — but B4 cost isn't here yet, and even after B4 the rewrite is a
// pure simplification (no information lost).
//
// Java equivalent: handled implicitly by the cost preference for fewer
// operators. Seed implements it directly because the static rewrite is
// trivially safe and produces a concretely-simpler plan tree.
type TypeFilterRedundantOverScanRule struct {
	matcher matching.BindingMatcher
}

// NewTypeFilterRedundantOverScanRule constructs the rule.
func NewTypeFilterRedundantOverScanRule() *TypeFilterRedundantOverScanRule {
	return &TypeFilterRedundantOverScanRule{
		matcher: NewExpressionMatcher[*expressions.LogicalTypeFilterExpression]("logical_type_filter"),
	}
}

// Matcher returns the pattern.
func (r *TypeFilterRedundantOverScanRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// FullUnorderedScanExpression AND every type in the scan's set is
// allowed by the type-filter (i.e. scan ⊆ filter).
func (r *TypeFilterRedundantOverScanRule) OnMatch(call *ExpressionRuleCall) {
	tf := matching.Get[*expressions.LogicalTypeFilterExpression](call.Bindings, r.matcher)
	innerExpr := tf.GetInner().GetRangesOver().Get()
	scan, ok := innerExpr.(*expressions.FullUnorderedScanExpression)
	if !ok {
		return
	}
	if !typesAreSubset(scan.GetRecordTypes(), tf.GetRecordTypes()) {
		return
	}
	call.Yield(scan)
}

// typesAreSubset reports whether every name in `sub` appears in `super`.
// O(len(sub) * len(super)) — the type-name lists are small (handful of
// entries), so a hash-set isn't worth the alloc.
func typesAreSubset(sub, super []string) bool {
	for _, s := range sub {
		found := false
		for _, t := range super {
			if s == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*TypeFilterRedundantOverScanRule)(nil)
