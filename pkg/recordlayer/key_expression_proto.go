// Portions derived from FoundationDB Record Layer (KeyExpression.java,
// NestingKeyExpression.java, FunctionKeyExpression.java,
// FieldKeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/metadatapb"
)

// fanTypeToProto converts Go FanType to proto Field.FanType.
// Go FanTypeNone → Proto SCALAR, FanTypeFanOut → FAN_OUT, FanTypeConcatenate → CONCATENATE.
// Matches Java's KeyExpression.FanType.toProto().
func fanTypeToProto(ft FanType) metadatapb.Field_FanType {
	switch ft {
	case FanTypeFanOut:
		return metadatapb.Field_FAN_OUT
	case FanTypeConcatenate:
		return metadatapb.Field_CONCATENATE
	default: // FanTypeNone
		return metadatapb.Field_SCALAR
	}
}

// fanTypeFromProto converts proto Field.FanType to Go FanType.
func fanTypeFromProto(ft metadatapb.Field_FanType) FanType {
	switch ft {
	case metadatapb.Field_FAN_OUT:
		return FanTypeFanOut
	case metadatapb.Field_CONCATENATE:
		return FanTypeConcatenate
	default: // SCALAR
		return FanTypeNone
	}
}

// ToKeyExpression serializes a FieldKeyExpression to proto.
// Matches Java's FieldKeyExpression.toKeyExpression(): the
// nullInterpretation field is written EXPLICITLY even at its NOT_UNIQUE
// default — the stored bytes carry it (byte-golden-pinned).
func (f *FieldKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	ft := fanTypeToProto(f.fanType)
	return &metadatapb.KeyExpression{
		Field: &metadatapb.Field{
			FieldName:          &f.fieldName,
			FanType:            &ft,
			NullInterpretation: nullInterpretationEnum(f.nullStandin),
		},
	}
}

// nullInterpretationEnum is Java's NullStandin.toProto (Key.java:395-407).
func nullInterpretationEnum(standin NullStandin) *metadatapb.Field_NullInterpretation {
	switch standin {
	case NullStandinNullUnique:
		return metadatapb.Field_UNIQUE.Enum()
	case NullStandinNotNull:
		return metadatapb.Field_NOT_NULL.Enum()
	default:
		return metadatapb.Field_NOT_UNIQUE.Enum()
	}
}

// nullStandinFromProto is Java's NullStandin.valueOf (Key.java:409-420). The
// proto2 enum is closed: an unknown value is kept as an unknown field and the
// field reads as its default, NOT_UNIQUE, in both engines.
func nullStandinFromProto(ni metadatapb.Field_NullInterpretation) NullStandin {
	switch ni {
	case metadatapb.Field_UNIQUE:
		return NullStandinNullUnique
	case metadatapb.Field_NOT_NULL:
		return NullStandinNotNull
	default:
		return NullStandinNull
	}
}

// ToKeyExpression serializes a CompositeKeyExpression (ThenKeyExpression) to proto.
// Matches Java's ThenKeyExpression.toKeyExpression().
func (c *CompositeKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	children := make([]*metadatapb.KeyExpression, len(c.expressions))
	for i, child := range c.expressions {
		children[i] = child.ToKeyExpression()
	}
	return &metadatapb.KeyExpression{
		Then: &metadatapb.Then{
			Child: children,
		},
	}
}

// ToKeyExpression serializes a NestingKeyExpression to proto.
// Matches Java's NestingKeyExpression.toKeyExpression().
func (n *NestingKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	ft := fanTypeToProto(n.fanType)
	return &metadatapb.KeyExpression{
		Nesting: &metadatapb.Nesting{
			Parent: &metadatapb.Field{
				FieldName:          &n.parentField,
				FanType:            &ft,
				NullInterpretation: nullInterpretationEnum(n.parentNullStandin),
			},
			Child: n.child.ToKeyExpression(),
		},
	}
}

// ToKeyExpression serializes an EmptyKeyExpression to proto.
// Matches Java's EmptyKeyExpression.toKeyExpression().
func (e *EmptyKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	return &metadatapb.KeyExpression{
		Empty: &metadatapb.Empty{},
	}
}

// ToKeyExpression serializes a RecordTypeKeyExpression to proto.
func (r *RecordTypeKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	return &metadatapb.KeyExpression{
		RecordTypeKey: &metadatapb.RecordTypeKey{},
	}
}

// maxKeyExpressionDepth caps recursion through nested Then/Nesting/Grouping
// etc. so a crafted proto with pathological deep nesting cannot blow the
// goroutine stack. 128 is well above any real schema (typical depth ≤ 10)
// but small enough that exceeding it signals an adversarial input rather
// than a legitimate schema.
const maxKeyExpressionDepth = 128

// KeyExpressionFromProto deserializes a protobuf KeyExpression to a Go KeyExpression.
// Exactly one field must be set. Matches Java's KeyExpression.fromProto().
func KeyExpressionFromProto(expr *metadatapb.KeyExpression) (KeyExpression, error) {
	return keyExpressionFromProtoDepth(expr, 0)
}

// keyExpressionFromProtoDepth is the recursive worker. Helpers that
// recurse pass depth+1 so a pathological proto hits the depth cap
// before the goroutine stack.
func keyExpressionFromProtoDepth(expr *metadatapb.KeyExpression, depth int) (KeyExpression, error) {
	if depth > maxKeyExpressionDepth {
		return nil, fmt.Errorf("key expression nested deeper than %d levels", maxKeyExpressionDepth)
	}
	if expr == nil {
		// An absent message field reads as its default instance in Java
		// (protobuf-java's getters never return null), so an absent child is
		// an expression with no root: a Nesting's child from stored bytes, and
		// in memory any child proto2 marks required.
		return nil, &KeyExpressionDeserializationError{Message: "Exactly one root must be specified for an index"}
	}

	var root KeyExpression
	found := 0

	if expr.Field != nil {
		found++
		f, err := fieldFromProto(expr.Field)
		if err != nil {
			return nil, err
		}
		root = f
	}
	if expr.Nesting != nil {
		found++
		child, err := nestingFromProto(expr.Nesting, depth+1)
		if err != nil {
			return nil, err
		}
		root = child
	}
	if expr.Then != nil {
		found++
		then, err := thenFromProto(expr.Then, depth+1)
		if err != nil {
			return nil, err
		}
		root = then
	}
	if expr.Empty != nil {
		found++
		root = EmptyKey()
	}
	if expr.RecordTypeKey != nil {
		found++
		root = RecordTypeKey()
	}
	if expr.Grouping != nil {
		found++
		g, err := groupingFromProto(expr.Grouping, depth+1)
		if err != nil {
			return nil, err
		}
		root = g
	}
	if expr.Value != nil {
		found++
		v, err := valueFromProto(expr.Value)
		if err != nil {
			return nil, err
		}
		root = Literal(v)
	}
	if expr.KeyWithValue != nil {
		found++
		kwv, err := keyWithValueFromProto(expr.KeyWithValue, depth+1)
		if err != nil {
			return nil, err
		}
		root = kwv
	}
	if expr.Version != nil {
		found++
		root = VersionKey()
	}
	if expr.Function != nil {
		found++
		fn, err := functionFromProto(expr.Function, depth+1)
		if err != nil {
			return nil, err
		}
		root = fn
	}
	if expr.Split != nil {
		found++
		sp, err := splitFromProto(expr.Split, depth+1)
		if err != nil {
			return nil, err
		}
		root = sp
	}
	if expr.List != nil {
		found++
		l, err := listFromProto(expr.List, depth+1)
		if err != nil {
			return nil, err
		}
		root = l
	}

	if expr.Dimensions != nil {
		found++
		d, err := dimensionsFromProto(expr.Dimensions, depth+1)
		if err != nil {
			return nil, err
		}
		root = d
	}

	if root == nil || found > 1 {
		// Java's text (KeyExpression.java:404-405), for every level of the
		// expression as Java's fromProto is recursive.
		return nil, &KeyExpressionDeserializationError{Message: "Exactly one root must be specified for an index"}
	}
	return root, nil
}

// dimensionsFromProto reconstructs a DimensionsKeyExpression from a proto Dimensions.
func dimensionsFromProto(d *metadatapb.Dimensions, depth int) (*DimensionsKeyExpression, error) {
	wholeKey, err := keyExpressionFromProtoDepth(d.WholeKey, depth)
	if err != nil {
		return nil, fmt.Errorf("dimensions whole_key: %w", err)
	}
	return Dimensions(wholeKey, int(d.GetPrefixSize()), int(d.GetDimensionsSize())), nil
}

// fieldFromProto reconstructs a FieldKeyExpression from a proto Field, as
// Java's FieldKeyExpression(Field) does (FieldKeyExpression.java:122-132): a
// Field without its name or its fan type, both required fields that only an
// in-memory proto or a partial parse can lack, is refused. A fan type number
// the enum does not declare is a missing fan type: protobuf-java parses it into
// the unknown fields. protobuf-go keeps it in the field, which the meta-data
// loader moves (proto_closed_enums.go); a Field that reaches here without that
// load is read the same way.
func fieldFromProto(f *metadatapb.Field) (*FieldKeyExpression, error) {
	if f.FieldName == nil {
		return nil, &KeyExpressionDeserializationError{Message: "Serialized Field is missing field name"}
	}
	if f.FanType == nil || !declaredEnum(f.ProtoReflect().Descriptor().Fields().ByName("fan_type"), protoreflect.EnumNumber(f.GetFanType())) {
		return nil, &KeyExpressionDeserializationError{Message: "Serialized Field is missing fan type"}
	}
	return &FieldKeyExpression{
		fieldName:   f.GetFieldName(),
		fanType:     fanTypeFromProto(f.GetFanType()),
		nullStandin: nullStandinFromProto(f.GetNullInterpretation()),
	}, nil
}

// nestingFromProto reconstructs a NestingKeyExpression from a proto Nesting, as
// Java's NestingKeyExpression(Nesting) does (NestingKeyExpression.java:67-73):
// the parent is read first, as a Field, then the child.
func nestingFromProto(n *metadatapb.Nesting, depth int) (KeyExpression, error) {
	if n.Parent == nil {
		return nil, &KeyExpressionDeserializationError{Message: "Serialized Nesting is missing parent"}
	}
	parent, err := fieldFromProto(n.Parent)
	if err != nil {
		return nil, err
	}
	child, err := keyExpressionFromProtoDepth(n.Child, depth)
	if err != nil {
		return nil, fmt.Errorf("nesting child: %w", err)
	}
	return &NestingKeyExpression{
		parentField:       parent.fieldName,
		fanType:           parent.fanType,
		child:             child,
		parentNullStandin: parent.nullStandin,
	}, nil
}

// thenFromProto reconstructs a CompositeKeyExpression from a proto Then, as
// Java's ThenKeyExpression(Then) does (ThenKeyExpression.java:77-86): every
// child decoded, a nested Then contributing its children (Concat flattens as
// Java's add does), and only then the count refused below two. So a Then
// holding one Then of two children is accepted with those two, as Java
// accepts it, and a nested Then an earlier Go wrote loads flat, as Java loads
// it.
func thenFromProto(t *metadatapb.Then, depth int) (KeyExpression, error) {
	exprs := make([]KeyExpression, len(t.Child))
	for i, child := range t.Child {
		expr, err := keyExpressionFromProtoDepth(child, depth)
		if err != nil {
			return nil, fmt.Errorf("then child %d: %w", i, err)
		}
		exprs[i] = expr
	}
	then := Concat(exprs...).(*CompositeKeyExpression)
	if len(then.expressions) < 2 {
		return nil, &KeyExpressionDeserializationError{Message: "Then must have at least 2 children"}
	}
	return then, nil
}

// groupingFromProto reconstructs a GroupingKeyExpression from a proto Grouping.
func groupingFromProto(g *metadatapb.Grouping, depth int) (*GroupingKeyExpression, error) {
	wholeKey, err := keyExpressionFromProtoDepth(g.WholeKey, depth)
	if err != nil {
		return nil, fmt.Errorf("grouping whole key: %w", err)
	}
	groupedCount := int(g.GetGroupedCount())
	columnSize := wholeKey.ColumnSize()
	if groupedCount < 0 || groupedCount > columnSize {
		return nil, fmt.Errorf("grouping grouped_count %d out of range [0, %d]", groupedCount, columnSize)
	}
	return &GroupingKeyExpression{
		wholeKey:     wholeKey,
		groupedCount: groupedCount,
	}, nil
}

// keyWithValueFromProto reconstructs a KeyWithValueExpression from a proto KeyWithValue.
// Validates split_point against inner.ColumnSize — a negative or out-of-range
// value from crafted proto would otherwise propagate into index-maintainer
// slicing and cause an OOB panic.
func keyWithValueFromProto(kwv *metadatapb.KeyWithValue, depth int) (*KeyWithValueExpression, error) {
	inner, err := keyExpressionFromProtoDepth(kwv.InnerKey, depth)
	if err != nil {
		return nil, fmt.Errorf("key_with_value inner key: %w", err)
	}
	splitPoint := int(kwv.GetSplitPoint())
	columnSize := inner.ColumnSize()
	if splitPoint < 0 || splitPoint > columnSize {
		return nil, fmt.Errorf("key_with_value split_point %d out of range [0, %d]", splitPoint, columnSize)
	}
	return KeyWithValue(inner, splitPoint), nil
}

// ToKeyExpression serializes GroupingKeyExpression to proto.
// Matches Java's GroupingKeyExpression.toKeyExpression().
func (g *GroupingKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	gc := int32(g.groupedCount)
	return &metadatapb.KeyExpression{
		Grouping: &metadatapb.Grouping{
			WholeKey:     g.wholeKey.ToKeyExpression(),
			GroupedCount: &gc,
		},
	}
}

// ToKeyExpression serializes LiteralKeyExpression to proto.
// Matches Java's LiteralKeyExpression.toKeyExpression().
func (l *LiteralKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	v, _ := valueToProto(l.value)
	return &metadatapb.KeyExpression{
		Value: v,
	}
}

// ToKeyExpression serializes KeyWithValueExpression to proto.
// Matches Java's KeyWithValueExpression.toKeyExpression().
func (k *KeyWithValueExpression) ToKeyExpression() *metadatapb.KeyExpression {
	sp := int32(k.splitPoint)
	return &metadatapb.KeyExpression{
		KeyWithValue: &metadatapb.KeyWithValue{
			InnerKey:   k.innerKey.ToKeyExpression(),
			SplitPoint: &sp,
		},
	}
}

// ToKeyExpression serializes a VersionKeyExpression to proto.
// Matches Java's VersionKeyExpression.toKeyExpression().
func (v *VersionKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	return &metadatapb.KeyExpression{
		Version: &metadatapb.Version{},
	}
}

// ToKeyExpression serializes a FunctionKeyExpression to proto.
// Matches Java's FunctionKeyExpression.toKeyExpression().
func (f *FunctionKeyExpression) ToKeyExpression() *metadatapb.KeyExpression {
	return &metadatapb.KeyExpression{
		Function: &metadatapb.Function{
			Name:      &f.name,
			Arguments: f.arguments.ToKeyExpression(),
		},
	}
}

// functionFromProto reconstructs a function key expression from a proto
// Function. A "cardinality" function deserialises to the dedicated
// CardinalityFunctionKeyExpression so its fast paths and createsDuplicates==false
// survive a catalog round-trip (the proto bytes are identical either way —
// Function{name:"cardinality", arguments}). Mirrors Java's
// FunctionKeyExpression.Registry dispatching the named builder.
func functionFromProto(fn *metadatapb.Function, depth int) (KeyExpression, error) {
	args, err := keyExpressionFromProtoDepth(fn.Arguments, depth)
	if err != nil {
		return nil, fmt.Errorf("function arguments: %w", err)
	}
	// Java's fromProto is create, and rethrows its refusal as a
	// DeserializationException of the same text (FunctionKeyExpression.java:
	// 235-241). A name Go does not register is loaded, where Java refuses it
	// ("Function not defined"; DIVERGENCES.md), so Go can open a store whose
	// index uses a function only a Java module registers.
	name := fn.GetName()
	if _, registered := LookupFunction(name); registered {
		if fault := functionConstructionFault(name, args); fault != nil {
			return nil, &KeyExpressionDeserializationError{Message: fault.Error()}
		}
	}
	f := FunctionKeyExpression{name: name, arguments: args}
	if name == FunctionNameCardinality {
		return &CardinalityFunctionKeyExpression{FunctionKeyExpression: f}, nil
	}
	return &f, nil
}

// splitFromProto reconstructs a SplitKeyExpression from a proto Split.
// Rejects splitSize <= 0 with a typed error instead of letting Split() panic
// — proto bytes from an untrusted catalog can trigger this path.
func splitFromProto(s *metadatapb.Split, depth int) (*SplitKeyExpression, error) {
	size := int(s.GetSplitSize())
	if size <= 0 {
		return nil, fmt.Errorf("split size must be positive, got %d", size)
	}
	joined, err := keyExpressionFromProtoDepth(s.Joined, depth)
	if err != nil {
		return nil, fmt.Errorf("split joined: %w", err)
	}
	return Split(joined, size), nil
}

// listFromProto reconstructs a ListKeyExpression from a proto List.
func listFromProto(l *metadatapb.List, depth int) (*ListKeyExpression, error) {
	// Java's ListKeyExpression(RecordKeyExpressionProto.List) accepts empty children lists.
	// Match Java: allow zero children for proto round-trip compatibility.
	children := make([]KeyExpression, len(l.Child))
	for i, child := range l.Child {
		expr, err := keyExpressionFromProtoDepth(child, depth)
		if err != nil {
			return nil, fmt.Errorf("list child %d: %w", i, err)
		}
		children[i] = expr
	}
	return ListExpr(children...), nil
}
