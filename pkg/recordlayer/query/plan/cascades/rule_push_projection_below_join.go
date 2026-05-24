package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushProjectionBelowJoinRule pushes a projection below a join when
// the projected columns belong to one or both sides. This reduces
// the number of columns carried through the join.
//
//	Project([A.ID, A.NAME], Join(A, B, ON a.id = b.aid))
//	  → Join(Project([ID, NAME, ID], A), B, ON a.id = b.aid)
//
// More generally: for each projected FieldValue, determine which side
// of the join it belongs to via alias prefix. Build per-side
// projections including only the needed columns. Join predicate
// columns are always preserved — if the ON clause references a.id
// and b.aid, those columns survive even if the outer projection
// doesn't mention them.
//
// The rule only fires on inner joins with exactly 2 ForEach
// quantifiers. It only handles FieldValue projections (not computed
// expressions). If no columns can be pruned (every column from both
// sides is already projected), the rule does not fire.
//
// Soundness: pushing a projection below a join is safe for inner
// joins because the projection merely narrows the column set — it
// does not filter rows. The join predicate columns are preserved so
// the join condition remains evaluable.
type PushProjectionBelowJoinRule struct {
	matcher matching.BindingMatcher
}

// NewPushProjectionBelowJoinRule constructs the rule.
func NewPushProjectionBelowJoinRule() *PushProjectionBelowJoinRule {
	return &PushProjectionBelowJoinRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *PushProjectionBelowJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the projection sits above a join and can push
// column requirements to one or both sides.
func (r *PushProjectionBelowJoinRule) OnMatch(call *ExpressionRuleCall) {
	proj := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	innerExpr := proj.GetInner().GetRangesOver().Get()
	sel, ok := innerExpr.(*expressions.SelectExpression)
	if !ok {
		return
	}

	// Only fire on inner joins with exactly 2 ForEach quantifiers.
	if sel.GetJoinType() != expressions.JoinInner {
		return
	}
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) != 2 {
		return
	}
	if quantifiers[0].Kind() != expressions.QuantifierForEach ||
		quantifiers[1].Kind() != expressions.QuantifierForEach {
		return
	}

	aliases := sel.GetSourceAliases()
	if len(aliases) < 2 {
		return
	}

	// Guard against infinite regress: if either quantifier already
	// has a LogicalProjectionExpression in its Reference, a prior
	// firing already pushed projections. Don't fire again.
	for _, q := range quantifiers {
		for _, m := range q.GetRangesOver().AllMembers() {
			if _, ok := m.(*expressions.LogicalProjectionExpression); ok {
				return
			}
		}
	}

	projVals := proj.GetProjectedValues()
	if len(projVals) == 0 {
		return
	}

	upper0 := strings.ToUpper(aliases[0])
	upper1 := strings.ToUpper(aliases[1])

	// Classify each projected value by side. If any projected value
	// is not a FieldValue, bail out — we only handle simple column
	// references.
	needed0 := make(map[string]bool) // unqualified upper-case field names needed from side 0
	needed1 := make(map[string]bool) // unqualified upper-case field names needed from side 1
	for _, v := range projVals {
		fv, ok := v.(*values.FieldValue)
		if !ok {
			return // non-FieldValue projection — bail
		}
		fvAlias, col := fieldValueAliasAndCol(fv)
		switch {
		case fvAlias == upper0:
			needed0[col] = true
		case fvAlias == upper1:
			needed1[col] = true
		default:
			return // unrecognized alias — bail
		}
	}

	// Collect columns referenced by join predicates — these must
	// survive through the projection even if the outer projection
	// doesn't mention them.
	for _, pred := range sel.GetPredicates() {
		walkPredicateFieldValues(pred, func(fv *values.FieldValue) {
			fvAlias, col := fieldValueAliasAndCol(fv)
			if fvAlias == upper0 {
				needed0[col] = true
			}
			if fvAlias == upper1 {
				needed1[col] = true
			}
		})
	}

	// Count how many distinct columns each side actually has.
	// We need to determine whether pushing a projection actually
	// prunes anything. If both sides already carry only the needed
	// columns, there's nothing to push.
	//
	// We can't easily count the total columns available on each side
	// without schema info, but we can detect the degenerate case:
	// if one side has no needed columns at all, we skip projecting
	// that side (the join might still need its rows but not its
	// columns — though that would be odd). The real pruning check
	// is: did we identify columns for at least one side that are a
	// strict subset of what the side provides? Without schema, we
	// fire whenever at least one side has needed columns, and let
	// downstream ProjectionElimRule clean up no-ops.

	if len(needed0) == 0 && len(needed1) == 0 {
		return
	}

	// Build per-side projections, stripping the alias prefix.
	newQ0 := quantifiers[0]
	if len(needed0) > 0 {
		innerRef0 := quantifiers[0].GetRangesOver()
		vals0 := fieldValuesFromSet(needed0)
		pushed0 := expressions.NewLogicalProjectionExpression(vals0,
			expressions.ForEachQuantifier(innerRef0))
		newQ0 = expressions.ForEachQuantifier(call.MemoizeExpression(pushed0))
	}

	newQ1 := quantifiers[1]
	if len(needed1) > 0 {
		innerRef1 := quantifiers[1].GetRangesOver()
		vals1 := fieldValuesFromSet(needed1)
		pushed1 := expressions.NewLogicalProjectionExpression(vals1,
			expressions.ForEachQuantifier(innerRef1))
		newQ1 = expressions.ForEachQuantifier(call.MemoizeExpression(pushed1))
	}

	// Build the new SelectExpression with projected quantifiers.
	newSel := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{newQ0, newQ1},
		sel.GetPredicates(),
		aliases,
		sel.GetJoinType(),
	)

	// Wrap in the outer projection (the projection list is
	// unchanged — it still uses qualified names that resolve
	// against the join's row context).
	newSelQ := expressions.ForEachQuantifier(call.MemoizeExpression(newSel))
	call.Yield(expressions.NewLogicalProjectionExpressionWithAliases(
		proj.GetProjectedValues(),
		proj.GetAliases(),
		newSelQ,
	))
}

// fieldValuesFromSet builds a sorted slice of FieldValue from a set
// of unqualified upper-case field names. Deterministic ordering
// ensures structural equality of the rewritten tree.
func fieldValuesFromSet(fields map[string]bool) []values.Value {
	// Sort for deterministic output.
	sorted := make([]string, 0, len(fields))
	for f := range fields {
		sorted = append(sorted, f)
	}
	// Simple insertion sort — field sets are tiny.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	out := make([]values.Value, len(sorted))
	for i, f := range sorted {
		out[i] = &values.FieldValue{Field: f, Typ: values.UnknownType}
	}
	return out
}

var _ ExpressionRule = (*PushProjectionBelowJoinRule)(nil)
