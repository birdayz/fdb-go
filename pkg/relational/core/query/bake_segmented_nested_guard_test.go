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
// callers. The enumeration is given as the command that produces it rather
// than as a bare number, because this count has already moved three times:
//
//	grep -rn "bakeSegmentedColumnRef(" pkg/ --include="*.go" | grep -v _test.go
//
// returns the definition plus SIX callers — five in cascades_translator.go and
// one in clustered_outer_scalar.go. Each admits its argument behind
// `fv == minted`, and every `minted` is a freshly built lazy FieldValue with
// nil Resolved and nil Child. That is a property of the call sites, not of the
// function, and pointer identity is not an invariant anyone should be asked to
// preserve by hand. This test is what makes the contract the function's own, so
// the guard cannot be read as dead code and deleted.
//
// The guard is a PASS-THROUGH, not a refusal: a resolved value arrives already
// addressed and stays evaluable, so reaching this function with one is not an
// error — only re-baking it is. An assertion here would convert a correct value
// into a planner failure, which is why no arm below expects one.

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
// It is also the argument shape that separates the call sites. Of the six the
// grep in the header returns, FOUR pass a real leg table (the ORDER-BY sort
// keys, one of the two translateProject passes, and the group-key and
// aggregate-operand passes) and TWO pass nil legs (the other translateProject
// pass and clustered_outer_scalar.go). Driving both means the pin covers what
// actually varies between callers, rather than covering one caller six times:
// every site funnels into this one function, so the caller-facing axis is the
// arguments, not the call.
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

// TestBakeSegmentedColumnRef_PassesThroughAChildBearingRef drives the guard's
// OTHER arm. The guard is a disjunction — `fv.Child != nil || fv.Resolved !=
// nil` — and the two pins above drive only the Resolved side, so without this
// the Child half ships untested and reads as covered.
//
// The value is built so ONLY the Child arm can answer: Resolved is nil, so a
// guard reduced to `fv.Resolved != nil` falls straight through to the
// first-match loop. Field is "N" and "N" IS an output column, so that loop
// would bake it — and baking a quantifier-addressed read against the FLAT
// output layout drops the correlation the value was addressed through, which
// is the same shape of loss as the nested case: a slot resolved against the
// wrong row.
func TestBakeSegmentedColumnRef_PassesThroughAChildBearingRef(t *testing.T) {
	t.Parallel()

	cols := []string{"ID", "N"}
	ref := logical.ColumnRef{Present: true, Bare: "N", Qualifier: "Q", Qualified: true}

	child := &values.FieldValue{
		Field: "N",
		Typ:   values.UnknownType,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("Q")),
	}
	if child.Resolved != nil {
		t.Fatalf("fixture error: the Child-arm pin must carry a NIL Resolved, "+
			"otherwise the Resolved arm answers and this test measures nothing. "+
			"got Resolved=%v", child.Resolved)
	}

	got := bakeSegmentedColumnRef(child, ref, cols, nil)
	if got != values.Value(child) {
		t.Fatalf("bakeSegmentedColumnRef REWROTE a child-bearing reference to %#v.\n\n"+
			"  A FieldValue with a Child is addressed THROUGH its quantifier, not "+
			"against the flat output layout. Re-baking it to a flat ordinal "+
			"discards the correlation and binds the read to a different row. "+
			"The guard is a disjunction and this is its second arm: if only "+
			"`fv.Resolved != nil` survives, this is what escapes.", got)
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
