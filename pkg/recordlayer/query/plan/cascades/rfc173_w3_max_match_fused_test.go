package cascades

// RFC-173 S3-W3 — the W2 EXIT criterion's matching half: max-match-map must
// handle FUSED multi-accessor paths (the compose/TranslationMap output shape
// live since the fulcrum). Verify prefix matching, and port Java's
// ExpandFusedFieldValueRule into MATCHING ONLY if it fails
// (MaxMatchMapSimplificationRuleSet.java:50 — never into the general
// simplifier; ComposeFieldValueOverFieldValueRule.java:41-43's stack-overflow
// warning is why they never co-reside).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// w3FusedRef builds the fused two-step ofOrdinal(m, slot).ofOrdinal(legOrd)
// reference the partition rule's rebase produces.
func w3FusedRef(t *testing.T, mergeQOV *values.QuantifiedObjectValue, slot, legOrd int) *values.FieldValue {
	t.Helper()
	step0, err := values.NewFieldValueOfOrdinal(mergeQOV, slot)
	if err != nil {
		t.Fatalf("bake _%d: %v", slot, err)
	}
	inner, err := values.NewFieldValueOfOrdinal(step0, legOrd)
	if err != nil {
		t.Fatalf("bake %d over _%d: %v", legOrd, slot, err)
	}
	fused, isFV := values.SimplifyValue(inner).(*values.FieldValue)
	if !isFV || fused.Resolved == nil || len(fused.Resolved.Accessors) != 2 {
		t.Fatalf("compose must fuse, got %T", values.SimplifyValue(inner))
	}
	return fused
}

func w3MergeQOV(name string) *values.QuantifiedObjectValue {
	leg := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
	})
	merged := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: leg, Ordinal: 0},
	})
	return values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(name), merged)
}

// TestRFC173W3_MaxMatchMap_FusedVsFused pins the direct case: a fused query
// value matches an identical fused candidate (structural equality over the
// ordinal path), and — post-identity-flip — a candidate whose path carries
// DIFFERENT display names at the same ordinals matches too (names are
// rendering, not identity; Java ResolvedAccessor.equals is ordinal-only).
func TestRFC173W3_MaxMatchMap_FusedVsFused(t *testing.T) {
	t.Parallel()
	m := w3MergeQOV("m")
	qv := w3FusedRef(t, m, 0, 1)
	cv := w3FusedRef(t, m, 0, 1)
	if mmm := ComputeMaxMatchMap(qv, cv, nil); mmm.Size() < 1 {
		t.Fatal("identical fused paths must max-match")
	}

	// Name-divergent twin: same child, same ordinal path, renamed steps.
	renamed := &values.FieldValue{
		Field: "RENAMED",
		Typ:   qv.Typ,
		Child: qv.Child,
		Resolved: values.NewFieldPathOfSingle("OTHER", 0, true).
			WithSuffix(values.NewFieldPathOfSingle("RENAMED", 1, true)),
	}
	if mmm := ComputeMaxMatchMap(qv, renamed, nil); mmm.Size() < 1 {
		t.Fatal("name-divergent fused twins (same ordinals) must max-match — ordinal-only identity (S3-W3)")
	}
}

// TestRFC173W3_MaxMatchMap_FusedVsChained pins the ported
// ExpandFusedFieldValueRule (matching-only, Java's exact placement -
// MaxMatchMapSimplificationRuleSet.java:50): the QUERY side is the FUSED
// one-node form (the compose rule's output), the CANDIDATE side the CHAINED
// two-node form (FieldValue over FieldValue - the pre-compose shape
// candidate builders produce). The expansion splits the fused path's last
// step so the pieces match - without it this returned NO match (verified red
// before the port; the W2 exit criterion's trigger).
func TestRFC173W3_MaxMatchMap_FusedVsChained(t *testing.T) {
	t.Parallel()
	m := w3MergeQOV("m")
	qv := w3FusedRef(t, m, 0, 1)

	// The chained candidate: outer lazy-ish baked step over an inner baked
	// step — the PRE-compose shape (two nodes, one accessor each).
	innerStep, err := values.NewFieldValueOfOrdinal(m, 0)
	if err != nil {
		t.Fatalf("bake _0: %v", err)
	}
	chained, err := values.NewFieldValueOfOrdinal(innerStep, 1)
	if err != nil {
		t.Fatalf("bake 1 over _0: %v", err)
	}

	mmm := ComputeMaxMatchMap(qv, chained, nil)
	if mmm.Size() < 1 {
		t.Fatal("fused query vs chained candidate must match through the ported ExpandFusedFieldValueRule (red without it)")
	}
}

// TestRFC173W3_MaxMatchMap_FusedVsChained_ThreeAccessor pins the DEEPER
// dimension the two-accessor pin missed (a review catch): a THREE-accessor
// fused path (a doubly-nested merge — `m._0._0.C`) must match a fully-CHAINED
// three-node candidate. The last-accessor-only split left the 2-accessor
// prefix fused, and the matcher compares the child before it could re-split,
// so this returned NO match (verified red before the full-unchain fix). The
// port now fully unchains in one step — Java's re-explored end state reached
// directly.
func TestRFC173W3_MaxMatchMap_FusedVsChained_ThreeAccessor(t *testing.T) {
	t.Parallel()
	leaf := values.NewRecordType("", false, []values.Field{{Name: "C", FieldType: values.NotNullLong, Ordinal: 0}})
	mid := values.NewRecordType("", false, []values.Field{{Name: values.OrdinalFieldName(0), FieldType: leaf, Ordinal: 0}})
	top := values.NewRecordType("", false, []values.Field{{Name: values.OrdinalFieldName(0), FieldType: mid, Ordinal: 0}})
	q := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), top)

	s0, err := values.NewFieldValueOfOrdinal(q, 0)
	if err != nil {
		t.Fatalf("bake _0: %v", err)
	}
	s1, err := values.NewFieldValueOfOrdinal(s0, 0)
	if err != nil {
		t.Fatalf("bake _0._0: %v", err)
	}
	s2, err := values.NewFieldValueOfOrdinal(s1, 0)
	if err != nil {
		t.Fatalf("bake _0._0.C: %v", err)
	}
	fused, isFV := values.SimplifyValue(s2).(*values.FieldValue)
	if !isFV || len(fused.Resolved.Accessors) != 3 {
		t.Fatalf("compose must fuse to a 3-accessor path, got %T (accessors=%v)", values.SimplifyValue(s2), fused)
	}

	// The chained candidate: three single-accessor nodes (never composed).
	c0, err := values.NewFieldValueOfOrdinal(q, 0)
	if err != nil {
		t.Fatalf("chained _0: %v", err)
	}
	c1, err := values.NewFieldValueOfOrdinal(c0, 0)
	if err != nil {
		t.Fatalf("chained _0._0: %v", err)
	}
	c2, err := values.NewFieldValueOfOrdinal(c1, 0)
	if err != nil {
		t.Fatalf("chained _0._0.C: %v", err)
	}
	if len(c2.Resolved.Accessors) != 1 {
		t.Fatalf("chained candidate node must carry ONE accessor, got %d", len(c2.Resolved.Accessors))
	}

	if mmm := ComputeMaxMatchMap(fused, c2, nil); mmm.Size() < 1 {
		t.Fatal("3-accessor fused must match the fully-chained candidate — the last-accessor-only split missed this (full-unchain fix)")
	}
}
