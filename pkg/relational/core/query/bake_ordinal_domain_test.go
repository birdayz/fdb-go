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

	// The LEG-WINDOW arm: a dotted read resolved through a leg boundary. Its
	// ordinal indexes the whole flat row (the window is a range within it), so
	// the domain is the row's column list — not the leg's slice of it.
	wideCols := []string{"L_A", "L_B", "R_A", "R_B"}
	wideDomain := values.OrdinalDomainOfColumnNames(wideCols)
	legs := []values.RecordTypeLeg{{Name: "R", Start: 2, Width: 2}}
	legRef := bakedRef(t, bakeFlatRefsAgainstColumns(
		values.NewFlatFieldValue("R.R_B", values.UnknownType), wideCols, legs...))
	if got, ok := legRef.OrdinalIn(wideDomain); !ok || got != 3 {
		t.Fatalf("leg-window bake of R.R_B = (%d,%v), want (3,true)", got, ok)
	}
	if _, ok := legRef.OrdinalIn(values.OrdinalDomainOfColumnNames([]string{"R_A", "R_B"})); ok {
		t.Fatal("leg-window bake answered in the LEG's own layout — its ordinal indexes the whole row")
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

		fv := bakedRef(t, bakeDottedRefsToLegQOV(values.NewFlatFieldValue("NAME", values.UnknownType), outer))
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

		lref := bakedRef(t, bakeDottedRefsToLegQOV(values.NewFlatFieldValue("L.NAME", values.UnknownType), outer))
		if got, ok := lref.OrdinalIn(leftDomain); !ok || got != 1 {
			t.Fatalf("L.NAME = (%d,%v), want (1,true)", got, ok)
		}
		if got, ok := lref.OrdinalIn(rightDomain); ok {
			t.Fatalf("L.NAME answered %d in R's layout — same leaf name, different column", got)
		}

		rref := bakedRef(t, bakeDottedRefsToLegQOV(values.NewFlatFieldValue("R.NAME", values.UnknownType), outer))
		if got, ok := rref.OrdinalIn(rightDomain); !ok || got != 0 {
			t.Fatalf("R.NAME = (%d,%v), want (0,true)", got, ok)
		}
		if got, ok := rref.OrdinalIn(leftDomain); ok {
			t.Fatalf("R.NAME answered %d in L's layout — same leaf name, different column", got)
		}
	})
}
