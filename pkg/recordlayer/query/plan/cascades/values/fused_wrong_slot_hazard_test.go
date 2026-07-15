package values

import (
	"errors"
	"testing"
)

// fakeMultiLegRow is a test-local OrdinalRow that reports itself MULTI-LEG (the
// merged-concat / clustered-box shape executor.PositionalRow exposes via
// MultiLeg()). The correct-or-loud eval guards consult this via
// rowIsMultiLeg — a leg-relative baked ordinal cannot be served by such a row
// without a leg binding, so the fall-through arms must go LOUD instead of
// reading a foreign leg's slot.
type fakeMultiLegRow struct {
	names []string
	slots []any
}

func (r *fakeMultiLegRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= len(r.slots) {
		return nil, false
	}
	return r.slots[ord], true
}

func (r *fakeMultiLegRow) MultiLeg() bool      { return true }
func (r *fakeMultiLegRow) TypeNames() []string { return r.names }

// TestFusedUnpinned_WrongSlotHazard is the values-level red→green pin for the
// FUSED-unpinned wrong-slot hole (RFC-173 F0). A source-relative leg reference
// keeps a LEG-RELATIVE ROOT ordinal. Its SINGLE-accessor spelling is refused
// over a multi-leg row (SourceRelativeBaked catches it). The FUSED spelling —
// composeFieldOverField / the withChildren rebuild-fuse append a suffix, so the
// path is MULTI-accessor while the root stays leg-relative and unpinned — used
// to slip the guard (SourceRelativeBaked requires len(Accessors)==1) and read a
// FOREIGN leg's root slot, then descend into it: silently NULL (unpinned
// default arm) or a wrong nested value. The guards now key on the ROOT's
// leg-relativity (RootIsLegRelativeUnpinned), not the accessor count, so the
// fused twin ALSO goes loud — restoring the property Java gets structurally
// (FieldValue.eval resolves the ordinal against the CHILD's own flowed Message,
// FieldValue.java:164-175, never a differently-composed row).
func TestFusedUnpinned_WrongSlotHazard(t *testing.T) {
	t.Parallel()

	// A 2-leg merged row: leg A occupies slots [0,1], leg B slots [2,3]. Leg B's
	// HDR is at LEG-relative ordinal 0 = ABSOLUTE merged slot 2. A leg-oblivious
	// read of root ordinal 0 lands on slot 0 = leg A's X (the FOREIGN leg) — the
	// exact wrong-slot misread the guard exists to prevent.
	mlRow := &fakeMultiLegRow{
		names: []string{"A.X", "A.Y", "B.HDR", "B.Z"},
		slots: []any{int64(1), int64(2), int64(99), int64(4)},
	}

	corrB := NamedCorrelationIdentifier("b")
	qovB := NewQuantifiedObjectValue(corrB)
	hdrType := NewRecordType("", false, []Field{{Name: "SUB", FieldType: NotNullLong, Ordinal: 0}})

	assertLoud := func(t *testing.T, site string, _ any, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: want a LOUD *UnboundEvalContextError, got a silent value (correct-or-loud violated)", site)
		}
		var uce *UnboundEvalContextError
		if !errors.As(err, &uce) {
			t.Fatalf("%s: want *UnboundEvalContextError, got %v", site, err)
		}
	}

	// (control) The SINGLE-accessor leg reference B.HDR#0 — already loud over a
	// multi-leg row (SourceRelativeBaked catches it). Kept to prove the fused
	// twin is held to the SAME correct-or-loud bar as its single-accessor peer.
	single := NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "HDR", 0, hdrType)
	if !single.SourceRelativeBaked() {
		t.Fatal("control: single-accessor leg ref must be SourceRelativeBaked")
	}
	v, err := single.Evaluate(&RowEvalContext{Positional: mlRow})
	assertLoud(t, "single-accessor B.HDR#0 over multi-leg RowEvalContext", v, err)
	v, err = single.Evaluate(mlRow)
	assertLoud(t, "single-accessor B.HDR#0 over bare multi-leg OrdinalRow", v, err)

	// Build the FUSED spelling via the ACTUAL producer named in the finding:
	// composeFieldOverField(field(field(qovB, HDR#0), SUB#0)) fuses the chain
	// into one node carrying [{HDR,0},{SUB,0}], root UNPINNED and leg-relative.
	inner := &FieldValue{Field: "HDR", Typ: hdrType, Child: qovB, Resolved: NewFieldPathOfSingle("HDR", 0, false)}
	outer := &FieldValue{Field: "SUB", Typ: NotNullLong, Child: inner, Resolved: NewFieldPathOfSingle("SUB", 0, false)}
	fusedV := composeFieldOverField(outer)
	if fusedV == nil {
		t.Fatal("composeFieldOverField declined the baked-over-baked chain")
	}
	fused := fusedV.(*FieldValue)

	// The shape that used to slip the guard: MULTI-accessor, root UNPINNED. The
	// OLD guard keyed on SourceRelativeBaked (which requires a single accessor)
	// and so MISSED this node; the NEW guard keys on the leg-relative root.
	if len(fused.Resolved.Accessors) != 2 {
		t.Fatalf("fused node accessors = %d, want 2 (HDR then SUB)", len(fused.Resolved.Accessors))
	}
	if fused.Resolved.FrontierPinned {
		t.Fatal("fused node must stay UNPINNED (WithSuffix inherits the inner's unpinned root)")
	}
	if fused.SourceRelativeBaked() {
		t.Fatal("fused node must NOT be SourceRelativeBaked (multi-accessor) — this is exactly why the old guard was skipped")
	}
	if !fused.RootIsLegRelativeUnpinned() {
		t.Fatal("fused node MUST be RootIsLegRelativeUnpinned — the new guard's key")
	}
	if _, isQOV := fused.Child.(*QuantifiedObjectValue); !isQOV {
		t.Fatalf("fused node child = %T, want *QuantifiedObjectValue (eval routes correlated)", fused.Child)
	}

	// The fused twin over the SAME multi-leg context — both eval arms. Old code
	// read slot 0 (leg A's X=1) then descended .SUB into an int64 → (nil, nil)
	// silently. Correct-or-loud now refuses.
	v, err = fused.Evaluate(&RowEvalContext{Positional: mlRow})
	assertLoud(t, "fused B.HDR.SUB over multi-leg RowEvalContext", v, err)
	v, err = fused.Evaluate(mlRow)
	assertLoud(t, "fused B.HDR.SUB over bare multi-leg OrdinalRow", v, err)

	// withChildren-parity: the rebuild-fuse (WithChildren over a baked
	// FieldValue child) must produce the SAME fused shape and be held to the
	// same loud bar — Java's withNewChild == ofFieldsAndFuseIfPossible.
	rebuiltV := withChildren(outer, []Value{inner})
	rebuilt, ok := rebuiltV.(*FieldValue)
	if !ok {
		t.Fatalf("withChildren rebuild produced %T, want *FieldValue", rebuiltV)
	}
	if len(rebuilt.Resolved.Accessors) != 2 || rebuilt.Resolved.FrontierPinned {
		t.Fatalf("withChildren rebuild shape = (accessors %d, pinned %v), want (2, false) — identical to composeFieldOverField", len(rebuilt.Resolved.Accessors), rebuilt.Resolved.FrontierPinned)
	}
	if !rebuilt.Resolved.Equals(fused.Resolved) {
		t.Fatal("withChildren rebuild must fuse to the IDENTICAL path as composeFieldOverField")
	}
	v, err = rebuilt.Evaluate(&RowEvalContext{Positional: mlRow})
	assertLoud(t, "withChildren-fused B.HDR.SUB over multi-leg RowEvalContext", v, err)
	v, err = rebuilt.Evaluate(mlRow)
	assertLoud(t, "withChildren-fused B.HDR.SUB over bare multi-leg OrdinalRow", v, err)

	// GUARDRAIL: a SINGLE-LEG row is UNAFFECTED — the fix only converts
	// silent-wrong to loud for a leg-relative root over a MULTI-leg row. Here the
	// fused node reads root ordinal 0 = HDR (the record), descends .SUB = 42.
	singleLeg := &fakeOrdinalRow{
		names: []string{"HDR", "Z"},
		slots: []any{&fakeOrdinalRow{names: []string{"SUB"}, slots: []any{int64(42)}}, int64(4)},
	}
	got, err := fused.Evaluate(&RowEvalContext{Positional: singleLeg})
	if err != nil {
		t.Fatalf("fused node over a SINGLE-leg row must resolve normally, got error %v", err)
	}
	if got != int64(42) {
		t.Fatalf("fused B.HDR.SUB over single-leg row = %v, want 42 (root ord 0 = HDR record, descend .SUB)", got)
	}
}

// TestFusedUnpinned_RCCollapse_RebasesToCorrectLeg pins the PLAN-TIME twin of
// the F0 root cause. When a merge TranslationMap replaces a fused node's leg QOV
// with the box's ordinal seed RC (withChildren over an RC literal), the collapse
// re-bases the UNPINNED leg-relative ROOT into the seed by the reference's LEG
// (LegAwareRootOrdinal). The collapse used to gate that rebase on
// SourceRelativeBaked (single-accessor only), so a FUSED unpinned node kept its
// RAW leg-relative root and fused the suffix onto seed field 0 — the FIRST leg,
// not the reference's own. For a NON-first-leg reference (B.HDR.SUB over a
// [A.X, A.Y, B.HDR, B.Z] seed) that silently produced A.X.SUB: a wrong-leg
// PINNED node the eval guard cannot catch (it fires only for unpinned roots).
// Rebasing on the leg-relative ROOT (RootIsLegRelativeUnpinned) lands the fuse
// on the B.HDR seed field — Java's structural quantifier rebase, no raw-ordinal
// carry-over.
func TestFusedUnpinned_RCCollapse_RebasesToCorrectLeg(t *testing.T) {
	t.Parallel()
	corrA := NamedCorrelationIdentifier("a")
	corrB := NamedCorrelationIdentifier("b")
	hdrType := NewRecordType("HDR", false, []Field{{Name: "SUB", FieldType: NotNullLong, Ordinal: 0}})
	legAType := NewRecordType("A", false, []Field{{Name: "X", FieldType: NotNullLong, Ordinal: 0}, {Name: "Y", FieldType: NotNullLong, Ordinal: 1}})
	legBType := NewRecordType("B", false, []Field{{Name: "HDR", FieldType: hdrType, Ordinal: 0}, {Name: "Z", FieldType: NotNullLong, Ordinal: 1}})
	qovA := NewQuantifiedObjectValueOfType(corrA, legAType)
	qovB := NewQuantifiedObjectValueOfType(corrB, legBType)

	// The machinery seed RC: each field a FrontierPinned FieldValue over its OWN
	// leg's QOV, legs concatenated (NewRawRecordConstructorValue) —
	// [A.X, A.Y, B.HDR, B.Z]. B.HDR sits at seed slot 2, leg-relative slot 0.
	mk := func(qov *QuantifiedObjectValue, ord int) RecordConstructorField {
		fv, err := NewFieldValueOfOrdinal(qov, ord)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal(%v, %d): %v", qov.Correlation, ord, err)
		}
		return RecordConstructorField{Name: fv.Field, Value: fv}
	}
	seedRC := NewRawRecordConstructorValue(mk(qovA, 0), mk(qovA, 1), mk(qovB, 0), mk(qovB, 1))

	// The FUSED unpinned reference B.HDR.SUB: leg-relative root ordinal 0.
	inner := &FieldValue{Field: "HDR", Typ: hdrType, Child: qovB, Resolved: NewFieldPathOfSingle("HDR", 0, false)}
	outer := &FieldValue{Field: "SUB", Typ: NotNullLong, Child: inner, Resolved: NewFieldPathOfSingle("SUB", 0, false)}
	fused := composeFieldOverField(outer).(*FieldValue)

	// Collapse over the seed RC (the merge TranslationMap's QOV→seed replacement).
	collapsed, ok := withChildren(fused, []Value{seedRC}).(*FieldValue)
	if !ok {
		t.Fatalf("collapse produced %T, want *FieldValue", withChildren(fused, []Value{seedRC}))
	}
	childQov, ok := collapsed.Child.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("collapsed child = %T, want *QuantifiedObjectValue (fused onto a leg seed field)", collapsed.Child)
	}
	// The discriminator: the collapse must land on leg B (the reference's own
	// leg), NOT leg A (seed slot 0). Raw-root collapse picked corrA.
	if childQov.Correlation != corrB {
		t.Fatalf("collapsed onto correlation %v, want %v (leg B) — the fused suffix fused onto the WRONG leg's seed field (raw-root collapse)", childQov.Correlation, corrB)
	}
	// And the fused path descends HDR then SUB (leg B's HDR record's SUB).
	if len(collapsed.Resolved.Accessors) != 2 || collapsed.Resolved.Accessors[0].Ordinal != 0 || collapsed.Resolved.Accessors[1].Field != "SUB" {
		t.Fatalf("collapsed path = %v, want [{HDR,0},{SUB,0}] over leg B", collapsed.Resolved.Accessors)
	}
}
