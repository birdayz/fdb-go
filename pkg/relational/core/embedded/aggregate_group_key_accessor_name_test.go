package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func aggkOneSource() *logical.LogicalAggregate {
	return &logical.LogicalAggregate{Input: logical.NewScan("orders", "")}
}

func aggkExactField(t testing.TB, correlation string, typ *values.RecordType, ordinals ...int) values.Value {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(qov, ordinals)
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%v): %v", ordinals, err)
	}
	return field
}

func TestAggregateGroupKeyMatcherAcceptsOneExactFieldIdentity(t *testing.T) {
	t.Parallel()

	typ := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: values.NullableString},
	}}
	left := aggkExactField(t, "ORDERS", typ, 0)
	right := aggkExactField(t, "ORDERS", typ, 0)

	if !values.SemanticEqualsUnderAliasMap(left, right, values.EmptyAliasMap()) {
		t.Fatal("two independently resolved exact reads of the same slot are not semantically equal")
	}
	if !fieldValueMatchesAggregateGroupKey(left, right, aggkOneSource()) {
		t.Fatal("aggregate group-key matcher rejected the same exact QOV and ordinal path")
	}
}

func TestAggregateGroupKeyMatcherRequiresTheExactQOVRoot(t *testing.T) {
	t.Parallel()

	typ := &values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NotNullLong}}}
	left := aggkExactField(t, "ORDERS", typ, 0)
	differentCorrelation := aggkExactField(t, "OTHER", typ, 0)
	if fieldValueMatchesAggregateGroupKey(left, differentCorrelation, aggkOneSource()) {
		t.Fatal("equal ordinals under distinct quantifier correlations matched")
	}

	nullableType := &values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NullableLong}}}
	differentType := aggkExactField(t, "ORDERS", nullableType, 0)
	if fieldValueMatchesAggregateGroupKey(left, differentType, aggkOneSource()) {
		t.Fatal("equal ordinals under QOVs with different exact flowed types matched")
	}
}

func TestAggregateGroupKeyMatcherComparesEveryAccessor(t *testing.T) {
	t.Parallel()

	address := &values.RecordType{Fields: []values.Field{
		{Name: "CITY", Ordinal: 0, FieldType: values.NotNullString},
		{Name: "ZIP", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	typ := &values.RecordType{Fields: []values.Field{
		{Name: "ADDR", Ordinal: 0, FieldType: address},
		{Name: "SHIPADDR", Ordinal: 1, FieldType: address},
	}}

	addrCity := aggkExactField(t, "ORDERS", typ, 0, 0)
	addrCityAgain := aggkExactField(t, "ORDERS", typ, 0, 0)
	shipCity := aggkExactField(t, "ORDERS", typ, 1, 0)
	addrZIP := aggkExactField(t, "ORDERS", typ, 0, 1)

	if !fieldValueMatchesAggregateGroupKey(addrCity, addrCityAgain, aggkOneSource()) {
		t.Fatal("identical two-accessor paths did not match")
	}
	if fieldValueMatchesAggregateGroupKey(addrCity, shipCity, aggkOneSource()) {
		t.Fatal("paths differing at the root accessor matched")
	}
	if fieldValueMatchesAggregateGroupKey(addrCity, addrZIP, aggkOneSource()) {
		t.Fatal("paths differing at the leaf accessor matched")
	}
}
