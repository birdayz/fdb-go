package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestPostAggregateBindingUsesRealOutputQuantifier(t *testing.T) {
	t.Parallel()

	inputType := &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	inputQOV := exactTestQOV(t, "INPUT", inputType)
	key := exactTestField(t, inputQOV, 0)
	operand := exactTestField(t, inputQOV, 1)
	aggregate := &logical.LogicalAggregate{
		GroupKeys:         []logical.GroupKey{{Display: "K", Value: key}},
		Calls:             []logical.AggregateCall{{Func: "SUM", Operand: "V"}},
		AggregateOperands: []values.Value{operand},
	}

	outputType := &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "SUM(V)", Ordinal: 1, FieldType: values.NullableLong},
	}}
	outputQOV := exactTestQOV(t, "GROUP_OUT", outputType)
	bound, err := bindPostAggregateValue(key, aggregate, outputQOV)
	if err != nil {
		t.Fatalf("bind group key: %v", err)
	}
	field := exactTestFieldView(t, bound)
	owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || owner.Correlation() != outputQOV.Correlation() {
		t.Fatalf("bound owner = %v, want real aggregate output correlation %v", owner, outputQOV.Correlation())
	}
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("bound path = %v, want [0]", got)
	}

	// Mutation-sensitive proof: the same draft cannot be bound to a row whose
	// native slot changes type. This is where an Unknown/current carrier used to
	// hide the disagreement.
	badQOV := exactTestQOV(t, "BAD_GROUP_OUT", &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullString},
		{Name: "SUM(V)", Ordinal: 1, FieldType: values.NullableLong},
	}})
	if _, err := bindPostAggregateValue(key, aggregate, badQOV); err == nil {
		t.Fatal("binding a LONG group key to a STRING native slot succeeded")
	}
}
