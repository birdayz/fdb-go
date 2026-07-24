package cascades

// Pins the fetch-push-down ordinal-preservation gate in
// ValueIndexScanMatchCandidate.buildTranslateValueFunction: a bare covered
// column's baked ordinal is PRESERVED when pushed below the Fetch (the bug that
// produced a lazy "ordinal -1" reference), but a FUSED multi-accessor path whose
// leaf name collides with a covered column is NOT baked single-accessor — that
// would drop the descent and read the wrong slot (wrong rows). The
// fused-collision axis is the dimension that was unprobed and let the bug in.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFetchTranslate_PreservesSingleAccessorOrdinal_DeclinesFused(t *testing.T) {
	t.Parallel()

	cand := newKnownDistinctValueIndexCandidate(
		"IDX_CITY",
		[]string{"T"},
		[]string{"CITY"}, // covered column
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("p0")},
		values.UnknownType,
		false,
		[]string{"ID"},
	)
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")

	// (a) BARE single-accessor baked column reference (the covering `r.c` shape,
	// source-relative OR frontier-pinned, both single-accessor) → the ordinal
	// must be PRESERVED and rebased onto the target.
	bare := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(src), "CITY", 5, values.UnknownType,
	)
	out, ok := cand.PushValueThroughFetch(bare, src, tgt)
	if !ok {
		t.Fatal("bare covered column must translate")
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved == nil {
		t.Fatalf("bare covered column must stay BAKED after push (got %#v) — dropping the ordinal is the bug", out)
	}
	if got := fv.Resolved.Root().Ordinal; got != 5 {
		t.Fatalf("preserved ordinal = %d, want 5", got)
	}
	if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); !isQOV || qov.Correlation != tgt {
		t.Fatalf("translated reference must rebase onto the target alias, child = %#v", fv.Child)
	}

	// (b) FUSED multi-accessor path `addr.city` (Field="CITY", Child=QOV,
	// Resolved=[ADDR@1, CITY@0]) whose LEAF name collides with the covered
	// column CITY. Its Root() ordinal is ADDR's (1) — baking it single-accessor
	// would drop the `.city` descent and read slot 1, the WRONG value. The gate
	// must DECLINE to bake it (fall through to a lazy reference — correct-or-loud,
	// never wrong rows).
	fusedPath := values.NewFieldPathOfSingle("ADDR", 1, false).
		WithSuffix(values.NewFieldPathOfSingle("CITY", 0, false))
	fused := &values.FieldValue{
		Field:    "CITY",
		Child:    values.NewQuantifiedObjectValue(src),
		Resolved: fusedPath,
		Typ:      values.UnknownType,
	}
	out2, ok2 := cand.PushValueThroughFetch(fused, src, tgt)
	if !ok2 {
		t.Fatal("fused covered-leaf column still translates (by name), just not baked")
	}
	fv2, isFV2 := out2.(*values.FieldValue)
	if !isFV2 {
		t.Fatalf("fused translation must be a FieldValue, got %#v", out2)
	}
	if fv2.Resolved != nil {
		t.Fatalf("FUSED multi-accessor path must NOT be baked single-accessor (would read the wrong slot) — "+
			"got Resolved=%v (root ordinal would be ADDR's, dropping the .city descent)", fv2.Resolved)
	}
}
