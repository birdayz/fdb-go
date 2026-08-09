package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestTypingAQuantifiedObjectValueDoesNotChangeItsIdentity refutes, by
// measurement, the claim that kept GetFlowedObjectValue untyped: that typing the
// ~40 GetResultValue implementations returning it would "change expression
// identity across the whole planner".
//
// A QuantifiedObjectValue's identity is its CORRELATION, in every path that
// decides identity, and the type is in none of them:
//
//	EqualsWithoutChildren       — compares Correlation
//	SemanticEqualsUnderAliasMap — compares Correlation through the alias map
//	SemanticHashCode            — folds the tag "qov", alias EXCLUDED
//
// Typing one changes what it SAYS, never which expression it IS. The claim was
// load-bearing — it is the whole reason the typed accessor was added beside the
// untyped one instead of replacing it — so it gets a measurement rather than a
// re-reading, and the measurement stays: the day a type joins the identity, every
// site that types a flowed value starts silently forking memo groups, and the
// symptom of that is a duplicated group, not a failed assertion.
func TestTypingAQuantifiedObjectValueDoesNotChangeItsIdentity(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("Q")
	rt := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	other := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullString, Ordinal: 1},
	}}

	untyped := values.NewQuantifiedObjectValue(alias)
	typed := values.NewQuantifiedObjectValueOfType(alias, rt)
	typedDifferently := values.NewQuantifiedObjectValueOfType(alias, other)

	// The fixture must actually differ in type, or all three checks below hold for
	// the uninteresting reason.
	if untyped.Type().Equals(typed.Type()) || typed.Type().Equals(typedDifferently.Type()) {
		t.Fatalf("fixture: the three values do not differ in type (%v / %v / %v)",
			untyped.Type(), typed.Type(), typedDifferently.Type())
	}

	for _, pair := range []struct {
		name string
		a, b values.Value
	}{
		{"untyped vs typed", untyped, typed},
		{"typed vs differently typed", typed, typedDifferently},
	} {
		if !values.EqualsWithoutChildren(pair.a, pair.b) {
			t.Errorf("%s: EqualsWithoutChildren says UNEQUAL — the type entered structural "+
				"node identity, so typing a flowed value now forks the memo group it "+
				"used to intern into", pair.name)
		}
		if !values.SemanticEqualsUnderAliasMap(pair.a, pair.b, values.AliasMap{}) {
			t.Errorf("%s: SemanticEqualsUnderAliasMap says UNEQUAL — identity is the "+
				"CORRELATION (RFC-197's thesis: the correlation, not the payload type)",
				pair.name)
		}
		if values.SemanticHashCode(pair.a) != values.SemanticHashCode(pair.b) {
			t.Errorf("%s: SemanticHashCode differs. Equal values MUST hash equal, so this "+
				"is a broken invariant on its own — and it is the form in which a typed "+
				"flowed value would silently stop finding its own memo group", pair.name)
		}
	}

	// And the correlation IS still the discriminator: same type, different alias
	// must be unequal, or the checks above pass because nothing is a discriminator.
	elsewhere := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("R"), rt)
	if values.EqualsWithoutChildren(typed, elsewhere) ||
		values.SemanticEqualsUnderAliasMap(typed, elsewhere, values.AliasMap{}) {
		t.Error("two QuantifiedObjectValues over DIFFERENT correlations compare equal — " +
			"the alias is what identity is, and without it the invariance measured above " +
			"is just an equality that ignores everything")
	}
}

// TestLogicalProjectionFallsBackToUntypedQOV pins the ONE shape for which a
// projection still declines to state its row, and it pins that arm as REACHABLE.
//
// THIS TEST WAS REPURPOSED AND ITS OLD NAME LIED. It shipped as
// TestLogicalProjectionResultValueStatesNoRowType, with a doc arguing that
// "stating no type is the honest answer" — the design RFC-226 REPLACED. The
// assertions still passed, because the shape it happens to build is the one
// shape that still declines, so the test read as green while its name and its
// prose asserted the opposite of shipped behaviour. That is worse than no test:
// a reader looking for "does a projection state its row" finds a passing test
// saying it must not.
//
// WHAT IS TRUE NOW. A projection states the row it produces — a record
// constructor over its projected columns — for every shape EXCEPT a single bare
// QuantifiedObjectValue, where values.ProjectionResultValue returns
// ErrWholeRowProjection and GetResultValue falls back to an untyped QOV.
//
// The fallback exists because that shape cannot honestly answer: the executor
// emits one positional slot per projection, so a one-slot projection of a whole
// row WRAPS it rather than passing it through, and the wrapper has no name for
// its single field. Declining is right for it. The arm is REACHABLE — the
// constructor below validates nothing, which is the whole point of building the
// shape here rather than asserting it cannot exist.
//
// The alarm is BIDIRECTIONAL. If this shape starts stating a row, the untyped
// decline was removed and readers begin trusting a row nobody derived. If any
// OTHER shape reaches the fallback, the guard widened —
// TestLogicalProjectionStatesProjectedRow below is the other half and drives the
// ordinary shape.
func TestLogicalProjectionFallsBackToUntypedQOV(t *testing.T) {
	t.Parallel()

	innerRow := rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)
	inner := NamedForEachQuantifier(values.NamedCorrelationIdentifier("IN"),
		InitialOf(&typedStubExpr{name: "src", typ: innerRow}))

	// The inner must state a row, or "the projection states none" is vacuous.
	if got, err := inner.GetFlowedObjectType(); err != nil || got == nil {
		t.Fatalf("fixture: the inner states no row type (%v, %v), so this test cannot "+
			"tell a stripped type from an absent one", got, err)
	}
	if _, typed := inner.GetFlowedObjectValue().Type().(*values.RecordType); !typed {
		t.Fatal("fixture: GetFlowedObjectValue did not type the value, so the site under " +
			"test has nothing to strip and the pin is vacuous")
	}

	proj := NewLogicalProjectionExpression(
		[]values.Value{values.NewQuantifiedObjectValue(inner.GetAlias())}, inner)

	rv := proj.GetResultValue()
	qov, isQOV := rv.(*values.QuantifiedObjectValue)
	if !isQOV {
		t.Fatalf("the one-slot whole-row projection produced a %T, not the untyped QOV "+
			"fallback.\n"+
			"  Either values.ProjectionResultValue stopped returning ErrWholeRowProjection "+
			"for this shape — in which case a projection is now claiming to produce a row "+
			"it cannot name, since the executor emits one positional slot per projection "+
			"and this shape WRAPS the inner row — or a constructor started rejecting the "+
			"shape, in which case say so and delete the fallback rather than leaving dead "+
			"code the comments describe as live.", rv)
	}
	if qov.Correlation != inner.GetAlias() {
		t.Errorf("fallback QOV is over %s, want the inner alias %s",
			qov.Correlation.Name(), inner.GetAlias().Name())
	}
	if _, stated := qov.Typ.(*values.RecordType); stated {
		t.Errorf("the FALLBACK states the row type %v, and it must state none.\n"+
			"  That row would be the INNER's, and this projection does not flow its\n"+
			"  inner's row — a reader that believes it takes a multi-leg row as the\n"+
			"  projection's output and then refuses to serve a source-relative ordinal\n"+
			"  against it (measured: `correlated FieldValue \"EL\" … multi-leg row cannot\n"+
			"  serve a source-relative ordinal`). An untyped QOV here is the pre-RFC-226\n"+
			"  decline, kept deliberately for the one shape that cannot answer.", qov.Typ)
	}
}

// TestLogicalProjectionStatesProjectedRow is the OTHER half of the pin above,
// and it is the half that would have caught the repurposing.
//
// The fallback test alone cannot distinguish "projections decline" (the design
// RFC-226 replaced) from "this ONE shape declines" (what ships), because the
// only shape it builds is the declining one. Driving an ordinary projection is
// what makes the pair discriminating: this one must state a real row, named by
// its output aliases, and it must NOT be the inner's row.
func TestLogicalProjectionStatesProjectedRow(t *testing.T) {
	t.Parallel()

	innerRow := rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)
	inner := NamedForEachQuantifier(values.NamedCorrelationIdentifier("IN"),
		InitialOf(&typedStubExpr{name: "src", typ: innerRow}))

	// One projected column out of a two-column inner, aliased. If the projection
	// flowed its inner's row this would state two fields named A and B.
	proj := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{values.NewFlatFieldValue("A", values.NotNullLong)},
		[]string{"RENAMED"}, inner)

	rv := proj.GetResultValue()
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("an ordinary projection produced a %T, want a RecordConstructorValue.\n"+
			"  A %T here means the whole-row FALLBACK swallowed an ordinary shape — the "+
			"guard in values.ProjectionResultValue widened, and every projection above it "+
			"stops stating a row.", rv, rv)
	}
	if len(rc.Fields) != 1 {
		t.Fatalf("projection states %d fields, want 1 — the projected column list has one "+
			"entry, and the inner's 2-column row must not leak through", len(rc.Fields))
	}
	if rc.Fields[0].Name != "RENAMED" {
		t.Errorf("projection field is named %q, want %q — the OUTPUT alias names the row, "+
			"not the underlying column", rc.Fields[0].Name, "RENAMED")
	}
	rt, typed := rc.Type().(*values.RecordType)
	if !typed {
		t.Fatalf("the stated row has type %T, not a RecordType — an untyped row is the "+
			"pre-RFC-226 decline and every consumer keyed on \"unstated\" stays off",
			rc.Type())
	}
	if len(rt.Fields) != 1 || rt.Fields[0].Name != "RENAMED" {
		t.Errorf("stated row type is %v, want a 1-field record named RENAMED", rt)
	}
}
