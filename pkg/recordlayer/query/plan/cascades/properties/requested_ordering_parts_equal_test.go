package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPartsEqualMatchesEquivalentExactParts(t *testing.T) {
	t.Parallel()

	partA := RequestedOrderingPart{
		Value:     propertyField(t, "REGION", values.NullableLong),
		SortOrder: RequestedSortOrderAscending,
	}
	partB := RequestedOrderingPart{
		Value:     propertyField(t, "REGION", values.NullableLong),
		SortOrder: RequestedSortOrderAscending,
	}
	if !PartsEqual([]RequestedOrderingPart{partA}, []RequestedOrderingPart{partB}) {
		t.Fatal("PartsEqual rejected equivalent exact field identities")
	}

	other := RequestedOrderingPart{
		Value:     propertyField(t, "AMOUNT", values.NullableLong),
		SortOrder: RequestedSortOrderAscending,
	}
	if PartsEqual([]RequestedOrderingPart{partA}, []RequestedOrderingPart{other}) {
		t.Fatal("PartsEqual conflated different exact field identities")
	}
}

func TestPartsEqualSeparatesExactFieldDomains(t *testing.T) {
	t.Parallel()

	recordRow := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "STATUS", FieldType: values.NullableLong},
	})
	aggregateRow := values.NewRecordType("", false, []values.Field{
		{Name: "CUSTOMER_ID", FieldType: values.NullableLong},
		{Name: "COUNT", FieldType: values.NullableLong},
	})
	inRecordRow := []RequestedOrderingPart{{
		Value:     propertyFieldIn(t, recordRow, "ID"),
		SortOrder: RequestedSortOrderAscending,
	}}
	inAggregateRow := []RequestedOrderingPart{{
		Value:     propertyFieldIn(t, aggregateRow, "CUSTOMER_ID"),
		SortOrder: RequestedSortOrderAscending,
	}}

	if PartsEqual(inRecordRow, inAggregateRow) {
		t.Fatal("PartsEqual conflated ordinal zero fields from different exact row domains")
	}
}
