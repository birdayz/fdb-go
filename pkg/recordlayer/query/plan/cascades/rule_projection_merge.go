package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ProjectionMergeRule collapses two nested LogicalProjections into one
// by COMPOSING the outer list through the inner:
//
//	Projection(P1) over Projection(P2) over X
//	→
//	Projection(P1 ∘ P2) over X
//
// Each outer slot that reads inner output k is substituted with P2[k]
// (baked-ordinal reads by ordinal; lazy flat names by unique
// output-name match). The pre-composition version kept P1 verbatim on
// the name-era contract that "the projection list is a side channel;
// rows pass through" — FALSE under the ordinal model, where P1's reads
// are baked against P2's OUTPUT row: dropping P2 reshaped the row
// underneath the baked ordinals (see OnMatch). A shape composition
// cannot prove declines the merge — both projections simply remain.
//
// This is more aggressive than Java's planner (which lets the cost
// model + cardinality estimates decide): no dedicated Java rule
// exists — the Memo's cost preference for fewer Projection operators
// emerges from physical plan choice (a fetch-only plan with the wider
// column list is cheaper than two stacked Projection operators). Go
// gets the same shape via this static rewrite.
type ProjectionMergeRule struct {
	matcher matching.BindingMatcher
}

// NewProjectionMergeRule constructs the rule.
func NewProjectionMergeRule() *ProjectionMergeRule {
	return &ProjectionMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *ProjectionMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over another
// LogicalProjection; yields a flat projection wrapping the inner's
// inner — with the outer's values COMPOSED through the inner's list.
//
// The original rule kept P1 verbatim, on the name-era contract that the
// projection list was "a side channel; rows pass through". Under the
// ordinal model that contract is FALSE: P1's values are BAKED by ordinal
// against the INNER projection's output row, so dropping the inner
// reshapes the row underneath the baked ordinals. `Project([Y#0],
// Project([COUNT(*)#1], StreamingAgg(keys=[COL1],…)))` merged to
// `Project([Y#0], StreamingAgg(…))` — reading the GROUP KEY instead of
// the count (SUM over a grouped UNION ALL branch returned key-sums;
// yamsql union_aggregate_java, RFC-180). Composition is the fix the
// rule's own caveat predicted: [#0]∘[#1] = [#1] — each outer slot k
// substitutes the inner's projected value at the ordinal it reads.
// A shape composition cannot prove (nested field path, unresolved lazy
// name, non-FieldValue outer slot) declines the merge — correctness over
// operator count; both projections simply remain.
func (r *ProjectionMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	innerProj, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return
	}
	innerVals := innerProj.GetProjectedValues()
	innerAliases := innerProj.GetAliases()
	if innerAliases != nil && len(innerAliases) != len(innerVals) {
		return // malformed alias list — decline rather than misname a slot
	}
	// Alias-preferring slot names — the SAME authority the executor's
	// runtime name resolution applies (values.OutputColumnName; its doc
	// records a prior hand-rolled copy of this rule causing an
	// OrdinalResolutionError). Matching bare ProjectionColumnName here
	// composed the WRONG COLUMN for `[a AS b, b AS c]` + outer lazy `b`:
	// runtime resolves `b` to the ALIAS slot (value a), the name-only match
	// picked value b.
	innerSlotName := func(j int) string {
		alias := ""
		if innerAliases != nil {
			alias = innerAliases[j]
		}
		return values.OutputColumnName(innerVals[j], alias)
	}
	outerVals := outer.GetProjectedValues()
	composed := make([]values.Value, len(outerVals))
	for i, v := range outerVals {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Child != nil {
			return // not provably composable — keep both projections
		}
		switch {
		case fv.Resolved != nil && len(fv.Resolved.Accessors) == 1:
			// BAKED read: substitute the inner's value at that output slot.
			ord := fv.Resolved.Accessors[0].Ordinal
			if ord < 0 || ord >= len(innerVals) || innerVals[ord] == nil {
				return
			}
			composed[i] = innerVals[ord]
		case fv.Resolved == nil:
			// LAZY flat name: compose by UNIQUE output-name match. Sound
			// because the composed value REPLACES the read outright — no
			// runtime name resolution survives the merge. Ambiguous or
			// missing → decline.
			match := -1
			for j, iv := range innerVals {
				if iv != nil && strings.EqualFold(innerSlotName(j), fv.Field) {
					if match >= 0 {
						return
					}
					match = j
				}
			}
			if match < 0 {
				return
			}
			composed[i] = innerVals[match]
		default:
			return
		}
	}
	// The OUTER's aliases name the merged output — the flat projection
	// replaces the outer, so its output schema must be identical.
	flat := expressions.NewLogicalProjectionExpressionWithAliases(
		composed,
		outer.GetAliases(),
		innerProj.GetInner(),
	)
	call.Yield(flat)
}

var _ ExpressionRule = (*ProjectionMergeRule)(nil)
