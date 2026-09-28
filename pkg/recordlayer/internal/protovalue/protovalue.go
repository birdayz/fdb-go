// Package protovalue reads a key expression's Value proto as Java's
// LiteralKeyExpression.fromProtoValue does (LiteralKeyExpression.java:134-173),
// the one reader of both the meta-data loader (a literal key, a record type's
// explicit key, an index predicate's operand) and the planner's translation of
// an index predicate, which cannot import the record layer.
package protovalue

import "fdb.dev/gen"

// MultipleValuesError is Java's refusal of a Value with more than one field
// set. The record layer reports it as the RecordCoreError Java throws.
type MultipleValuesError struct{}

func (*MultipleValuesError) Error() string { return "More than one value encoded in value" }

// FromProto is the one field p sets, nil when it sets none (or p is nil), and
// a MultipleValuesError when it sets several.
func FromProto(p *gen.Value) (any, error) {
	if p == nil {
		return nil, nil
	}
	var value any
	found := 0
	set := func(present bool, v any) {
		if present {
			found++
			value = v
		}
	}
	set(p.DoubleValue != nil, p.GetDoubleValue())
	set(p.FloatValue != nil, p.GetFloatValue())
	set(p.LongValue != nil, p.GetLongValue())
	set(p.BoolValue != nil, p.GetBoolValue())
	set(p.StringValue != nil, p.GetStringValue())
	set(p.BytesValue != nil, p.BytesValue)
	set(p.IntValue != nil, p.GetIntValue())
	if found > 1 {
		return nil, &MultipleValuesError{}
	}
	return value, nil
}
