package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// pkOrderingEq builds an equality ComparisonRange for use as a scan
// comparison, mirroring the fixture pattern used across the cascades
// package's own PK-gate tests.
func pkOrderingEq(t *testing.T, v any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, v)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build equality range for %v", v)
	}
	return res.Range
}

// pkOrderingGT builds a non-equality (>) ComparisonRange.
func pkOrderingGT(t *testing.T, v any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, v)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build > range for %v", v)
	}
	return res.Range
}

// TestPKScanOrdering_NilPlan pins the nil-plan guard.
func TestPKScanOrdering_NilPlan(t *testing.T) {
	t.Parallel()
	got := PKScanOrdering(nil)
	if got.IsKnown {
		t.Fatalf("PKScanOrdering(nil) = %#v, want unknown ordering", got)
	}
}

// TestPKScanOrdering_NoPrimaryKey pins the empty-PK guard.
func TestPKScanOrdering_NoPrimaryKey(t *testing.T) {
	t.Parallel()
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	got := PKScanOrdering(plan)
	if got.IsKnown {
		t.Fatalf("PKScanOrdering(no PK) = %#v, want unknown ordering", got)
	}
}

// TestPKScanOrdering_Unbound pins the pre-existing behavior for a scan with
// no comparisons at all: every PK column is a sorted key, in scan direction.
func TestPKScanOrdering_Unbound(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k})

	got := PKScanOrdering(plan)
	if !got.IsKnown || len(got.Keys) != 2 || got.Keys[0] != id || got.Keys[1] != k {
		t.Fatalf("PKScanOrdering(unbound) = %#v, want [ID, K] ascending", got)
	}
	if got.DescendingAt(0) || got.DescendingAt(1) {
		t.Fatalf("PKScanOrdering(unbound) = %#v, want ascending", got)
	}
}

// TestPKScanOrdering_ReverseUnbound pins reverse-direction propagation for an
// unbound scan.
func TestPKScanOrdering_ReverseUnbound(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, true).
		WithPrimaryKey([]values.Value{id, k})

	got := PKScanOrdering(plan)
	if !got.IsKnown || len(got.Keys) != 2 {
		t.Fatalf("PKScanOrdering(reverse unbound) = %#v, want 2 descending keys", got)
	}
	if !got.DescendingAt(0) || !got.DescendingAt(1) {
		t.Fatalf("PKScanOrdering(reverse unbound) = %#v, want descending", got)
	}
}

// TestPKScanOrdering_EqualityPrefixDropped is the core regression pin: a
// leading equality comparison on the first PK column must NOT appear in the
// returned Keys — mirroring RecordQueryIndexPlan.HintOrdering's firstNonEq
// logic and Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons,
// whose equality-bound prefix only populates the fixed-binding map, never the
// ordering sequence.
//
// Before the fix, this scan (id = ?) reported the SAME Ordering{Keys:[ID,K]}
// as a fully unbound scan, so plan partitioning could not distinguish a
// Fixed-bound scan from a Sorted-unbound one — see
// TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan in the
// cascades package for the end-to-end proof this fix closes.
func TestPKScanOrdering_EqualityPrefixDropped(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k}).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingEq(t, int64(7))})

	got := PKScanOrdering(plan)
	if !got.IsKnown || len(got.Keys) != 1 || got.Keys[0] != k {
		t.Fatalf("PKScanOrdering(id=7) = %#v, want only [K] (ID dropped as equality-bound)", got)
	}
	if got.DescendingAt(0) {
		t.Fatalf("PKScanOrdering(id=7) = %#v, want ascending K", got)
	}
}

// TestPKScanOrdering_FullyEqualityBound pins the degenerate case: every PK
// column equality-bound leaves nothing to sort by, so the ordering is
// unknown/empty — not a zero-length-but-known Ordering.
func TestPKScanOrdering_FullyEqualityBound(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k}).
		WithScanComparisons([]*predicates.ComparisonRange{
			pkOrderingEq(t, int64(7)),
			pkOrderingEq(t, int64(9)),
		})

	got := PKScanOrdering(plan)
	if got.IsKnown {
		t.Fatalf("PKScanOrdering(id=7,k=9) = %#v, want unknown ordering", got)
	}
}

// TestPKScanOrdering_NonEqualityLeadingComparisonKeepsFullKey pins that a
// non-equality leading comparison does NOT trim anything — only a genuine
// equality prefix consumes a sort position, exactly like
// RecordQueryIndexPlan.HintOrdering's firstNonEq scan, which stops at the
// first non-equality comparison.
func TestPKScanOrdering_NonEqualityLeadingComparisonKeepsFullKey(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k}).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingGT(t, int64(7))})

	got := PKScanOrdering(plan)
	if !got.IsKnown || len(got.Keys) != 2 || got.Keys[0] != id || got.Keys[1] != k {
		t.Fatalf("PKScanOrdering(id>7) = %#v, want full [ID, K] (no equality prefix to drop)", got)
	}
}

// TestPKScanOrdering_EqualityPrefixThenRangeStopsAtFirstNonEquality pins the
// three-column boundary case: id=? (equality), k>? (range) — only id is
// dropped; k and the trailing column stay.
func TestPKScanOrdering_EqualityPrefixThenRangeStopsAtFirstNonEquality(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	m := &values.FieldValue{Field: "M", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k, m}).
		WithScanComparisons([]*predicates.ComparisonRange{
			pkOrderingEq(t, int64(7)),
			pkOrderingGT(t, int64(3)),
		})

	got := PKScanOrdering(plan)
	if !got.IsKnown || len(got.Keys) != 2 || got.Keys[0] != k || got.Keys[1] != m {
		t.Fatalf("PKScanOrdering(id=7,k>3) = %#v, want [K, M] (only ID dropped)", got)
	}
}

// TestRecordQueryScanPlan_HintOrdering_DelegatesToPKScanOrdering pins that
// the plan's own HintOrdering (what the cost model / partitioning actually
// calls) is exactly PKScanOrdering, not a stale duplicate.
func TestRecordQueryScanPlan_HintOrdering_DelegatesToPKScanOrdering(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id}).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingEq(t, int64(1))})

	got := plan.HintOrdering()
	want := PKScanOrdering(plan)
	if got.IsKnown != want.IsKnown || len(got.Keys) != len(want.Keys) {
		t.Fatalf("HintOrdering() = %#v, want PKScanOrdering() = %#v", got, want)
	}
}

// TestPKScanOrdering_FloatPKColumnTerminatesTheClaim covers PKScanOrdering's
// ordering-claim truncation DIRECTLY.
//
// It is tested here, at this level, because SQL does not reach it. MEASURED:
// deleting the truncation from PKScanOrdering leaves every plan-shape
// assertion in embedded_test.TestFloatPrimaryKeyScanClaimsNoOrder green — the
// sort those queries keep is kept by OrderedPrimaryScanRule's decline, which
// is the guard that actually decides SQL sort elision on the primary-key path.
//
// That does NOT make this one dead. PKScanOrdering is also what plan
// PARTITIONING compares (expression_partition.go's orderingsEqual) and what the
// cost model reads, so an unsound claim here co-partitions plans that deliver
// different orders — a defect with no SQL-visible symptom today and every
// reason to acquire one. An untested guard is a guard that gets deleted green,
// so it gets its own test rather than borrowing another's coverage.
func TestPKScanOrdering_FloatPKColumnTerminatesTheClaim(t *testing.T) {
	t.Parallel()
	// The LAYOUT is the authority for a flat key's type — the keys themselves
	// are minted with UnknownType on this path — so the float has to be
	// declared there or the guard is asked a question it cannot answer.
	layout := values.NewRecordType("P", false, []values.Field{
		{Name: "G", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "E", FieldType: values.NullableDouble, Ordinal: 1},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 2},
	})
	g := &values.FieldValue{Field: "G", Typ: values.UnknownType}
	e := &values.FieldValue{Field: "E", Typ: values.UnknownType}
	v := &values.FieldValue{Field: "V", Typ: values.UnknownType}

	t.Run("float in the middle truncates everything after it", func(t *testing.T) {
		t.Parallel()
		plan := NewRecordQueryScanPlan([]string{"P"}, layout, false).
			WithPrimaryKey([]values.Value{g, e, v})
		got := PKScanOrdering(plan)
		if !got.IsKnown || len(got.Keys) != 1 {
			t.Fatalf("PKScanOrdering(PK = G, E DOUBLE, V) = %#v, want exactly [G]. A DOUBLE "+
				"primary-key column is visited in FDB tuple order, where a negative NaN "+
				"comes FIRST and the comparator ranks it GREATEST; and because every NaN is "+
				"one logical tie class split across two disjoint physical blocks, V is not "+
				"ordered within that tie either", got)
		}
	})

	t.Run("float first claims nothing at all", func(t *testing.T) {
		t.Parallel()
		plan := NewRecordQueryScanPlan([]string{"P"}, layout, false).
			WithPrimaryKey([]values.Value{e, g})
		got := PKScanOrdering(plan)
		if got.IsKnown {
			t.Fatalf("PKScanOrdering(PK = E DOUBLE, G) = %#v, want an UNKNOWN ordering: the "+
				"leading coordinate is the one that is misordered", got)
		}
	})

	t.Run("an all-integer primary key keeps its full claim", func(t *testing.T) {
		t.Parallel()
		// The control. Without it, a truncation that returned nothing for every
		// primary key would satisfy both cases above.
		plan := NewRecordQueryScanPlan([]string{"P"}, layout, false).
			WithPrimaryKey([]values.Value{g, v})
		got := PKScanOrdering(plan)
		if !got.IsKnown || len(got.Keys) != 2 {
			t.Fatalf("PKScanOrdering(PK = G, V, both BIGINT) = %#v, want both keys claimed; "+
				"the float terminates the claim, it does not delete it", got)
		}
	})
}
