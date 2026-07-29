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

// The CORRELATION element of column identity, on the axis where it is the only
// element that can decide: ONE TABLE reached through TWO quantifiers (a
// self-join). Both references carry the same layout token — the layouts are
// literally the same table's — and the same ordinal, so the domain check and
// the ordinal check both PASS for both. Only the child's correlation separates
// them, and if the site does not check it, the foreign quantifier's column is
// rebased onto the fetch target and reads THIS row's index entry: wrong row,
// silently. This is the dimension that was unprobed when the site was migrated
// from names to ordinals — the name check it replaced could not decide it
// either, so the conversion inherited the hole.
func TestFetchTranslate_SameTableTwoQuantifiers_CorrelationDecides(t *testing.T) {
	t.Parallel()

	// Two independently built layouts for the same table, as two quantifiers
	// over one table produce. They are structurally identical, so the tokens
	// are EQUAL — asserted, because the whole point is that the domain element
	// cannot separate these two references.
	layoutQ1 := testRecordRowType("T", "ID", "ADDR", "CITY")
	layoutQ2 := testRecordRowType("T", "ID", "ADDR", "CITY")
	if values.OrdinalDomainOfType(layoutQ1) != values.OrdinalDomainOfType(layoutQ2) {
		t.Fatal("two quantifiers over one table must share a layout token — " +
			"if they ever stop, this test no longer probes the correlation axis")
	}

	cand := newKnownDistinctValueIndexCandidate(
		"IDX_CITY",
		[]string{"T"},
		[]string{"CITY"},
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("p0")},
		layoutQ1,
		false,
		[]string{"ID"},
	)
	q1 := values.NamedCorrelationIdentifier("q1")
	q2 := values.NamedCorrelationIdentifier("q2")
	tgt := values.NamedCorrelationIdentifier("tgt")

	// The fetch's OWN quantifier's column pushes.
	own := testColumnRef(values.NewQuantifiedObjectValue(q1), layoutQ1, "CITY", values.UnknownType)
	out, ok := cand.PushValueThroughFetch(own, q1, tgt)
	if !ok {
		t.Fatal("the source quantifier's covered column must push")
	}
	if fv, isFV := out.(*values.FieldValue); !isFV || fv.Resolved == nil || fv.Resolved.Root().Ordinal != 2 {
		t.Fatalf("own-quantifier push must preserve ordinal 2, got %#v", out)
	}

	// The OTHER quantifier's column — same table, same token, same ordinal,
	// same leaf name — must NOT translate. Nothing but the correlation is
	// different, which is exactly why it is the element being tested.
	foreign := testColumnRef(values.NewQuantifiedObjectValue(q2), layoutQ2, "CITY", values.UnknownType)
	if out, ok := cand.PushValueThroughFetch(foreign, q1, tgt); ok {
		t.Fatalf("a reference to ANOTHER quantifier's row must not be pushed onto this fetch's "+
			"index entry (same table, same layout token, same ordinal — only the correlation "+
			"differs); got %#v", out)
	}

	// Symmetry: the same value IS pushable when the fetch's source is q2, so
	// the refusal above is the pairing being checked and not the value being
	// rejected on some other ground.
	if _, ok := cand.PushValueThroughFetch(foreign, q2, tgt); !ok {
		t.Fatal("q2's column must push through q2's own fetch — otherwise the refusal above " +
			"proves nothing about the correlation")
	}

	// A CHILDLESS baked reference has no correlation to check and still
	// pushes: it reads the row being evaluated, and (domain, ordinal) fully
	// determine the column inside the record-descriptor frontier. Pinned so
	// the correlation gate above cannot be tightened into closing this door —
	// the post-ordinalization shape is the common one below a fetch.
	childless := values.NewFieldValueWithResolvedOrdinalInDomain(
		"CITY", 2, values.UnknownType, values.OrdinalDomainOfType(layoutQ1))
	if _, ok := cand.PushValueThroughFetch(childless, q1, tgt); !ok {
		t.Fatal("a CHILDLESS baked covered reference must still push — it has no correlation to " +
			"check and needs none")
	}
}
