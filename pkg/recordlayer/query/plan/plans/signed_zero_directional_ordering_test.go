package plans

// An index scan's rich ordering over a coordinate bound by a
// signed-zero-widened float equality.
//
// FIXED and SORTED are not two spellings of "ordered". FIXED means the
// coordinate states NO order — every admitted row carries one sort value — so
// it satisfies a request in EITHER direction, from a forward scan. SORTED means
// the coordinate is ordered in one definite direction and satisfies only that
// one.
//
// A signed-zero-widened equality is SORTED, not FIXED, and the distinction is
// not a nicety: -0.0 and +0.0 are admitted by ONE equality (the predicate
// comparator checks IEEE equality) while ranking as TWO sort values (the sort
// comparator is faithful to java.lang.Double.compare, which puts -0.0 first, and
// to FDB tuple order). Recording it FIXED lets `WHERE z = 0.0 ORDER BY z DESC`
// elide its sort and be answered from a FORWARD scan, which returns the two zero
// blocks ascending — a wrong answer, pinned end to end by
// TestFDB_SignedZeroEqualityDoesNotOrderThePKSuffix in pkg/relational/sqldriver.

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func signedZeroDirectionalRange(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	return res.Range
}

// signedZeroDirectionalOrdering builds the rich ordering of a forward index scan
// on a single DOUBLE key column bound by literal, over PK (ID).
func signedZeroDirectionalRow() *values.RecordType {
	return values.NewRecordType("signed_zero", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "Z", FieldType: values.NullableDouble, Ordinal: 1},
	})
}

func signedZeroDirectionalOrdering(t *testing.T, literal any) *properties.RichOrdering {
	t.Helper()
	row := signedZeroDirectionalRow()
	idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"IDX_Z",
			[]*predicates.ComparisonRange{signedZeroDirectionalRange(t, literal)},
			[]string{"T"}, row, false /* forward */)
	}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble})
	w := idx.WithIndexMetadata([]string{"Z"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	ord := w.HintRichOrdering()
	if ord == nil {
		t.Fatal("nil rich ordering")
	}
	return ord
}

func signedZeroDirectionalRequest(t testing.TB, dir properties.RequestedSortOrder) *properties.RequestedOrdering {
	t.Helper()
	return properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     testFieldIn(t, signedZeroDirectionalRow(), "requested", "Z"),
			SortOrder: dir,
		}},
		properties.DistinctnessPreserveDistinctness, false)
}

// TestIndexScanRichOrdering_SignedZeroEqualityIsDirectionalNotFixed: a FORWARD
// scan satisfies ORDER BY z ASC and must NOT satisfy ORDER BY z DESC.
func TestIndexScanRichOrdering_SignedZeroEqualityIsDirectionalNotFixed(t *testing.T) {
	t.Parallel()

	for _, literal := range []any{0.0, math.Copysign(0, -1)} {
		literal := literal
		t.Run(map[bool]string{true: "positive_zero", false: "negative_zero"}[!math.Signbit(literal.(float64))], func(t *testing.T) {
			t.Parallel()
			ord := signedZeroDirectionalOrdering(t, literal)

			if !ord.Satisfies(signedZeroDirectionalRequest(t, properties.RequestedSortOrderAscending)) {
				t.Fatal("a FORWARD scan opens the two signed-zero blocks in key order, so it " +
					"does satisfy ORDER BY z ASC; refusing it forfeits a sound claim")
			}
			if ord.Satisfies(signedZeroDirectionalRequest(t, properties.RequestedSortOrderDescending)) {
				t.Fatal("a FORWARD scan must NOT satisfy ORDER BY z DESC. -0.0 and +0.0 are " +
					"admitted by one equality but rank as TWO sort values " +
					"(java.lang.Double.compare puts -0.0 first), so the coordinate is SORTED " +
					"ascending, not FIXED. Treating it as FIXED elides the sort and answers " +
					"the query from a forward scan, returning the blocks ascending.")
			}
		})
	}
}

// TestIndexScanRichOrdering_OrdinaryFloatEqualityStaysFixed is the control. A
// non-zero float equality pins ONE physical key, so every admitted row really
// does carry one sort value and the coordinate is order-free — both directions
// are satisfied from the forward scan, and the refusal above must not reach it.
func TestIndexScanRichOrdering_OrdinaryFloatEqualityStaysFixed(t *testing.T) {
	t.Parallel()

	ord := signedZeroDirectionalOrdering(t, 1.5)
	for _, dir := range []properties.RequestedSortOrder{
		properties.RequestedSortOrderAscending,
		properties.RequestedSortOrderDescending,
	} {
		if !ord.Satisfies(signedZeroDirectionalRequest(t, dir)) {
			t.Fatalf("an equality pinning ONE physical key is FIXED and satisfies ORDER BY z %v", dir)
		}
	}
}
