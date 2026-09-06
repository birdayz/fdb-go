package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// newPlanExprBaseForType has two exits and only one of them was ever driven.
// The ordinary one snapshots the flowed type, builds the identity layout, and
// publishes the layout's carrier. The other fires when that layout is REFUSED:
// an ERASED record — RECORD by code, not a *values.RecordType by construction —
// has no field vector to tile, so the values-owned validator rejects it, and
// this function falls back to minting a bare quantified object under a fresh
// correlation with NO physical properties at all.
//
// That second exit matters to RFC-242's argument rather than only to coverage.
// The argument is that a union's leg types are established once and that the
// planner's second alignment destroyed a property that already held; it rests
// on leg 0's type being the flowed type whichever route produced it. Three
// routes produce it, and this is the third — the one whose type identity comes
// from `SnapshotExactType` rather than from a carrier the layout constructed.
// A review lap enumerated two and was short by one, twice, which is what a
// count of branches does when nothing drives them.
//
// So this drives both exits and asserts what separates them, rather than only
// that neither errors: the erased exit publishes a result value and NO ordinal
// properties, the concrete exit publishes properties, and both carry a type
// that is Equals to what was asked for. Type equality alone would pass on the
// wrong exit, because both exits get the type right; the properties are what
// say which one ran.
func TestResultBaseForAnErasedRecordTakesTheCarrierlessExit(t *testing.T) {
	t.Parallel()

	erased := values.NewAnyRecordType(false)
	if _, concrete := erased.(*values.RecordType); concrete {
		t.Fatal("NewAnyRecordType returned a concrete *RecordType, so this test no longer " +
			"builds the erased shape it names and would drive the ordinary exit instead")
	}

	base, err := newPlanExprBaseForType("erased-record-test", erased)
	if err != nil {
		t.Fatalf("newPlanExprBaseForType over an erased record: %v — the fallback exit is "+
			"gone or no longer reached, and leg 0's type now comes from somewhere this "+
			"test does not describe", err)
	}
	if base.resultValue == nil {
		t.Fatal("the erased exit published no result value at all")
	}
	if got := base.resultValue.Type(); !got.Equals(erased) {
		t.Fatalf("erased exit published type %s, want %s: the fallback is supposed to carry "+
			"the snapshotted flowed type unchanged, which is what makes leg 0's type the "+
			"flowed type on this route too", got, erased)
	}
	if base.ordinalPhysicalProperties != nil {
		t.Fatal("the erased exit published ordinal properties — it is defined by having none, " +
			"so either the identity layout now accepts an erased record (and this exit is " +
			"dead) or the fallback started constructing one")
	}
}

// The control. Without it the assertion above passes for a function that
// always returns a carrierless base, and "no properties" would say nothing.
func TestResultBaseForAConcreteRecordTakesTheLayoutExit(t *testing.T) {
	t.Parallel()

	concrete := values.NewRecordType("R", false, []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NullableLong},
	})

	base, err := newPlanExprBaseForType("concrete-record-test", concrete)
	if err != nil {
		t.Fatalf("newPlanExprBaseForType over a concrete record: %v", err)
	}
	if base.ordinalPhysicalProperties == nil {
		t.Fatal("the concrete record published no ordinal properties, so this control no " +
			"longer separates the two exits and the erased assertion beside it is vacuous")
	}
	if got := base.resultValue.Type(); !got.Equals(concrete) {
		t.Fatalf("concrete exit published type %s, want %s", got, concrete)
	}
}
