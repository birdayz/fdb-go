package protovalue

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// FromProto reads a stored Value as Java's LiteralKeyExpression.fromProtoValue
// does: the one value set, nil for none, and a refusal for more than one,
// where the planner's copy of this reader took the first field set.
func TestFromProtoRefusesTwoValues(t *testing.T) {
	t.Parallel()
	if v, err := FromProto(&gen.Value{IntValue: proto.Int32(3)}); err != nil || v != int32(3) {
		t.Errorf("one value: %v, %v", v, err)
	}
	if v, err := FromProto(&gen.Value{}); err != nil || v != nil {
		t.Errorf("no value: %v, %v", v, err)
	}
	if v, err := FromProto(nil); err != nil || v != nil {
		t.Errorf("nil: %v, %v", v, err)
	}
	_, err := FromProto(&gen.Value{LongValue: proto.Int64(1), StringValue: proto.String("a")})
	var multiple *MultipleValuesError
	if !errors.As(err, &multiple) || err.Error() != "More than one value encoded in value" {
		t.Errorf("two values: %v", err)
	}
}
