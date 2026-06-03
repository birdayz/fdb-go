package cascades

import (
	"bytes"
	"reflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// InComparisonToExplodeRule rewrites a LogicalFilterExpression whose
// predicate list contains a ComparisonPredicate with ComparisonIn.
//
// Single-element IN → simple equality (no union):
//
//	Filter([col IN (v1), ...other...], inner)
//	  →  Filter([col = v1, ...other...], inner)
//
// Multi-element IN → SelectExpression with ExplodeExpression:
//
//	Filter([col IN (v1, v2, v3), ...other...], inner)
//	  →  SelectExpression(
//	       resultValue = QOV(innerAlias),
//	       quantifiers = [
//	         ForEach(Filter([col = QOV(explodeAlias), ...other...], inner)),
//	         ForEach(Explode([v1, v2, v3])),
//	       ],
//	       predicates = [],
//	     )
//
// Mirrors Java's InComparisonToExplodeRule. The ImplementInJoinRule
// (PLANNING phase) handles this SelectExpression shape and produces
// InJoinPlan or InUnionPlan. The inner LogicalFilterExpression's
// equality predicate (col = QOV(explodeAlias)) is matched by the
// index-matching infrastructure, which creates an index scan with
// the column equality-bound to the explode alias. ImplementInJoinRule
// detects this correlation via the inner plan's RichOrdering.
//
// Guards:
//   - At least one ComparisonIn predicate.
//   - The IN-list Operand must evaluate (without row context) to a
//     non-empty []any.
//   - The filter must have an inner Quantifier (no bare filter).
type InComparisonToExplodeRule struct {
	matcher matching.BindingMatcher
}

func NewInComparisonToExplodeRule() *InComparisonToExplodeRule {
	return &InComparisonToExplodeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter_in_explode"),
	}
}

func (r *InComparisonToExplodeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *InComparisonToExplodeRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)

	// Idempotency guard: if this Reference already contains a
	// SelectExpression with an ExplodeExpression quantifier, the
	// multi-element IN has already been transformed. Skip to prevent
	// infinite memo growth from fresh-alias SelectExpressions.
	for _, m := range call.Reference.Members() {
		if sel, ok := m.(*expressions.SelectExpression); ok {
			for _, q := range sel.GetQuantifiers() {
				if ref := q.GetRangesOver(); ref != nil {
					if getExplodeExpression(ref) != nil {
						return
					}
				}
			}
		}
	}

	preds := f.GetPredicates()

	inIdx := -1
	var inPred *predicates.ComparisonPredicate
	for i, p := range preds {
		cp, ok := p.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if cp.Comparison.Type == predicates.ComparisonIn {
			inIdx = i
			inPred = cp
			break
		}
	}
	if inIdx < 0 {
		return
	}

	rhs := inPred.Comparison.Operand.Evaluate(nil)
	list, ok := rhs.([]any)
	if !ok || len(list) == 0 {
		return
	}

	// Dedupe the IN-list, mirroring Java's InComparisonToExplodeRule, which
	// wraps the value comparand in ArrayDistinctValue (the ValueComparison
	// branch). Without this, `col IN (1, 1, 1)` explodes to three Explode
	// iterations and the InJoin emits one duplicate row per repeated literal
	// (`a IN (1,1,1)` on a PK returned three copies of the same row). Done
	// before the single-element collapse below so `col IN (1, 1, 1)` reduces
	// to a plain `col = 1` equality. Order-preserving (first occurrence) to
	// match ArrayDistinctValue's distinct-not-sort semantics.
	list = distinctInListValues(list)

	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	otherPreds := make([]predicates.QueryPredicate, 0, len(preds)-1)
	for i, p := range preds {
		if i != inIdx {
			otherPreds = append(otherPreds, p)
		}
	}

	// Single-element IN → simple equality.
	if len(list) == 1 {
		eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, list[0])
		eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)
		newPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
		newPreds = append(newPreds, eqPred)
		newPreds = append(newPreds, otherPreds...)
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerRef.Get()))
		call.Yield(expressions.NewLogicalFilterExpression(newPreds, innerQ))
		return
	}

	// Multi-element IN → SelectExpression with ExplodeExpression.
	//
	// 1. Create ExplodeExpression wrapping the IN-list as a
	//    ConstantValue with ArrayType so ExplodeExpression.GetResultValue
	//    infers the correct element type.
	explodeValue := &values.ConstantValue{
		Value: list,
		Typ:   values.NewArrayType(false, values.UnknownType),
	}
	explodeExpr := expressions.NewExplodeExpression(explodeValue)
	explodeRef := call.MemoizeExpression(explodeExpr)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	// 2. Build the inner LogicalFilterExpression with the equality
	//    predicate (col = QOV(explodeAlias)) plus any other predicates.
	//    The equality RHS is a QuantifiedObjectValue referencing the
	//    explode quantifier — this correlation flows through the
	//    SelectExpression's CanCorrelate=true into the inner expression.
	explodedQOV := values.NewQuantifiedObjectValue(explodeQ.GetAlias())
	eqCmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: explodedQOV}
	eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)

	innerPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
	innerPreds = append(innerPreds, eqPred)
	innerPreds = append(innerPreds, otherPreds...)

	innerScanQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerRef.Get()))
	innerFilter := expressions.NewLogicalFilterExpression(innerPreds, innerScanQ)
	innerFilterRef := call.MemoizeExpression(innerFilter)
	innerFilterQ := expressions.ForEachQuantifier(innerFilterRef)

	// 3. Build a predicate-free SelectExpression with the inner and
	//    explode quantifiers. The resultValue is QOV(innerAlias) — the
	//    shape ImplementInJoinRule expects.
	resultValue := values.NewQuantifiedObjectValue(innerFilterQ.GetAlias())
	selectExpr := expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{innerFilterQ, explodeQ},
		nil, // no predicates — ImplementInJoinRule requires this
	)
	call.Yield(selectExpr)
}

// distinctInListValues returns in with duplicate elements removed,
// preserving first-occurrence order. Mirrors the runtime semantics of
// Java's ArrayDistinctValue applied to a constant IN-list. SQL value
// equality: []byte compares by content; other scalar literals by ==.
func distinctInListValues(in []any) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		dup := false
		for _, seen := range out {
			if inListValueEqual(seen, v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// inListValueEqual reports SQL value equality for two IN-list literals.
// It must never panic: an IN list can carry array / vector literals
// (`WHERE v IN ([1,0], [0,1])`) that fold to non-comparable slices
// ([]float64, []any, ...), so a bare `==` would crash planning. []byte
// (BYTES) and other non-comparable kinds compare structurally; comparable
// scalars (int64, float64, string, bool) use ==.
func inListValueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if ab, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		return ok && bytes.Equal(ab, bb)
	}
	if !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return reflect.DeepEqual(a, b)
	}
	return a == b
}

var _ ExpressionRule = (*InComparisonToExplodeRule)(nil)
