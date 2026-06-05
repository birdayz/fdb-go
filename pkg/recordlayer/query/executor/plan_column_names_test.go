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
