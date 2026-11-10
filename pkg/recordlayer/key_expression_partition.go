package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/partition"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FunctionNamePartitionV1 is the required Go-only interval coordinate function.
const FunctionNamePartitionV1 = "fdb_partition_v1"

// PartitionRoutingError reports NULL, non-BIGINT or invalid-width routing.
type PartitionRoutingError = partition.RoutingError

func init() {
	// This is an extension, not a member of Java's core function registry.
	RegisterFunctionSpec(FunctionNamePartitionV1, FunctionSpec{
		Evaluator: evaluatePartition, MinArguments: 2, MaxArguments: 2, ColumnSize: 1,
	})
}

// PartitionFunctionKeyExpression is a single-valued interval coordinate. Its
// ordinary Function proto is required so readers without the function fail closed.
type PartitionFunctionKeyExpression struct {
	FunctionKeyExpression
}

// PartitionExpr constructs fdb_partition_v1 over an ordinary BIGINT field and a
// positive BIGINT interval literal. Metadata validation checks the field's type.
func PartitionExpr(source KeyExpression, width int64) *PartitionFunctionKeyExpression {
	p := &PartitionFunctionKeyExpression{FunctionKeyExpression: FunctionKeyExpression{
		name: FunctionNamePartitionV1, arguments: Concat(source, Literal(width)),
	}}
	if _, err := partitionArguments(p.arguments); err != nil {
		p.fault, p.faultSeq = err, nextBuildFaultSeq()
	}
	return p
}

// Specification returns the validated immutable arguments, not query bindings.
func (p *PartitionFunctionKeyExpression) Specification() (*PartitionSpec, error) {
	return partitionArguments(p.arguments)
}

// Evaluate refuses invalid routing before any record or index mutation is staged.
func (p *PartitionFunctionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if _, err := partitionArguments(p.arguments); err != nil {
		return nil, err
	}
	return p.FunctionKeyExpression.Evaluate(record, msg)
}

func evaluatePartition(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
	if len(arguments) != 1 || len(arguments[0]) != 2 {
		return nil, &KeyExpressionError{Message: "partition function requires exactly one pair of arguments"}
	}
	width, ok := arguments[0][1].(int64)
	if !ok {
		return nil, &PartitionRoutingError{Value: arguments[0][1], Reason: "interval width must be a BIGINT"}
	}
	p, err := partition.Route(arguments[0][0], width)
	if err != nil {
		return nil, err
	}
	return [][]any{{p}}, nil
}

func partitionArguments(arguments KeyExpression) (*PartitionSpec, error) {
	args, ok := arguments.(*CompositeKeyExpression)
	if !ok || len(args.expressions) != 2 {
		return nil, &KeyExpressionError{Message: "partition function requires a source field and interval literal"}
	}
	field, ok := args.expressions[0].(*FieldKeyExpression)
	if !ok || field.fanType != FanTypeNone || field.fieldName == "" || field.nullStandin == NullStandinNotNull {
		return nil, &KeyExpressionError{Message: "partition source must be an ordinary scalar field"}
	}
	literal, ok := args.expressions[1].(*LiteralKeyExpression)
	if !ok {
		return nil, &KeyExpressionError{Message: "partition interval must be a positive BIGINT literal"}
	}
	width, ok := literal.value.(int64)
	if !ok || width <= 0 {
		return nil, &KeyExpressionError{Message: "partition interval must be a positive BIGINT literal"}
	}
	return &PartitionSpec{sourceColumn: field.fieldName, intervalWidth: width}, nil
}

func validatePartitionFunction(arguments KeyExpression, desc protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	spec, err := partitionArguments(arguments)
	if err != nil {
		return nil, err
	}
	fields, err := validateKeyExpressionFields(arguments, desc)
	if err != nil {
		return nil, err
	}
	fd := desc.Fields().ByName(protoreflect.Name(spec.sourceColumn))
	if fd == nil || fd.Kind() != protoreflect.Int64Kind || fd.IsList() {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("partition source %q must be a scalar BIGINT", spec.sourceColumn)}
	}
	return fields, nil
}
