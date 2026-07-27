package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPointProbeHintCost_ChargesFetchRateNotScanRate pins the fix for a real
// production defect found while verifying TestFDB_MultiwayJoinOrder_Nway: a
// full-PK or full-unique-index equality point-probe is executed as ONE
// isolated GetRange/Get round trip with nothing to amortize over — the SAME
// physical shape as a Fetch (RecordQueryFetchFromPartialRecordPlan), verified
// against the executor (key_value_cursor.go's initIterator issues its own
// GetRange per invocation; flat_map_cursor.go's OnNext opens a fresh inner
// cursor per outer row with NO pipelining; split_helper.go's loadWithSplit
// does one blocking tx.Get().Get() in the common unsplit case — see
// properties.FetchCPU's doc comment for the full trace). Charging the
// AMORTIZED sequential-scan rate (ScanCPU) — or, worse, literally zero — for
// this isolated round trip was pricing the identical physical operation
// differently depending on which plan node performed it.
func TestPointProbeHintCost_ChargesFetchRateNotScanRate(t *testing.T) {
	t.Parallel()

	stats := properties.FixedStatistics{Cardinality: 1000}
	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))

	t.Run("RecordQueryScanPlan full-PK point-probe", func(t *testing.T) {
		t.Parallel()
		plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithPrimaryKey(pk).
			WithScanComparisons([]*predicates.ComparisonRange{eq})
		got := plan.HintCost(nil, stats)
		if got.Cardinality != 1 {
			t.Fatalf("cardinality = %v, want 1 (point probe)", got.Cardinality)
		}
		if got.CPU != properties.FetchCPU {
			t.Fatalf("CPU = %v, want properties.FetchCPU (%v) — an isolated point-probe round trip, not the amortized scan rate", got.CPU, properties.FetchCPU)
		}
	})

	t.Run("RecordQueryIndexPlan unique full-equality point-probe", func(t *testing.T) {
		t.Parallel()
		plan := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{eq}, []string{"T"}, values.UnknownType, false).
			WithIndexMetadata([]string{"ID"}, []string{"ID"}, true)
		got := plan.HintCost(nil, stats)
		if got.Cardinality != 1 {
			t.Fatalf("cardinality = %v, want 1 (point probe)", got.Cardinality)
		}
		if got.CPU != properties.FetchCPU {
			t.Fatalf("CPU = %v, want properties.FetchCPU (%v) — was literally 0 before this fix, an even starker version of the same defect", got.CPU, properties.FetchCPU)
		}
	})
}

// TestPointProbeHintCost_NonUniqueOrPartialBindUnaffected confirms the fix is
// narrowly scoped to the PROVEN point-probe case: a non-unique index equality
// (a genuine bucket, potentially many rows via one amortized streaming range
// read) and a partial PK prefix (not a point probe at all) must still be
// priced at the ScanCPU-based bucket formula, unchanged.
func TestPointProbeHintCost_NonUniqueOrPartialBindUnaffected(t *testing.T) {
	t.Parallel()
	stats := properties.FixedStatistics{Cardinality: 1000}
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))

	t.Run("non-unique index equality stays a bucket", func(t *testing.T) {
		t.Parallel()
		plan := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{eq}, []string{"T"}, values.UnknownType, false).
			WithIndexMetadata([]string{"ID"}, []string{"ID"}, false)
		got := plan.HintCost(nil, stats)
		if got.Cardinality <= 1 {
			t.Fatalf("cardinality = %v, want a bucket (>1) — a non-unique equality is not a point probe", got.Cardinality)
		}
		wantCPU := got.Cardinality * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier
		if got.CPU != wantCPU {
			t.Fatalf("CPU = %v, want %v (the amortized bucket-scan rate, untouched by the point-probe fix)", got.CPU, wantCPU)
		}
	})

	t.Run("partial composite PK prefix stays a bucket", func(t *testing.T) {
		t.Parallel()
		pk2 := []values.Value{
			&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
			&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
		}
		plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithPrimaryKey(pk2).
			WithScanComparisons([]*predicates.ComparisonRange{eq})
		got := plan.HintCost(nil, stats)
		if got.Cardinality <= 1 {
			t.Fatalf("cardinality = %v, want a bucket (>1) — only one of two PK columns is bound", got.Cardinality)
		}
		wantCPU := got.Cardinality * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier
		if got.CPU != wantCPU {
			t.Fatalf("CPU = %v, want %v (unchanged bucket rate)", got.CPU, wantCPU)
		}
	})
}

// TestInJoinHintCost_BothTermsAreFetchRate pins the InJoin fix: each IN value
// pays TWO isolated, unamortized round trips (the index point-probe itself,
// then the base-record fetch it resolves) — both at FetchCPU, not the
// previous ScanCPU+FetchCPU mix that priced the index point-probe at the
// amortized sequential rate.
//
// The inner MUST be a genuinely provable point probe (full-PK equality bind)
// for this branch to fire — see isProvablePointProbe and
// TestInJoinHintCost_ScalesOffChildCardinalityWhenNotAPointProbe for the case
// where it is not.
func TestInJoinHintCost_BothTermsAreFetchRate(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk).
		WithScanComparisons([]*predicates.ComparisonRange{eq})
	plan := NewRecordQueryInJoinPlan(inner, "x", false, false)
	plan.SetInValues([]any{int64(1), int64(2), int64(3)})
	// The child cost passed here is deliberately NOT what inner.HintCost would
	// actually produce (Cardinality 1) — a genuine point probe ignores the
	// child cost entirely, so an absurd child cardinality must not leak
	// through.
	got := plan.HintCost([]properties.Cost{{Cardinality: 999}}, nil)
	if got.Cardinality != 3 {
		t.Fatalf("cardinality = %v, want 3 (the IN-list length, child cardinality ignored for a proven point probe)", got.Cardinality)
	}
	want := 3.0 * (properties.FetchCPU + properties.FetchCPU) * properties.PhysicalWrapperCostMultiplier
	if got.CPU != want {
		t.Fatalf("CPU = %v, want %v (both round trips at FetchCPU)", got.CPU, want)
	}
}
