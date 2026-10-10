// Portions derived from FoundationDB Record Layer (
// InComparisonToExplodeRule.java, ArrayDistinctValue.java,
// SelectExpression.java, LogicalFilterExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"bytes"
	"math"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// InComparisonToExplodeRule rewrites an IN ComparisonPredicate. Over a
// SelectExpression it is Java's rule (see onSelect); over Go's WHERE carrier,
// a LogicalFilterExpression, it rewrites as follows.
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
		matcher: NewExpressionMatcher[expressions.RelationalExpressionWithPredicates]("in_explode").WithRootPredicate(
			func(e expressions.RelationalExpressionWithPredicates) bool {
				switch e.(type) {
				case *expressions.LogicalFilterExpression, *expressions.SelectExpression:
				default:
					return false
				}
				for _, pred := range e.GetPredicates() {
					if comparison, ok := pred.(*predicates.ComparisonPredicate); ok && comparison.Comparison.Type == predicates.ComparisonIn {
						return true
					}
				}
				return false
			},
		),
	}
}

func (r *InComparisonToExplodeRule) ConstraintDependencies() []any { return nil }

func (r *InComparisonToExplodeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *InComparisonToExplodeRule) OnMatch(call *ExpressionRuleCall) {
	switch e := matching.Get[expressions.RelationalExpressionWithPredicates](call.Bindings, r.matcher).(type) {
	case *expressions.LogicalFilterExpression:
		r.onFilter(call, e)
	case *expressions.SelectExpression:
		r.onSelect(call, e)
	}
}

// onSelect is Java's rule: every explodable IN of the select becomes an
// equality to a new Explode quantifier of the same select, whose result value
// is unchanged. SplitSelectExtractIndependentQuantifiersRule then lifts the
// explodes above the rest, so the IN join ranges over the select's own access
// and result (`INJOIN { ISCAN | MAP }`). A single-element list is an equality.
func (r *InComparisonToExplodeRule) onSelect(call *ExpressionRuleCall, sel *expressions.SelectExpression) {
	// An outer join select has a fixed binary shape and a strict carrier owns
	// its per-row contract; neither may gain a quantifier.
	if !sel.ChildrenAsSet() || hasStrictSingleQuantifier(sel.GetQuantifiers()) {
		return
	}
	if inExplodedAlready(call.Reference, sel) {
		return
	}
	if r.onBlockOverOneSource(call, sel) {
		return
	}
	quantifiers := append([]expressions.Quantifier(nil), sel.GetQuantifiers()...)
	var transformed []predicates.QueryPredicate
	changed := false
	for _, p := range sel.GetPredicates() {
		in, ok := explodableIn(p)
		if !ok {
			transformed = append(transformed, p)
			continue
		}
		if eq, single := in.singleEquality(); single {
			transformed = append(transformed, eq)
			changed = true
			continue
		}
		explodeExpr, ok, err := in.explodeExpression()
		if err != nil {
			call.Fail(err)
			return
		}
		if !ok {
			transformed = append(transformed, p)
			continue
		}
		explodeQ := expressions.ForEachQuantifier(call.MemoizeExpression(explodeExpr))
		equalities, ok, err := in.equalities(explodeQ)
		if err != nil {
			call.Fail(err)
			return
		}
		if !ok {
			transformed = append(transformed, p)
			continue
		}
		quantifiers = append(quantifiers, explodeQ)
		transformed = append(transformed, equalities...)
		changed = true
	}
	if !changed {
		return
	}
	var aliases []string
	if names := sel.GetSourceAliases(); len(names) == len(sel.GetQuantifiers()) {
		aliases = append([]string(nil), names...)
		for _, q := range quantifiers[len(names):] {
			aliases = append(aliases, q.GetAlias().Name())
		}
	}
	result, err := expressions.NewSelectExpressionWithJoinType(sel.GetResultValue(), quantifiers, transformed, aliases, sel.GetJoinType())
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(result)
}

// onBlockOverOneSource explodes the one non-singleton record IN of a block over
// a single source into the shape the filter arm builds: the equalities filter
// the source, correlated to the explode, under a select of no predicates, which
// lowers to a FlatMap probing the source per exploded row. A record explode is a
// relation source to Go's join rules, so the general form would only join it
// materialized; a scalar IN keeps the general form, which the IN-join rules
// implement. The filter's quantifier keeps the source's name and row, so the
// block's result value reads it unchanged.
func (r *InComparisonToExplodeRule) onBlockOverOneSource(call *ExpressionRuleCall, sel *expressions.SelectExpression) bool {
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) != 1 || quantifiers[0].Kind() != expressions.QuantifierForEach || quantifiers[0].IsNullOnEmpty() {
		return false
	}
	source := quantifiers[0]
	var in inExplosion
	inIdx := -1
	for i, p := range sel.GetPredicates() {
		candidate, ok := explodableIn(p)
		if !ok {
			continue
		}
		if _, single := candidate.singleEquality(); single || inIdx >= 0 || !values.IsRecord(candidate.pred.Operand.Type()) {
			return false
		}
		in, inIdx = candidate, i
	}
	if inIdx < 0 {
		return false
	}
	explodeExpr, ok, err := in.explodeExpression()
	if err != nil {
		call.Fail(err)
		return true
	}
	if !ok {
		return false
	}
	explodeQ := expressions.ForEachQuantifier(call.MemoizeExpression(explodeExpr))
	equalities, ok, err := in.equalities(explodeQ)
	if err != nil {
		call.Fail(err)
		return true
	}
	if !ok {
		return false
	}
	filterPreds := append([]predicates.QueryPredicate(nil), equalities...)
	for i, p := range sel.GetPredicates() {
		if i != inIdx {
			filterPreds = append(filterPreds, p)
		}
	}
	filter, err := expressions.NewLogicalFilterExpression(filterPreds, source)
	if err != nil {
		call.Fail(err)
		return true
	}
	filterQ := expressions.NamedForEachQuantifier(source.GetAlias(), call.MemoizeExpression(filter))
	block, err := expressions.NewSelectExpression(sel.GetResultValue(), []expressions.Quantifier{filterQ, explodeQ}, nil)
	if err != nil {
		call.Fail(err)
		return true
	}
	call.Yield(block)
	return true
}

// inExplodedAlready reports whether ref already holds sel with its INs
// exploded: a select owning all of sel's quantifiers plus an Explode. The
// explode quantifier is minted fresh, so memo dedup cannot catch a re-firing.
func inExplodedAlready(ref *expressions.Reference, sel *expressions.SelectExpression) bool {
	own := make(map[values.CorrelationIdentifier]struct{}, len(sel.GetQuantifiers()))
	for _, q := range sel.GetQuantifiers() {
		own[q.GetAlias()] = struct{}{}
	}
	for _, m := range ref.AllMembers() {
		other, ok := m.(*expressions.SelectExpression)
		if !ok || other == sel || len(other.GetQuantifiers()) <= len(own) {
			continue
		}
		shared, explodes := 0, 0
		for _, q := range other.GetQuantifiers() {
			if _, mine := own[q.GetAlias()]; mine {
				shared++
			} else if q.GetRangesOver() != nil && getExplodeExpression(q.GetRangesOver()) != nil {
				explodes++
			}
		}
		if shared == len(own) && explodes > 0 && shared+explodes == len(other.GetQuantifiers()) {
			return true
		}
	}
	return false
}

func (r *InComparisonToExplodeRule) onFilter(call *ExpressionRuleCall, f *expressions.LogicalFilterExpression) {
	// Idempotency guard: if this Reference already contains a
	// SelectExpression with an ExplodeExpression quantifier, the
	// multi-element IN has already been transformed. Skip to prevent
	// infinite memo growth from fresh-alias SelectExpressions.
	for _, m := range call.Reference.AllMembers() {
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

	// Only the FIRST IN is considered; the rewritten filter carries the rest,
	// and the rule fires on it in turn.
	inIdx := -1
	for i, p := range preds {
		if cp, ok := p.(*predicates.ComparisonPredicate); ok && cp.Comparison.Type == predicates.ComparisonIn {
			inIdx = i
			break
		}
	}
	if inIdx < 0 {
		return
	}
	in, ok := explodableIn(preds[inIdx])
	if !ok {
		return
	}

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
	if eqPred, single := in.singleEquality(); single {
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

	// Multi-element IN → SelectExpression with ExplodeExpression, whose
	// inner LogicalFilterExpression carries the equality to the explode
	// (col = QOV(explodeAlias)) plus the other predicates. The correlation
	// flows through the SelectExpression's CanCorrelate=true into the inner.
	explodeExpr, ok, err := in.explodeExpression()
	if err != nil {
		call.Fail(err)
		return
	}
	if !ok {
		return
	}
	explodeRef := call.MemoizeExpression(explodeExpr)
	explodeQ := expressions.ForEachQuantifier(explodeRef)
	equalities, ok, err := in.equalities(explodeQ)
	if err != nil {
		call.Fail(err)
		return
	}
	if !ok {
		return
	}
	innerPreds := append(equalities, otherPreds...)

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

// inExplosion is an IN predicate whose list can drive an Explode.
type inExplosion struct {
	pred         *predicates.ComparisonPredicate
	list         []any
	runtimeArray bool
}

// explodableIn reports whether p is an IN whose list an Explode can range over.
func explodableIn(p predicates.QueryPredicate) (inExplosion, bool) {
	inPred, ok := p.(*predicates.ComparisonPredicate)
	if !ok || inPred.Comparison.Type != predicates.ComparisonIn {
		return inExplosion{}, false
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
	// parameter arm for exactly them. Go's driver binds statement parameters
	// before planning, so `x IN (?, 999)` and `x IN ?` reach this rule as
	// literal lists and explode (measured, 2026-10-07: InJoin over the index);
	// no SQL reaches it with an unbound parameter.
	if !values.IsConstantValue(inPred.Comparison.Operand) {
		return inExplosion{}, false
	}
	// Plan-time IN-list extraction. A comparand that fails to evaluate (a
	// NULL element, `1 / 0`) is exploded as it is, Java's
	// arrayDistinct(comparand): the explode raises when the plan opens.
	rhs, err := inPred.Comparison.Operand.Evaluate(nil)
	runtimeArray := err != nil || values.IsRecord(inPred.Operand.Type())
	list, ok := rhs.([]any)
	if !runtimeArray && (!ok || len(list) == 0) {
		return inExplosion{}, false
	}
	// Dedupe the IN-list, mirroring Java's InComparisonToExplodeRule, which
	// wraps the value comparand in ArrayDistinctValue (the ValueComparison
	// branch). Without this, `col IN (1, 1, 1)` explodes to three Explode
	// iterations and the InJoin emits one duplicate row per repeated literal
	// (`a IN (1,1,1)` on a PK returned three copies of the same row). Done
	// before the single-element collapse so `col IN (1, 1, 1)` reduces
	// to a plain `col = 1` equality. Order-preserving (first occurrence) to
	// match ArrayDistinctValue's distinct-not-sort semantics.
	list = distinctInListValues(list)
	// An index cannot probe a NaN (stored NaNs pack by payload), so a list
	// holding one stays a residual filter.
	if inListHasNaN(list) {
		return inExplosion{}, false
	}
	return inExplosion{pred: inPred, list: list, runtimeArray: runtimeArray}, true
}

// singleEquality is a one-element list as the equality it means.
func (in inExplosion) singleEquality() (predicates.QueryPredicate, bool) {
	if in.runtimeArray || len(in.list) != 1 {
		return nil, false
	}
	eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, in.list[0])
	return predicates.NewComparisonPredicate(in.pred.Operand, eqCmp), true
}

// explodeExpression is the Explode over the list. ok=false when the probe has
// no exact element type yet: the rewrite is an optional normalization, and an
// Unknown-typed explode/QOV pair must not be published.
func (in inExplosion) explodeExpression() (*expressions.ExplodeExpression, bool, error) {
	elementType, ok := exactInExplodeElementType(in.pred, in.list)
	if !ok {
		return nil, false, nil
	}
	var explodeValue values.Value = &values.ConstantValue{
		Value: in.list,
		Typ:   values.NewArrayType(false, elementType),
	}
	// A row-independent array that fails to evaluate (a NULL element: 0A000;
	// `1 / 0`: 22012) is exploded as it is, as Java's rule explodes
	// arrayDistinct(comparand) with no constancy check: the explode
	// evaluates it when the plan opens and fails there, even over an empty
	// table.
	if in.runtimeArray {
		explodeValue = values.NewArrayDistinctValue(in.pred.Comparison.Operand)
	}
	explode, err := expressions.NewExplodeExpression(explodeValue)
	if err != nil {
		return nil, false, err
	}
	return explode, true, nil
}

// equalities binds the probe to each exploded element. Java deconstructs record
// membership into positional scalar equalities, making each constituent
// available to compound index matching.
func (in inExplosion) equalities(explodeQ expressions.Quantifier) ([]predicates.QueryPredicate, bool, error) {
	explodedQOV, err := explodeQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, false, err
	}
	if !values.IsRecord(explodedQOV.FlowedType()) {
		eqCmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: explodedQOV}
		return []predicates.QueryPredicate{predicates.NewComparisonPredicate(in.pred.Operand, eqCmp)}, true, nil
	}
	probeFields, err := values.DeconstructRecord(in.pred.Operand)
	if err != nil {
		return nil, false, err
	}
	listFields, err := values.DeconstructRecord(explodedQOV)
	if err != nil {
		return nil, false, err
	}
	if len(probeFields) != len(listFields) {
		return nil, false, nil
	}
	equalities := make([]predicates.QueryPredicate, len(probeFields))
	for i, field := range probeFields {
		equalities[i] = predicates.NewComparisonPredicate(field, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: listFields[i]})
	}
	return equalities, true, nil
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

func inListHasNaN(list []any) bool {
	for _, v := range list {
		switch f := v.(type) {
		case float64:
			if math.IsNaN(f) {
				return true
			}
		case float32:
			if f != f {
				return true
			}
		}
	}
	return false
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
