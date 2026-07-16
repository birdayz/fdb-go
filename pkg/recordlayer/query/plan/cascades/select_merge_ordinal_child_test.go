package cascades

// Pins proving the SelectMerge ordinal-child guard actually FIRES — an
// unpinned tripwire that looks healthy is worse than none: if the guard or
// its ContainsBakedOrdinal probe had a walk bug, it would sit dead. The pins
// hand-construct the violating shapes.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// bakedLegRC builds a 2-way-ordinal-seed-shaped result value: an RC whose
// fields are BAKED FieldValue.ofOrdinalNumber references over one leg QOV —
// the ordinal join seed shape in miniature.
func bakedLegRC(t *testing.T, corr values.CorrelationIdentifier) *values.RecordConstructorValue {
	t.Helper()
	legType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qov := values.NewQuantifiedObjectValueOfType(corr, legType)
	baked0, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	baked1, err := values.NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	return values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: baked0},
		values.RecordConstructorField{Name: "V", Value: baked1},
	)
}

// ordinalChildSelect builds an inner-equivalent child SelectExpression whose
// result value is BAKED (an ordinal 2-way select as SelectMergeRule sees it).
func ordinalChildSelect(t *testing.T) *expressions.SelectExpression {
	t.Helper()
	scan := &expressions.FullUnorderedScanExpression{}
	q := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("leg"), expressions.InitialOf(scan),
	)
	return expressions.NewSelectExpression(
		bakedLegRC(t, values.NamedCorrelationIdentifier("leg")),
		[]expressions.Quantifier{q},
		nil,
	)
}

// TestSelectMergeRule_OrdinalChildComposes pins the SelectMerge composition
// behavior for an ORDINAL child: merging into a multi-quantifier parent is
// legitimate composition (no panic), and the single-quantifier pure-wrapper
// merge of the same child proceeds silently.
func TestSelectMergeRule_OrdinalChildComposes(t *testing.T) {
	t.Parallel()

	// POSITIVE pin: an
	// ORDINAL child select merging into a multi-quantifier parent is
	// LEGITIMATE composition -- no panic, and the merged select splices the
	// child quantifiers in.
	child := ordinalChildSelect(t)
	childQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	otherQ := expressions.ForEachQuantifier(expressions.InitialOf(&expressions.FullUnorderedScanExpression{}))
	parent := expressions.NewSelectExpression(
		childQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{childQ, otherQ},
		nil,
	)
	yieldedMulti := FireExpressionRule(NewSelectMergeRule(), expressions.InitialOf(parent))
	if len(yieldedMulti) == 0 {
		t.Fatal("ordinal child into multi-quantifier parent must MERGE (composition is legal), got no yields")
	}
	// The yield must actually COMPOSE, not just exist: the merged select
	// splices the child's quantifiers in place of the child quantifier
	// (parent 2 − child 1 + child's own 1 = 2, with the child's LEG alias now
	// a direct quantifier), and the merged result value reads through the
	// child's leg — no reference to the retired child alias survives.
	mergedMulti, isSel := yieldedMulti[0].(*expressions.SelectExpression)
	if !isSel {
		t.Fatalf("multi-quantifier merge yielded %T, want *SelectExpression", yieldedMulti[0])
	}
	var hasLeg bool
	for _, q := range mergedMulti.GetQuantifiers() {
		if q.GetAlias() == values.NamedCorrelationIdentifier("leg") {
			hasLeg = true
		}
		if q.GetAlias() == childQ.GetAlias() {
			t.Fatalf("merged select still binds the RETIRED child quantifier %s — the child was not spliced", childQ.GetAlias())
		}
	}
	if !hasLeg || len(mergedMulti.GetQuantifiers()) != 2 {
		t.Fatalf("merged select quantifiers = %d (leg present: %v), want 2 with the child's leg spliced in", len(mergedMulti.GetQuantifiers()), hasLeg)
	}
	if corr := values.GetCorrelatedToOfValue(mergedMulti.GetResultValue()); func() bool {
		_, refsRetired := corr[childQ.GetAlias()]
		return refsRetired
	}() {
		t.Fatal("merged result value still references the retired child alias — the fuse/compose rebase did not run")
	}

	// Allowed: a PURE WRAPPER (single-quantifier) parent over the same child
	// merges without panicking — the post-merge select is exactly the child's
	// quantifier set (the derived-table/WHERE-fold shape the arity walk
	// already counts).
	child2 := ordinalChildSelect(t)
	wrapperQ := expressions.ForEachQuantifier(expressions.InitialOf(child2))
	wrapper := expressions.NewSelectExpression(
		wrapperQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{wrapperQ},
		nil,
	)
	yielded := FireExpressionRule(NewSelectMergeRule(), expressions.InitialOf(wrapper))
	if len(yielded) != 1 {
		t.Fatalf("pure-wrapper merge over an ordinal child must proceed, got %d yields", len(yielded))
	}
	merged, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *SelectExpression", yielded[0])
	}
	if n := len(merged.GetQuantifiers()); n != 1 {
		t.Fatalf("merged wrapper has %d quantifiers, want the child's 1", n)
	}

	// Allowed: a FILTER child whose flowed value is baked, in a
	// MULTI-quantifier parent — filter-merge only dissolves the filter (its
	// predicates pull up; the parent quantifier re-ranges over the ordinal
	// box beneath, which stays a box). This shape arises from a JOIN's
	// preserved leg carrying a filter over an ordinal box (RFC-153
	// joined-preserved), which an earlier any-WithPredicates assertion
	// boundary false-positived on — pinned here as a no-panic merge.
	child3 := ordinalChildSelect(t)
	filterPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "ID"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{filterPred},
		expressions.ForEachQuantifier(expressions.InitialOf(child3)),
	)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	otherQ2 := expressions.ForEachQuantifier(expressions.InitialOf(&expressions.FullUnorderedScanExpression{}))
	multiParent := expressions.NewSelectExpression(
		filterQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{filterQ, otherQ2},
		nil,
	)
	yielded = FireExpressionRule(NewSelectMergeRule(), expressions.InitialOf(multiParent))
	if len(yielded) == 0 {
		t.Fatal("filter-over-ordinal-box merge in a multi-quantifier parent must proceed (no panic, no decline), got 0 yields")
	}
	// Every yield must keep the parent's 2 ForEach quantifiers — the filter
	// dissolves (its predicate pulls up) but the ordinal box is NEVER
	// flattened into the parent.
	for yi, y := range yielded {
		fm, ok := y.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("yield %d is %T, want *SelectExpression", yi, y)
		}
		if n := len(fm.GetQuantifiers()); n != 2 {
			t.Fatalf("yield %d changed the quantifier count to %d, want 2 — the ordinal box must stay a box", yi, n)
		}
	}
}
