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
	//
	// The guard used to establish that by baking with an EMPTY ColumnRef and
	// requiring it to resolve, i.e. by exercising the first-dot split itself.
	// That split is gone, so the fixture is now proved the only way left that
	// means anything: with the parser's segments, which is also the only way a
	// leg is legitimately reachable.
	probe := bakeDottedRefsToLegQOVWithRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType),
		logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true}, outer)
	if fv, ok := probe.(*values.FieldValue); !ok || fv.Resolved == nil {
		t.Fatalf("fixture: qualified L.NAME did not bake (%#v) — there is no leg L "+
			"carrying NAME, so the refusal below would hold vacuously", probe)
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
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("L"), "L", 0, 2),
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("R"), "R", 2, 1),
	}

	// This guard has INVERTED. It previously asserted that the no-segments
	// baker really did resolve "L.NAME" by splitting it, which is what made
	// bakeSegmentedColumnRef's refusal below a contrast worth pinning: one
	// path resolved the text, the other refused it.
	//
	// The splitting path is gone, so the two paths now AGREE, and agreement is
	// the property to hold. A resolution here would mean the re-split came
	// back — the direction of the alarm is growth, not collapse.
	noSegments := bakeFlatRefsAgainstColumns(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), cols)
	if fv, ok := noSegments.(*values.FieldValue); !ok || fv.Resolved != nil {
		t.Fatalf("the no-segments baker RESOLVED %q to %#v. It must decline: a name that "+
			"arrived without parse-tree segments has no qualifier, so reaching a leg "+
			"window means the first-dot re-split has been reintroduced", "L.NAME", noSegments)
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
func TestBakers_UncapturedRefIsUnqualified(t *testing.T) {
	t.Parallel()
	outer := twoLegSelect()

	// INVERTED. This test was named ...KeepTheSplittingBehaviour and asserted
	// the opposite of what it asserts now: that an uncaptured ref resolved
	// "L.NAME" to leg L's ordinal 1, because "a producer that carries no
	// segments must keep the behaviour it had".
	//
	// That was the debt stated as a requirement. Keeping the behaviour meant
	// keeping a first-dot slice, which manufactures a qualifier no parser
	// produced and cannot tell a qualified `L.NAME` from a quoted `"L.NAME"`.
	// The rule now matches Java's: an Identifier's qualifier is an explicit
	// segment list, so a carrier stating no segments is UNQUALIFIED and cannot
	// select a leg.
	var uncaptured logical.ColumnRef // Present is false — no producer stated anything
	out := bakeDottedRefsToLegQOVWithRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), uncaptured, outer)
	if fv, isFV := out.(*values.FieldValue); !isFV || fv.Resolved != nil {
		t.Fatalf("an uncaptured L.NAME baked to %#v — it must stay lazy. Resolving it "+
			"requires inventing the qualifier L out of the bytes before the dot, which "+
			"is the re-split this change removed", out)
	}

	// CONTROL: the SAME baker with the SAME text resolves when the parser's
	// segments say it is qualified. Without this the decline above would also
	// pass with the baker gutted.
	segmented := logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true}
	ref := bakedRef(t, bakeDottedRefsToLegQOVWithRef(
		values.NewFlatFieldValue("L.NAME", values.UnknownType), segmented, outer))
	leftDomain := values.OrdinalDomainOfColumnNames([]string{"ID", "NAME"})
	if got, ok := ref.OrdinalIn(leftDomain); !ok || got != 1 {
		t.Fatalf("control: SEGMENTED L.NAME = (%d,%v), want (1,true) — a genuine qualified "+
			"reference must still reach its leg, or the decline above proves only that "+
			"the baker is dead", got, ok)
	}

	cols := []string{"ID", "NAME", "OTHER"}
	legs := []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("L"), "L", 0, 2),
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("R"), "R", 2, 1),
	}
	in := values.NewFlatFieldValue("L.NAME", values.UnknownType)
	if got := bakeSegmentedColumnRef(in, uncaptured, cols, legs); got != values.Value(in) {
		t.Fatalf("the segment-carrying baker acted on an UNCAPTURED ref (%#v) — with no "+
			"segments stated it has nothing to resolve by. It used to defer to a caller "+
			"that split the name; there is no such caller now, and both paths decline",
			got)
	}
}

// TestSortKeyRef_ReconcilesAgainstTheCanonicalText pins the ORDER BY channel's
// triple. SortKey has carried Bare/Qualifier/Qualified since RFC-180 F-3 and
// only the JOINED Expr ever reached the baker — this is the accessor that
// stopped that.
func TestSortKeyRef_ReconcilesAgainstTheCanonicalText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		key  logical.SortKey
		want logical.ColumnRef
	}{
		{
			"a qualified key carries its qualifier",
			logical.SortKey{Expr: "D.X", Bare: "X", Qualifier: "D", Qualified: true},
			logical.ColumnRef{Present: true, Bare: "X", Qualifier: "D", Qualified: true},
		},
		{
			"a bare key is one segment",
			logical.SortKey{Expr: "X", Bare: "X"},
			logical.ColumnRef{Present: true, Bare: "X"},
		},
		{
			// The shape the whole change exists for: `ORDER BY "D.X"` naming one
			// quoted column. Splitting it would sort by leg D's X.
			"a quoted one-segment key with a dot is still one segment",
			logical.SortKey{Expr: "D.X", Bare: "D.X"},
			logical.ColumnRef{Present: true, Bare: "D.X"},
		},
		{
			// A POSITIONAL key's Expr is a rendering of a SELECT-list slot, not
			// a column reference, so it must claim nothing even if segments were
			// left on it by a rebase.
			"a positional key claims nothing",
			logical.SortKey{Expr: "D.X", Bare: "X", Qualifier: "D", Qualified: true, Pos: 2},
			logical.ColumnRef{},
		},
		{
			// An aggregate/computed key: its canonical rendering is not the
			// segments joined, so the triple does not describe it.
			"a computed key's rendering is not its segments",
			logical.SortKey{Expr: "SUM(D.X)", Bare: "X", Qualifier: "D", Qualified: true},
			logical.ColumnRef{},
		},
	} {
		if got := tc.key.Ref(); got != tc.want {
			t.Errorf("%s: Ref() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestAggregateCallRef_OnlyABareColumnOperandCaptures pins the aggregate
// channel. COUNT(*) and computed operands have no column reference to segment,
// and a triple claimed for them would authorize a leg-window lookup on a
// rendering like `SUM(A+B)`.
func TestAggregateCallRef_OnlyABareColumnOperandCaptures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call logical.AggregateCall
		want logical.ColumnRef
	}{
		{
			"a qualified bare-column operand carries its qualifier",
			logical.AggregateCall{
				Func: "SUM", Operand: "V.X", BareColumn: true,
				Qualified: true, Bare: "X", Qualifier: "V",
			},
			logical.ColumnRef{Present: true, Bare: "X", Qualifier: "V", Qualified: true},
		},
		{
			"COUNT(*) captures nothing",
			logical.AggregateCall{Func: "COUNT", Operand: "*", Star: true},
			logical.ColumnRef{},
		},
		{
			"a COMPUTED operand captures nothing — its rendering is not a reference",
			logical.AggregateCall{
				Func: "SUM", Operand: "V.X + 1", Qualified: true,
				Bare: "X", Qualifier: "V",
			},
			logical.ColumnRef{},
		},
		{
			"a quoted one-segment operand with a dot is still one segment",
			logical.AggregateCall{Func: "SUM", Operand: "V.X", BareColumn: true, Bare: "V.X"},
			logical.ColumnRef{Present: true, Bare: "V.X"},
		},
		{
			// CONTRACT pin, not a reachable-input reproducer, and recorded as
			// such because that decides what it is worth. Every computed operand
			// this file's producers build renders differently from its segments,
			// so the case above is also declined by the reconciliation alone.
			// That makes BareColumn's authority an accident of today's canonical
			// rendering unless it decides on its own — and a rendering that
			// happens to spell the segments (a parenthesised or re-canonicalised
			// operand) is one rendering change away.
			"a rendering that SPELLS the segments still needs BareColumn to be a reference",
			logical.AggregateCall{
				Func: "SUM", Operand: "V.X", BareColumn: false,
				Qualified: true, Bare: "X", Qualifier: "V",
			},
			logical.ColumnRef{},
		},
	} {
		if got := tc.call.Ref(); got != tc.want {
			t.Errorf("%s: Ref() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestTranslateSort_RoutesAQuotedKeyAwayFromTheLegWindow pins the ORDER BY
// channel's ROUTING, not just its accessor.
//
// SortKey.Ref() being correct buys nothing if translateSort never consults it,
// and the two are separately breakable: deleting the consumer leaves every
// Ref() unit test green while the key resolves by a dot slice again. The
// difference is only observable where the two disagree, so the key here is one
// quoted segment whose spelling names a live leg.
func TestTranslateSort_RoutesAQuotedKeyAwayFromTheLegWindow(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	key := func(k logical.SortKey) values.Value {
		sorted := tr.translateOp(logical.NewSort(
			inner(scan("Order", "o"), scan("Customer", "c")), []logical.SortKey{k}))
		sortExpr, isSort := sorted.(*expressions.LogicalSortExpression)
		if !isSort {
			t.Fatalf("translate produced %T, want a sort", sorted)
		}
		if len(sortExpr.GetSortKeys()) != 1 {
			t.Fatalf("got %d sort keys, want 1", len(sortExpr.GetSortKeys()))
		}
		return sortExpr.GetSortKeys()[0].Value
	}

	// Guard the fixture: the QUALIFIED spelling really does resolve, or the
	// refusal below would be indistinguishable from "this key never resolved".
	qualified := key(logical.SortKey{
		Expr: "C.NAME", Bare: "NAME", Qualifier: "C", Qualified: true,
	})
	qfv, isFV := qualified.(*values.FieldValue)
	if !isFV || qfv.Resolved == nil {
		t.Fatalf("fixture: the qualified key C.NAME did not resolve (%#v) — there is no "+
			"leg C column NAME for a slice to reach, so the pin below proves nothing",
			qualified)
	}

	// The same bytes, seen by the parser as ONE segment: `ORDER BY "C.NAME"`.
	whole := key(logical.SortKey{Expr: "C.NAME", Bare: "C.NAME"})
	wfv, isFV := whole.(*values.FieldValue)
	if !isFV {
		t.Fatalf("quoted key produced %#v, want a FieldValue", whole)
	}
	if wfv.Resolved != nil || wfv.Child != nil {
		t.Fatalf("a quoted one-segment ORDER BY key %q resolved to %#v — translateSort "+
			"stopped consulting the key's segments and sliced its rendering at the dot, "+
			"so the sort orders by leg C's NAME instead of the column so named",
			"C.NAME", whole)
	}
}

// TestTranslateAggregate_RoutesAQuotedGroupKeyAwayFromTheLegWindow is the
// GROUP BY channel's routing pin, and it exists for the reason the sort one
// does: GroupKey.Ref() being correct proves nothing about whether
// translateAggregate consults it, and deleting the consumer left every unit
// test in this file green.
func TestTranslateAggregate_RoutesAQuotedGroupKeyAwayFromTheLegWindow(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	groupKey := func(g logical.GroupKey) values.Value {
		agg := tr.translateOp(&logical.LogicalAggregate{
			Input:     inner(scan("Order", "o"), scan("Customer", "c")),
			GroupKeys: []logical.GroupKey{g},
			Calls:     []logical.AggregateCall{{Func: "COUNT", Operand: "*", Star: true}},
			Aliases:   []string{"N"},
		})
		gb, isGB := agg.(*expressions.GroupByExpression)
		if !isGB {
			t.Fatalf("translate produced %T, want a group-by", agg)
		}
		keys := gb.GetGroupingKeys()
		if len(keys) != 1 {
			t.Fatalf("got %d grouping keys, want 1", len(keys))
		}
		return keys[0]
	}

	// Guard the fixture: the QUALIFIED spelling really does resolve.
	qualified := groupKey(logical.GroupKey{
		Display: "C.NAME", Bare: "NAME", Qualifier: "C", Qualified: true,
	})
	if qfv, isFV := qualified.(*values.FieldValue); !isFV || qfv.Resolved == nil {
		t.Fatalf("fixture: the qualified group key C.NAME did not resolve (%#v) — there "+
			"is no leg C column NAME for a slice to reach", qualified)
	}

	// The same bytes as ONE segment: `GROUP BY "C.NAME"`.
	whole := groupKey(logical.GroupKey{Display: "C.NAME", Bare: "C.NAME"})
	wfv, isFV := whole.(*values.FieldValue)
	if !isFV {
		t.Fatalf("quoted group key produced %#v, want a FieldValue", whole)
	}
	if wfv.Resolved != nil || wfv.Child != nil {
		t.Fatalf("a quoted one-segment GROUP BY key %q resolved to %#v — "+
			"translateAggregate stopped consulting the key's segments and sliced its "+
			"rendering at the dot, so it groups by leg C's NAME instead of the column "+
			"so named", "C.NAME", whole)
	}
}

// TestSingleForEachBake_AQualifiedReadDeclines pins the DELETED arm.
//
// bakeDottedRefsToLegQOV's single-ForEach branch used to compare a qualifier
// against its one layout's binding text and bake FLAT on a match. It was the
// third dotted reader on this path, it reported zero over the real-FDB corpus,
// and a panic at its match point was reached by nothing across
// ./pkg/relational/... nor ./pkg/recordlayer/query/... — so it was deleted.
//
// A deletion of unreachable code cannot be pinned by a test that goes red when
// the code returns: nothing reaches it either way. What CAN be pinned is the
// disposition that replaced it — a qualified read over a single-ForEach select
// DECLINES — and that is what this asserts. Restoring the arm makes a matching
// qualifier bake again and turns this red, which is the only handle the
// deletion has.
//
// The bare read beside it must keep baking. The arm removed was the QUALIFIER
// comparison; the same baker's bare-leaf path is live, is a separate debt
// entry, and a deletion that took it too would be a silent behaviour change on
// a busy path.
func TestSingleForEachBake_AQualifiedReadDeclines(t *testing.T) {
	t.Parallel()
	inner := legSelect("ID", "NAME")
	q := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("T"), expressions.InitialOf(inner))
	outer := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T")),
		[]expressions.Quantifier{q}, nil, []string{"T"})

	// The BARE leaf still bakes — the live path, and the guard that says this
	// fixture reaches the baker at all.
	bare := bakeDottedRefsToLegQOVWithRef(values.NewFlatFieldValue("NAME", values.UnknownType), logical.ColumnRef{}, outer)
	bfv, isFV := bare.(*values.FieldValue)
	if !isFV || bfv.Resolved == nil {
		t.Fatalf("fixture: the bare leaf NAME did not bake (%#v) — this baker's live arm "+
			"must work, or the decline below means only that nothing got here", bare)
	}

	// The QUALIFIED read — qualified by the quantifier's OWN alias, the exact
	// shape the deleted arm matched — must not bake.
	in := values.NewFlatFieldValue("T.NAME", values.UnknownType)
	if out := bakeDottedRefsToLegQOVWithRef(in, logical.ColumnRef{}, outer); out != values.Value(in) {
		t.Fatalf("a qualified read over a single-ForEach select baked to %#v — the arm "+
			"that matched the qualifier against the layout's binding TEXT is deleted, "+
			"and its replacement disposition is to decline and be loud at evaluation",
			out)
	}
}
