package query

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A projection that repeats an output name has TWO rows: the SQL names a
// derived source publishes for resolution, where the repeated name stays
// repeated so a reference that spells it is ambiguous, and the row the plan
// flows, where the record constructor names the repeat by the
// name-addressability suffix. One per-slot naming rule feeds both derivations
// and only the exact type deduplicates. When the dedup reached the labels,
// `C."X"` over `WITH C AS (SELECT MIN(…) AS "X", MAX(…) AS "X" …)` resolved to
// one column instead of reporting 42702.
func TestProjectionLabelsStayRepeatedWhereTheExactTypeDeduplicates(t *testing.T) {
	t.Parallel()
	// Two exact typed values: a quantified object over a primitive flowed type
	// IS an exact LONG, where a bare literal only carries a placeholder type.
	first, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	second, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	proj := &logical.LogicalProject{
		Input:           &logical.LogicalScan{Table: "T"},
		Projections:     []string{"1", "2"},
		Aliases:         []string{"X", "X"},
		ProjectedValues: []values.Value{first, second},
	}
	labels, err := ExactLogicalOutputLabels(proj, nil, nil)
	if err != nil {
		t.Fatalf("ExactLogicalOutputLabels: %v", err)
	}
	if want := []string{"X", "X"}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %v, want %v — the SQL names must stay repeated for resolution", labels, want)
	}
	typ, err := ExactLogicalResultType(proj, nil)
	if err != nil {
		t.Fatalf("ExactLogicalResultType: %v", err)
	}
	record, ok := typ.(*values.RecordType)
	if !ok {
		t.Fatalf("exact type = %T, want a record", typ)
	}
	names := make([]string, len(record.Fields))
	for i, f := range record.Fields {
		names[i] = f.Name
	}
	if want := []string{"X", "X_2"}; !slices.Equal(names, want) {
		t.Fatalf("exact row = %v, want %v — the row the plan flows names the repeat by the suffix", names, want)
	}
}
