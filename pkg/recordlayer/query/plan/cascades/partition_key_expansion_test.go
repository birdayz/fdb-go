package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

func TestPartitionValuesMatchColumnIncludesSource(t *testing.T) {
	t.Parallel()
	row := indexExpansionRowType("T", "SOURCE", "OTHER")
	coordinate := func(alias string, ordinal int) values.Value {
		source := indexExpansionField(t, values.NamedCorrelationIdentifier(alias), row, ordinal)
		v, err := values.NewPartitionValue(source, 10)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	query, same, other := coordinate("query", 0), coordinate("candidate", 0), coordinate("candidate", 1)
	for _, pair := range [][2]values.Value{{query, same}, {same, query}} {
		if !valuesMatchColumn(pair[0], pair[1]) {
			t.Fatal("independent same-source partition columns did not match")
		}
	}
	for _, pair := range [][2]values.Value{{query, other}, {other, query}} {
		if valuesMatchColumn(pair[0], pair[1]) {
			t.Fatal("equal-width partition columns with different sources matched")
		}
	}
}

func TestPartitionKeyExpandsToTypedValue(t *testing.T) {
	t.Parallel()
	root := &gen.KeyExpression{Function: &gen.Function{
		Name: proto.String("fdb_partition_v1"),
		Arguments: &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
			keyExpressionField("SOURCE", gen.Field_SCALAR),
			{Value: &gen.Value{LongValue: proto.Int64(10)}},
		}}},
	}}
	distinct := false
	alias := values.UniqueCorrelationIdentifier()
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"local", []string{"T"}, []string{""}, []string{"fdb_partition_v1"},
		[]values.CorrelationIdentifier{alias}, indexExpansionRowType("T", "SOURCE", "ID"),
		false, []string{"ID"}, &distinct,
	).WithRootKeyExpression(root)
	top := fanoutExpansionTopSelect(t, candidate.GetTraversal())
	if len(top.GetPredicates()) != 1 {
		t.Fatalf("predicates = %v, want a partition placeholder", top.GetPredicates())
	}
	placeholder, ok := top.GetPredicates()[0].(*predicates.Placeholder)
	if !ok || placeholder.ParameterAlias != alias {
		t.Fatalf("placeholder = %#v", top.GetPredicates()[0])
	}
	coordinate, ok := placeholder.Value.(*values.PartitionValue)
	if !ok || coordinate.Width != 10 || !coordinate.Type().Equals(values.NotNullLong) {
		t.Fatalf("partition key expanded to %#v", placeholder.Value)
	}
	if len(values.GetCorrelatedToOfValue(coordinate)) != 1 {
		t.Fatal("partition coordinate lost its base correlation")
	}
	if candidate.CreatesDuplicates() {
		t.Fatal("scalar partition key became a fan-out candidate")
	}
	for _, args := range [][]values.Value{
		{&values.ConstantValue{Value: int32(1), Typ: values.NotNullInt}, &values.ConstantValue{Value: int64(10), Typ: values.NotNullLong}},
		{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, &values.ConstantValue{Value: int32(10), Typ: values.NotNullInt}},
		{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong}},
		{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	} {
		if _, err := functionKeyToValue("fdb_partition_v1", args); err == nil {
			t.Fatalf("invalid typed partition arguments accepted: %v", args)
		}
	}
}
