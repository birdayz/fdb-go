package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PlanLegConcatLayout is EXPORTED, so its result crosses a package boundary and
// the immutability of a plan's result type stops being something this package
// can enforce by inspection.
//
// The walk's single-leaf arm returns the scan plan's own `rt.Fields` slice
// verbatim. Without a copy the exported call hands out a writable view of the
// plan: one assignment through the returned layout rewrites the plan's result
// type, and every later reader — the cursor's ordinal reads included — sees the
// rewritten column.
func TestPlanLegConcatLayout_ReturnsADefensiveCopy(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("A")
	rt := nljTestLayouts["OUTER"] // ID, CATEGORY
	scan := plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.Type(rt), false)

	planType, isRT := scan.GetResultType().(*values.RecordType)
	if !isRT {
		t.Fatalf("fixture: scan result type is %T, want *values.RecordType", scan.GetResultType())
	}
	if len(planType.Fields) == 0 {
		t.Fatal("fixture: scan plan states no fields, so nothing can be corrupted")
	}
	before := planType.Fields[0].Name

	layout := PlanLegConcatLayout(scan, alias)
	if layout == nil || len(layout.Fields) == 0 {
		t.Fatalf("fixture: PlanLegConcatLayout declined the scan leg (%v)", layout)
	}

	// The caller mutates what it was handed — which an exported value that
	// documents no ownership is entitled to do.
	layout.Fields[0].Name = "CORRUPTED"

	if got := planType.Fields[0].Name; got != before {
		t.Fatalf("mutating the returned layout REWROTE the plan's result type: "+
			"field 0 was %q, now %q.\n"+
			"  PlanLegConcatLayout handed out the plan's own Fields slice, so the\n"+
			"  layout and the plan are the same memory. Every later reader of the\n"+
			"  plan — including the cursor's ordinal reads — now sees a column the\n"+
			"  plan never declared.", before, got)
	}
}

// The same aliasing, one level up and INSIDE the walk: the NLJ arm concatenates
// the two sides, and the scan arm hands it the leg plan's own `rt.Fields`. When
// that slice has spare capacity, `append(outerFields, innerFields...)` writes
// the inner's fields into the PLAN's backing array — so two concats derived over
// the SAME outer leg plan share storage and the second overwrites the first's
// tail.
//
// This exercises planBuriedLegConcat DIRECTLY rather than through
// PlanLegConcatLayout. Going through the exported entry point cannot detect it:
// the defensive copy that entry point makes snapshots the first result before
// the second derivation runs, so the corruption happens and is then hidden. That
// is not a hypothetical — this test was FIRST written against the exported path
// and stayed green under the very mutation it names.
//
// The fixture builds its RecordType with a hand-made spare-capacity slice for
// the same reason: `nljTestLayout` allocates `make([]Field, len(cols))`, whose
// cap equals its len, so append always reallocates and the hazard cannot arise.
// A test that cannot express the defect is not coverage.
func TestPlanBuriedLegConcat_DoesNotAppendIntoALegPlansBackingArray(t *testing.T) {
	t.Parallel()

	// len 2, cap 8 — the spare capacity an append can write into.
	backing := make([]values.Field, 2, 8)
	backing[0] = values.Field{Name: "ID", FieldType: values.UnknownType, Ordinal: 0}
	backing[1] = values.Field{Name: "CATEGORY", FieldType: values.UnknownType, Ordinal: 1}
	outerRT := &values.RecordType{RecordName: "OUTER", Fields: backing}
	if cap(outerRT.Fields) <= len(outerRT.Fields) {
		t.Fatalf("fixture: outer fields cap %d must exceed len %d, or the append "+
			"reallocates and the hazard cannot arise",
			cap(outerRT.Fields), len(outerRT.Fields))
	}

	outerAlias := values.NamedCorrelationIdentifier("A")
	joinAlias := values.NamedCorrelationIdentifier("J")

	// ONE outer scan plan, reused as the outer leg of two different joins.
	sharedOuter := plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.Type(outerRT), false)
	mkJoin := func(table string, rt *values.RecordType, innerAlias string) plans.RecordQueryPlan {
		return plans.NewRecordQueryNestedLoopJoinPlan(
			sharedOuter, plans.NewRecordQueryScanPlan([]string{table}, values.Type(rt), false),
			nil, plans.JoinInner, outerAlias, values.NamedCorrelationIdentifier(innerAlias), nil)
	}

	fields1, _, ok := planBuriedLegConcat(mkJoin("INNER", nljTestLayouts["INNER"], "B"), joinAlias, 0)
	if !ok || len(fields1) == 0 {
		t.Fatal("fixture: the walk declined the first join — an INNER NLJ over two " +
			"scans is exactly the shape it reduces, so a decline means the fixture " +
			"stopped building that shape and the test proves nothing")
	}
	snapshot := make([]string, len(fields1))
	for i, f := range fields1 {
		snapshot[i] = f.Name
	}

	// A SECOND concat over the same outer leg. With the append-onto-outer form
	// this writes through into `backing[2:]`, which is where fields1's tail lives.
	if _, _, ok2 := planBuriedLegConcat(mkJoin("SHADOW", nljTestLayouts["SHADOW"], "C"), joinAlias, 0); !ok2 {
		t.Fatal("fixture: the walk declined the second join — see the first")
	}

	for i, f := range fields1 {
		if f.Name != snapshot[i] {
			t.Fatalf("deriving a second concat over the SAME leg plan rewrote the first: "+
				"field %d was %q, now %q (first concat now %v).\n"+
				"  Both concats appended onto the outer leg plan's own Fields slice, so\n"+
				"  they share its spare capacity and the second overwrote the first's\n"+
				"  tail. A layout that changes under an unrelated derivation cannot\n"+
				"  anchor an ordinal.", i, snapshot[i], f.Name, fields1)
		}
	}
}
