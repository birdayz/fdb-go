package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// DistinctOverUnionDedupRule removes structurally-equivalent siblings
// from a LogicalUnion that sits inside a LogicalDistinct.
//
//	Distinct(Union(A, B, A')) where A SemanticEquals A'
//	→
//	Distinct(Union(A, B))
//
// Soundness: the outer Distinct dedupes the FULL row stream, so any
// duplicate rows contributed by equivalent siblings get squashed.
// Producing them in the first place is wasted work. SQL-equivalent:
// DISTINCT(UNION ALL of equivalent operands) = DISTINCT(union of
// distinct ones).
//
// Why we don't apply this directly to bare LogicalUnion (without an
// outer Distinct): UNION ALL semantics PRESERVE duplicates. Removing
// equivalent siblings would silently change row counts.
//
// Termination: the rule yields a Distinct wrapping a Quantifier over
// a FRESH Reference holding the deduped Union. The fresh Reference
// would defeat the sameChildReferences pointer-identity dedup, but
// Reference.Insert's SemanticEquals fallback (post-680e664a) catches
// the structural equivalence and absorbs the second yield. Without
// that fallback, this rule non-terminates — the original revert in
// c789c211 documents the failure.
type DistinctOverUnionDedupRule struct {
	matcher matching.BindingMatcher
}

// NewDistinctOverUnionDedupRule constructs the rule.
func NewDistinctOverUnionDedupRule() *DistinctOverUnionDedupRule {
	return &DistinctOverUnionDedupRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct"),
	}
}

// Matcher returns the pattern.
func (r *DistinctOverUnionDedupRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner is a LogicalUnion with at least one
// pair of structurally-equivalent siblings.
func (r *DistinctOverUnionDedupRule) OnMatch(call *ExpressionRuleCall) {
	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerExpr := d.GetInner().GetRangesOver().Get()
	u, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		return
	}
	deduped, removed := dedupUnionChildren(u.GetQuantifiers())
	if !removed {
		return
	}
	newUnion := expressions.NewLogicalUnionExpression(deduped)
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(newUnion))
	call.Yield(expressions.NewLogicalDistinctExpression(innerQ))
}

// dedupUnionChildren returns a slice of `qs` where each Quantifier's
// inner expression appears at most once (under SemanticEquals with
// empty alias map). Returns the deduped slice + a boolean indicating
// whether any pruning happened.
func dedupUnionChildren(qs []expressions.Quantifier) ([]expressions.Quantifier, bool) {
	out := make([]expressions.Quantifier, 0, len(qs))
	removed := false
	for _, q := range qs {
		current := q.GetRangesOver().Get()
		isDup := false
		for _, kept := range out {
			if expressions.SemanticEquals(current, kept.GetRangesOver().Get(), expressions.EmptyAliasMap()) {
				isDup = true
				break
			}
		}
		if isDup {
			removed = true
			continue
		}
		out = append(out, q)
	}
	return out, removed
}

var _ ExpressionRule = (*DistinctOverUnionDedupRule)(nil)
