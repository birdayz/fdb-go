package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustLegConcatConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct leg-concat fixture: " + err.Error())
	}
	return value
}

func legConcatLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, col := range cols {
		fields[i] = values.Field{Name: col, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

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
	rt := legConcatLayout("OUTER", "ID", "CATEGORY")
	scan := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.Type(rt), false))

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
	backing[0] = values.Field{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}
	backing[1] = values.Field{Name: "CATEGORY", FieldType: values.NotNullString, Ordinal: 1}
	outerRT := &values.RecordType{RecordName: "OUTER", Fields: backing}
	if cap(outerRT.Fields) <= len(outerRT.Fields) {
		t.Fatalf("fixture: outer fields cap %d must exceed len %d, or the append "+
			"reallocates and the hazard cannot arise",
			cap(outerRT.Fields), len(outerRT.Fields))
	}

	outerAlias := values.NamedCorrelationIdentifier("A")
	joinAlias := values.NamedCorrelationIdentifier("J")

	// ONE outer scan plan, reused as the outer leg of two different joins.
	sharedOuter := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.Type(outerRT), false))
	mkJoin := func(table string, rt *values.RecordType, innerAlias string) plans.RecordQueryPlan {
		inner := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
			[]string{table}, values.Type(rt), false))
		resultType := legConcatLayout("JoinResult", "OUTER_ID", "OUTER_CATEGORY", "INNER_ID", "INNER_VALUE")
		result := values.NewQueriedValue(nil, resultType)
		return mustLegConcatConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			sharedOuter, inner, nil, plans.JoinInner, outerAlias,
			values.NamedCorrelationIdentifier(innerAlias), result))
	}

	fields1, _, ok := planBuriedLegConcat(mkJoin(
		"INNER", legConcatLayout("INNER", "ID", "OUTER_ID"), "B"), joinAlias, 0)
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
	if _, _, ok2 := planBuriedLegConcat(mkJoin(
		"SHADOW", legConcatLayout("SHADOW", "ID", "NOTE"), "C"), joinAlias, 0); !ok2 {
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

// A CONCAT'S FIELD ORDINALS MUST BE ITS OWN POSITIONS, and the leg that is not
// first is the one that proves it.
//
// Each leaf arm of the walk reports its leg PLAN's result-type fields, numbered
// from 0 in that plan's own row. Concatenating two of them therefore reads
// 0,1,0,1 unless the walk rebases — and the `base` parameter it already threads
// is exactly the offset to rebase by, since the flat-run leg beside those fields
// is stamped [base, base+len).
//
// The consequence is not cosmetic and not local. A values.RecordType whose
// Fields[i].Ordinal != i is not an EXACT type at all: the snapshot rejects it
// with "record field ordinal does not equal its position", so
// values.NewQuantifiedObjectValue over the concat fails and the entire ordinal
// seed declines. What reaches a reader is "this shape does not ordinalize",
// which is indistinguishable from a shape that genuinely cannot — so the defect
// costs plans silently and shows up only as a census class moving.
//
// A ONE-LEG fixture cannot express any of this: at base 0 the leg's own
// numbering IS its position and the bug is invisible. The join below is the
// smallest shape where the second leg sits at a non-zero offset.
func TestPlanBuriedLegConcat_OrdinalsAreConcatPositionsNotLegPositions(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("A")
	joinAlias := values.NamedCorrelationIdentifier("J")

	outer := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.Type(legConcatLayout("OUTER", "ID", "CATEGORY")), false))
	inner := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"INNER"}, values.Type(legConcatLayout("INNER", "IID", "NOTE")), false))
	resultType := legConcatLayout("JoinResult", "ID", "CATEGORY", "IID", "NOTE")
	join := mustLegConcatConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		outer, inner, nil, plans.JoinInner, outerAlias,
		values.NamedCorrelationIdentifier("B"), values.NewQueriedValue(nil, resultType)))

	fields, legs, ok := planBuriedLegConcat(join, joinAlias, 0)
	if !ok {
		t.Fatal("fixture: the walk declined an INNER NLJ over two scans, which is " +
			"exactly the shape it reduces — nothing below is being tested")
	}
	if len(fields) != 4 {
		t.Fatalf("fixture: concat has %d field(s), want 4 — the second leg must start "+
			"at a NON-ZERO offset or this test cannot express the defect", len(fields))
	}
	for i, f := range fields {
		if f.Ordinal != i {
			t.Fatalf("concat field %d (%q) carries Ordinal %d.\n"+
				"  A leg reports its plan's own numbering, so an un-rebased second leg\n"+
				"  restarts at 0 and the row reads 0,1,0,1. That type is not EXACT:\n"+
				"  values.NewQuantifiedObjectValue over it fails with \"record field\n"+
				"  ordinal does not equal its position\", the ordinal seed declines, and\n"+
				"  the decline presents as a shape that does not ordinalize.",
				i, f.Name, f.Ordinal)
		}
	}

	// The leg WINDOWS and the field ordinals are two statements of the same
	// offsets, and they have to agree — a window at [2,4) over fields numbered
	// 0,1,0,1 addresses slots that do not exist.
	if len(legs) != 2 {
		t.Fatalf("fixture: concat states %d leg window(s), want 2", len(legs))
	}
	if legs[0].Start != 0 || legs[0].Width != 2 || legs[1].Start != 2 || legs[1].Width != 2 {
		t.Fatalf("leg windows are %+v; want [0,2) and [2,4) — the same offsets the "+
			"field ordinals above state", legs)
	}

	// The end-to-end consequence, driven rather than argued: the concat must be
	// admissible as an exact type. This is the call reconstructFoldStep1Seed
	// makes, and it is what failed.
	concat := &values.RecordType{Fields: fields}
	if _, err := values.NewQuantifiedObjectValue(joinAlias, concat); err != nil {
		t.Fatalf("NewQuantifiedObjectValue over the concat failed: %v.\n"+
			"  The ordinal seed is built from exactly this call, so a malformed concat\n"+
			"  does not surface as a type error anywhere a reader would see it — the\n"+
			"  seed simply declines.", err)
	}
}

// The DEFENSIVE-COPY half of the rebase, kept separate because it is a
// different way to lose: rebasing writes an Ordinal, and the slice being
// rebased is the leg PLAN's own. Renumbering in place would move the plan's
// declared ordinals out from under every other reader of that plan.
func TestPlanBuriedLegConcat_RebasingDoesNotRenumberTheLegPlan(t *testing.T) {
	t.Parallel()

	innerRT := legConcatLayout("INNER", "IID", "NOTE")
	outer := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.Type(legConcatLayout("OUTER", "ID", "CATEGORY")), false))
	inner := mustLegConcatConstruct(plans.NewRecordQueryScanPlan(
		[]string{"INNER"}, values.Type(innerRT), false))
	resultType := legConcatLayout("JoinResult", "ID", "CATEGORY", "IID", "NOTE")
	join := mustLegConcatConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		outer, inner, nil, plans.JoinInner, values.NamedCorrelationIdentifier("A"),
		values.NamedCorrelationIdentifier("B"), values.NewQueriedValue(nil, resultType)))

	if _, _, ok := planBuriedLegConcat(join, values.NamedCorrelationIdentifier("J"), 0); !ok {
		t.Fatal("fixture: the walk declined the join")
	}

	planRT, isRT := inner.GetResultType().(*values.RecordType)
	if !isRT {
		t.Fatalf("fixture: inner result type is %T", inner.GetResultType())
	}
	for i, f := range planRT.Fields {
		if f.Ordinal != i {
			t.Fatalf("the inner LEG PLAN's own field %d (%q) now carries Ordinal %d.\n"+
				"  The rebase renumbered the plan's result type in place instead of a\n"+
				"  copy, so the plan now declares the offsets it happened to occupy in\n"+
				"  one concat — and every other reader of that plan inherits them.",
				i, f.Name, f.Ordinal)
		}
	}
}
