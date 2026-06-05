package executor

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestPlanColumnNames_MapReportsPostRenameNames pins the codex RFC-078 fix: a
// RecordQueryMapPlan reports its OWN result-value column names, NOT the pre-rename
// names of its inner. ImplementUnorderedUnionRule wraps a mismatched-named UNION
// branch in a rename Map; if planColumnNamesWithMD descended through that Map and
// reported the inner's (pre-rename) names, executeUnorderedUnion's position-remap
// would remap a SECOND time over the already-renamed row, reading missing keys →
// NULLs. Reporting the Map's post-rename names makes srcKeys == firstBranchKeys, so
// the executor remap is correctly a no-op for an already-renamed branch.
func TestPlanColumnNames_MapReportsPostRenameNames(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	// A rename Map: output column X reads the inner row's key Y (as
	// ImplementUnorderedUnionRule's columnRenameValue builds).
	renameRV := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: &values.FieldValue{Field: "Y"}},
	)
	mapPlan := plans.NewRecordQueryMapPlan(scan, renameRV)

	got := planColumnNames(mapPlan)
	if len(got) != 1 || got[0] != "X" {
		t.Fatalf("Map plan must report its result-value (post-rename) names [X], got %v", got)
	}
}

// TestPlanColumnNames_StreamingAggReportsOutputSchema pins the RFC-078 streaming-agg
// fix: planColumnNamesWithMD reports a StreamingAgg plan's output schema (alias),
// NOT the input scan's columns (which it would return by descending through the
// agg's GetInner). Without this the UNION position-remap mis-names the branch and
// drops a mismatched-alias aggregate branch's rows.
func TestPlanColumnNames_StreamingAggReportsOutputSchema(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	agg := plans.NewRecordQueryStreamingAggregationPlan(
		scan,
		nil, // no grouping keys (scalar aggregate)
		[]expressions.AggregateSpec{{Function: expressions.AggCount, Alias: "X"}},
	)
	got := planColumnNames(agg)
	if len(got) != 1 || got[0] != "X" {
		t.Fatalf("StreamingAgg must report its output alias [X], not the scan's columns, got %v", got)
	}
}

// TestPlanColumnNames_AggregateIndexReportsOutputSchema pins the RFC-081 fix: a bare
// RecordQueryAggregateIndexPlan reports its OWN output schema (group columns + the
// canonical aggregate name) — NOT nil (which it returned before, falling through to its
// UnknownType result type). These are exactly the keys aggregateIndexCursor writes, so the
// UNION position-remap can normalize a grouped aggregate-index branch.
func TestPlanColumnNames_AggregateIndexReportsOutputSchema(t *testing.T) {
	t.Parallel()
	idx := plans.NewRecordQueryIndexPlan("cnt_by_g", nil, []string{"GA"}, values.UnknownType, false)
	agg := plans.NewRecordQueryAggregateIndexPlan(idx, "GA", values.UnknownType, "COUNT").
		WithGroupColumns([]string{"G"}, "")
	got := planColumnNames(agg)
	if len(got) != 2 || got[0] != "G" || got[1] != "COUNT(*)" {
		t.Fatalf("AggregateIndex must report [G COUNT(*)] (group col + canonical), got %v", got)
	}
}

// TestPlanColumnNames_MultiIntersectionReportsResultValueNames pins the RFC-081 fix: a
// RecordQueryMultiIntersectionOnValuesPlan reports its result value's RecordConstructorValue
// field names VERBATIM — the exact keys the merge cursor writes (RecordConstructorValue.Evaluate
// keys by f.Name) — so a multi-aggregate grouped union branch is position-remappable.
//
// A MIXED-CASE field name pins the verbatim contract specifically (and makes the explicit MI
// arm necessary, not redundant with the GetResultType fallback): the fallback upper-cases, so it
// would report "MixedKey" as "MIXEDKEY" and mismatch the merge cursor's exact-case row key — the
// RFC-078 codex-P2 NULL-bug class. Production MI field names happen to be upper (so the codex-P2
// bug can't surface via SQL today), but pinning the verbatim contract guards against that.
func TestPlanColumnNames_MultiIntersectionReportsResultValueNames(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"GA"}, values.UnknownType, false)
	rv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "G", Value: &values.FieldValue{Field: "G"}},
		values.RecordConstructorField{Name: "COUNT(*)", Value: &values.FieldValue{Field: "COUNT(*)"}},
		// Mixed-case: must be reported verbatim, NOT upper-cased by the GetResultType fallback.
		values.RecordConstructorField{Name: "MixedKey", Value: &values.FieldValue{Field: "MixedKey"}},
	)
	mi := plans.NewRecordQueryMultiIntersectionOnValuesPlan(
		[]plans.RecordQueryPlan{scan, scan}, nil, rv,
	)
	got := planColumnNames(mi)
	if len(got) != 3 || got[0] != "G" || got[1] != "COUNT(*)" || got[2] != "MixedKey" {
		t.Fatalf("MultiIntersection must report result-value field names VERBATIM [G COUNT(*) MixedKey], got %v", got)
	}
}
