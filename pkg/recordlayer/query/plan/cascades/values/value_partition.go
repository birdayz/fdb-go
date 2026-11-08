package values

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/internal/partition"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const partitionValueTypeURL = "type.fdb.dev/recordlayer/PartitionValueV1"

// PartitionValueError reports an invalid typed interval coordinate.
type PartitionValueError struct{ Message string }

func (e *PartitionValueError) Error() string { return "partition value: " + e.Message }

// PartitionValue is the internal BIGINT floor coordinate, not SQL division.
// Width is part of its identity and is never a query parameter.
type PartitionValue struct {
	Source Value
	Width  int64
}

func NewPartitionValue(source Value, width int64) (*PartitionValue, error) {
	if source == nil || source.Type().Code() != TypeCodeLong {
		return nil, &PartitionValueError{Message: "source must have BIGINT type"}
	}
	if _, err := partition.Coordinate(0, width); err != nil {
		return nil, err
	}
	return &PartitionValue{Source: source, Width: width}, nil
}

func (v *PartitionValue) Children() []Value { return []Value{v.Source} }
func (*PartitionValue) Name() string        { return "fdb_partition_v1" }
func (*PartitionValue) Type() Type          { return NotNullLong }
func (v *PartitionValue) Evaluate(ctx any) (any, error) {
	if v.Source == nil {
		return nil, &PartitionValueError{Message: "missing source"}
	}
	x, err := v.Source.Evaluate(ctx)
	if err != nil {
		return nil, err
	}
	return partition.Route(x, v.Width)
}

func (v *PartitionValue) rebuild(children []Value) (*PartitionValue, error) {
	if len(children) != 1 {
		return nil, &PartitionValueError{Message: "rebuild requires exactly one source"}
	}
	return NewPartitionValue(children[0], v.Width)
}

func (c *SerializationContext) partitionValueToProto(v *PartitionValue) (*gen.PValue, error) {
	if _, err := NewPartitionValue(v.Source, v.Width); err != nil {
		return nil, err
	}
	source, err := c.ValueToProto(v.Source)
	if err != nil {
		return nil, err
	}
	width, err := c.ValueToProto(&ConstantValue{Value: v.Width, Typ: NotNullLong})
	if err != nil {
		return nil, err
	}
	// The Go-only Any uses the existing neutral children envelope; it is not
	// serialized as Java arithmetic, whose division has different semantics.
	data, err := proto.Marshal(&gen.PDerivedValue{Children: []*gen.PValue{source, width}})
	if err != nil {
		return nil, err
	}
	return &gen.PValue{SpecificValue: &gen.PValue_AdditionalValues{AdditionalValues: &anypb.Any{TypeUrl: partitionValueTypeURL, Value: data}}}, nil
}

func (c *SerializationContext) partitionValueFromProto(data []byte) (Value, error) {
	payload := &gen.PDerivedValue{}
	if err := proto.Unmarshal(data, payload); err != nil {
		return nil, &PartitionValueError{Message: fmt.Sprintf("invalid serialized coordinate: %v", err)}
	}
	if len(payload.Children) != 2 {
		return nil, &PartitionValueError{Message: "serialized coordinate requires source and interval"}
	}
	source, err := c.ValueFromProto(payload.Children[0])
	if err != nil {
		return nil, err
	}
	width, err := c.ValueFromProto(payload.Children[1])
	if err != nil {
		return nil, err
	}
	literal, ok := width.(*ConstantValue)
	if !ok || literal.Type().Code() != TypeCodeLong {
		return nil, &PartitionValueError{Message: "serialized interval must be a BIGINT literal"}
	}
	w, ok := literal.Value.(int64)
	if !ok {
		return nil, &PartitionValueError{Message: "serialized interval must be a BIGINT literal"}
	}
	return NewPartitionValue(source, w)
}
