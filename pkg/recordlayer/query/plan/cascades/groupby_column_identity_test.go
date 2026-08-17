package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestOrderingSatisfiesGroupingKeys_NestedShadow pins RFC-187 S10 (and by the
// shared primitive, the S7/S9 group-key membership sites): a grouping key on a
// nested field addr.city must NOT be satisfied by an ordering on a
// same-leaf-named top-level column city, which would wrongly enable a
// streaming aggregation over the wrong ordering.
func TestOrderingSatisfiesGroupingKeys_NestedShadow(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("T")
	addressType := values.NewRecordType("Address", false, []values.Field{
		{Name: "CITY", FieldType: values.NotNullString, Ordinal: 0},
	})
	rowType := values.NewRecordType("GroupingRow", false, []values.Field{
		{Name: "CITY", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "ADDR", FieldType: addressType, Ordinal: 1},
	})
	root, err := values.NewQuantifiedObjectValue(src, rowType)
	root = mustConstruct(t, root, err)
	resolve := func(ordinals ...int) values.Value {
		t.Helper()
		value, resolveErr := values.ResolveFieldOrdinals(root, ordinals)
		return mustConstruct(t, value, resolveErr)
	}
	flat := func() values.Value {
		return resolve(0)
	}
	nested := func() values.Value {
		return resolve(1, 0)
	}
	ordOn := func(k values.Value) properties.Ordering {
		return properties.Ordering{IsKnown: true, Keys: []values.Value{k}, Descending: []bool{false}}
	}

	if orderingSatisfiesGroupingKeys(ordOn(flat()), []values.Value{nested()}) {
		t.Fatal("ordering on top-level city satisfied a nested addr.city grouping key (wrong)")
	}
	if !orderingSatisfiesGroupingKeys(ordOn(flat()), []values.Value{flat()}) {
		t.Fatal("ordering on city failed to satisfy a city grouping key (regressed the positive case)")
	}
}
