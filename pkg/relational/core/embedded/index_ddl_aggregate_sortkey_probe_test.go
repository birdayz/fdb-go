package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The RFC-202 S3 measurement this file started as (the aggregate plan-shape
// dump) is consumed by the generator's aggregate arm; what survives is the
// one fact that arm LEANS on and nothing else pins: over an aggregate plan,
// the sort keys carry the PROJECTION's value instances — pointer-identical to
// LogicalProject.ProjectedValues slots — so aggregateOrderRefs
// (pkg/relational/core/query/ddl/generator_aggregate.go) can recover the
// select ordinal by identity and translate it through OutputSlots to a
// grouping ordinal or the aggregate call.

func aggProbeTemplate(t *testing.T) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("agg_probe_tpl")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("COL1", api.NewLongType(true), 1),
		metadata.NewColumnSpec("COL2", api.NewLongType(true), 2),
		metadata.NewColumnSpec("COL3", api.NewLongType(true), 3),
		metadata.NewColumnSpec("COL4", api.NewLongType(true), 4),
	}, []string{"COL1"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	return tmpl
}

// TestIndexDDLProbe_AggregateSortKeysAreProjectionInstances pins the identity
// contract the aggregate index arm's ORDER BY mapping rests on. If the plan
// visitor stops canonicalising sort keys to the projection's value instances,
// aggregateOrderRefs maps every key to "not in the projection list" and every
// ordered aggregate index declaration (permuted min/max included) fails —
// fail-closed, but this test names the actual broken contract.
func TestIndexDDLProbe_AggregateSortKeysAreProjectionInstances(t *testing.T) {
	t.Parallel()
	tmpl := aggProbeTemplate(t)
	op, err := probePlan(t, tmpl,
		"SELECT col1, col2, col3, MAX(col4) FROM t1 GROUP BY col1, col2, col3 ORDER BY col1, col2, MAX(col4), col3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	proj, ok := op.(*logical.LogicalProject)
	if !ok {
		t.Fatalf("top operator is %T, want Project", op)
	}
	sort, ok := proj.Input.(*logical.LogicalSort)
	if !ok {
		t.Fatalf("below Project is %T, want Sort", proj.Input)
	}
	agg, ok := sort.Input.(*logical.LogicalAggregate)
	if !ok {
		t.Fatalf("below Sort is %T, want Aggregate", sort.Input)
	}
	if len(sort.Keys) != 4 || len(proj.ProjectedValues) != 4 {
		t.Fatalf("shape drifted: %d sort keys over %d projected values", len(sort.Keys), len(proj.ProjectedValues))
	}
	// ORDER BY col1, col2, MAX(col4), col3 against SELECT col1, col2, col3,
	// MAX(col4): sort key k must be the SAME instance as the projection slot
	// it names.
	wantSlot := []int{0, 1, 3, 2}
	for i, k := range sort.Keys {
		if k.Value == nil || k.Value != proj.ProjectedValues[wantSlot[i]] {
			t.Fatalf("sort key %d (%q) is not the projection slot %d instance — "+
				"aggregateOrderRefs' identity mapping (generator_aggregate.go) is broken "+
				"and ordered aggregate index DDL fails wholesale", i, k.Expr, wantSlot[i])
		}
	}
	// And the aggregate's OutputSlots address [group keys..., calls...] — the
	// other half of the translation.
	if len(agg.OutputSlots) != 4 || agg.OutputSlots[3].NativeOrdinal != len(agg.GroupKeys) {
		t.Fatalf("OutputSlots drifted: %v", agg.OutputSlots)
	}
}
