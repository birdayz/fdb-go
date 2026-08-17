package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestTranslateLeafPredicates pins TranslateLeafPredicates: embedded
// comparison operands rebase through the TranslationMap (leaf-only,
// whole-tree per embedded Value), a baked LHS fuses over the replacement via
// the rebuild arm, and an unrelated predicate comes back pointer-identical
// through the shared spine.
func TestTranslateLeafPredicates(t *testing.T) {
	t.Parallel()
	legA := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 0},
	})
	merged := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legA, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: legB, Ordinal: 1},
	})
	qovA := mustQOV(t, values.NamedCorrelationIdentifier("a"), legA)
	qovB := mustQOV(t, values.NamedCorrelationIdentifier("b"), legB)
	upperQOV := mustQOV(t, values.NamedCorrelationIdentifier("u"), merged)
	legOrdinal := map[values.CorrelationIdentifier]int{qovA.Correlation(): 0, qovB.Correlation(): 1}
	m := values.NewTranslationMapBuilder().
		When(qovA.Correlation()).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		return mustResolveFieldOrdinals(t, upperQOV, legOrdinal[a])
	}).
		When(qovB.Correlation()).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		return mustResolveFieldOrdinals(t, upperQOV, legOrdinal[a])
	}).
		Build()

	lhs := mustResolveFieldOrdinals(t, qovA, 0) // a.ID
	rhs := mustResolveFieldOrdinals(t, qovB, 0) // b.W
	pred := NewAnd(&ComparisonPredicate{
		Operand:    lhs,
		Comparison: Comparison{Type: ComparisonEquals, Operand: rhs},
	})

	out := TranslateLeafPredicates(pred, m)
	if out == pred {
		t.Fatal("predicate with map-matched aliases must be rebuilt")
	}
	cmp := out.(*AndPredicate).SubPredicates[0].(*ComparisonPredicate)
	newLHS, ok := values.AsFieldValue(cmp.Operand)
	if !ok || newLHS.ChildValue() != upperQOV || newLHS.Path().Len() != 2 {
		t.Fatalf("LHS did not rebase+fuse over the upper QOV: %+v", newLHS)
	}
	newRHS, ok := values.AsFieldValue(cmp.Comparison.Operand)
	if !ok || newRHS.ChildValue() != upperQOV || newRHS.Path().Len() != 2 {
		t.Fatalf("RHS did not rebase+fuse over the upper QOV: %+v", newRHS)
	}

	// Unrelated predicate: pointer-identical through the whole spine, and the
	// identity map short-circuits before any walk.
	other := mustQOV(t, values.NamedCorrelationIdentifier("zz"), values.NotNullLong)
	unrelated := NewAnd(NewValuePredicate(other))
	if got := TranslateLeafPredicates(unrelated, m); got != unrelated {
		t.Fatal("predicate with no matching aliases must return the input pointer")
	}
	if got := TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().Build()); got != pred {
		t.Fatal("identity map must return the input pointer")
	}
}

// TestTranslateLeafPredicates_ValueBearingFields pins the spine's coverage: a
// DistanceRank comparison's QueryVector (a correlation surface per
// Comparison.GetCorrelatedTo) and a PredicateWithValueAndRanges' anchor value
// + range comparisons all rebase — a silent default-arm/field skip would
// leave stale aliases after a cross-boundary rebase.
func TestTranslateLeafPredicates_ValueBearingFields(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("src")
	newAlias := values.NamedCorrelationIdentifier("dst")
	rowType := values.NewRecordType("translation_row", false, []values.Field{
		{Name: "EMB", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 2},
	})
	oldQOV := mustQOV(t, oldAlias, rowType)
	m := values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return mustQOV(t, newAlias, rowType)
	}).Build()
	refersNew := func(v values.Value) bool {
		_, has := values.GetCorrelatedToOfValue(v)[newAlias]
		return has
	}

	// QueryVector: the vector operand is correlated to the source alias; the
	// K operand is a constant. Only the vector must change.
	vecCmp := Comparison{
		Type:        ComparisonDistanceRankLessThanOrEq,
		Operand:     &values.ConstantValue{Value: int64(5), Typ: values.NotNullLong},
		QueryVector: mustResolveFieldOrdinals(t, oldQOV, 0),
	}
	vecPred := &ComparisonPredicate{Operand: predicateTestField(t, "EMB", nil), Comparison: vecCmp}
	out := TranslateLeafPredicates(vecPred, m).(*ComparisonPredicate)
	if out == vecPred || !refersNew(out.Comparison.QueryVector) {
		t.Fatalf("QueryVector not rebased: %v", out.Comparison.QueryVector)
	}
	if out.Comparison.Type != vecCmp.Type || out.Comparison.Operand != vecCmp.Operand {
		t.Fatal("non-vector comparison fields must be preserved")
	}

	// PredicateWithValueAndRanges: anchor value + a range comparison operand
	// both reference the source alias.
	pvr := NewPredicateWithValueAndRanges(
		mustResolveFieldOrdinals(t, oldQOV, 1),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{{Type: ComparisonEquals, Operand: mustResolveFieldOrdinals(t, oldQOV, 2)}},
			nil,
		)},
	)
	outPVR := TranslateLeafPredicates(pvr, m).(*PredicateWithValueAndRanges)
	if outPVR == pvr || !refersNew(outPVR.GetValue()) {
		t.Fatalf("PVR anchor value not rebased: %v", outPVR.GetValue())
	}
	// The rebased operand still carries a correlation (dst), so
	// re-classification files it DEFERRED — even though the fixture had
	// (mis-)bucketed it compilable; the translated shape decides the split.
	if cmps := outPVR.GetRanges()[0].GetDeferredRanges(); len(cmps) != 1 || !refersNew(cmps[0].Operand) {
		t.Fatalf("PVR range comparison must be rebased AND re-classified deferred (still correlated), got %+v", outPVR.GetRanges()[0])
	}

	// Pointer stability preserved for both when nothing matches.
	missMap := values.NewTranslationMapBuilder().
		When(values.NamedCorrelationIdentifier("zz")).
		Then(func(_ values.CorrelationIdentifier, leaf values.Value) values.Value { return leaf }).
		Build()
	if got := TranslateLeafPredicates(vecPred, missMap); got != QueryPredicate(vecPred) {
		t.Fatal("vector predicate must be pointer-stable on a miss")
	}
	if got := TranslateLeafPredicates(pvr, missMap); got != QueryPredicate(pvr) {
		t.Fatal("PVR must be pointer-stable on a miss")
	}

	// RE-CLASSIFICATION (Java RangeConstraints.translateCorrelations:349-366):
	// a DEFERRED comparison whose transform strips its last correlation must
	// land in the COMPILABLE bucket — the translated shape decides the split,
	// never the old bucket.
	constMap := values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	}).Build()
	deferredPVR := NewPredicateWithValueAndRanges(predicateTestField(t, "ID", values.NotNullLong), []*RangeConstraints{NewRangeConstraints(
		nil,
		[]Comparison{{Type: ComparisonEquals, Operand: mustQOV(t, oldAlias)}},
	)},
	)
	outConst := TranslateLeafPredicates(deferredPVR, constMap).(*PredicateWithValueAndRanges)
	reclassified := outConst.GetRanges()[0]
	if len(reclassified.GetDeferredRanges()) != 0 || len(reclassified.GetCompilableComparisons()) != 1 {
		t.Fatalf("de-correlated deferred comparison must be re-classified compilable, got compilable=%d deferred=%d",
			len(reclassified.GetCompilableComparisons()), len(reclassified.GetDeferredRanges()))
	}
}

// TestTranslationMapBuilder_Snapshot pins Build()'s immutability: reusing the
// builder after Build must not mutate the map already handed out.
func TestTranslationMapBuilder_Snapshot(t *testing.T) {
	t.Parallel()
	b := values.NewTranslationMapBuilder()
	empty := b.Build()
	b.When(values.NamedCorrelationIdentifier("late")).
		Then(func(_ values.CorrelationIdentifier, leaf values.Value) values.Value { return leaf })
	if !empty.DefinesOnlyIdentities() {
		t.Fatal("a built empty map must stay identity-only after further builder use")
	}
	if empty.ContainsSourceAlias(values.NamedCorrelationIdentifier("late")) {
		t.Fatal("a built map must not pick up aliases added to the builder afterwards")
	}
}

// TestTranslateLeafPredicates_Existential pins that the shared spine
// translates an ExistentialValuePredicate's embedded QOV (Java
// ExistentialValuePredicate.translateLeafPredicate:94-100 — a default-arm
// silent skip would silently fail to translate it), and that a map whose
// replacement is NOT a QOV fails atomically rather than yielding a mis-shaped
// predicate or panicking after partial reconstruction.
func TestTranslateLeafPredicates_Existential(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("ex")
	newAlias := values.NamedCorrelationIdentifier("ex2")
	pred := mustExistentialAlias(t, oldAlias)

	renamed := TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return mustQOV(t, newAlias)
	}).Build())
	ev, ok := renamed.(*ExistentialValuePredicate)
	if !ok || renamed == QueryPredicate(pred) {
		t.Fatalf("existential predicate must be rebuilt, got %T", renamed)
	}
	if qov, ok := values.AsQuantifiedObjectValue(ev.Value); !ok || qov.Correlation() != newAlias {
		t.Fatalf("existential QOV = %v, want translated to %v", ev.Value, newAlias)
	}
	if ev.Comparison.Type != ComparisonIsNotNull {
		t.Fatal("comparison must be preserved through the rebuild")
	}

	// Non-QOV replacement: the checked authority returns no predicate and a
	// stable structural code. The legacy Value-only wrapper fails closed with
	// nil and must not resurrect the old panic/partial-graph behavior.
	invalidMap := values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return predicateTestField(t, "X", values.NotNullLong)
	}).Build()
	invalid, err := TranslateLeafPredicatesChecked(pred, invalidMap)
	if invalid != nil || err == nil {
		t.Fatalf("invalid checked translation = (%T,%v), want nil,error", invalid, err)
	}
	coded, ok := err.(interface {
		Code() values.ResolutionErrorCode
	})
	if !ok || coded.Code() != values.RewriteInvalidCallbackOutput {
		t.Fatalf("invalid checked translation error = %v, want RewriteInvalidCallbackOutput", err)
	}
	if got := TranslateLeafPredicates(pred, invalidMap); got != nil {
		t.Fatalf("legacy invalid translation = %T, want nil fail-closed", got)
	}
	if qov, ok := values.AsQuantifiedObjectValue(pred.Value); !ok || qov.Correlation() != oldAlias {
		t.Fatal("failed translation mutated its input predicate")
	}
}
