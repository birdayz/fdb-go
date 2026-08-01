package embedded

import (
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// physicalKeyComponentTypes derives the tuple representation of each key
// component from the key-expression topology and every record descriptor the
// key can evaluate against. A disagreement across record types is UnknownType;
// choosing the first descriptor would make a shared index encode query bounds
// with one type's width while scanning another type's entries.
func physicalKeyComponentTypes(
	expression recordlayer.KeyExpression,
	recordTypes []*recordlayer.RecordType,
) []values.Type {
	if expression == nil {
		return nil
	}
	size := expression.ColumnSize()
	if size <= 0 {
		return nil
	}
	if len(recordTypes) == 0 {
		return unknownPhysicalTypes(size)
	}
	root := expression.ToKeyExpression()
	var merged []values.Type
	for _, recordType := range recordTypes {
		var descriptor protoreflect.MessageDescriptor
		if recordType != nil {
			descriptor = recordType.Descriptor
		}
		current := keyExpressionTypes(root, descriptor, physicalRecordTypeKeyType(recordType))
		current = alignPhysicalTypes(current, size)
		if merged == nil {
			merged = current
			continue
		}
		for i := range merged {
			merged[i] = mergePhysicalTypes(merged[i], current[i])
		}
	}
	return alignPhysicalTypes(merged, size)
}

func keyExpressionTypes(
	expression *gen.KeyExpression,
	descriptor protoreflect.MessageDescriptor,
	recordTypeKeyType values.Type,
) []values.Type {
	if expression == nil {
		return nil
	}
	switch {
	case expression.Field != nil:
		field := expression.Field
		if descriptor == nil || field.FieldName == nil {
			return []values.Type{values.UnknownType}
		}
		fieldDescriptor := descriptor.Fields().ByName(protoreflect.Name(field.GetFieldName()))
		if fieldDescriptor == nil || field.GetFanType() == gen.Field_CONCATENATE {
			return []values.Type{values.UnknownType}
		}
		return []values.Type{physicalTypeForField(fieldDescriptor)}

	case expression.Then != nil:
		var result []values.Type
		for _, child := range expression.Then.GetChild() {
			result = append(result, keyExpressionTypes(child, descriptor, recordTypeKeyType)...)
		}
		return result

	case expression.Nesting != nil:
		nesting := expression.Nesting
		var childDescriptor protoreflect.MessageDescriptor
		if descriptor != nil && nesting.Parent != nil && nesting.Parent.FieldName != nil {
			parent := descriptor.Fields().ByName(protoreflect.Name(nesting.Parent.GetFieldName()))
			if parent != nil && parent.Kind() == protoreflect.MessageKind {
				childDescriptor = parent.Message()
			}
		}
		return keyExpressionTypes(nesting.Child, childDescriptor, recordTypeKeyType)

	case expression.Grouping != nil:
		return keyExpressionTypes(expression.Grouping.GetWholeKey(), descriptor, recordTypeKeyType)

	case expression.KeyWithValue != nil:
		all := keyExpressionTypes(expression.KeyWithValue.GetInnerKey(), descriptor, recordTypeKeyType)
		split := int(expression.KeyWithValue.GetSplitPoint())
		if split < 0 {
			split = 0
		}
		return alignPhysicalTypes(all, split)

	case expression.Dimensions != nil:
		return keyExpressionTypes(expression.Dimensions.GetWholeKey(), descriptor, recordTypeKeyType)

	case expression.List != nil:
		var result []values.Type
		for _, child := range expression.List.GetChild() {
			result = append(result, keyExpressionTypes(child, descriptor, recordTypeKeyType)...)
		}
		return result

	case expression.Function != nil:
		if strings.EqualFold(expression.Function.GetName(), "cardinality") {
			return []values.Type{values.NullableInt}
		}
		return []values.Type{values.UnknownType}

	case expression.Value != nil:
		literal := expression.Value
		switch {
		case literal.FloatValue != nil:
			return []values.Type{values.NullableFloat}
		case literal.DoubleValue != nil:
			return []values.Type{values.NullableDouble}
		case literal.IntValue != nil:
			return []values.Type{values.NullableInt}
		case literal.LongValue != nil:
			return []values.Type{values.NullableLong}
		case literal.BoolValue != nil:
			return []values.Type{values.NullableBoolean}
		case literal.StringValue != nil:
			return []values.Type{values.NullableString}
		case literal.BytesValue != nil:
			return []values.Type{values.NullableBytes}
		default:
			return []values.Type{values.UnknownType}
		}

	case expression.RecordTypeKey != nil:
		return []values.Type{recordTypeKeyType}
	case expression.Version != nil:
		return []values.Type{values.NullableVersion}
	case expression.Split != nil:
		return unknownPhysicalTypes(int(expression.Split.GetSplitSize()))
	case expression.Empty != nil:
		return nil
	default:
		return nil
	}
}

// physicalRecordTypeKeyType mirrors RecordTypeKeyExpression.Evaluate: integer
// metadata keys are normalized to int64; unsupported non-integer explicit keys
// fall back to the message type name and are therefore physically strings.
func physicalRecordTypeKeyType(recordType *recordlayer.RecordType) values.Type {
	if recordType == nil {
		return values.UnknownType
	}
	switch recordType.GetRecordTypeKey().(type) {
	case int, int32, int64:
		return values.NotNullLong
	default:
		return values.NotNullString
	}
}

func physicalTypeForField(field protoreflect.FieldDescriptor) values.Type {
	nullable := field.HasPresence() && field.Cardinality() != protoreflect.Required
	code := values.TypeCodeUnknown
	switch field.Kind() {
	case protoreflect.BoolKind:
		code = values.TypeCodeBoolean
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		code = values.TypeCodeInt
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind, protoreflect.EnumKind:
		code = values.TypeCodeLong
	case protoreflect.FloatKind:
		code = values.TypeCodeFloat
	case protoreflect.DoubleKind:
		code = values.TypeCodeDouble
	case protoreflect.StringKind:
		code = values.TypeCodeString
	case protoreflect.BytesKind:
		code = values.TypeCodeBytes
	case protoreflect.MessageKind:
		if field.Message() != nil && string(field.Message().FullName()) == "com.apple.foundationdb.record.UUID" {
			code = values.TypeCodeUuid
		}
	}
	if code == values.TypeCodeUnknown {
		return values.UnknownType
	}
	return values.NewPrimitiveType(code, nullable)
}

func mergePhysicalTypes(left, right values.Type) values.Type {
	if left == nil || right == nil ||
		left.Code() == values.TypeCodeUnknown || right.Code() == values.TypeCodeUnknown ||
		left.Code() != right.Code() {
		return values.UnknownType
	}
	if left.Equals(right) {
		return left
	}
	// Key components are scalar by metadata validation. A nullability mismatch
	// across record types does not change tuple width; retain the common code and
	// conservatively mark it nullable.
	if left.Code().IsPrimitive() || left.Code() == values.TypeCodeUuid {
		return values.NewPrimitiveType(left.Code(), left.IsNullable() || right.IsNullable())
	}
	return values.UnknownType
}

func unknownPhysicalTypes(size int) []values.Type {
	result := make([]values.Type, size)
	for i := range result {
		result[i] = values.UnknownType
	}
	return result
}

func alignPhysicalTypes(types []values.Type, size int) []values.Type {
	result := unknownPhysicalTypes(size)
	for i := 0; i < len(types) && i < size; i++ {
		if types[i] != nil {
			result[i] = types[i]
		}
	}
	return result
}
