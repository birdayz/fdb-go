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
// Why this is sound: the outer Distinct dedupes the FULL row stream,
// so any duplicate copies of the same row contributed by equivalent
// siblings get squashed. The optimization is to AVOID producing those
// duplicates in the first place. Result rows are identical.
//
// Why we don't apply this directly to bare LogicalUnion (without an
// outer Distinct): UNION ALL semantics PRESERVE duplicates. Removing
// equivalent siblings would silently change row counts.
//
// SemanticEquals here uses an empty AliasMap — two siblings are
// considered duplicates only if their structure matches WITHOUT any
// alias-aware translation. Stricter than Java (which uses the cost
// model + memo dedup); the seed errs on the side of conservative
// rewrites until the full memo lands in B3+.
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
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(newUnion))
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
