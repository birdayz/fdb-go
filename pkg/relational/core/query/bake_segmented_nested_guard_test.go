package query

// bakeSegmentedColumnRef bakes a LAZY carrier — a FieldValue that names a
// column and has not yet been bound to a slot. This file pins the boundary of
// that word: a reference which is ALREADY resolved, in particular a fused
// struct descent, must come back untouched.
//
// THE HAZARD, stated as rows rather than as types. The resolver fuses a nested
// reference onto ONE value: `n.sk` is FieldValue{Field:"N", Resolved:[N,SK]} —
// the struct root in Field, the member in the accessor path. Without the
// guard, the flat first-match compares Field ("N", the STRUCT) against the
// output columns; a base-record layout does expose a column "N" (the struct
// itself), so the match succeeds and the function returns a SINGLE-accessor
// ordinal for it. The value then reads the WHOLE STRUCT where the member was
// asked for. There is no error at any point — the ordinal is valid, the domain
// is right, and the rows are wrong.
//
// WHY THIS IS A UNIT PIN AND NOT A QUERY. No end-to-end test can separate "the
// guard declined" from "the guard was never reached": today nothing hands this
// function a resolved value at all (see the reachability note below), so every
// query passes either way. The two answers diverge the moment a producer
// changes, which is exactly when a green suite must stop being green. So the
// fused value is constructed here and handed to the function directly.
//
// REACHABILITY. The hazard is unreachable today, and only by agreement among
// callers: each of bakeSegmentedColumnRef's call sites admits its argument
// behind `fv == minted`, and every `minted` is a freshly built lazy FieldValue
// with nil Resolved and nil Child. That is a property of the call sites, not
// of the function, and pointer identity is not an invariant anyone should be
// asked to preserve by hand. This test is what makes the contract the
// function's own, so the guard cannot be read as dead code and deleted.

import (
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// fusedNestedRef builds the value the resolver mints for `n.sk` over a table
// whose column N is a struct with member SK at field ordinal 0: one FieldValue
// rooted on the struct column and carrying the descent as an accessor suffix.
func fusedNestedRef(rootOrdinal int, cols []string) *values.FieldValue {
	return &values.FieldValue{
		Field: "N",
		Typ:   values.UnknownType,
		Resolved: &values.FieldPath{
			Accessors: []values.ResolvedAccessor{
				{Field: "N", Ordinal: rootOrdinal},
				{Field: "SK", Ordinal: 0},
			},
			Domain: values.OrdinalDomainOfColumnNames(cols),
		},
	}
}

func TestBakeSegmentedColumnRef_DeclinesAFusedNestedDescent(t *testing.T) {
	t.Parallel()

	// The struct root IS one of the flat output columns. That is the whole
	// hazard: the columns are not adversarial, they are the ordinary layout a
	// base-record scan of `t(id BIGINT, n gst)` produces, and "N" names the
	// struct. A layout WITHOUT N would make the test pass with the guard
	// removed, which is the coverage-shaped version of this test that proves
	// nothing.
	cols := []string{"ID", "N"}
	ref := logical.ColumnRef{Present: true, Bare: "SK", Qualifier: "N", Qualified: true}

	nested := fusedNestedRef(1, cols)
	got := bakeSegmentedColumnRef(nested, ref, cols, nil)

	fv, isField := got.(*values.FieldValue)
	if !isField {
		t.Fatalf("bakeSegmentedColumnRef returned %T, want the *values.FieldValue it was handed", got)
	}
	if fv != nested {
		t.Fatalf("bakeSegmentedColumnRef REWROTE a fused nested descent.\n"+
			"  handed: Field=%q Resolved=%v\n"+
			"  got:    Field=%q Resolved=%v\n\n"+
			"  A resolved descent is already addressed. Re-baking it can only "+
			"replace the accessor path with a single ordinal for the value's "+
			"leading segment — and that segment is the STRUCT ROOT, not a "+
			"source-leg qualifier. The result reads the whole struct N where "+
			"the member N.SK was asked for: wrong rows, no error anywhere.",
			nested.Field, accessorPathOf(nested), fv.Field, accessorPathOf(fv))
	}
	// State the wrong answer explicitly, so the failure above is not the only
	// thing describing it: the struct root sits at ordinal 1 of cols, and that
	// is precisely the single-accessor read the unguarded first-match returns.
	if len(fv.Resolved.Accessors) != 2 {
		t.Fatalf("the fused descent lost accessors: got %v, want the 2-step path [N SK]. "+
			"A 1-step path IS the struct-root read this guard exists to prevent.",
			accessorPathOf(fv))
	}
}

// TestBakeSegmentedColumnRef_DeclinesAFusedNestedDescentBeforeTheLegWindow
// drives the SECOND wrong-answer route inside the same function, and it is a
// separate pin because the guard can be got wrong in two independent ways.
//
// The function resolves in two stages: the flat first-match on fv.Field, then —
// only for a QUALIFIED ref — a leg-window lookup keyed on ref.Qualifier. The
// test above closes the first. This one closes the second by making the flat
// match MISS (no column "N" in the layout) while a leg NAMED "N" is in scope
// carrying a column "SK". A guard placed after the first-match loop instead of
// at the entry would pass the test above and still hand back a leg-window
// ordinal here.
//
// It is also the argument shape that separates the call sites. Of the five,
// four pass a real leg table (the ORDER-BY sort keys, the two projection
// passes and the group-key/aggregate-operand passes) and one passes nil legs.
// Driving both means the pin covers what actually varies between callers,
// rather than covering one caller five times: every site funnels into this one
// function, so the caller-facing axis is the arguments, not the call.
func TestBakeSegmentedColumnRef_DeclinesAFusedNestedDescentBeforeTheLegWindow(t *testing.T) {
	t.Parallel()

	// No "N" among the flat columns, so the first-match cannot fire and the
	// leg window is the only route left. A leg IS named N — a source aliased N
	// is an ordinary thing to write — and it carries SK, so the lookup the
	// guard must pre-empt would succeed.
	cols := []string{"ID", "SK", "CO"}
	legs := []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("N"), "N", 1, 2),
	}
	ref := logical.ColumnRef{Present: true, Bare: "SK", Qualifier: "N", Qualified: true}

	nested := fusedNestedRef(0, cols)
	got := bakeSegmentedColumnRef(nested, ref, cols, legs)

	if got != values.Value(nested) {
		fv, _ := got.(*values.FieldValue)
		t.Fatalf("a fused nested descent resolved through a LEG WINDOW to %v.\n\n"+
			"  The reference's leading segment N is a STRUCT ROOT — the descent "+
			"reads member SK of column N. Treating it as a leg qualifier picks "+
			"leg N's column SK, a different value from a different place, with "+
			"no error. The guard must decline at the FUNCTION ENTRY, ahead of "+
			"both the flat first-match and this lookup; a guard placed between "+
			"the two closes only half the hazard.", accessorPathOf(fv))
	}
}

// TestBakeSegmentedColumnRef_StillBakesTheLazyCarrier is the control, and it is
// not optional. Without it the test above is satisfied by a function that
// declines EVERYTHING — including the lazy carriers it exists to bake — which
// would be a silent regression reported as a pass. It also proves the cols and
// ref used above are a population the function really acts on, rather than an
// input it was always going to return unchanged.
func TestBakeSegmentedColumnRef_StillBakesTheLazyCarrier(t *testing.T) {
	t.Parallel()

	cols := []string{"ID", "N"}
	ref := logical.ColumnRef{Present: true, Bare: "N", Qualifier: "", Qualified: false}

	lazy := &values.FieldValue{Field: "N", Typ: values.UnknownType}
	got := bakeSegmentedColumnRef(lazy, ref, cols, nil)

	fv, isField := got.(*values.FieldValue)
	if !isField {
		t.Fatalf("bakeSegmentedColumnRef returned %T, want *values.FieldValue", got)
	}
	if fv.Resolved == nil {
		t.Fatalf("bakeSegmentedColumnRef left a LAZY carrier unbaked (Field=%q, cols=%v). "+
			"The nested guard must partition on RESOLVEDNESS, not turn the "+
			"function off: a lazy flat reference naming an output column is "+
			"exactly what this function is for.", fv.Field, cols)
	}
	if len(fv.Resolved.Accessors) != 1 || fv.Resolved.Accessors[0].Ordinal != 1 {
		t.Fatalf("lazy carrier baked to %v, want the single accessor ordinal 1 (column N of %v)",
			accessorPathOf(fv), cols)
	}
}

// accessorPathOf renders a FieldValue's resolved path for failure messages —
// "[N#1 SK#0]" — so a wrong-slot read is legible in the output instead of
// having to be reconstructed from a %#v dump.
func accessorPathOf(fv *values.FieldValue) string {
	if fv == nil || fv.Resolved == nil {
		return "<lazy>"
	}
	out := "["
	for i, a := range fv.Resolved.Accessors {
		if i > 0 {
			out += " "
		}
		out += a.Field + "#" + strconv.Itoa(a.Ordinal)
	}
	return out + "]"
}
