package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPlanColumnNames_MapReportsPostRenameNames pins the RFC-078 fix: a
// RecordQueryMapPlan reports its OWN result-value column names, NOT the pre-rename
// names of its inner. ImplementUnorderedUnionRule wraps a mismatched-named UNION
// branch in a rename Map; if planColumnNamesWithMD descended through that Map and
// reported the inner's (pre-rename) names, executeUnorderedUnion's position-remap
// would remap a SECOND time over the already-renamed row, reading missing keys →
// NULLs. Reporting the Map's post-rename names makes srcKeys == firstBranchKeys, so
// the executor remap is correctly a no-op for an already-renamed branch.
func TestPlanColumnNames_MapReportsPostRenameNames(t *testing.T) {
	t.Parallel()
	rowType := exactTestRowType(values.Field{Name: "Y", FieldType: values.NullableLong})
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false))
	// A rename Map: output column X reads the inner row's key Y (as
	// ImplementUnorderedUnionRule's columnRenameValue builds).
	renameRV := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: mustTestFieldOrdinal(t, scan.GetResultValue(), 0)},
	)
	mapPlan := mustExecutorConstruct(plans.NewRecordQueryMapPlan(scan, renameRV))

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
	inputType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, inputType, false))
	agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
		scan,
		nil, // no grouping keys (scalar aggregate)
		[]expressions.AggregateSpec{{Function: expressions.AggCount, Alias: "X"}},
	))
	got := planColumnNames(agg)
	if len(got) != 1 || got[0] != "X" {
		t.Fatalf("StreamingAgg must report its output alias [X], not the scan's columns, got %v", got)
	}
}

// TestPlanColumnNames_AggregateIndexReportsOutputSchema pins the RFC-081 fix: a bare
// RecordQueryAggregateIndexPlan reports its OWN output schema (group columns + the
// canonical aggregate name) — NOT nil (which it returned before, falling through to its
// plan's declared result type). These are exactly the keys aggregateIndexCursor writes, so the
// UNION position-remap can normalize a grouped aggregate-index branch.
func TestPlanColumnNames_AggregateIndexReportsOutputSchema(t *testing.T) {
	t.Parallel()
	indexType := exactTestRowType(values.Field{Name: "G", FieldType: values.NotNullString})
	resultType := exactTestRowType(
		values.Field{Name: "G", FieldType: values.NotNullString},
		values.Field{Name: "COUNT(*)", FieldType: values.NotNullLong},
	)
	idx := mustExecutorConstruct(plans.NewRecordQueryIndexPlan("cnt_by_g", nil, []string{"GA"}, indexType, false))
	agg := mustExecutorConstruct(plans.NewRecordQueryAggregateIndexPlan(idx, "GA", resultType, "COUNT")).
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
// RFC-078 NULL-bug class. Production MI field names happen to be upper (so the bug can't surface
// via SQL today), but pinning the verbatim contract guards against that.
func TestPlanColumnNames_MultiIntersectionReportsResultValueNames(t *testing.T) {
	t.Parallel()
	resultType := exactTestRowType(
		values.Field{Name: "G", FieldType: values.TypeString},
		values.Field{Name: "COUNT(*)", FieldType: values.NullableLong},
		values.Field{Name: "MixedKey", FieldType: values.TypeString},
	)
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"GA"}, resultType, false))
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), resultType)
	rv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "G", Value: mustTestFieldOrdinal(t, root, 0)},
		values.RecordConstructorField{Name: "COUNT(*)", Value: mustTestFieldOrdinal(t, root, 1)},
		// Mixed-case: must be reported verbatim, NOT upper-cased by the GetResultType fallback.
		values.RecordConstructorField{Name: "MixedKey", Value: mustTestFieldOrdinal(t, root, 2)},
	)
	mi := mustExecutorConstruct(plans.NewRecordQueryMultiIntersectionOnValuesPlan(
		[]plans.RecordQueryPlan{scan, scan}, nil, rv,
	))
	got := planColumnNames(mi)
	if len(got) != 3 || got[0] != "G" || got[1] != "COUNT(*)" || got[2] != "MixedKey" {
		t.Fatalf("MultiIntersection must report result-value field names VERBATIM [G COUNT(*) MixedKey], got %v", got)
	}
}

// TestPlanColumnNames_StopsAtProjection is the executor-side twin of the
// cascades package's TestPhysicalPlanColumnNames_StopsAtProjection, and the two
// exist as a PAIR on purpose: the walkers' own comments say they mirror each
// other, and a mirror claim that nothing checks is how they drift.
//
// RFC-226 rev 5 claimed both walkers descend past a projection and both needed a
// don't-descend arm added. Neither does and neither needs one — the projection
// arm is the first thing in each loop. Pinned here so the claim is settled by a
// test rather than re-derived by the next reader.
func TestPlanColumnNames_StopsAtProjection(t *testing.T) {
	t.Parallel()

	innerRow := values.NewRecordType("", true, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "A", FieldType: values.NotNullLong},
	})
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, innerRow, false))
	proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{mustTestFieldOrdinal(t, scan.GetResultValue(), 1)},
		[]string{"RENAMED"}, scan))

	got := planColumnNamesWithMD(proj, nil)
	if len(got) != 1 || got[0] != "RENAMED" {
		t.Fatalf("planColumnNamesWithMD(Projection) = %v, want [RENAMED].\n"+
			"  [ID A] means the walker descended PAST the projection and the union "+
			"position-remap would key on columns the projection does not emit. nil means "+
			"the projection arm was removed.", got)
	}
}
