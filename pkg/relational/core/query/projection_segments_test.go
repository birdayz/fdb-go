package query

// A projection's name is a RENDERING. `A.B` written as a qualified reference
// and `"A.B"` written as one quoted identifier are the same bytes, so a
// consumer that recovers the qualifier by slicing at the first dot cannot tell
// them apart — it reads the quoted column as a reference to source A and binds
// it to A's row.
//
// The parse tree can tell them apart, and always could: qualification is FullId
// SEGMENT COUNT. These pins are on the segment triple LogicalProject now carries
// beside Projections, and on the two bakers that consume it.
//
// They are white-box for the reason the sibling pins on this defect class are
// (cluster_ref_attribution_test.go): the misattribution needs a column whose
// name collides with a live leg qualifier, and every query that does not
// construct that collision resolves the same either way.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// twoLegSelect is the L/R fixture: two ForEach legs, each declaring NAME, at
// DIFFERENT ordinals — so a reference resolved to the wrong leg answers with
// the wrong column rather than coincidentally the right one.
func twoLegSelect() expressions.RelationalExpression {
	left := legSelect("ID", "NAME")
	right := legSelect("NAME", "ID")
	lq := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("L"), expressions.InitialOf(left))
	rq := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("R"), expressions.InitialOf(right))
	return expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		[]expressions.Quantifier{lq, rq}, nil, []string{"L", "R"})
}

// TestLegQOVBake_QuotedWholeNameIsNotAQualifiedReference is the pin.
//
// `"L.NAME"` as ONE quoted identifier and `L.NAME` as a qualified reference
// render identically. The leg-QOV baker splits before it does anything else —
// it has no exact-name precedence to rescue the quoted spelling first — so with
// only the rendering in hand the quoted column becomes leg L's NAME, silently,
// over a row that may not even carry it.
func TestLegQOVBake_QuotedWholeNameIsNotAQualifiedReference(t *testing.T) {
	t.Parallel()
	outer := twoLegSelect()

	// Guard the fixture: L must really be a leg carrying NAME, or the test
	// would pass for the wrong reason — nothing to misattribute to.
	probe := bakeDottedRefsToLegQOV(values.NewFlatFieldValue("L.NAME", values.UnknownType), outer)
	if fv, ok := probe.(*values.FieldValue); !ok || fv.Resolved == nil {
		t.Fatalf("fixture: L.NAME did not bake (%#v) — there is no leg L.NAME for a split to hit", probe)
	}

	in := values.NewFlatFieldValue("L.NAME", values.UnknownType)
	quoted := logical.ColumnRef{Present: true, Bare: "L.NAME"}
	out := bakeDottedRefsToLegQOVWithRef(in, quoted, outer)
	if out != values.Value(in) {
		t.Fatalf("a quoted one-segment column %q was baked to %#v — the baker read the "+
			"name's dot as a qualifier boundary and bound the column to leg L's row",
			"L.NAME", out)
	}
}

// TestLegQOVBake_SegmentedQualifiedRefStillBakes is the other direction. The
// rule is "the segments decide", not "stop baking": a genuinely qualified
// reference must still resolve, and to the SAME leg the split resolved it to.
func TestLegQOVBake_SegmentedQualifiedRefStillBakes(t *testing.T) {
	t.Parallel()
	outer := twoLegSelect()
	leftDomain := values.OrdinalDomainOfColumnNames([]string{"ID", "NAME"})
	rightDomain := values.OrdinalDomainOfColumnNames([]string{"NAME", "ID"})

	qualified := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true}
	ref := bakedRef(t, bakeDottedRefsToLegQOVWithRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), qualified, outer))
	if got, ok := ref.OrdinalIn(leftDomain); !ok || got != 1 {
		t.Fatalf("segmented L.NAME = (%d,%v), want (1,true) — the segment-carrying path "+
			"must resolve what the splitting path resolved", got, ok)
	}
	if got, ok := ref.OrdinalIn(rightDomain); ok {
		t.Fatalf("segmented L.NAME answered %d in R's layout — same leaf name, different column", got)
	}
}

// TestFlatColumnBake_QuotedWholeNameNeverReachesALegWindow pins the same
// discrimination on the flat baker.
//
// Its exact-name precedence hides the defect wherever the quoted column is
// present in the flat layout — which is why the pin puts it ABSENT. That is not
// a contrived state: the flat layout is the input's derivable output columns,
// and a projection can name a column the input's own layout does not declare
// (the reference then stays lazy and is loud at eval). With only the rendering
// in hand the miss falls through to the leg window and resolves to leg L's
// NAME, turning a loud miss into a wrong answer.
func TestFlatColumnBake_QuotedWholeNameNeverReachesALegWindow(t *testing.T) {
	t.Parallel()
	cols := []string{"ID", "NAME", "OTHER"}
	legs := []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("L"), "L", 0, 2),
		values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("R"), "R", 2, 1),
	}

	// Guard the fixture: the splitting path really does resolve this text.
	split := bakeFlatRefsAgainstColumns(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), cols, legs...)
	if fv, ok := split.(*values.FieldValue); !ok || fv.Resolved == nil {
		t.Fatalf("fixture: the splitting path left %q lazy (%#v) — nothing for the "+
			"segments to have to refuse", "L.NAME", split)
	}

	in := values.NewFlatFieldValue("L.NAME", values.UnknownType)
	quoted := logical.ColumnRef{Present: true, Bare: "L.NAME"}
	out := bakeSegmentedColumnRef(in, quoted, cols, legs)
	if out != values.Value(in) {
		t.Fatalf("a quoted one-segment column %q resolved through a LEG WINDOW to %#v — "+
			"an unqualified reference has no qualifier to select a leg with",
			"L.NAME", out)
	}

	// The SAME refusal with a leftover Qualifier present. This is a CONTRACT
	// pin, not a reachable-input reproducer, and the distinction is recorded
	// because it decides what the check is worth: no producer here fills
	// Qualifier while leaving Qualified false, so the case above is also
	// refused by Qualifier being "". That makes the rule an accident of one
	// producer unless Qualified alone decides it — and the day a producer
	// carries a qualifier it then clears (the derived-table shell's prefix
	// strip is exactly that shape), the accident stops holding.
	stale := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: false}
	staleIn := values.NewFlatFieldValue("L.NAME", values.UnknownType)
	if out := bakeSegmentedColumnRef(staleIn, stale, cols, legs); out != values.Value(staleIn) {
		t.Fatalf("a reference the parser saw as ONE segment resolved through leg L's "+
			"window to %#v — the segment COUNT is the authority, not whether a "+
			"Qualifier string happens to be empty", out)
	}

	// The whole-name reference still resolves when the layout DOES declare it:
	// the refusal is of the leg window, not of the column.
	withCol := bakeSegmentedColumnRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), quoted,
		[]string{"ID", "L.NAME"}, legs)
	fv, ok := withCol.(*values.FieldValue)
	if !ok || fv.Resolved == nil {
		t.Fatalf("a quoted column the layout DOES declare must resolve to it; got %#v", withCol)
	}
	if got, ok := fv.OrdinalIn(values.OrdinalDomainOfColumnNames([]string{"ID", "L.NAME"})); !ok || got != 1 {
		t.Fatalf("quoted column resolved to (%d,%v), want (1,true)", got, ok)
	}
}

// TestBakers_UncapturedRefKeepTheSplittingBehaviour is the fallback direction,
// and it is the one that decides whether this change is safe to land
// incrementally. Several LogicalProject producers do not carry segments yet; a
// zero ColumnRef must leave them EXACTLY where they were, splitting the
// rendered name, or converting one producer silently breaks the rest.
//
// It is pinned rather than assumed because "absent" and "unqualified" are the
// same zero value in that struct and mean opposite things: read as
// "unqualified", an uncaptured reference would stop resolving through leg
// windows altogether.
func TestBakers_UncapturedRefKeepTheSplittingBehaviour(t *testing.T) {
	t.Parallel()
	outer := twoLegSelect()
	leftDomain := values.OrdinalDomainOfColumnNames([]string{"ID", "NAME"})

	var uncaptured logical.ColumnRef // Present is false — no producer stated anything
	ref := bakedRef(t, bakeDottedRefsToLegQOVWithRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), uncaptured, outer))
	if got, ok := ref.OrdinalIn(leftDomain); !ok || got != 1 {
		t.Fatalf("uncaptured L.NAME = (%d,%v), want (1,true) — a producer that carries no "+
			"segments must keep the behaviour it had", got, ok)
	}

	cols := []string{"ID", "NAME", "OTHER"}
	legs := []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("L"), "L", 0, 2),
		values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("R"), "R", 2, 1),
	}
	in := values.NewFlatFieldValue("L.NAME", values.UnknownType)
	if out := bakeSegmentedColumnRef(in, uncaptured, cols, legs); out != values.Value(in) {
		t.Fatalf("the segment-carrying baker acted on an UNCAPTURED ref (%#v) — with no "+
			"segments stated it has nothing to resolve by and must defer to the caller "+
			"that splits", out)
	}
}
