package executor

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A join leg's row layout is stated TWICE by two independent walks, and a
// disagreement between them is silent wrong rows.
//
// The PLANNER states it from the leg's chosen plan (cascades.PlanLegConcatLayout
// over planBuriedLegConcat) to decide what ordinal a leg-correlated reference
// carries. The RUNTIME states it from the leg rows themselves
// (concatLegPositionals) when it builds the merged row that reference will read.
// If the planner says a column is at slot 2 and the cursor puts it at slot 1, the
// read succeeds and answers with a neighbour's value — no error, no wrong-type,
// just a different row.
//
// Java has no such pair to reconcile: a physical plan's flowed type IS its
// quantifier's flowed type (Quantifier.java:806-810 reads the ranged-over
// Reference's result type), so the planner and the executor are looking at one
// object. Go derives the layout twice, which makes the agreement an invariant to
// PIN rather than a property to assume — the same reasoning that produced the
// ordinalJoinSpans/OrdinalSeedLegWindows cross-agreement fixture beside this one.
//
// The pin is a comparison, not two independent expectations, so a mutation to
// EITHER walk fails it. Reversing the leg order, shifting an offset, or dropping a
// window on one side reds the test; making the same change on both sides is the
// only way to keep it green, which is exactly the property an agreement invariant
// should have.

// legLayoutFixture builds one leg as both sides see it: a typed scan plan for the
// planner walk, and a PositionalRow of the same shape for the runtime walk.
func legLayoutFixture(table string, cols ...string) (plans.RecordQueryPlan, *PositionalRow) {
	fields := make([]values.Field, len(cols))
	slots := make([]any, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		slots[i] = int64(i)
	}
	rt := &values.RecordType{Fields: fields}
	return plans.NewRecordQueryScanPlan([]string{table}, rt, false), &PositionalRow{Type: rt, Slots: slots}
}

// legWindowsOf renders a row type's leg table as comparable text: identity, start,
// width. Identity rather than Name, because the name is a display spelling and two
// legs may share one (RFC-197).
func legWindowsOf(rt *values.RecordType) []string {
	out := make([]string, len(rt.Legs))
	for i, lg := range rt.Legs {
		out[i] = fmt.Sprintf("%s@[%d,%d)", lg.Alias.Name(), lg.Start, lg.Start+lg.Width)
	}
	return out
}

func TestLegLayout_PlannerAndRuntimeAgree(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("A")
	innerAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.NamedCorrelationIdentifier("J")

	// Asymmetric widths: a 3+2 concat makes every offset distinct, so a rotated or
	// swapped leg table is a different answer rather than a coincidence. Two legs
	// of equal width would let an order reversal survive the offset comparison.
	outerPlan, outerRow := legLayoutFixture("OUTER", "A0", "A1", "A2")
	innerPlan, innerRow := legLayoutFixture("INNER", "B0", "B1")

	nlj := plans.NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan, nil, plans.JoinInner, outerAlias, innerAlias,
		values.NewRecordConstructorValue(),
	)

	planner := cascades.PlanLegConcatLayout(nlj, joinAlias)
	if planner == nil {
		t.Fatal("the planner walk declined a plain INNER join of two typed scans — " +
			"the simplest shape it has an arm for. With no planner-side layout there " +
			"is nothing to compare, so this test would pass vacuously; that is why the " +
			"decline is fatal rather than skipped.")
	}
	runtimeRow := concatLegPositionals(outerRow, innerRow, outerAlias, innerAlias)
	if runtimeRow == nil || runtimeRow.Type == nil {
		t.Fatal("concatLegPositionals produced no merged row for two positional legs")
	}

	// 1. The concat's WIDTH and column order.
	if got, want := len(runtimeRow.Type.Fields), len(planner.Fields); got != want {
		t.Fatalf("merged row width disagrees: runtime %d columns, planner %d. A "+
			"planner ordinal is an index into this row; a width disagreement means "+
			"some ordinals index past its end and others index the wrong column.",
			got, want)
	}
	for i := range planner.Fields {
		pf, rf := planner.Fields[i], runtimeRow.Type.Fields[i]
		if pf.Name != rf.Name {
			t.Fatalf("merged slot %d: planner calls it %q, runtime calls it %q. The "+
				"slot is positional so the NAME decides nothing on its own, but the two "+
				"walks disagreeing about which column landed here means they disagree "+
				"about the ORDER, and the order is what an ordinal indexes.",
				i, pf.Name, rf.Name)
		}
		if rf.Ordinal != i {
			t.Fatalf("runtime slot %d carries Ordinal %d — the merged row's fields must "+
				"be renumbered into the concat's own domain, or a leg-relative ordinal "+
				"survives into the merged row and reads its old leg's slot", i, rf.Ordinal)
		}
	}

	// 2. The LEG TABLE — the part a rotation moves. Compared as an ordered
	// sequence: first-claim precedence is what every reader of this table uses
	// (addBuriedLegLayouts, bindMergedOuterLegs), so the ORDER is load-bearing and
	// a set comparison would let a reversal through.
	gotWindows, wantWindows := legWindowsOf(runtimeRow.Type), legWindowsOf(planner)
	if len(gotWindows) != len(wantWindows) {
		t.Fatalf("leg table size disagrees: runtime %v, planner %v", gotWindows, wantWindows)
	}
	for i := range wantWindows {
		if gotWindows[i] != wantWindows[i] {
			t.Fatalf("leg window %d disagrees: runtime %v, planner %v (full tables: "+
				"runtime %v, planner %v).\nA leg-correlated read is placed by the "+
				"PLANNER's window and served by the RUNTIME's; when they disagree the "+
				"read resolves to a slot inside another leg and returns that leg's "+
				"value with no error at all.", i, gotWindows[i], wantWindows[i],
				gotWindows, wantWindows)
		}
	}

	// 3. The windows must actually cover the row they claim to describe — an
	// agreement between two identically-wrong walks is still wrong.
	covered := 0
	for _, lg := range runtimeRow.Type.Legs {
		covered += lg.Width
	}
	if covered != len(runtimeRow.Type.Fields) {
		t.Fatalf("the leg windows cover %d of %d merged columns (%v). Both walks "+
			"agreeing on a table that does not tile the row means a column belongs to "+
			"no leg, and a read correlated to the leg that should own it finds nothing.",
			covered, len(runtimeRow.Type.Fields), gotWindows)
	}
}

// TestLegLayout_PlannerAndRuntimeAgreeOnOffsetsWhenNamesDiverge isolates the ONE
// place the two walks deliberately differ, so the divergence is stated rather than
// discovered later as a failure of the test above.
//
// concatLegPositionals renames a width-1 leg whose single column is the positional
// placeholder `_0` to the leg's alias (legFieldName), so a downstream BARE read of
// a bare-scalar UNNEST alias resolves to the element. The planner walk keeps `_0`.
// That is a DISPLAY-name difference and it must stay one: the offsets, widths and
// identities — everything an ordinal depends on — have to remain identical.
func TestLegLayout_PlannerAndRuntimeAgreeOnOffsetsWhenNamesDiverge(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("A")
	innerAlias := values.NamedCorrelationIdentifier("X")
	joinAlias := values.NamedCorrelationIdentifier("J")

	outerPlan, outerRow := legLayoutFixture("OUTER", "A0", "A1")
	innerPlan, innerRow := legLayoutFixture("ELEM", values.OrdinalFieldName(0))

	nlj := plans.NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan, nil, plans.JoinInner, outerAlias, innerAlias,
		values.NewRecordConstructorValue(),
	)
	planner := cascades.PlanLegConcatLayout(nlj, joinAlias)
	if planner == nil {
		t.Fatal("the planner walk declined the width-1 element leg shape")
	}
	runtimeRow := concatLegPositionals(outerRow, innerRow, outerAlias, innerAlias)

	// The rename happened — otherwise this test is pinning nothing.
	if runtimeRow.Type.Fields[2].Name != "X" {
		t.Fatalf("runtime slot 2 is named %q, want the alias X. legFieldName's "+
			"width-1 `_0` rename is the divergence this test exists to bound; if it "+
			"stopped happening the bound is vacuous and the test should be retargeted "+
			"onto whatever replaced it.", runtimeRow.Type.Fields[2].Name)
	}
	if planner.Fields[2].Name != values.OrdinalFieldName(0) {
		t.Fatalf("planner slot 2 is named %q, want %q — if the planner walk started "+
			"renaming too, the two sides no longer diverge here and this test should "+
			"become part of the strict agreement above",
			planner.Fields[2].Name, values.OrdinalFieldName(0))
	}

	// Everything an ordinal depends on is still identical.
	if got, want := len(runtimeRow.Type.Fields), len(planner.Fields); got != want {
		t.Fatalf("width disagrees: runtime %d, planner %d — a rename may not change "+
			"the concat's shape", got, want)
	}
	gotWindows, wantWindows := legWindowsOf(runtimeRow.Type), legWindowsOf(planner)
	for i := range wantWindows {
		if i >= len(gotWindows) || gotWindows[i] != wantWindows[i] {
			t.Fatalf("leg windows diverged along with the name: runtime %v, planner "+
				"%v. The `_0` rename is licensed as a DISPLAY change only; moving an "+
				"offset with it turns a cosmetic divergence into a wrong-slot read.",
				gotWindows, wantWindows)
		}
	}
}
