package values

import (
	"testing"
)

// RFC-173 S3-W2 commit-1 pins — the TranslationMap port and the fuse-on-
// rebuild arm (Java Value.translateCorrelations + FieldValue.withNewChild =
// ofFieldsAndFuseIfPossible). DARK: no production caller until the fulcrum
// commit; these pins exercise the machinery on the exact merge-case shape
// PartitionSelectRule.java:283-322 will build.

// mergeMiniature builds the merge-case fixture: legs a{ID,V} and b{W},
// the collapsed lower's merged type Record(_0: legA, _1: legB), the upper
// quantifier over it, and the map {a → ofOrdinal(u,0), b → ofOrdinal(u,1)}.
func mergeMiniature(t *testing.T) (qovA, qovB, upperQOV *QuantifiedObjectValue, m TranslationMap) {
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
	qovA = NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("a"), legA)
	qovB = NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("b"), legB)
	upperQOV = NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("u"), merged)

	legOrdinal := map[CorrelationIdentifier]int{
		qovA.Correlation: 0,
		qovB.Correlation: 1,
	}
	toUpper := func(sourceAlias CorrelationIdentifier, _ Value) Value {
		fv, err := NewFieldValueOfOrdinal(upperQOV, legOrdinal[sourceAlias])
		if err != nil {
			t.Fatalf("merge-map bake: %v", err)
		}
		return fv
	}
	m = NewTranslationMapBuilder().
		When(qovA.Correlation).Then(toUpper).
		When(qovB.Correlation).Then(toUpper).
		Build()
	return qovA, qovB, upperQOV, m
}

// TestRFC173S3_TranslationMap_MergeCaseMiniature pins the load-bearing
// composition: the map swaps the QOV leaf for PLAIN ofOrdinalNumber over the
// upper (PartitionSelectRule.java:302 — the function owns the shape, never
// the composition), and the enclosing BAKED reference fuses during the
// rebuild into ONE node over the upper QOV (Java's withNewChild mechanic).
func TestRFC173S3_TranslationMap_MergeCaseMiniature(t *testing.T) {
	t.Parallel()
	qovA, _, upperQOV, m := mergeMiniature(t)

	bakedRef, err := NewFieldValueOfOrdinal(qovA, 1) // a.V, baked
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	out := TranslateCorrelations(bakedRef, m)
	fused, ok := out.(*FieldValue)
	if !ok || fused == bakedRef {
		t.Fatalf("translate = %T %v, want a NEW fused *FieldValue", out, out)
	}
	if fused.Child != upperQOV {
		t.Fatalf("fused child = %v, want the upper QOV", fused.Child)
	}
	want := NewFieldPathOfSingle(OrdinalFieldName(0), 0, true).WithSuffix(NewFieldPathOfSingle("V", 1, true))
	if fused.Resolved == nil || !fused.Resolved.Equals(want) {
		t.Fatalf("fused path = %+v, want [(_0,0),(V,1)]", fused.Resolved)
	}
	if fused.Field != "V" {
		t.Fatalf("fused display = %q, want the LAST step's name V", fused.Field)
	}

	// Runtime sanity over the merged positional row: root slot 0 (leg A's
	// row), descend to slot 1 (V).
	legARow := &fakeOrdinalRow{names: []string{"ID", "V"}, slots: []any{int64(7), int64(42)}}
	got, err := fused.Evaluate(&ordEvalBinder{id: upperQOV.Correlation, bound: &fakeOrdinalRow{
		names: []string{OrdinalFieldName(0), OrdinalFieldName(1)},
		slots: []any{legARow, nil},
	}})
	if err != nil || got != int64(42) {
		t.Fatalf("fused merge read = (%v, %v), want (42, nil)", got, err)
	}

	// A LAZY enclosing reference does NOT fuse (the coexistence gate): the
	// child is swapped in place and the node stays lazy over the baked
	// replacement — today's chained-eval semantics.
	lazyRef := NewFieldValue(qovA, "V", NotNullLong)
	lazyOut, ok := TranslateCorrelations(lazyRef, m).(*FieldValue)
	if !ok || lazyOut == lazyRef {
		t.Fatal("lazy ref must be rebuilt (child swapped), not returned unchanged")
	}
	if lazyOut.Resolved != nil {
		t.Fatalf("lazy ref must NOT fuse (gate requires both baked), got path %+v", lazyOut.Resolved)
	}
	if inner, ok := lazyOut.Child.(*FieldValue); !ok || inner.Resolved == nil || inner.Child != upperQOV {
		t.Fatalf("lazy ref's child = %v, want the baked ofOrdinal replacement over the upper QOV", lazyOut.Child)
	}
}

// TestRFC173S3_TranslationMap_IdentityAndMiss pins pointer stability: the
// identity early-out (DefinesOnlyIdentities) and a map whose aliases don't
// appear both return the INPUT pointer — CoW all the way down.
func TestRFC173S3_TranslationMap_IdentityAndMiss(t *testing.T) {
	t.Parallel()
	qovA, _, _, _ := mergeMiniature(t)
	ref, err := NewFieldValueOfOrdinal(qovA, 0)
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

// TestRFC173S3_TranslationMap_NeverRetranslate pins the visitNewLeaves=false
// semantics through TranslateCorrelations: a SELF-REFERENTIAL substitution
// (the replacement references the SAME source alias) terminates with exactly
// one application — Java TreeLike.java:242-262, Go ReplaceLeavesOnceMaybe.
func TestRFC173S3_TranslationMap_NeverRetranslate(t *testing.T) {
	t.Parallel()
	qovA, _, _, _ := mergeMiniature(t)
	applications := 0
	m := NewTranslationMapBuilder().
		When(qovA.Correlation).
		Then(func(_ CorrelationIdentifier, _ Value) Value {
			applications++
			// The replacement itself references alias a — a plain Replace
			// would re-match it and loop forever.
			fv, err := NewFieldValueOfOrdinal(qovA, 0)
			if err != nil {
				t.Fatalf("bake: %v", err)
			}
			return fv
		}).
		Build()

	ref := NewFieldValue(qovA, "ID", NotNullLong)
	out := TranslateCorrelations(ref, m)
	if applications != 1 {
		t.Fatalf("translation function applied %d times, want exactly 1 (replacement leaves are never re-translated)", applications)
	}
	outFV, ok := out.(*FieldValue)
	if !ok || outFV == ref {
		t.Fatalf("translate = %v, want a rebuilt node", out)
	}
}

// TestRFC173S3_RebuildFuse_EquivalentToCompose is the W2 ruling's demanded
// property pin: rebuild(fuse) ≡ compose(chain). Forcing a rebuild of a baked
// chain through the generic Replace spine (swapping the base QOV for a fresh
// equal-typed pointer) must produce the SAME fused node the simplifier's
// compose rule produces — two mechanisms, one canonical form.
func TestRFC173S3_RebuildFuse_EquivalentToCompose(t *testing.T) {
	t.Parallel()
	_, outer := bakedChain(t)
	composed := SimplifyValue(outer).(*FieldValue)

	baseQOV := composed.Child.(*QuantifiedObjectValue)
	freshQOV := NewQuantifiedObjectValueOfType(baseQOV.Correlation, baseQOV.Type())
	rebuilt := Replace(outer, func(v Value) Value {
		if v == baseQOV {
			return freshQOV
		}
		return v
	})
	fused, ok := rebuilt.(*FieldValue)
	if !ok {
		t.Fatalf("rebuild = %T, want *FieldValue", rebuilt)
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

// TestRFC173S3_RebuildFuse_GateDarkness pins the fuse gate on the rebuild
// arm: lazy and mixed chains rebuilt through Replace keep their CHAINED
// shape — fusing them would change memo identity and Explain corpus-wide
// (the W1 darkness argument, now applied to the rebuild path).
func TestRFC173S3_RebuildFuse_GateDarkness(t *testing.T) {
	t.Parallel()
	qov := s3NestedQOV(t)
	freshQOV := NewQuantifiedObjectValueOfType(qov.Correlation, qov.Type())
	swap := func(v Value) Value {
		if v == qov {
			return freshQOV
		}
		return v
	}

	// Lazy chain: rebuilt, still two chained lazy nodes.
	lazyOuter := NewFieldValue(NewFieldValue(qov, "NESTED", nil), "Y", NotNullLong)
	out := Replace(lazyOuter, swap).(*FieldValue)
	if out.Resolved != nil {
		t.Fatalf("lazy chain fused on rebuild (path %+v) — gate requires both baked", out.Resolved)
	}
	if inner, ok := out.Child.(*FieldValue); !ok || inner.Resolved != nil || inner.Child != freshQOV {
		t.Fatalf("lazy chain shape changed on rebuild: child = %v", out.Child)
	}

	// Mixed: lazy outer over baked inner — no fuse (outer not baked).
	bakedInner, err := NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	mixedOuter := NewFieldValue(bakedInner, "Y", NotNullLong)
	mixedOut := Replace(mixedOuter, swap).(*FieldValue)
	if mixedOut.Resolved != nil {
		t.Fatal("lazy-over-baked fused on rebuild — gate requires BOTH ends baked")
	}
}

// The TranslateLeafPredicates pins live in the predicates package
// (rfc173_s3_translate_leaf_test.go) — the spine belongs there and the
// import direction forbids testing it here.
