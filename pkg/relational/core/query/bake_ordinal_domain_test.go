package query

// The translator's bakers resolve a parsed leaf against a DECLARED column
// list and emit an ordinal. RFC-197 step 0 makes that ordinal answerable only
// inside the layout it indexes, so each baker must state that layout — the
// same list it just resolved against, derived in the same breath so the two
// cannot drift.
//
// These are dimension tests, not presence checks: each asserts the baked
// reference answers OrdinalIn for its OWN layout AND fails closed for a
// foreign one. Dropping any stamp reddens the first half; stamping the WRONG
// list reddens the second. Without them the stamps are inert — the first
// consumer is the covered-column boundary, which sees the resolver's bakes
// (pinned at the candidate) rather than these, and the buckets that will read
// them (name-keyed, translator) have not migrated yet.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// bakedRef digs the single baked FieldValue out of a baked result.
func bakedRef(t *testing.T, v values.Value) *values.FieldValue {
	t.Helper()
	fv, ok := v.(*values.FieldValue)
	if !ok {
		t.Fatalf("bake produced %T, want *values.FieldValue", v)
	}
	if fv.Resolved == nil {
		t.Fatalf("bake produced a LAZY reference: %#v", fv)
	}
	return fv
}

func TestBakeFlatRefsAgainstColumns_StatesTheColumnListAsItsDomain(t *testing.T) {
	t.Parallel()

	cols := []string{"ID", "NAME", "TOTAL"}
	domain := values.OrdinalDomainOfColumnNames(cols)
	// A layout with the SAME column NAMES in a different order: every name
	// resolves in both, so only the domain distinguishes them, and reading
	// one's ordinal in the other is a wrong slot rather than a miss.
	foreign := values.OrdinalDomainOfColumnNames([]string{"NAME", "TOTAL", "ID"})

	fv := bakedRef(t, bakeFlatRefsAgainstColumns(values.NewFlatFieldValue("NAME", values.UnknownType), cols))
	if got, ok := fv.OrdinalIn(domain); !ok || got != 1 {
		t.Fatalf("flat bake of NAME = (%d,%v), want (1,true) — the baker must state the column list it resolved against", got, ok)
	}
	if got, ok := fv.OrdinalIn(foreign); ok {
		t.Fatalf("flat bake answered %d in a layout with the same names in a different order", got)
	}

	// The LEG-WINDOW arm is GONE from this baker, and this assertion is its
	// inverse. It previously read: a dotted `R.R_B` with no parse-tree
	// segments resolved through the leg boundary to flat ordinal 3, and the
	// test pinned that its domain was the whole row rather than the leg's
	// slice. That resolution required slicing "R.R_B" at its first dot to
	// invent the qualifier "R" — the re-split this baker no longer performs.
	//
	// The rule now: no segments means no qualifier, so a dot is just a
	// character in the name. `R.R_B` matches no flat column verbatim and
	// therefore DECLINES, staying lazy and going loud at evaluation. The
	// segment-carrying caller (bakeSegmentedColumnRef) is where a genuine
	// qualified reference reaches a leg window, and it is pinned separately.
	wideCols := []string{"L_A", "L_B", "R_A", "R_B"}
	out := bakeFlatRefsAgainstColumns(
		values.NewFlatFieldValue("R.R_B", values.UnknownType), wideCols)
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved != nil {
		t.Fatalf("a dotted name with no parse-tree segments baked to %#v — it must stay "+
			"lazy: without segments there is no qualifier to select a leg window with, "+
			"and slicing the name to invent one is the re-split this baker retired", out)
	}
	// CONTROL: the same baker still resolves a name that IS a flat column, so
	// the decline above is the dot being refused rather than the baker being
	// inert. Without this, deleting the baker's body would pass the check.
	hit := bakedRef(t, bakeFlatRefsAgainstColumns(
		values.NewFlatFieldValue("R_B", values.UnknownType), wideCols))
	if got, ok := hit.OrdinalIn(values.OrdinalDomainOfColumnNames(wideCols)); !ok || got != 3 {
		t.Fatalf("control: flat bake of R_B = (%d,%v), want (3,true) — the baker must "+
			"still resolve ordinary names, or the decline above proves nothing", got, ok)
	}
}

// legSelect builds a SELECT whose result value is a record constructor, so
// expressionOutputColumns can derive its flat column list — the shape
// bakeDottedRefsToLegQOV needs from a leg.
func legSelect(cols ...string) *expressions.SelectExpression {
	fields := make([]values.RecordConstructorField, len(cols))
	for i, c := range cols {
		fields[i] = values.RecordConstructorField{Name: c, Value: values.NewFlatFieldValue(c, values.UnknownType)}
	}
	return expressions.NewSelectExpression(values.NewRecordConstructorValue(fields...), nil, nil)
}

func TestBakeDottedRefsToLegQOV_StatesTheLegLayoutAsItsDomain(t *testing.T) {
	t.Parallel()

	t.Run("single ForEach bakes FLAT against the quantifier's row", func(t *testing.T) {
		t.Parallel()
		inner := legSelect("ID", "NAME")
		q := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("T"), expressions.InitialOf(inner))
		outer := expressions.NewSelectExpressionWithAliases(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T")),
			[]expressions.Quantifier{q}, nil, []string{"T"})

		fv := bakedRef(t, bakeDottedRefsToLegQOVWithRef(values.NewFlatFieldValue("NAME", values.UnknownType), logical.ColumnRef{}, outer))
		layout := values.OrdinalDomainOfColumnNames([]string{"ID", "NAME"})
		if got, ok := fv.OrdinalIn(layout); !ok || got != 1 {
			t.Fatalf("single-ForEach flat bake = (%d,%v), want (1,true)", got, ok)
		}
		if _, ok := fv.OrdinalIn(values.OrdinalDomainOfColumnNames([]string{"NAME", "ID"})); ok {
			t.Fatal("flat bake answered in a layout it does not index")
		}
	})

	t.Run("multi-leg bakes LEG-ADDRESSED against the leg's own row", func(t *testing.T) {
		t.Parallel()
		// Two legs sharing a column NAME at DIFFERENT ordinals — the original
		// bug class. The reference is qualified, so it must bake against the
		// named leg's layout, and its ordinal must not answer in the sibling's.
		left := legSelect("ID", "NAME")
		right := legSelect("NAME", "ID")
		lq := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), expressions.InitialOf(left))
		rq := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("R"), expressions.InitialOf(right))
		outer := expressions.NewSelectExpressionWithAliases(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
			[]expressions.Quantifier{lq, rq}, nil, []string{"L", "R"})

		leftDomain := values.OrdinalDomainOfColumnNames([]string{"ID", "NAME"})
		rightDomain := values.OrdinalDomainOfColumnNames([]string{"NAME", "ID"})

		// Driven by the PARSER's segments. These used to pass an empty
		// ColumnRef and rely on the baker slicing "L.NAME" at its first dot to
		// recover the qualifier; that split is gone. The subject of this
		// subtest was never the split — it is the leg-addressed DOMAIN, and
		// every assertion below is unchanged.
		lSeg := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true}
		rSeg := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "R", Qualified: true}

		lref := bakedRef(t, bakeDottedRefsToLegQOVWithRef(values.NewFlatFieldValue("L.NAME", values.UnknownType), lSeg, outer))
		if got, ok := lref.OrdinalIn(leftDomain); !ok || got != 1 {
			t.Fatalf("L.NAME = (%d,%v), want (1,true)", got, ok)
		}
		if got, ok := lref.OrdinalIn(rightDomain); ok {
			t.Fatalf("L.NAME answered %d in R's layout — same leaf name, different column", got)
		}

		rref := bakedRef(t, bakeDottedRefsToLegQOVWithRef(values.NewFlatFieldValue("R.NAME", values.UnknownType), rSeg, outer))
		if got, ok := rref.OrdinalIn(rightDomain); !ok || got != 0 {
			t.Fatalf("R.NAME = (%d,%v), want (0,true)", got, ok)
		}
		if got, ok := rref.OrdinalIn(leftDomain); ok {
			t.Fatalf("R.NAME answered %d in L's layout — same leaf name, different column", got)
		}
	})
}
