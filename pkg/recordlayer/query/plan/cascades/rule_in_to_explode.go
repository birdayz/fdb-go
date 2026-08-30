package cascades

import (
	"bytes"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
//   - The IN-list Operand must be PLAN-TIME CONSTANT. Not merely
//     "evaluates without a row context": a list holding a column
//     reference evaluates to a []any with a NULL in it, because
//     fieldValue.Evaluate(nil) answers (nil, nil) rather than an error.
//     See the comment at the check itself for why that distinction is
//     load-bearing.
//   - Being constant, it must then evaluate to a non-empty []any.
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

	// The comparand must be PLAN-TIME CONSTANT, and that is a stronger
	// question than "does it evaluate without a row".
	//
	// An IN list may hold a column — `b IN (a, 999)` — and then its values are
	// not known until the row is read. Such a list resolves to an
	// ArrayConstructorValue over the item Values, and folding it here would ask
	// each item to evaluate with a nil context: fieldValue.Evaluate(nil)
	// answers (nil, nil), NOT an error, so the fold below would succeed and
	// yield [NULL, 999]. This rule would then explode over that NULL and the
	// query would plan, run, and silently return the rows of
	// `b IN (NULL, 999)` — a wrong answer with no error anywhere.
	//
	// IsConstantValue recurses through children, so it cannot be fooled by a
	// composite holding a column reference, and it is the only check here that
	// separates "no value yet" from "the value is NULL". A non-constant IN
	// stays a residual filter.
	//
	// THIS GUARD IS SOUND BUT NARROWER THAN THE PROPERTY IT STANDS FOR, and the
	// difference is worth stating rather than discovering. What makes an IN list
	// explodeable is ROW-INDEPENDENCE — Java's test is correlation to the inner
	// quantifier — not plan-time constancy. IsConstantValue answers false for
	// ParameterValue, ConstantObjectValue and ParameterObjectValue, all of which
	// are row-independent and would explode safely; Java has a dedicated
	// parameter arm for exactly them. So `x IN (?, 999)` is correct here but
	// planned as a residual filter, and for THAT shape an index probe genuinely
	// is lost. For the column case — the one this guard was added for — there
	// was no probe to lose, since ComparisonIn is not scan-range compatible.
	// Widening the predicate to row-independence is tracked in TODO.md.
	if !values.IsConstantValue(inPred.Comparison.Operand) {
		return
	}
	// Plan-time IN-list extraction: an erroring or non-list comparand
	// declines to transform (returns) rather than failing planning.
	rhs, err := inPred.Comparison.Operand.Evaluate(nil)
	if err != nil {
		return
	}
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
	//
	// The inner quantifier is REUSED, not re-minted. This rewrite changes only
	// the predicate list; the expression it yields is an alternative in the
	// SAME memo group as `f`, and a LogicalFilterExpression's result value is a
	// QOV over its inner quantifier's alias. Minting a fresh quantifier here
	// therefore published an alternative whose RESULT CORRELATION differed from
	// the group's other members, so a correlation held from OUTSIDE the group
	// resolved against an alias this alternative does not carry, and the
	// executor failed with `exact QOV "q$N" ... has no declared runtime
	// binding`. Planning succeeded; only execution failed, which is why a
	// plan-only probe of every affected shape comes back clean.
	//
	// The shapes that reached it, enumerated rather than characterised, since
	// the characterisation is what was wrong before: over the 24-arm cross of
	// LEFT/RIGHT/INNER x predicate on the left/right relation x indexed/
	// unindexed column x duplicate/distinct IN list, exactly three failed —
	// LEFT with an indexed column, RIGHT with an indexed column, and RIGHT with
	// an UNINDEXED one, all reading the join clause's RIGHT-HAND relation with
	// a duplicate IN list. NOT the null-padded side: under RIGHT JOIN that
	// relation is the PRESERVED one. NOT always indexed either. See
	// TestFDB_QOVBindingMinimalShape.
	//
	// The multi-element path below is free to mint quantifiers precisely
	// because it does NOT yield them bare: it wraps them in a SelectExpression
	// whose own result value re-exports the inner filter, so the new aliases
	// stay encapsulated. FilterDropTruePredicatesRule is the closer analogue to
	// this branch — same inner, rewritten predicates — and it likewise reuses
	// f.GetInner().
	//
	// The predicates already reference f.GetInner()'s alias, so no rebase is
	// needed either; the rebase existed only to follow the mint.
	if len(list) == 1 {
		eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, list[0])
		eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)
		newPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
		newPreds = append(newPreds, eqPred)
		newPreds = append(newPreds, otherPreds...)
		filter, err := expressions.NewLogicalFilterExpression(newPreds, f.GetInner())
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(filter)
		return
	}

	// Multi-element IN → SelectExpression with ExplodeExpression.
	//
	// 1. Create ExplodeExpression wrapping the IN-list as a
	//    ConstantValue with ArrayType so ExplodeExpression.GetResultValue
	//    infers the correct element type.
	elementType, ok := exactInExplodeElementType(inPred, list)
	if !ok {
		// This is an optional normalization. If semantic analysis has not
		// established an exact scalar type for the LHS yet, retain the original
		// IN predicate instead of publishing an Unknown-typed explode/QOV pair.
		return
	}
	explodeValue := &values.ConstantValue{
		Value: list,
		Typ:   values.NewArrayType(false, elementType),
	}
	explodeExpr, err := expressions.NewExplodeExpression(explodeValue)
	if err != nil {
		call.Fail(err)
		return
	}
	explodeRef := call.MemoizeExpression(explodeExpr)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	// 2. Build the inner LogicalFilterExpression with the equality
	//    predicate (col = QOV(explodeAlias)) plus any other predicates.
	//    The equality RHS is a QuantifiedObjectValue referencing the
	//    explode quantifier — this correlation flows through the
	//    SelectExpression's CanCorrelate=true into the inner expression.
	explodedQOV, err := explodeQ.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	eqCmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: explodedQOV}
	eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)

	innerPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
	innerPreds = append(innerPreds, eqPred)
	innerPreds = append(innerPreds, otherPreds...)

	// The BOUND inner quantifier is reused, not re-memoized.
	//
	// Reference.Get() returns the FIRST member only — it is the convenience
	// accessor for single-member references, and explored multi-member
	// references are meant to be iterated with Members/AllMembers. So
	// `MemoizeExpression(innerRef.Get())` published a COPY of the inner group
	// holding exactly one of its alternatives and dropped every other one the
	// inner had accumulated.
	//
	// That is not a wrong answer — it is a silently NARROWED SEARCH SPACE, and
	// the corpus recorded the consequence without anyone reading it: restoring
	// the alternatives moves 18 plan-shape headers across 5 committed *_in__*
	// family files, which means those scenarios had been blessing plans WORSE
	// than the ones the planner can now reach. Rows are unchanged. The
	// transition is carried by a retirement ledger under
	// factorycorpus/retirements/.
	//
	// The vendored-corpus golden moves by exactly ONE query, and it shows the
	// shape of the improvement:
	//
	//	Fetch(PredicatesFilter(IndexScan(IDX_REGION_PLAN, [=, *] COVERING)))
	//	Fetch(InJoin(PredicatesFilter(IndexScan(...COVERING)), binding))
	//
	// The IN list now drives an index probe instead of sitting as a residual
	// filter. Note for anyone re-running that gate: its failure prints a
	// POSITIONAL line count ("10126 line(s) differ"), which one inserted line
	// inflates to most of the file. Diff the regenerated golden before believing
	// the number.
	//
	// Java does not do this: InComparisonToExplodeRule re-adds the bound inner
	// quantifiers verbatim (transformedQuantifiers.addAll(bindings.getAll(
	// innerQuantifierMatcher))) and mints exactly one quantifier, over the
	// ExplodeExpression. Reusing f.GetInner() is that behaviour.
	//
	// The rebase went with the mint: the predicates already carry
	// f.GetInner()'s alias, so source == target made it a no-op copy.
	innerFilter, err := expressions.NewLogicalFilterExpression(innerPreds, f.GetInner())
	if err != nil {
		call.Fail(err)
		return
	}
	innerFilterRef := call.MemoizeExpression(innerFilter)
	innerFilterQ := expressions.ForEachQuantifier(innerFilterRef)

	// 3. Build a predicate-free SelectExpression with the inner and
	//    explode quantifiers. The resultValue is QOV(innerAlias) — the
	//    shape ImplementInJoinRule expects.
	resultValue, err := innerFilterQ.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	selectExpr, err := expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{innerFilterQ, explodeQ},
		nil, // no predicates — ImplementInJoinRule requires this
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(selectExpr)
}

// exactInExplodeElementType chooses the exact type carried by each explode
// row. Prefer an exact array element type on the IN comparand when one is
// available. ResolveIn currently stores the evaluated list with an unstated
// type, so the exactly-resolved LHS is the compatibility-checked authority in
// that shape. A NULL list member makes the element carrier nullable.
func exactInExplodeElementType(
	inPred *predicates.ComparisonPredicate,
	list []any,
) (values.Type, bool) {
	if inPred == nil {
		return nil, false
	}

	var elementType values.Type
	if inPred.Comparison.Operand != nil {
		if arrayType, ok := inPred.Comparison.Operand.Type().(*values.ArrayType); ok && arrayType != nil {
			elementType = arrayType.ElementType
		}
	}
	if elementType == nil && inPred.Operand != nil {
		elementType = inPred.Operand.Type()
	}
	if elementType == nil {
		return nil, false
	}
	if _, err := values.SnapshotExactType(elementType); err != nil {
		return nil, false
	}

	nullable := elementType.IsNullable()
	for _, item := range list {
		if item == nil {
			nullable = true
			break
		}
	}
	elementType = values.WithNullability(elementType, nullable)
	if _, err := values.SnapshotExactType(elementType); err != nil {
		return nil, false
	}
	return elementType, true
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
