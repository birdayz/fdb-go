package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The NO-PLAN underivable-leg witness has to tell two declining legs apart.
//
// It used to render `%T` of the quantifier. expressions.Quantifier is a STRUCT,
// so that produced the identical string for every leg in the population: the
// census's witness list then carried one line per ALIAS and nothing about why
// the quantifier could state no type — which is the only thing the witness is
// for, since the count is already in UnderivableLegs.
//
// These pin the DISCRIMINATION, not the wording: two quantifiers that decline
// for DIFFERENT reasons must not render the same witness. A revert to `%T`
// collapses every case below onto one string and fails all three.
func TestDescribeLegQuantifier_DiscriminatesTheDecliningShapes(t *testing.T) {
	t.Parallel()

	typed := values.Type(values.NewRecordType("WitnessRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "CATEGORY", FieldType: values.NullableString},
	}))

	// A quantifier ranging over nothing at all.
	empty := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("EMPTY"), nil)

	// A legacy malformed reference whose single member is nil. RFC-232 no longer
	// admits an untyped relational member, so nil is the only representable
	// diagnostic adversary for "the reference has a member but no result type".
	untypedRef := expressions.InitialOf(nil)
	untyped := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("UNTYPED"), untypedRef)

	// A reference whose member flows a real row type.
	typedScan, err := expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, typed)
	if err != nil {
		t.Fatalf("typed logical scan: %v", err)
	}
	typedRef := expressions.InitialOf(typedScan)
	typedPlan, err := plans.NewRecordQueryScanPlan([]string{"OUTER"}, typed, false)
	if err != nil {
		t.Fatalf("typed physical scan: %v", err)
	}
	typedRef.InsertFinal(typedPlan)
	typedQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("TYPED"), typedRef)

	emptyW := describeLegQuantifier(empty)
	untypedW := describeLegQuantifier(untyped)
	typedW := describeLegQuantifier(typedQ)

	// The three must be pairwise distinct. This is the assertion `%T` fails.
	for _, pair := range []struct {
		aName, a, bName, b string
	}{
		{"ranges-over-nothing", emptyW, "nil-member", untypedW},
		{"ranges-over-nothing", emptyW, "typed-member", typedW},
		{"nil-member", untypedW, "typed-member", typedW},
	} {
		if pair.a == pair.b {
			t.Fatalf("witness for %s and %s render IDENTICALLY (%q).\n"+
				"  The witness cannot tell two legs apart, so the census's witness list\n"+
				"  reports only that SOME leg declined — which UnderivableLegs already\n"+
				"  counts. A non-discriminating witness is not diagnostics.",
				pair.aName, pair.bName, pair.a)
		}
	}

	// And each must actually name what it found, not merely differ by alias:
	// stripping the alias must leave them still distinct.
	stripAlias := func(s, alias string) string { return strings.ReplaceAll(s, alias, "«alias»") }
	e2 := stripAlias(emptyW, "EMPTY")
	u2 := stripAlias(untypedW, "UNTYPED")
	ty2 := stripAlias(typedW, "TYPED")
	if e2 == u2 || e2 == ty2 || u2 == ty2 {
		t.Fatalf("witnesses differ ONLY by alias — no-members=%q untyped=%q typed=%q.\n"+
			"  The alias is already printed by the census line itself, so a witness that\n"+
			"  adds nothing else is the collapsed `%%T` form wearing a different string.",
			e2, u2, ty2)
	}

	// The typed case must report the member's flowed type, which is the fact the
	// residue turns on (GetFlowedObjectType derives the layout from exactly it).
	if !strings.Contains(typedW, "ID") || !strings.Contains(typedW, "CATEGORY") {
		t.Fatalf("typed-member witness %q does not name the member's flowed columns; "+
			"the member type summary is what makes the witness actionable", typedW)
	}
	if !strings.Contains(e2, "NOTHING") {
		t.Fatalf("no-member witness %q does not say the quantifier ranges over nothing", e2)
	}
}
