package values

import (
	"testing"
)

// Pins for the TranslationMap port and the fuse-on-rebuild arm (Java
// Value.translateCorrelations + FieldValue.withNewChild =
// ofFieldsAndFuseIfPossible), exercised on the merge-case shape
// PartitionSelectRule.java:283-322 (and its Go port, rule_partition_select.go)
// builds.

// mergeMiniature builds the merge-case fixture: legs a{ID,V} and b{W},
// the collapsed lower's merged type Record(_0: legA, _1: legB), the upper
// quantifier over it, and the map {a → ofOrdinal(u,0), b → ofOrdinal(u,1)}.
func mergeMiniature(t *testing.T) (qovA, qovB, upperQOV *quantifiedObjectValue, m TranslationMap) {
	t.Helper()
	legA := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: NotNullLong, Ordinal: 1},
	})
	legB := NewRecordType("", false, []Field{
		{Name: "W", FieldType: NotNullLong, Ordinal: 0},
	})
	merged := NewRecordType("", false, []Field{
		{Name: OrdinalFieldName(0), FieldType: legA, Ordinal: 0},
		{Name: OrdinalFieldName(1), FieldType: legB, Ordinal: 1},
	})
	qovA = mustQOV(t, NamedCorrelationIdentifier("a"), legA)
	qovB = mustQOV(t, NamedCorrelationIdentifier("b"), legB)
	upperQOV = mustQOV(t, NamedCorrelationIdentifier("u"), merged)

	legOrdinal := map[CorrelationIdentifier]int{
		qovA.Correlation(): 0,
		qovB.Correlation(): 1,
	}
	toUpper := func(sourceAlias CorrelationIdentifier, _ Value) Value {
		fv, err := ResolveOrdinalSeedField(upperQOV, legOrdinal[sourceAlias])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}
	m = NewTranslationMapBuilder().
		When(qovA.Correlation()).Then(toUpper).
		When(qovB.Correlation()).Then(toUpper).
		Build()
	return qovA, qovB, upperQOV, m
}

// TestTranslationMap_MergeCaseMiniature pins the load-bearing composition:
// the map swaps the QOV leaf for PLAIN ofOrdinalNumber over the upper
// (PartitionSelectRule.java:302 — the function owns the shape, never the
// composition), and the enclosing BAKED reference fuses during the rebuild
// into ONE node over the upper QOV (Java's withNewChild mechanic).
func TestTranslationMap_MergeCaseMiniature(t *testing.T) {
	t.Parallel()
	qovA, _, upperQOV, m := mergeMiniature(t)

	bakedRef, err := ResolveOrdinalSeedField(qovA, 1) // a.V, baked
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	out := TranslateCorrelations(bakedRef, m)
	fused, ok := out.(*fieldValue)
	if !ok || fused == bakedRef {
		t.Fatalf("translate = %T %v, want a NEW fused *fieldValue", out, out)
	}
	if fused.Child != upperQOV {
		t.Fatalf("fused child = %v, want the upper QOV", fused.Child)
	}
	want := newFieldPathOfSingle(OrdinalFieldName(0), 0, true).WithSuffix(newFieldPathOfSingle("V", 1, true))
	if fused.Resolved == nil || !fused.Resolved.Equals(want) {
		t.Fatalf("fused path = %+v, want [(_0,0),(V,1)]", fused.Resolved)
	}
	if fused.Field != "V" {
		t.Fatalf("fused display = %q, want the LAST step's name V", fused.Field)
	}

	// Runtime sanity over the merged positional row: root slot 0 (leg A's
	// row), descend to slot 1 (V).
	legARow := &fakeOrdinalRow{names: []string{"ID", "V"}, slots: []any{int64(7), int64(42)}}
	got, err := fused.Evaluate(&ordEvalBinder{id: upperQOV.Correlation(), bound: &fakeOrdinalRow{
		names: []string{OrdinalFieldName(0), OrdinalFieldName(1)},
		slots: []any{legARow, nil},
	}})
	if err != nil || got != int64(42) {
		t.Fatalf("fused merge read = (%v, %v), want (42, nil)", got, err)
	}

	// The ordinary (unpinned) exact resolver takes the same checked fusion
	// path. This is the mutation control for the removed lazy compatibility
	// shape: translation must publish one admitted, fully resolved field, not a
	// chained node whose outer access waits for runtime name lookup.
	plainRef, err := ResolveFieldOrdinals(qovA, []int{1})
	if err != nil {
		t.Fatalf("plain exact field: %v", err)
	}
	plainOut := TranslateCorrelations(plainRef, m)
	plainView, admitted := AsFieldValue(plainOut)
	if !admitted || plainOut == plainRef {
		t.Fatalf("plain exact translation = %T %v, want a new admitted FieldValue", plainOut, plainOut)
	}
	if plainView.ChildValue() != upperQOV || plainView.Path() == nil || len(plainView.Path().Ordinals()) != 2 {
		t.Fatalf("plain exact translation = child %v path %v, want fused [0,1] over upper QOV", plainView.ChildValue(), plainView.Path())
	}
}

// TestTranslateCorrelationsChecked_ExactFieldValueAtomicity pins RFC-232 at
// the real merge rewrite boundary. A valid seed-to-merge translation stays an
// admitted exact FieldValue; a same-ordinal replacement with a different leaf
// type errors and publishes neither the original nor a partial rebuilt node.
func TestTranslateCorrelationsChecked_ExactFieldValueAtomicity(t *testing.T) {
	t.Parallel()

	legType := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: NotNullLong, Ordinal: 1},
	})
	mergedType := NewRecordType("", false, []Field{
		{Name: OrdinalFieldName(0), FieldType: legType, Ordinal: 0},
	})
	qovA := mustQOV(t, NamedCorrelationIdentifier("exact_a"), legType)
	upperQOV := mustQOV(t, NamedCorrelationIdentifier("exact_u"), mergedType)
	legField, err := ResolveOrdinalSeedField(qovA, 1)
	if err != nil {
		t.Fatalf("seed field: %v", err)
	}
	mergeSlot, err := ResolveOrdinalSeedField(upperQOV, 0)
	if err != nil {
		t.Fatalf("merge slot: %v", err)
	}
	m := NewTranslationMapBuilder().When(qovA.Correlation()).Then(
		func(CorrelationIdentifier, Value) Value { return mergeSlot },
	).Build()
	translated, err := TranslateCorrelationsChecked(legField, m)
	if err != nil {
		t.Fatalf("checked merge translation: %v", err)
	}
	view, admitted := AsFieldValue(translated)
	if !admitted {
		t.Fatalf("translated value = %T, want admitted exact FieldValue", translated)
	}
	if view.ChildValue() != upperQOV || view.Path() == nil ||
		!view.Path().IsFrontierPinned() || len(view.Path().Ordinals()) != 2 {
		t.Fatalf("translated exact field = child %v path %v, want pinned merge path over upper QOV", view.ChildValue(), view.Path())
	}
	if !view.ResultType().Equals(NotNullLong) {
		t.Fatalf("translated result type = %v, want exact LONG NOT NULL", view.ResultType())
	}

	wrongMerged := NewRecordType("", false, []Field{
		{Name: OrdinalFieldName(0), FieldType: NewRecordType("", false, []Field{
			{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: NotNullString, Ordinal: 1},
		}), Ordinal: 0},
	})
	wrongUpper := mustQOV(t, upperQOV.Correlation(), wrongMerged)
	wrongSlot, err := ResolveOrdinalSeedField(wrongUpper, 0)
	if err != nil {
		t.Fatalf("wrong replacement slot: %v", err)
	}
	wrongMap := NewTranslationMapBuilder().When(qovA.Correlation()).Then(
		func(CorrelationIdentifier, Value) Value { return wrongSlot },
	).Build()
	bad, err := TranslateCorrelationsChecked(legField, wrongMap)
	if err == nil || bad != nil {
		t.Fatalf("wrong-type checked translation = (%v, %v), want (nil, typed error)", bad, err)
	}
	var coded interface{ Code() ResolutionErrorCode }
	if !asResolutionCode(err, &coded) || coded.Code() != FieldIncompatibleRoot {
		t.Fatalf("wrong-type error = %v, want FieldIncompatibleRoot", err)
	}
	if _, admitted := AsFieldValue(legField); !admitted {
		t.Fatal("failed translation mutated the original exact FieldValue")
	}
	if shim := TranslateCorrelations(legField, wrongMap); shim != nil {
		t.Fatalf("unchecked shim returned %T after exact rebuild failure; want loud nil, never original/partial", shim)
	}
}

// TestTranslationMap_IdentityAndMiss pins pointer stability: the identity
// early-out (DefinesOnlyIdentities) and a map whose aliases don't appear
// both return the INPUT pointer — CoW all the way down.
func TestTranslationMap_IdentityAndMiss(t *testing.T) {
	t.Parallel()
	qovA, _, _, _ := mergeMiniature(t)
	ref, err := ResolveOrdinalSeedField(qovA, 0)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}

	empty := NewTranslationMapBuilder().Build()
	if !empty.DefinesOnlyIdentities() {
		t.Fatal("empty map must define only identities")
	}
	if out := TranslateCorrelations(ref, empty); out != ref {
		t.Fatalf("identity map must return the input pointer, got %v", out)
	}

	miss := NewTranslationMapBuilder().
		When(NamedCorrelationIdentifier("zz")).
		Then(func(_ CorrelationIdentifier, leaf Value) Value { return leaf }).
		Build()
	if out := TranslateCorrelations(ref, miss); out != ref {
		t.Fatalf("no-alias-match must return the input pointer, got %v", out)
	}
}

// TestTranslationMap_NeverRetranslate pins the visitNewLeaves=false
// semantics through TranslateCorrelations: a SELF-REFERENTIAL substitution
// (the replacement references the SAME source alias) terminates with exactly
// one application — Java TreeLike.java:242-262, Go ReplaceLeavesOnceMaybe.
func TestTranslationMap_NeverRetranslate(t *testing.T) {
	t.Parallel()
	qovA, _, _, _ := mergeMiniature(t)
	id, err := ResolveFieldOrdinals(qovA, []int{0})
	if err != nil {
		t.Fatalf("ID field: %v", err)
	}
	v, err := ResolveFieldOrdinals(qovA, []int{1})
	if err != nil {
		t.Fatalf("V field: %v", err)
	}
	// The replacement has the same exact record shape as qovA and contains
	// qovA leaves. A traversal that revisits replacement leaves would apply
	// the translation forever; visitNewLeaves=false must apply it once.
	replacement := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "ID", Value: id},
		RecordConstructorField{Name: "V", Value: v},
	)
	applications := 0
	m := NewTranslationMapBuilder().
		When(qovA.Correlation()).
		Then(func(_ CorrelationIdentifier, _ Value) Value {
			applications++
			return replacement
		}).
		Build()

	ref, err := ResolveOrdinalSeedField(qovA, 0)
	if err != nil {
		t.Fatalf("source field: %v", err)
	}
	out := TranslateCorrelations(ref, m)
	if applications != 1 {
		t.Fatalf("translation function applied %d times, want exactly 1 (replacement leaves are never re-translated)", applications)
	}
	if _, admitted := AsFieldValue(out); !admitted || out == ref {
		t.Fatalf("translate = %T %v, want a rebuilt admitted FieldValue", out, out)
	}
}

// TestRebuildFuse_EquivalentToCompose pins the invariant rebuild(fuse) ≡
// compose(chain). Forcing a rebuild of a baked chain through the generic
// Replace spine (swapping the base QOV for a fresh equal-typed pointer) must
// produce the SAME fused node the simplifier's compose rule produces — two
// mechanisms, one canonical form.
func TestRebuildFuse_EquivalentToCompose(t *testing.T) {
	t.Parallel()
	_, outer := bakedChain(t)
	composed := SimplifyValue(outer).(*fieldValue)

	baseQOV := composed.Child.(*quantifiedObjectValue)
	freshQOV := mustQOV(t, baseQOV.Correlation(), baseQOV.Type())
	rebuilt := Replace(outer, func(v Value) Value {
		if v == baseQOV {
			return freshQOV
		}
		return v
	})
	fused, ok := rebuilt.(*fieldValue)
	if !ok {
		t.Fatalf("rebuild = %T, want *fieldValue", rebuilt)
	}
	if fused.Child != freshQOV {
		t.Fatalf("rebuild child = %v, want the fresh QOV", fused.Child)
	}
	if fused.Resolved == nil || !fused.Resolved.Equals(composed.Resolved) {
		t.Fatalf("rebuild path %+v != compose path %+v — rebuild(fuse) must equal compose(chain)", fused.Resolved, composed.Resolved)
	}
	if fused.Field != composed.Field || !typesEqual(fused.Typ, composed.Typ) {
		t.Fatalf("rebuild (Field=%q, Typ=%v) != compose (Field=%q, Typ=%v)", fused.Field, fused.Typ, composed.Field, composed.Typ)
	}
	if fused.Resolved.FrontierPinned != composed.Resolved.FrontierPinned {
		t.Fatal("rebuild and compose must agree on the frontier pin (receiver inheritance)")
	}
}

// TestRebuildFuse_GateRequiresBothBaked pins the fuse gate on the rebuild
// arm: lazy and mixed chains rebuilt through Replace keep their CHAINED
// shape — fusing them would change memo identity and Explain rendering for
// every plan containing the pattern, not just this one.
func TestRebuildFuse_GateRequiresBothBaked(t *testing.T) {
	t.Parallel()
	qov := s3NestedQOV(t)
	freshQOV := mustQOV(t, qov.Correlation(), qov.Type())
	swap := func(v Value) Value {
		if v == qov {
			return freshQOV
		}
		return v
	}

	// Lazy chain: rebuilt, still two chained lazy nodes.
	lazyOuter := newFieldValue(newFieldValue(qov, "NESTED", nil), "Y", NotNullLong)
	out := Replace(lazyOuter, swap).(*fieldValue)
	if out.Resolved != nil {
		t.Fatalf("lazy chain fused on rebuild (path %+v) — gate requires both baked", out.Resolved)
	}
	if inner, ok := out.Child.(*fieldValue); !ok || inner.Resolved != nil || inner.Child != freshQOV {
		t.Fatalf("lazy chain shape changed on rebuild: child = %v", out.Child)
	}

	// Mixed: lazy outer over baked inner — no fuse (outer not baked).
	bakedInner, err := newFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	mixedOuter := newFieldValue(bakedInner, "Y", NotNullLong)
	mixedOut := Replace(mixedOuter, swap).(*fieldValue)
	if mixedOut.Resolved != nil {
		t.Fatal("lazy-over-baked fused on rebuild — gate requires BOTH ends baked")
	}

	// Mixed the other way: BAKED outer over a LAZY inner — no fuse (inner has
	// no path to prepend; the outer keeps its own path over the rebuilt lazy
	// child).
	lazyInner := newFieldValue(qov, "NESTED", nil)
	bakedOuter := &fieldValue{Field: "Y", Typ: NotNullLong, Child: lazyInner, Resolved: newFieldPathOfSingle("Y", 1, false)}
	bakedOut := Replace(bakedOuter, swap).(*fieldValue)
	if bakedOut.Resolved == nil || len(bakedOut.Resolved.Accessors) != 1 {
		t.Fatalf("baked-over-lazy rebuild path = %+v, want the outer's own single-accessor path unchanged", bakedOut.Resolved)
	}
	if inner, ok := bakedOut.Child.(*fieldValue); !ok || inner.Resolved != nil {
		t.Fatalf("baked-over-lazy rebuild child = %v, want the rebuilt LAZY inner", bakedOut.Child)
	}

	// Baked outer over a baked CHILDLESS inner (the recursive-CTE wrap shape):
	// no fuse through the rebuild — mirrors compose's inner.Child != nil gate
	// (there is no base to re-anchor onto).
	wrapInner := newFieldValueWithResolvedOrdinal("NESTED", 0, UnknownType)
	bakedOverWrap := &fieldValue{Field: "Y", Typ: NotNullLong, Child: wrapInner, Resolved: newFieldPathOfSingle("Y", 1, false)}
	// The swap fn can't fire (no QOV under a childless inner) — force the
	// rebuild by replacing the wrap inner with an equal copy.
	wrapCopy := newFieldValueWithResolvedOrdinal("NESTED", 0, UnknownType)
	wrapOut := Replace(bakedOverWrap, func(v Value) Value {
		if v == wrapInner {
			return wrapCopy
		}
		return v
	}).(*fieldValue)
	if wrapOut.Resolved == nil || len(wrapOut.Resolved.Accessors) != 1 || wrapOut.Child != wrapCopy {
		t.Fatalf("baked-over-childless-baked must stay chained (rebuild≡compose on the childless axis), got path %+v child %v", wrapOut.Resolved, wrapOut.Child)
	}
}

// TestTranslationMap_BuilderVerify pins the builder's duplicate-alias
// panic (Java's Verify, RegularTranslationMap.java:204 — a silent overwrite
// would drop a rebase) and the typed-nil map guard.
func TestTranslationMap_BuilderVerify(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("a")
	identity := func(_ CorrelationIdentifier, leaf Value) Value { return leaf }

	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate source alias must panic (a silent overwrite drops a rebase)")
			}
		}()
		NewTranslationMapBuilder().When(a).Then(identity).When(a).Then(identity)
	}()

	// Typed-nil map: no deref, input returned.
	var nilMap *RegularTranslationMap
	root := mustQOV(t, NamedCorrelationIdentifier("typed_nil"), NewRecordType("", false, []Field{
		{Name: "X", FieldType: NotNullLong, Ordinal: 0},
	}))
	ref, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("typed-nil exact field: %v", err)
	}
	if out := TranslateCorrelations(ref, nilMap); out != ref {
		t.Fatalf("typed-nil map must return the input, got %v", out)
	}
}

// The TranslateLeafPredicates pins live in the predicates package — the
// spine belongs there and the import direction forbids testing it here.

// TestIsPositionalMergeRC pins the structural merge recognizer's edges: the
// exact PartitionSelectRule shape matches; every deviation — named fields,
// out-of-order `_i`, duplicate leg correlations, a single field, an
// AnchoredJoin marker, non-QOV values — declines. The recognizer is
// load-bearing: a false positive would produce positional rows for what is
// really a projection; a false negative reverts a merge level to the name
// model.
func TestIsPositionalMergeRC(t *testing.T) {
	t.Parallel()
	qovA := mustQOV(t, NamedCorrelationIdentifier("a"))
	qovB := mustQOV(t, NamedCorrelationIdentifier("b"))
	field := func(name string, v Value) RecordConstructorField {
		return RecordConstructorField{Name: name, Value: v}
	}

	good := NewRawRecordConstructorValue(field("_0", qovA), field("_1", qovB))
	if !IsPositionalMergeRC(good) {
		t.Fatal("the exact merge shape must match")
	}
	cases := map[string]Value{
		"named field":        NewRawRecordConstructorValue(field("LEFT", qovA), field("_1", qovB)),
		"out-of-order names": NewRawRecordConstructorValue(field("_1", qovA), field("_0", qovB)),
		"duplicate leg":      NewRawRecordConstructorValue(field("_0", qovA), field("_1", qovA)),
		"single field":       NewRawRecordConstructorValue(field("_0", qovA)),
		"non-QOV value":      NewRawRecordConstructorValue(field("_0", qovA), field("_1", newFlatFieldValue("X", NotNullLong))),
		"non-RC":             qovA,
	}
	for name, v := range cases {
		if IsPositionalMergeRC(v) {
			t.Errorf("%s must NOT match the merge shape", name)
		}
	}
}
