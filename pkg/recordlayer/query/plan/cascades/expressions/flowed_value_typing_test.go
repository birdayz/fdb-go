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

// TestLogicalProjectionResultValueStatesNoRowType pins the FIRST site found that
// must not take the typed flowed value, with its reason attached so it cannot be
// "cleaned up" onto the typed accessor for consistency.
//
// It was believed to be the only one. It is not: GroupBy, Insert and Update pass
// their inner through while producing a different row too, for three different
// reasons, and they are pinned as a family in flowed_value_exemptions_test.go —
// along with the three sites that LOOK like the family and are actually
// Java-faithful (Delete, Union, Intersection), because the analogy is what would
// spread the exemption to them.
//
// This one keeps its own test because its reason is its own: a projection outputs
// its PROJECTED columns, and the fix is a row built from GetProjectedValues.
//
// LogicalProjectionExpression.GetResultValue passes its inner's flowed object
// through, which is Java's contract verbatim
// (LogicalProjectionExpression.java:105) and harmless in Java, where the class
// serves only legacy RecordQuery planning. Go uses it as the SQL projection, and
// there the inherited contract is false: a projection outputs its PROJECTED
// columns, not its inner's row.
//
// Untyped, that falsehood asserts nothing. Typed, it asserts that the projection
// flows the inner's whole row — legs included — and a downstream reader that
// believes it refuses to serve a source-relative ordinal against the resulting
// multi-leg row. Stating no type is the honest answer until the projection can
// state the row it actually produces.
func TestLogicalProjectionResultValueStatesNoRowType(t *testing.T) {
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
		t.Fatalf("projection result value is a %T; the passthrough shape changed and this "+
			"pin no longer describes the site", rv)
	}
	if qov.Correlation != inner.GetAlias() {
		t.Errorf("projection result value is over %s, want the inner alias %s",
			qov.Correlation.Name(), inner.GetAlias().Name())
	}
	if _, stated := qov.Typ.(*values.RecordType); stated {
		t.Errorf("the projection's result value STATES the row type %v.\n"+
			"  That row is the INNER's, and a projection does not flow its inner's row —\n"+
			"  it flows the columns it projects. A reader that believes the statement takes\n"+
			"  the inner's multi-leg row as the projection's output and then refuses to\n"+
			"  serve a source-relative ordinal against it (measured:\n"+
			"  `correlated FieldValue \"EL\" … multi-leg row cannot serve a source-relative\n"+
			"  ordinal` on a CTE + derived-table + unnest three-source shape).\n"+
			"  Fix the value before typing it: build the projected row from\n"+
			"  GetProjectedValues and GetAliases.", qov.Typ)
	}
}
