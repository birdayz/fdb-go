package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// noLayoutFlatMapLeg builds the leg shape the corpus census names as the ENTIRE
// residue of the layout derivation: a leg whose chosen plan is a
// *plans.RecordQueryFlatMapPlan — the accumulated side of an N-way join — whose
// result value is a bare *values.QuantifiedObjectValue.
//
// That result value is the defect. A FlatMap's row is whatever its result value
// computes, so the layout is the result value's to state, and a bare quantifier
// object states nothing: the quantifier it names carries no row type. Booked as
// CQ-63.
func noLayoutFlatMapLeg(alias values.CorrelationIdentifier) plans.RecordQueryPlan {
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	inner := plans.NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
	return plans.NewRecordQueryFlatMapPlan(
		scan, inner,
		values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"),
		// The measured shape: the FlatMap flows its outer quantifier's object
		// straight through, and that object is UNTYPED.
		values.NewQuantifiedObjectValue(alias),
		false,
	)
}

// TestOrdinalSeedLegWindows_DeclinesANoLayoutFlatMapLeg pins a NEGATIVE result
// that a removed arm would otherwise have taken with it.
//
// The obvious next move for a leg the plan walk cannot reduce is to read its
// layout off its own RESULT VALUE as an ordinal seed — values.OrdinalSeedLegWindows
// already emits per-leg windows and a merged row type, and it is the shared
// authority the executor agrees with. That arm was written, measured against the
// real-FDB corpus, and removed because it declines on every leg in the residue.
//
// A removed arm leaves no trace, so without this test the measurement that
// justified removing it evaporates and the next person re-derives it — or, worse,
// adds the arm back on the reasoning that it "should" work and ships a layout for
// legs that have none. What is pinned here is exactly the fact that made the
// removal correct, and the failure message says what it re-arms.
func TestOrdinalSeedLegWindows_DeclinesANoLayoutFlatMapLeg(t *testing.T) {
	t.Parallel()

	legAlias := values.NamedCorrelationIdentifier("A")
	plan := noLayoutFlatMapLeg(legAlias)

	// 1. The shape itself: the result value is not a record constructor, so the
	// seed authority cannot even be consulted. This is the measured decline point —
	// the corpus witness reads "seed escape DECLINES: result value is
	// *values.QuantifiedObjectValue, not a record constructor".
	fm := plan.(*plans.RecordQueryFlatMapPlan)
	if rc, isRC := fm.GetResultValue().(*values.RecordConstructorValue); isRC {
		t.Fatalf("the fixture's FlatMap result value is a %d-field record constructor, "+
			"not the bare quantifier object the corpus measured. This test is pinning a "+
			"shape the residue does not have; re-derive it from a NO-LAYOUT census "+
			"witness before trusting anything it says.", len(rc.Fields))
	}
	if got := describeSeedEscape(plan); !strings.Contains(got, "DECLINES") {
		t.Fatalf("describeSeedEscape reported %q for the residue's own leg shape. If "+
			"the seeded escape now ACCEPTS this shape, the arm that reads a leg layout "+
			"off its result value is re-armed and should be reinstated deliberately — "+
			"with the corpus re-measured, because the removal rested on it declining "+
			"everywhere.", got)
	}

	// 2. THE WIDTH HAZARD, and where it now stands.
	//
	// The obvious repair for "the result value is not a record constructor" is to
	// wrap it in one, and that used to walk straight into a worse answer than a
	// decline: `isMixedSeedElement` admits a bare quantifier object whose type is
	// not a RecordType — Java's isPrimitive() whole-object element — and an UNTYPED
	// quantifier object is not a RecordType either. Each untyped LEG was therefore
	// admitted as a one-column scalar element, so a 2-slot constructor of bare
	// untyped legs was ACCEPTED and a leg flowing a whole multi-column row got a
	// ONE-column window. Silent layouts at the wrong widths.
	//
	// Typing the flowed object value closes it, and closes it at the SOURCE rather
	// than by tightening the predicate: a leg's quantifier now states its row, so
	// the leg reads as a RecordType and the element arm rejects it. That is the
	// direction that matters, because the predicate CANNOT be tightened into doing
	// this job — demanding a STATED type declines the genuine whole-object element
	// of a struct array, whose type is UNKNOWN because Go does not infer array
	// element types that far, and `SELECT "X" FROM TS, TS."ITEMS" AS "X"` stops
	// resolving (measured). The discrimination lives on the leg side or nowhere.
	typedLeg := func(name string) values.Value {
		return values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(name),
			&values.RecordType{Nullable: true, Fields: []values.Field{
				{Name: "C1", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "C2", FieldType: values.NotNullLong, Ordinal: 1},
			}})
	}
	twoTypedLegs := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: typedLeg("A")},
		values.RecordConstructorField{Name: "B", Value: typedLeg("B")},
	)
	if windows, merged := values.OrdinalSeedLegWindows(twoTypedLegs); windows != nil || merged != nil {
		t.Fatalf("a 2-slot record constructor of bare TYPED leg objects was ACCEPTED "+
			"(%d windows, merged %v). Each leg flows a 2-column row; admitting it as the "+
			"seed's whole-object element gives it a ONE-column window and every field "+
			"after it reads at the wrong offset. A leg that states a row type must be "+
			"rejected by the element arm.", len(windows), merged)
	}

	// The residual, stated rather than implied: an UNTYPED quantifier object is
	// still admitted as a one-column element, because that is what a struct-array
	// element looks like and there is no way to tell the two apart from the type
	// alone. It is not the hazard above only because a LEG is no longer untyped —
	// measured flowed 848 of 848 leg derivations, underivable 0, and
	// ASSERTED by the bakeability census gate. If that gate ever goes yellow, this
	// is what it re-arms.
	untypedPair := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: values.NewQuantifiedObjectValue(legAlias)},
		values.RecordConstructorField{Name: "B", Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B"))},
	)
	windows, merged := values.OrdinalSeedLegWindows(untypedPair)
	if windows == nil || merged == nil {
		t.Fatalf("OrdinalSeedLegWindows now DECLINES a record constructor of bare " +
			"untyped quantifier objects. That is the safer answer, and it may be the " +
			"right one — but it also declines the struct-array whole-object element, " +
			"which is untyped for an unrelated reason, and the executor twin " +
			"(unnestMixedSeedSpans/ordinalJoinSpans) has to move with it or the " +
			"cross-agreement invariant breaks. Re-measure both sides before adopting " +
			"this as the new expectation.")
	}
	for alias, w := range windows {
		if len(w.Typ.Fields) != 1 {
			t.Fatalf("window %s has %d columns; an untyped quantifier object is admitted "+
				"as a ONE-column whole-object element", alias, len(w.Typ.Fields))
		}
	}
	if len(merged.Fields) != 2 {
		t.Fatalf("merged type has %d columns, want 2 one-column elements", len(merged.Fields))
	}
}

// TestLegRowTypes_UnderivableLegStatesNothing pins the other half: a leg neither
// derivation can state contributes NO layout rather than a guess.
//
// The residue is CQ-63's acceptance number. A derivation that invented a layout
// here — an empty record type, the reference's own untyped object, the first leg's
// type — would drive the count to zero while every read correlated to such a leg
// still resolves against a row nobody described.
func TestLegRowTypes_UnderivableLegStatesNothing(t *testing.T) {
	t.Parallel()

	legAlias := values.NamedCorrelationIdentifier("A")
	plan := noLayoutFlatMapLeg(legAlias)

	// A quantifier over an UNTYPED reference: what the residue's legs carry, and
	// the reason the faithful instrument declines them.
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	q := expressions.NamedForEachQuantifier(legAlias, ref)

	got := legRowTypesFromQuantifiers(legRowTypeSource{Quantifier: q, Plan: plan, Alias: legAlias})
	if got != nil {
		t.Fatalf("a leg neither the quantifier's flowed object type nor the plan walk "+
			"can state produced a layout: %v. Every entry in this map is a claim that a "+
			"leg-relative ordinal indexes a known row; inventing one for a leg with no "+
			"stated row turns the missing layout into a wrong-slot read, and drops "+
			"CQ-53's blocker out of the census that measures it.", got)
	}
}

// TestLegRowTypes_FaithfulInstrumentAnswersFromTheQuantifier is the positive
// direction: where the leg's reference members ARE typed, the layout comes from
// the QUANTIFIER, with no physical plan in hand at all.
//
// This is the whole point of routing the derivation through
// Quantifier.GetFlowedObjectType (Java's Quantifier.java:806-810, what
// translateCorrelations rebases against) rather than through a bespoke walk over
// the chosen plan. Passing a nil plan is deliberate: it makes the subordinate walk
// unavailable, so an answer here can only have come from the quantifier.
func TestLegRowTypes_FaithfulInstrumentAnswersFromTheQuantifier(t *testing.T) {
	t.Parallel()

	legAlias := values.NamedCorrelationIdentifier("A")
	rt := nljTestLayouts["OUTER"] // ID, CATEGORY
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, values.Type(rt)))
	q := expressions.NamedForEachQuantifier(legAlias, ref)

	got := legRowTypesFromQuantifiers(legRowTypeSource{Quantifier: q, Alias: legAlias})
	layout, ok := got[legAlias]
	if !ok || layout == nil {
		t.Fatalf("the quantifier over a TYPED reference stated no layout (%v), with no "+
			"plan available to fall back on. The faithful instrument is the one Java "+
			"uses; if it cannot answer for a plain typed scan reference, every layout "+
			"in the corpus is coming from the subordinate walk and the citation to "+
			"Quantifier.java:806-810 is decoration.", got)
	}
	if len(layout.Fields) != len(rt.Fields) {
		t.Fatalf("the flowed layout has %d columns, the reference's row type has %d — "+
			"the layout must be the row the quantifier flows, unwrapped from its "+
			"RELATION wrapper and otherwise untouched", len(layout.Fields), len(rt.Fields))
	}
	for i := range rt.Fields {
		if layout.Fields[i].Name != rt.Fields[i].Name {
			t.Fatalf("flowed layout slot %d is %q, the reference's is %q — a reordering "+
				"here relabels every ordinal derived from it",
				i, layout.Fields[i].Name, rt.Fields[i].Name)
		}
	}
}
