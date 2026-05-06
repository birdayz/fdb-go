package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// fanTypeToProto converts Go FanType to proto Field.FanType.
// Go FanTypeNone → Proto SCALAR, FanTypeFanOut → FAN_OUT, FanTypeConcatenate → CONCATENATE.
// Matches Java's KeyExpression.FanType.toProto().
func fanTypeToProto(ft FanType) gen.Field_FanType {
	switch ft {
	case FanTypeFanOut:
		return gen.Field_FAN_OUT
	case FanTypeConcatenate:
		return gen.Field_CONCATENATE
	default: // FanTypeNone
		return gen.Field_SCALAR
	}
}

// fanTypeFromProto converts proto Field.FanType to Go FanType.
func fanTypeFromProto(ft gen.Field_FanType) FanType {
	switch ft {
	case gen.Field_FAN_OUT:
		return FanTypeFanOut
	case gen.Field_CONCATENATE:
		return FanTypeConcatenate
	default: // SCALAR
		return FanTypeNone
	}
}

// ToKeyExpression serializes a FieldKeyExpression to proto.
// Matches Java's FieldKeyExpression.toKeyExpression().
func (f *FieldKeyExpression) ToKeyExpression() *gen.KeyExpression {
	ft := fanTypeToProto(f.fanType)
	return &gen.KeyExpression{
		Field: &gen.Field{
			FieldName: &f.fieldName,
			FanType:   &ft,
		},
	}
}

// ToKeyExpression serializes a CompositeKeyExpression (ThenKeyExpression) to proto.
// Matches Java's ThenKeyExpression.toKeyExpression().
func (c *CompositeKeyExpression) ToKeyExpression() *gen.KeyExpression {
	children := make([]*gen.KeyExpression, len(c.expressions))
	for i, child := range c.expressions {
		children[i] = child.ToKeyExpression()
	}
	return &gen.KeyExpression{
		Then: &gen.Then{
			Child: children,
		},
	}
}

// ToKeyExpression serializes a NestingKeyExpression to proto.
// Matches Java's NestingKeyExpression.toKeyExpression().
func (n *NestingKeyExpression) ToKeyExpression() *gen.KeyExpression {
	ft := fanTypeToProto(n.fanType)
	return &gen.KeyExpression{
		Nesting: &gen.Nesting{
			Parent: &gen.Field{
				FieldName: &n.parentField,
				FanType:   &ft,
			},
			Child: n.child.ToKeyExpression(),
		},
	}
}

// ToKeyExpression serializes an EmptyKeyExpression to proto.
// Matches Java's EmptyKeyExpression.toKeyExpression().
func (e *EmptyKeyExpression) ToKeyExpression() *gen.KeyExpression {
	return &gen.KeyExpression{
		Empty: &gen.Empty{},
	}
}

// ToKeyExpression serializes a RecordTypeKeyExpression to proto.
// A bare RecordTypeKeyExpression serializes as RecordTypeKey{}.
// With a nested expression, serializes as Then{RecordTypeKey{}, nested} matching
// Java's concat(recordTypeKey(), nested).
func (r *RecordTypeKeyExpression) ToKeyExpression() *gen.KeyExpression {
	if r.nested == nil {
		return &gen.KeyExpression{
			RecordTypeKey: &gen.RecordTypeKey{},
		}
	}
	// With nested → Then(RecordTypeKey, nested)
	return &gen.KeyExpression{
		Then: &gen.Then{
			Child: []*gen.KeyExpression{
				{RecordTypeKey: &gen.RecordTypeKey{}},
				r.nested.ToKeyExpression(),
			},
		},
	}
}

// maxKeyExpressionDepth caps recursion through nested Then/Nesting/Grouping
// etc. so a crafted proto with pathological deep nesting cannot blow the
// goroutine stack. 128 is well above any real schema (typical depth ≤ 10)
// but small enough that exceeding it signals an adversarial input rather
// than a legitimate schema. Reviewer-flagged hardening in swingshift-35.
const maxKeyExpressionDepth = 128

// KeyExpressionFromProto deserializes a protobuf KeyExpression to a Go KeyExpression.
// Exactly one field must be set. Matches Java's KeyExpression.fromProto().
func KeyExpressionFromProto(expr *gen.KeyExpression) (KeyExpression, error) {
	return keyExpressionFromProtoDepth(expr, 0)
}

// keyExpressionFromProtoDepth is the recursive worker. Helpers that
// recurse pass depth+1 so a pathological proto hits the depth cap
// before the goroutine stack.
func keyExpressionFromProtoDepth(expr *gen.KeyExpression, depth int) (KeyExpression, error) {
	if depth > maxKeyExpressionDepth {
		return nil, fmt.Errorf("key expression nested deeper than %d levels", maxKeyExpressionDepth)
	}
	if expr == nil {
		return nil, fmt.Errorf("nil key expression proto")
	}

	var root KeyExpression
	found := 0

	if expr.Field != nil {
		found++
		root = fieldFromProto(expr.Field)
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
		root = Literal(valueFromProto(expr.Value))
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
		return nil, fmt.Errorf("exactly one key expression type must be set, found %d", found)
	}
	return root, nil
}

// dimensionsFromProto reconstructs a DimensionsKeyExpression from a proto Dimensions.
func dimensionsFromProto(d *gen.Dimensions, depth int) (*DimensionsKeyExpression, error) {
	if d.WholeKey == nil {
		return nil, fmt.Errorf("dimensions expression missing whole_key")
	}
	wholeKey, err := keyExpressionFromProtoDepth(d.WholeKey, depth)
	if err != nil {
		return nil, fmt.Errorf("dimensions whole_key: %w", err)
	}
	return Dimensions(wholeKey, int(d.GetPrefixSize()), int(d.GetDimensionsSize())), nil
}

// fieldFromProto reconstructs a FieldKeyExpression from a proto Field.
func fieldFromProto(f *gen.Field) *FieldKeyExpression {
	return &FieldKeyExpression{
		fieldName: f.GetFieldName(),
		fanType:   fanTypeFromProto(f.GetFanType()),
	}
}

// nestingFromProto reconstructs a NestingKeyExpression from a proto Nesting.
func nestingFromProto(n *gen.Nesting, depth int) (KeyExpression, error) {
	if n.Parent == nil {
		return nil, fmt.Errorf("nesting expression missing parent field")
	}
	child, err := keyExpressionFromProtoDepth(n.Child, depth)
	if err != nil {
		return nil, fmt.Errorf("nesting child: %w", err)
	}
	return &NestingKeyExpression{
		parentField: n.Parent.GetFieldName(),
		fanType:     fanTypeFromProto(n.Parent.GetFanType()),
		child:       child,
	}, nil
}

// thenFromProto reconstructs a CompositeKeyExpression from a proto Then.
// Java flattens nested Then children; Go's CompositeKeyExpression naturally
// handles this since Concat doesn't nest.
func thenFromProto(t *gen.Then, depth int) (KeyExpression, error) {
	if len(t.Child) < 2 {
		return nil, fmt.Errorf("then expression requires at least 2 children, got %d", len(t.Child))
	}
	exprs := make([]KeyExpression, len(t.Child))
	for i, child := range t.Child {
		expr, err := keyExpressionFromProtoDepth(child, depth)
		if err != nil {
			return nil, fmt.Errorf("then child %d: %w", i, err)
		}
		exprs[i] = expr
	}
	return Concat(exprs...), nil
}

// groupingFromProto reconstructs a GroupingKeyExpression from a proto Grouping.
func groupingFromProto(g *gen.Grouping, depth int) (*GroupingKeyExpression, error) {
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
func keyWithValueFromProto(kwv *gen.KeyWithValue, depth int) (*KeyWithValueExpression, error) {
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
func (g *GroupingKeyExpression) ToKeyExpression() *gen.KeyExpression {
	gc := int32(g.groupedCount)
	return &gen.KeyExpression{
		Grouping: &gen.Grouping{
			WholeKey:     g.wholeKey.ToKeyExpression(),
			GroupedCount: &gc,
		},
	}
}

// ToKeyExpression serializes LiteralKeyExpression to proto.
// Matches Java's LiteralKeyExpression.toKeyExpression().
func (l *LiteralKeyExpression) ToKeyExpression() *gen.KeyExpression {
	v, _ := valueToProto(l.value)
	return &gen.KeyExpression{
		Value: v,
	}
}

// ToKeyExpression serializes KeyWithValueExpression to proto.
// Matches Java's KeyWithValueExpression.toKeyExpression().
func (k *KeyWithValueExpression) ToKeyExpression() *gen.KeyExpression {
	sp := int32(k.splitPoint)
	return &gen.KeyExpression{
		KeyWithValue: &gen.KeyWithValue{
			InnerKey:   k.innerKey.ToKeyExpression(),
			SplitPoint: &sp,
		},
	}
}

// ToKeyExpression serializes a VersionKeyExpression to proto.
// Matches Java's VersionKeyExpression.toKeyExpression().
func (v *VersionKeyExpression) ToKeyExpression() *gen.KeyExpression {
	return &gen.KeyExpression{
		Version: &gen.Version{},
	}
}

// ToKeyExpression serializes a FunctionKeyExpression to proto.
// Matches Java's FunctionKeyExpression.toKeyExpression().
func (f *FunctionKeyExpression) ToKeyExpression() *gen.KeyExpression {
	return &gen.KeyExpression{
		Function: &gen.Function{
			Name:      &f.name,
			Arguments: f.arguments.ToKeyExpression(),
		},
	}
}

// functionFromProto reconstructs a FunctionKeyExpression from a proto Function.
func functionFromProto(fn *gen.Function, depth int) (*FunctionKeyExpression, error) {
	args, err := keyExpressionFromProtoDepth(fn.Arguments, depth)
	if err != nil {
		return nil, fmt.Errorf("function arguments: %w", err)
	}
	return FunctionExpr(fn.GetName(), args), nil
}

// splitFromProto reconstructs a SplitKeyExpression from a proto Split.
// Rejects splitSize <= 0 with a typed error instead of letting Split() panic
// — proto bytes from an untrusted catalog can trigger this path.
func splitFromProto(s *gen.Split, depth int) (*SplitKeyExpression, error) {
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
func listFromProto(l *gen.List, depth int) (*ListKeyExpression, error) {
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
