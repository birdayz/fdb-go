package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestBakedBoxRefCallback_FusedUnpinnedRebasesToCorrectLeg pins the box-merge
// collapse (bakedBoxRefCallback) against the F0 root cause in its PLAN-TIME
// spelling. The box concat RC is [A.X, A.Y, B.HDR, B.Z]; the box is named by its
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
	legAType := values.NewRecordType("A", false, []values.Field{{Name: "X", FieldType: values.NotNullLong, Ordinal: 0}, {Name: "Y", FieldType: values.NotNullLong, Ordinal: 1}})
	legBType := values.NewRecordType("B", false, []values.Field{{Name: "HDR", FieldType: hdrType, Ordinal: 0}, {Name: "Z", FieldType: values.NotNullLong, Ordinal: 1}})
	qovA := values.NewQuantifiedObjectValueOfType(corrA, legAType)
	qovB := values.NewQuantifiedObjectValueOfType(corrB, legBType)

	// The box concat seed RC: each field a pinned FieldValue over its own leg QOV.
	mk := func(qov *values.QuantifiedObjectValue, ord int) values.RecordConstructorField {
		fv, err := values.NewFieldValueOfOrdinal(qov, ord)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal(%v,%d): %v", qov.Correlation, ord, err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}
	boxRC := values.NewRawRecordConstructorValue(mk(qovA, 0), mk(qovA, 1), mk(qovB, 0), mk(qovB, 1))
	rcByAlias := map[values.CorrelationIdentifier]values.Value{corrB: boxRC}

	// The FUSED unpinned reference B.HDR.SUB addressed at the box alias. An
	// UNTYPED reference QOV is box-level (boxLevel treats a non-record type as the
	// box's bare untyped RV), which is the shape the WHERE-EXISTS wrapper flows.
	refChild := values.NewQuantifiedObjectValue(corrB)
	inner := &values.FieldValue{Field: "HDR", Typ: hdrType, Child: refChild, Resolved: values.NewFieldPathOfSingle("HDR", 0, false)}
	outer := &values.FieldValue{Field: "SUB", Typ: values.NotNullLong, Child: inner, Resolved: values.NewFieldPathOfSingle("SUB", 0, false)}
	fusedRef, ok := values.WithChildren(outer, []values.Value{inner}).(*values.FieldValue)
	if !ok {
		t.Fatal("failed to build fused reference")
	}
	if len(fusedRef.Resolved.Accessors) != 2 || fusedRef.Resolved.FrontierPinned || fusedRef.SourceRelativeBaked() {
		t.Fatalf("fused ref shape = (accessors %d, pinned %v, srb %v), want (2, false, false)", len(fusedRef.Resolved.Accessors), fusedRef.Resolved.FrontierPinned, fusedRef.SourceRelativeBaked())
	}

	collapsed, ok := bakedBoxRefCallback(rcByAlias)(fusedRef).(*values.FieldValue)
	if !ok {
		t.Fatalf("collapse produced %T, want *FieldValue", bakedBoxRefCallback(rcByAlias)(fusedRef))
	}
	childQov, ok := collapsed.Child.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("collapsed child = %T, want *QuantifiedObjectValue", collapsed.Child)
	}
	if childQov.Correlation != corrB {
		t.Fatalf("box-merge collapse landed on leg %v, want %v (leg B) — the fused suffix fused onto the WRONG leg's seed field (raw-root collapse)", childQov.Correlation, corrB)
	}
	if len(collapsed.Resolved.Accessors) != 2 || collapsed.Resolved.Accessors[0].Ordinal != 0 || collapsed.Resolved.Accessors[1].Field != "SUB" {
		t.Fatalf("collapsed path = %v, want [{HDR,0},{SUB,0}] over leg B", collapsed.Resolved.Accessors)
	}
}
