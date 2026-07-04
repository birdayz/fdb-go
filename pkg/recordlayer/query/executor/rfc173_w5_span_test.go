package executor

// RFC-173 W5 commit 2 — executor pins for the span-derivation extension (the
// gathered unnest's MIXED element carried through a partition collapse) and
// the design-ruling Q6 dimension pin (the NLJ birth's nil-legRVs windows).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRFC173W5_MixedElementSpanSynthesis pins the terminal synthesis: a
// SINGLE-accessor pinned ref over a merge quantifier whose referenced slot is
// a bare NON-RECORD QOV (the gathered unnest's mixed element after the
// partition collapsed {source, Explode}) resolves to a synthesized 1-field
// element leg — alias = the SLOT QOV's correlation (the AS alias, never the
// merge alias), the sole column named from the enclosing RC field. Without
// legRVs the shape must DECLINE (fail-safe, never mis-windowed); with a
// RECORD slot the merge-leg run resolution is UNCHANGED (the load-bearing
// non-record guard).
func TestRFC173W5_MixedElementSpanSynthesis(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qovS := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("S"), legS)
	elemQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("EL"), values.NotNullLong)

	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: values.NotNullLong, Ordinal: 1},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)

	elemRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 1)
	if err != nil {
		t.Fatalf("bake element slot: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "ARR", Value: s3FusedRef(t, mergeQOV, 0, 1)},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	legRVs := map[values.CorrelationIdentifier]values.Value{
		values.NamedCorrelationIdentifier("m"): values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: qovS},
			values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: elemQOV},
		),
	}

	spans, _, ok := ordinalJoinSpansOf(top, legRVs)
	if !ok {
		t.Fatal("the mixed-element translated top must derive windows (the W5 span synthesis)")
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (the S run + the synthesized element leg)", len(spans))
	}
	if spans[0].Alias.Name() != "S" || spans[0].Width != 2 {
		t.Fatalf("span 0 = %s width %d, want the full S run (width 2)", spans[0].Alias, spans[0].Width)
	}
	el := spans[1]
	if el.Alias.Name() != "EL" {
		t.Fatalf("element span alias = %s, want the SLOT QOV's correlation EL (never the merge alias)", el.Alias)
	}
	if el.Width != 1 || len(el.LegType.Fields) != 1 || el.LegType.Fields[0].Name != "EL" {
		t.Fatalf("element leg = %+v, want the 1-field synthesis named from the enclosing RC field", el.LegType)
	}

	// Fail-safe: no legRVs → the fused outer refs cannot resolve → decline.
	if _, _, ok := ordinalJoinSpansOf(top, nil); ok {
		t.Fatal("the translated top must DECLINE without legRVs")
	}

	// The non-record guard: a RECORD slot referenced single-accessor is a
	// merge-leg run, NOT an element — the pristine positional-merge RC's own
	// consumers keep resolving unchanged (partial coverage here, so the probe
	// declines rather than synthesizing a bogus leg).
	recRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 0)
	if err != nil {
		t.Fatalf("bake record slot: %v", err)
	}
	topRec := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "R", Value: recRef},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	if _, _, ok := ordinalJoinSpansOf(topRec, legRVs); ok {
		t.Fatal("a RECORD slot must not synthesize an element leg (partial merge-run coverage declines)")
	}
}

// TestRFC173W5_NLJBirthNilLegRVsDimension is the design-ruling Q6 pin: the
// NLJ birth derives its spans WITHOUT legRVs (newOrdinalJoinBirth), so a
// TRANSLATED top (fused post-merge refs) births with WindowsOK=false — the
// windows are recovered downstream (downstreamLegWindows) by consumers, and
// the birth's own Datum stays name-model bare. This dimension is a LOUD
// decline today, pinned as such; per the ruling it PROMOTES to a fix
// immediately if it ever produces a wrong ANSWER instead (the fix — NLJ-side
// legRV recovery like the FlatMap's — is otherwise an S4-rider, since S4 may
// obsolete the windows entirely).
func TestRFC173W5_NLJBirthNilLegRVsDimension(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	b0, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("bake B#0: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "BID", Value: b0},
	)

	birth, err := newOrdinalJoinBirth(top, nil)
	if err != nil {
		t.Fatalf("birth: %v", err)
	}
	if birth == nil || !birth.Enabled {
		t.Fatal("a baked translated top must birth ordinal (positional authority intact)")
	}
	if birth.WindowsOK {
		t.Fatal("the NLJ birth has no legRVs — a translated top's windows must NOT derive at birth (they are recovered downstream); if this ever flips silently, re-check the Datum name-model reads of this shape")
	}
}
