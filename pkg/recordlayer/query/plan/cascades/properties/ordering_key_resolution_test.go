package properties

// What orderingKeyFor may and may not equate.
//
// The ordering set is still addressed by a rendered string, so this function is
// where a requested ordering key meets a provided one. Every arm it has is a
// claim that two renderings denote the same column, and the arms differ in how
// much of the column's identity they are willing to drop to say yes.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// resolutionThreeColumnRow is a layout wide enough that a column's ordinal and
// a one-column layout's ordinal 0 cannot coincide.
func resolutionThreeColumnRow() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "COL", FieldType: values.NullableLong, Ordinal: 2},
	})
}

func resolutionOneColumnRow() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "COL", FieldType: values.NullableLong, Ordinal: 0},
	})
}

func orderingOverOneKey(key values.Value) *RichOrdering {
	return NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{key},
		DistinctOverAllKeys())
}

// TestOrderingKeyForRefusesTwoStatedOrdinals is the conflation net.
//
// Two values that have both STATED an ordinal are decided by their ordinals and
// their layouts, never by their spelling. The pair here shares only its display
// name: ordinal 2 of a three-column row against ordinal 0 of a one-column row.
// Resolving it would report a provided ordering on one column as satisfying a
// request for another, and the sort would be elided against an order the plan
// does not provide.
//
// This is what makes the remaining name comparison in orderingKeyFor safe rather
// than merely small: CanBridgeOrderingFieldValues refuses outright when both
// sides carry a resolved path, so the name is consulted only where one side has
// no ordinal to compare.
func TestOrderingKeyForRefusesTwoStatedOrdinals(t *testing.T) {
	t.Parallel()

	provided := propertyFieldIn(t, resolutionOneColumnRow(), "COL")
	requested := propertyFieldIn(t, resolutionThreeColumnRow(), "COL")

	if values.ColumnNameValue(provided) != values.ColumnNameValue(requested) {
		t.Fatalf("test setup: the two must share their ordinal-FREE rendering "+
			"(%q vs %q), or no name comparison would fire on them",
			values.ColumnNameValue(provided), values.ColumnNameValue(requested))
	}
	if values.ExplainValue(provided) == values.ExplainValue(requested) {
		t.Fatal("test setup: the two must differ in their full rendering, or " +
			"the exact arm resolves them and nothing is being tested")
	}

	if key, ok := orderingOverOneKey(provided).orderingKeyFor(requested); ok {
		t.Fatalf("orderingKeyFor resolved %q to the provided key %q.\n\n"+
			"These are DIFFERENT columns: ordinal 2 of a three-column row and "+
			"ordinal 0 of a one-column row. They agree on their display name and "+
			"nothing else, so the only comparison that says yes is one that "+
			"erases the ordinal on purpose. A provided ordering on one column "+
			"then satisfies a request for another and the enforcer sort is "+
			"elided against an order that does not exist.",
			values.ExplainValue(requested), key)
	}

	// The exact arm must still work, so this cannot pass by refusing everything.
	same := propertyFieldIn(t, resolutionOneColumnRow(), "COL")
	if _, ok := orderingOverOneKey(provided).orderingKeyFor(same); !ok {
		t.Fatal("orderingKeyFor no longer resolves a requested key that states " +
			"the SAME ordinal in the SAME layout as the provided key — that is " +
			"the common path and every elision depends on it")
	}
}
