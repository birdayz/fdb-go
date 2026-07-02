package cascades

// RFC-173 Slice 2 W2 — red→green pins proving the two coexistence-window
// drift asserts actually FIRE (review: an unpinned tripwire that looks
// healthy is worse than none — if the assert or its ContainsBakedOrdinal
// probe had a walk bug, W3 would go live with a dead assert). Both asserts
// are unreachable from production in W2 (nothing constructs baked result
// values); these pins hand-construct the violating shapes.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// bakedLegRC builds a 2-way-ordinal-seed-shaped result value: an RC whose
// fields are BAKED FieldValue.ofOrdinalNumber references over one leg QOV —
// the W3 gated-join seed shape in miniature.
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

// TestRFC173S2_SelectMergeDriftAssert_Fires pins that merging an ORDINAL
// child into a MULTI-quantifier parent panics (the cluster-arity gate
// mis-scoped — contract ruling #1's loud assert), and that the ALLOWED
// single-quantifier pure-wrapper merge of the same child proceeds silently.
func TestRFC173S2_SelectMergeDriftAssert_Fires(t *testing.T) {
	t.Parallel()

	// Violation: parent has the ordinal child PLUS a second ForEach.
	child := ordinalChildSelect(t)
	childQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	otherQ := expressions.ForEachQuantifier(expressions.InitialOf(&expressions.FullUnorderedScanExpression{}))
	parent := expressions.NewSelectExpression(
		childQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{childQ, otherQ},
		nil,
	)
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Error("SelectMergeRule must PANIC when an ORDINAL child merges into a multi-quantifier parent — the drift assert is dead")
				return
			}
			if msg, ok := r.(string); !ok || !strings.Contains(msg, "RFC-173") {
				t.Errorf("panic is not the RFC-173 drift assert: %v", r)
			}
		}()
		FireExpressionRule(NewSelectMergeRule(), expressions.InitialOf(parent))
	}()

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
	// box beneath, which stays a box). This is the W3b flip's first live
	// shape (RFC-153 joined-preserved) and the earlier any-WithPredicates
	// assert boundary false-positived on it — pinned as a no-panic merge.
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

// TestRFC173S2_ReStampDriftAssert_Fires pins that re-anchoring a BAKED
// FieldValue to a dotted merge name (PartitionSelectRule's re-stamp,
// rebaseBuriedLowerReferences) panics — the silent baked→lazy degradation
// over a re-typed child that the eager bake forbids — while the same
// re-stamp over a LAZY reference proceeds unchanged in behavior.
func TestRFC173S2_ReStampDriftAssert_Fires(t *testing.T) {
	t.Parallel()

	legCorr := values.NamedCorrelationIdentifier("a")
	mergeCorr := values.NamedCorrelationIdentifier("$m")
	buried := map[values.CorrelationIdentifier]struct{}{legCorr: {}}

	legType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qov := values.NewQuantifiedObjectValueOfType(legCorr, legType)
	baked, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedPred := &predicates.ComparisonPredicate{
		Operand:    baked,
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Error("rebaseBuriedLowerReferences must PANIC on a BAKED reference — the drift assert is dead")
				return
			}
			if msg, ok := r.(string); !ok || !strings.Contains(msg, "RFC-173") {
				t.Errorf("panic is not the RFC-173 drift assert: %v", r)
			}
		}()
		rebaseBuriedLowerReferences(bakedPred, buried, mergeCorr)
	}()

	// Control: a LAZY buried reference re-stamps to the dotted merge name as
	// before (the name model's re-anchoring, untouched).
	lazyPred := &predicates.ComparisonPredicate{
		Operand:    values.NewFieldValue(values.NewQuantifiedObjectValue(legCorr), "ID", values.NotNullLong),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	got := rebaseBuriedLowerReferences(lazyPred, buried, mergeCorr)
	cp, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("re-stamped predicate is %T", got)
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok || fv.Field != "A.ID" {
		t.Fatalf("lazy re-stamp = %+v, want FieldValue A.ID over the merge QOV (unchanged name-model behavior)", cp.Operand)
	}
}
