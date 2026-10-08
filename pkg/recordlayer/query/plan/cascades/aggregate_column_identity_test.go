package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggColumnMatches_NestedShadowsTopLevel pins RFC-187 S4/S5/S8: the single
// chokepoint every aggregate-index match site now routes through
// (MatchesGroupBy, MatchesSingleAggregateOf, groupColComparisonIndex /
// groupingColumnIndex, the
// streaming-agg-from-index eligibility checks) compares a query grouping-key /
// aggregate-operand against a declared aggregate-index column by full accessor
// PATH — so a nested `addr.city` grouping key never matches a same-leaf-named
// top-level `city` aggregate index (which would aggregate the wrong column).
func TestAggColumnMatches_NestedShadowsTopLevel(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("T")
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "CITY", Ordinal: 0, FieldType: values.NullableString},
		{Name: "ADDR", Ordinal: 1, FieldType: &values.RecordType{Nullable: true, Fields: []values.Field{
			{Name: "CITY", Ordinal: 0, FieldType: values.NullableString},
		}}},
		{Name: "NAME", Ordinal: 2, FieldType: values.NullableString},
	}}
	root, err := values.NewQuantifiedObjectValue(src, rowType)
	if err != nil {
		t.Fatalf("construct exact row root: %v", err)
	}
	flatRequest, err := values.FieldByName("CITY")
	if err != nil {
		t.Fatalf("construct CITY request: %v", err)
	}
	flatCity, err := values.ResolveFieldAccess(root, []values.FieldRequest{flatRequest})
	if err != nil {
		t.Fatalf("resolve flat CITY: %v", err)
	}
	nestedAddrCity, err := values.ResolveFieldOrdinals(root, []int{1, 0})
	if err != nil {
		t.Fatalf("resolve ADDR.CITY: %v", err)
	}
	bakedCity, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve ordinal CITY: %v", err)
	}

	if aggColumnMatches(nestedAddrCity, []string{"CITY"}) {
		t.Fatal("nested addr.city matched a top-level CITY aggregate column (would aggregate wrong column)")
	}
	if !aggColumnMatches(flatCity, []string{"CITY"}) {
		t.Fatal("flat city failed to match the CITY aggregate column (regressed the positive match)")
	}
	if !aggColumnMatches(bakedCity, []string{"CITY"}) {
		t.Fatal("baked city failed to match the CITY aggregate column (baked/lazy bridge regressed)")
	}
	// Case-insensitive, and a different leaf must not match.
	if !aggColumnMatches(flatCity, []string{"city"}) {
		t.Fatal("aggregate column match is not case-insensitive")
	}
	name, err := values.ResolveFieldOrdinals(root, []int{2})
	if err != nil {
		t.Fatalf("resolve NAME: %v", err)
	}
	if aggColumnMatches(name, []string{"CITY"}) {
		t.Fatal("column NAME matched aggregate column CITY")
	}
	// A nested aggregate column is its full path: addr.city matches it, the
	// top-level city does not.
	if !aggColumnMatches(nestedAddrCity, []string{"ADDR", "CITY"}) {
		t.Fatal("nested addr.city failed to match the ADDR.CITY aggregate column")
	}
	if aggColumnMatches(flatCity, []string{"ADDR", "CITY"}) || aggColumnMatches(bakedCity, []string{"ADDR", "CITY"}) {
		t.Fatal("top-level city matched the ADDR.CITY aggregate column (would aggregate the wrong column)")
	}
}
