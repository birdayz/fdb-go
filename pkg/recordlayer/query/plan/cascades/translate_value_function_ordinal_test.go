package cascades

// Pins the fetch-push-down ordinal gate in
// ValueIndexScanMatchCandidate.buildTranslateValueFunction: a bare covered
// column's baked ordinal is PRESERVED when pushed below the Fetch (the bug that
// produced a lazy "ordinal -1" reference), but a FUSED multi-accessor path whose
// leaf name collides with a covered column is refused outright — baking it
// single-accessor would drop the descent and read the wrong slot (wrong rows),
// and pushing it as a lazy NAME read is the same wrong column one indirection
// later. The fused-collision axis is the dimension that was unprobed and let the
// original bug in.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFetchTranslate_PreservesSingleAccessorOrdinal_DeclinesFused(t *testing.T) {
	t.Parallel()

	rowType := testRecordRowType("T", "ID", "ADDR", "CITY")
	cand := newKnownDistinctValueIndexCandidate(
		"IDX_CITY",
		[]string{"T"},
		[]string{"CITY"}, // covered column
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("p0")},
		rowType,
		false,
		[]string{"ID"},
	)
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")

	// (a) BARE single-accessor baked column reference (the covering `r.c` shape,
	// source-relative OR frontier-pinned, both single-accessor) → the ordinal
	// must be PRESERVED and rebased onto the target.
	bare := testColumnRef(values.NewQuantifiedObjectValue(src), rowType, "CITY", values.UnknownType)
	out, ok := cand.PushValueThroughFetch(bare, src, tgt)
	if !ok {
		t.Fatal("bare covered column must translate")
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved == nil {
		t.Fatalf("bare covered column must stay BAKED after push (got %#v) — dropping the ordinal is the bug", out)
	}
	if got := fv.Resolved.Root().Ordinal; got != 2 {
		t.Fatalf("preserved ordinal = %d, want 2 (CITY's slot in the record layout)", got)
	}
	if _, stated := fv.OrdinalIn(values.OrdinalDomainOfType(rowType)); !stated {
		t.Fatalf("pushed reference must state the layout its ordinal indexes, got %#v", fv.Resolved)
	}
	if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); !isQOV || qov.Correlation != tgt {
		t.Fatalf("translated reference must rebase onto the target alias, child = %#v", fv.Child)
	}

	// (b) The FRONTIER-PINNED single-accessor form of the same column — the
	// shape the join/merge machinery produces — must translate identically.
	// Both bake kinds are admitted because each STATES the layout it indexes;
	// admitting them is not a favour to the pin, it is the domain check
	// passing.
	pinnedSrc := values.NewQuantifiedObjectValueOfType(src, rowType)
	pinned, err := values.NewFieldValueOfOrdinal(pinnedSrc, 2)
	if err != nil {
		t.Fatalf("pinned bake: %v", err)
	}
	outPinned, okPinned := cand.PushValueThroughFetch(pinned, src, tgt)
	if !okPinned {
		t.Fatal("frontier-pinned covered column must translate")
	}
	if fvp, isFV := outPinned.(*values.FieldValue); !isFV || fvp.Resolved == nil || fvp.Resolved.Root().Ordinal != 2 {
		t.Fatalf("pinned push must preserve ordinal 2, got %#v", outPinned)
	}

	// (c) FUSED multi-accessor path `addr.city` (Field="CITY", Child=QOV,
	// Resolved=[ADDR@1, CITY@0]) whose LEAF name collides with the covered
	// column CITY. Its Root() ordinal is ADDR's (1) — baking it single-accessor
	// would drop the `.city` descent and read slot 1, the WRONG value; pushing
	// it lazily hands the index entry's own CITY column to a predicate that
	// asked for the nested one. It must DECLINE.
	fusedPath := values.NewFieldPathOfSingleInDomain("ADDR", 1, false, values.OrdinalDomainOfType(rowType)).
		WithSuffix(values.NewFieldPathOfSingle("CITY", 0, false))
	fused := &values.FieldValue{
		Field:    "CITY",
		Child:    values.NewQuantifiedObjectValue(src),
		Resolved: fusedPath,
		Typ:      values.UnknownType,
	}
	out2, ok2 := cand.PushValueThroughFetch(fused, src, tgt)
	if ok2 {
		t.Fatalf("FUSED multi-accessor path must NOT push below the fetch (its root ordinal is ADDR's, "+
			"and the leaf name is not an identity) — got %#v", out2)
	}

	// (c2) The same refusal where the fused path's ROOT ordinal IS a covered
	// one: `t.city.sub` over a nested CITY at slot 2, the covered slot. Here
	// answering with the root ordinal is not merely unproven, it is actively
	// wrong — the push would emit a single-accessor read of CITY and silently
	// drop the `.sub` descent, returning the whole nested record where a
	// sub-field was asked for. The leaf DISPLAY name ("SUB") is not covered,
	// so a name-keyed site declined this by accident; the ordinal site must
	// decline it on purpose.
	nestedFused := values.NewFieldPathOfSingleInDomain("CITY", 2, false, values.OrdinalDomainOfType(rowType)).
		WithSuffix(values.NewFieldPathOfSingle("SUB", 0, false))
	nested := &values.FieldValue{
		Field:    "SUB",
		Child:    values.NewQuantifiedObjectValue(src),
		Resolved: nestedFused,
		Typ:      values.UnknownType,
	}
	if out, ok := cand.PushValueThroughFetch(nested, src, tgt); ok {
		t.Fatalf("a fused path whose ROOT ordinal is covered must still decline — pushing it drops the "+
			"descent and reads the parent slot; got %#v", out)
	}

	// (d) A reference whose ordinal indexes ANOTHER layout — the same integer,
	// a different column. A join box's assembled row is the real producer of
	// this shape (a leg-relative ordinal on a merged row), and the name check
	// this site used to run could not even express the question.
	otherRowType := testRecordRowType("OTHER", "CITY", "ID", "ADDR")
	foreign := testColumnRef(values.NewQuantifiedObjectValue(src), otherRowType, "CITY", values.UnknownType)
	if out3, ok3 := cand.PushValueThroughFetch(foreign, src, tgt); ok3 {
		t.Fatalf("an ordinal from a DIFFERENT layout must not be read as a record-descriptor ordinal, got %#v", out3)
	}

	// (e) A LAZY reference carrying only the covered display name: no ordinal
	// to check, and the name is not a fallback.
	lazy := values.NewFieldValue(values.NewQuantifiedObjectValue(src), "CITY", values.UnknownType)
	if out4, ok4 := cand.PushValueThroughFetch(lazy, src, tgt); ok4 {
		t.Fatalf("a LAZY reference must not push on the strength of its display name, got %#v", out4)
	}
}
