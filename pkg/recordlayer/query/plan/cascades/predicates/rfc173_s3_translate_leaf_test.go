package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-173 S3-W2 commit-1 pin — TranslateLeafPredicates: embedded comparison
// operands rebase through the TranslationMap (leaf-only, whole-tree per
// embedded Value), a baked LHS fuses over the replacement via the rebuild
// arm, and an unrelated predicate comes back pointer-identical through the
// shared spine.
func TestRFC173S3_TranslateLeafPredicates(t *testing.T) {
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
	qovA := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("a"), legA)
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("b"), legB)
	upperQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("u"), merged)
	legOrdinal := map[values.CorrelationIdentifier]int{qovA.Correlation: 0, qovB.Correlation: 1}
	m := values.NewTranslationMapBuilder().
		When(qovA.Correlation).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		fv, err := values.NewFieldValueOfOrdinal(upperQOV, legOrdinal[a])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}).
		When(qovB.Correlation).Then(func(a values.CorrelationIdentifier, _ values.Value) values.Value {
		fv, err := values.NewFieldValueOfOrdinal(upperQOV, legOrdinal[a])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}).
		Build()

	lhs, err := values.NewFieldValueOfOrdinal(qovA, 0) // a.ID baked
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	rhs := values.NewFieldValue(qovB, "W", values.NotNullLong) // b.W lazy
	pred := NewAnd(&ComparisonPredicate{
		Operand:    lhs,
		Comparison: Comparison{Type: ComparisonEquals, Operand: rhs},
	})

	out := TranslateLeafPredicates(pred, m)
	if out == pred {
		t.Fatal("predicate with map-matched aliases must be rebuilt")
	}
	cmp := out.(*AndPredicate).SubPredicates[0].(*ComparisonPredicate)
	newLHS := cmp.Operand.(*values.FieldValue)
	if newLHS.Child != upperQOV || newLHS.Resolved == nil || len(newLHS.Resolved.Accessors) != 2 {
		t.Fatalf("LHS did not rebase+fuse over the upper QOV: %+v", newLHS)
	}
	newRHS := cmp.Comparison.Operand.(*values.FieldValue)
	if newRHS.Resolved != nil {
		t.Fatal("lazy RHS must not fuse")
	}
	if inner, ok := newRHS.Child.(*values.FieldValue); !ok || inner.Child != upperQOV {
		t.Fatalf("lazy RHS child = %v, want the baked replacement over the upper QOV", newRHS.Child)
	}

	// Unrelated predicate: pointer-identical through the whole spine, and the
	// identity map short-circuits before any walk.
	other := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("zz"), values.NotNullLong)
	unrelated := NewAnd(NewValuePredicate(values.NewFieldValue(other, "X", values.NotNullLong)))
	if got := TranslateLeafPredicates(unrelated, m); got != unrelated {
		t.Fatal("predicate with no matching aliases must return the input pointer")
	}
	if got := TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().Build()); got != pred {
		t.Fatal("identity map must return the input pointer")
	}
}

// TestRFC173S3_TranslateLeafPredicates_ValueBearingFields pins the review
// catches on the spine's coverage: a DistanceRank comparison's QueryVector
// (a correlation surface per Comparison.GetCorrelatedTo) and a
// PredicateWithValueAndRanges' anchor value + range comparisons all rebase —
// each was a silent default-arm/field skip leaving stale aliases after a
// cross-boundary rebase.
func TestRFC173S3_TranslateLeafPredicates_ValueBearingFields(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("src")
	newAlias := values.NamedCorrelationIdentifier("dst")
	oldQOV := values.NewQuantifiedObjectValue(oldAlias)
	m := values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return values.NewQuantifiedObjectValue(newAlias)
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
		QueryVector: values.NewFieldValue(oldQOV, "EMB", nil),
	}
	vecPred := &ComparisonPredicate{Operand: values.NewFlatFieldValue("EMB", nil), Comparison: vecCmp}
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
		values.NewFieldValue(oldQOV, "ID", values.NotNullLong),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{{Type: ComparisonEquals, Operand: values.NewFieldValue(oldQOV, "V", values.NotNullLong)}},
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

	// RE-CLASSIFICATION (review catch, Java RangeConstraints
	// .translateCorrelations:349-366): a DEFERRED comparison whose transform
	// strips its last correlation must land in the COMPILABLE bucket — the
	// translated shape decides the split, never the old bucket.
	constMap := values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	}).Build()
	deferredPVR := NewPredicateWithValueAndRanges(
		values.NewFlatFieldValue("ID", values.NotNullLong),
		[]*RangeConstraints{NewRangeConstraints(
			nil,
			[]Comparison{{Type: ComparisonEquals, Operand: values.NewQuantifiedObjectValue(oldAlias)}},
		)},
	)
	outConst := TranslateLeafPredicates(deferredPVR, constMap).(*PredicateWithValueAndRanges)
	reclassified := outConst.GetRanges()[0]
	if len(reclassified.GetDeferredRanges()) != 0 || len(reclassified.GetCompilableComparisons()) != 1 {
		t.Fatalf("de-correlated deferred comparison must be re-classified compilable, got compilable=%d deferred=%d",
			len(reclassified.GetCompilableComparisons()), len(reclassified.GetDeferredRanges()))
	}
}

// TestRFC173S3_TranslationMapBuilder_Snapshot pins Build()'s immutability
// (review catch): reusing the builder after Build must not mutate the map
// already handed out.
func TestRFC173S3_TranslationMapBuilder_Snapshot(t *testing.T) {
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

// TestRFC173S3_TranslateLeafPredicates_Existential pins the review catch: the
// shared spine translates an ExistentialValuePredicate's embedded QOV (Java
// ExistentialValuePredicate.translateLeafPredicate :94-100 — the default-arm
// silent skip was the exact silent-non-translation failure class the W2
// ruling flagged), and a map whose replacement is NOT a QOV panics loudly
// through the Must constructor rather than yielding a mis-shaped predicate.
func TestRFC173S3_TranslateLeafPredicates_Existential(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("ex")
	newAlias := values.NamedCorrelationIdentifier("ex2")
	pred := NewExistentialAlias(oldAlias)

	renamed := TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().
		When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
		return values.NewQuantifiedObjectValue(newAlias)
	}).Build())
	ev, ok := renamed.(*ExistentialValuePredicate)
	if !ok || renamed == QueryPredicate(pred) {
		t.Fatalf("existential predicate must be rebuilt, got %T", renamed)
	}
	if qov, ok := ev.Value.(*values.QuantifiedObjectValue); !ok || qov.Correlation != newAlias {
		t.Fatalf("existential QOV = %v, want translated to %v", ev.Value, newAlias)
	}
	if ev.Comparison.Type != ComparisonIsNotNull {
		t.Fatal("comparison must be preserved through the rebuild")
	}

	// Non-QOV replacement: loud panic (the Must constructor's invariant),
	// never a silently mis-shaped predicate.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("non-QOV replacement for an existential QOV must panic")
			}
		}()
		TranslateLeafPredicates(pred, values.NewTranslationMapBuilder().
			When(oldAlias).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
			return values.NewFlatFieldValue("X", values.NotNullLong)
		}).Build())
	}()
}
