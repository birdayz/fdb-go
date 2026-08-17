package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestBakedBoxRefCallback_FusedUnpinnedRebasesToCorrectLeg pins the box-merge
// collapse (bakedBoxRefCallback) against the F0 root cause in its PLAN-TIME
// spelling. The box concat RC is [A.HDR, A.Y, B.HDR, B.Z]; the box is named by its
// rightmost leg B, so a leg-addressed reference B.HDR.SUB arrives under the box
// alias. That reference is a FUSED unpinned node ([{HDR,0},{SUB,0}], root leg
// ordinal 0). The collapse must re-base the leg-relative ROOT into the seed by
// the reference's OWN leg (LegAwareRootOrdinal) and fuse the .SUB suffix onto
// B.HDR (seed slot 2) — keeping Child == leg B.
//
// The gate used to be SourceRelativeBaked (single-accessor only), so the fused
// node kept its RAW leg-relative root 0 and fused .SUB onto seed slot 0 = A.X:
// a silent WRONG-leg comparand (the same failure the single-accessor comment
// records once turned `e.id IS NULL` into `d.id IS NULL`, zero rows). The gate
// is now RootIsLegRelativeUnpinned, which covers the fused twin.
func TestBakedBoxRefCallback_FusedUnpinnedRebasesToCorrectLeg(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b") // the box is named by its rightmost leg B

	hdrType := values.NewRecordType("HDR", false, []values.Field{{Name: "SUB", FieldType: values.NotNullLong, Ordinal: 0}})
	legAType := values.NewRecordType("A", false, []values.Field{{Name: "HDR", FieldType: hdrType, Ordinal: 0}, {Name: "Y", FieldType: values.NotNullLong, Ordinal: 1}})
	legBType := values.NewRecordType("B", false, []values.Field{{Name: "HDR", FieldType: hdrType, Ordinal: 0}, {Name: "Z", FieldType: values.NotNullLong, Ordinal: 1}})
	qovA := selectMergeQOV(t, corrA, legAType)
	qovB := selectMergeQOV(t, corrB, legBType)

	// The box concat seed RC: each field is an exact FieldValue over its own leg
	// QOV. The callback uses those correlations and leg-local ordinals only.
	mk := func(qov values.QuantifiedObjectValue, ord int) values.RecordConstructorField {
		value := selectMergeOrdinalSeedField(t, qov, ord)
		field, ok := values.AsFieldValue(value)
		if !ok {
			t.Fatalf("ResolveOrdinalSeedField(%v,%d) = %T, want admitted FieldValue", qov.Correlation(), ord, value)
		}
		return values.RecordConstructorField{Name: field.DisplayName(), Value: field}
	}
	boxRC := values.NewRawRecordConstructorValue(mk(qovA, 0), mk(qovA, 1), mk(qovB, 0), mk(qovB, 1))
	rcByAlias := map[values.CorrelationIdentifier]values.Value{corrB: boxRC}

	// The FUSED unpinned reference B.HDR.SUB addressed at the box alias. Both
	// legs intentionally expose the same nested type at local ordinal 0, making
	// the exact source-relative [0,0] path constructible while the B correlation
	// remains the sole authority that selects RC slot 2 instead of A's slot 0.
	boxType := values.NewRecordType("BOX", false, []values.Field{
		{Name: "A_HDR", FieldType: hdrType, Ordinal: 0},
		{Name: "A_Y", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "B_HDR", FieldType: hdrType, Ordinal: 2},
		{Name: "B_Z", FieldType: values.NotNullLong, Ordinal: 3},
	})
	refChild := selectMergeQOV(t, corrB, boxType)
	fusedRefValue := selectMergeFieldOrdinals(t, refChild, 0, 0)
	fusedRef, ok := values.AsFieldValue(fusedRefValue)
	if !ok || fusedRef.Path() == nil {
		t.Fatalf("failed to build exact fused reference: %T", fusedRefValue)
	}
	if fusedRef.Path().Len() != 2 || fusedRef.Path().IsFrontierPinned() {
		t.Fatalf("fused ref shape = (accessors %d, pinned %v), want (2, false)", fusedRef.Path().Len(), fusedRef.Path().IsFrontierPinned())
	}

	collapsedValue := bakedBoxRefCallback(rcByAlias)(fusedRef)
	collapsed, ok := values.AsFieldValue(collapsedValue)
	if !ok {
		t.Fatalf("collapse produced %T, want admitted FieldValue", collapsedValue)
	}
	childQov, ok := values.AsQuantifiedObjectValue(collapsed.ChildValue())
	if !ok {
		t.Fatalf("collapsed child = %T, want admitted QuantifiedObjectValue", collapsed.ChildValue())
	}
	if childQov.Correlation() != corrB {
		t.Fatalf("box-merge collapse landed on leg %v, want %v (leg B) — the fused suffix fused onto the WRONG leg's seed field (raw-root collapse)", childQov.Correlation(), corrB)
	}
	path := collapsed.Path()
	ordinals := path.Ordinals()
	suffix, hasSuffix := path.Accessor(1)
	suffixName := ""
	if hasSuffix {
		suffixName, _ = suffix.DisplayName()
	}
	if len(ordinals) != 2 || ordinals[0] != 0 || ordinals[1] != 0 || suffixName != "SUB" {
		t.Fatalf("collapsed path = %v, want [{HDR,0},{SUB,0}] over leg B", ordinals)
	}
}
