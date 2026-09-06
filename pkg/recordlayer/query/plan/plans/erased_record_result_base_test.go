package plans

import (
	"errors"
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
	// %s renders both an erased record and a nullable one as "RECORD", so a bare
	// pair reads "RECORD, want RECORD" on the failure that matters most. Print the
	// Go type and the nullability beside it.
	if got := base.resultValue.Type(); !got.Equals(erased) {
		t.Fatalf("erased exit published type %s (%T, nullable=%v), want %s (%T, nullable=%v): "+
			"the fallback is supposed to carry the snapshotted flowed type unchanged, which is "+
			"what makes leg 0's type the flowed type on this route too",
			got, got, got.IsNullable(), erased, erased, erased.IsNullable())
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

// The fourth route, and the one three enumerations missed.
//
// `newPlanExprBaseForQuantifier` has its own exits, and only its LAST one falls
// through to `newPlanExprBaseForType` (the function the two tests above drive).
// When the quantifier HAS a child whose provided layout is unavailable with
// `OrdinalLayoutDynamicCarrier` — which is exactly what a child plan carrying an
// erased record reports — it returns the quantifier's own flowed object value
// directly, with no layout, no carrier substitution and no `Equals` check.
//
// It is production-reachable rather than hypothetical: `NewAnyRecordType` is
// constructed at `proto_field_type.go:196`, so a leaf whose declared type is an
// erased record can be a union leg's child, and the union's leg-0 base is
// computed by this function.
//
// It satisfies RFC-242's invariant trivially — nothing is substituted, so leg 0's
// type IS the flowed type — which is precisely why the RFC states an invariant
// rather than a list. Three laps enumerated the routes and every one was short.
// This pins the route so the fourth correction is not prose.
func TestResultBaseForAQuantifierOverAnErasedChildKeepsTheFlowedValue(t *testing.T) {
	t.Parallel()

	erased := values.NewAnyRecordType(false)
	child, err := NewRecordQueryScanPlan([]string{"T"}, erased, false)
	if err != nil {
		t.Fatalf("scan over an erased record: %v", err)
	}
	// The precondition this route is defined by. Without it the assertions below
	// would pass for a plan whose layout is merely malformed, which is a
	// different error and a different branch.
	if _, layoutErr := child.ProvidedOutputLayout(); layoutErr == nil {
		t.Fatal("the erased child published a layout, so this test no longer reaches the " +
			"dynamic-carrier route it names")
	} else {
		var unavailable *OrdinalLayoutUnavailableError
		if !errors.As(layoutErr, &unavailable) || unavailable.Code != OrdinalLayoutDynamicCarrier {
			t.Fatalf("the erased child's layout error is %v, want OrdinalLayoutDynamicCarrier: "+
				"this route is selected by that code specifically", layoutErr)
		}
	}

	q := QuantifierOverPlan(child)
	// And the other half of the precondition: a NON-nil child is what separates
	// this exit from the `newPlanExprBaseForType` fallback, which produces a base
	// of the same shape one function down.
	if selectedPlanFromQuantifier(q) == nil {
		t.Fatal("the quantifier has no selected child, so a carrierless result below would be " +
			"the newPlanExprBaseForType fallback rather than this route")
	}

	base, err := newPlanExprBaseForQuantifier("erased-child-test", q)
	if err != nil {
		t.Fatalf("newPlanExprBaseForQuantifier over an erased child: %v", err)
	}
	if base.resultValue == nil {
		t.Fatal("the dynamic-carrier route published no result value")
	}
	if got := base.resultValue.Type(); !got.Equals(erased) {
		t.Fatalf("dynamic-carrier route published type %s (%T), want %s (%T): this route is "+
			"supposed to hand back the quantifier's own flowed value untouched, which is what "+
			"makes it satisfy the invariant trivially", got, got, erased, erased)
	}
	// The type check above cannot tell this route from the fallback one function
	// down: `newPlanExprBaseForType` over the SAME erased type also yields a
	// carrierless base of that exact type, so every assertion here and the
	// concrete control stay green if this route is replaced by it. What differs
	// is WHOSE value comes back — that fallback mints a fresh
	// `UniqueCorrelationIdentifier`, while this route is defined by handing the
	// quantifier's own flowed value straight through. So the identity is the
	// assertion, and the type is only its shape.
	qov, isQOV := values.AsQuantifiedObjectValue(base.resultValue)
	if !isQOV {
		t.Fatalf("the dynamic-carrier route published %T, want a QuantifiedObjectValue: this "+
			"route returns the quantifier's flowed value, which is one by construction",
			base.resultValue)
	}
	if got, want := qov.Correlation(), q.GetAlias(); got != want {
		t.Fatalf("the dynamic-carrier route published correlation %v, want the quantifier's own "+
			"%v: a FRESH correlation here means the newPlanExprBaseForType fallback ran instead, "+
			"which this test cannot distinguish by type alone", got, want)
	}
	if base.ordinalPhysicalProperties != nil {
		t.Fatal("the dynamic-carrier route published ordinal properties — it is defined by " +
			"returning before any layout is built")
	}
}

// The control for the route above: the same construction over a CONCRETE record
// takes the pass-through exit and DOES publish a layout. Without it, "no
// properties" says nothing about which of the two exits ran.
func TestResultBaseForAQuantifierOverAConcreteChildTakesThePassThrough(t *testing.T) {
	t.Parallel()

	concrete := values.NewRecordType("R", false, []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NullableLong},
	})
	child, err := NewRecordQueryScanPlan([]string{"T"}, concrete, false)
	if err != nil {
		t.Fatalf("scan over a concrete record: %v", err)
	}
	if _, layoutErr := child.ProvidedOutputLayout(); layoutErr != nil {
		t.Fatalf("the concrete child published no layout (%v), so this control no longer "+
			"separates the two exits", layoutErr)
	}
	base, err := newPlanExprBaseForQuantifier("concrete-child-test", QuantifierOverPlan(child))
	if err != nil {
		t.Fatalf("newPlanExprBaseForQuantifier over a concrete child: %v", err)
	}
	if base.ordinalPhysicalProperties == nil {
		t.Fatal("the concrete child's quantifier published no ordinal properties, so the " +
			"dynamic-carrier assertion beside it is vacuous")
	}
}
