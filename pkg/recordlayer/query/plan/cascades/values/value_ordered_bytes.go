// Portions derived from FoundationDB Record Layer (ToOrderedBytesValue.java,
// FromOrderedBytesValue.java, TupleOrdering.java, Key.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/tupleordering"
)

// OrderedBytesDirection enumerates the four ordering modes Java's
// `TupleOrdering.Direction` supports — combinations of (ASC|DESC) ×
// (NULLS_FIRST|NULLS_LAST). Mirrors Java's enum verbatim so plan
// hashes / explain output diff cleanly across language boundaries.
type OrderedBytesDirection int

const (
	// OrderedBytesAscNullsFirst sorts ascending; NULL sorts BEFORE
	// any non-null value (lowest).
	OrderedBytesAscNullsFirst OrderedBytesDirection = iota
	// OrderedBytesAscNullsLast sorts ascending; NULL sorts AFTER any
	// non-null value (highest).
	OrderedBytesAscNullsLast
	// OrderedBytesDescNullsFirst sorts descending; NULL sorts BEFORE
	// (which becomes "highest" under DESC, since the iteration is
	// reversed).
	OrderedBytesDescNullsFirst
	// OrderedBytesDescNullsLast sorts descending; NULL sorts AFTER.
	OrderedBytesDescNullsLast
)

// String renders the direction for explain / debug print.
func (d OrderedBytesDirection) String() string {
	switch d {
	case OrderedBytesAscNullsFirst:
		return "ASC_NULLS_FIRST"
	case OrderedBytesAscNullsLast:
		return "ASC_NULLS_LAST"
	case OrderedBytesDescNullsFirst:
		return "DESC_NULLS_FIRST"
	case OrderedBytesDescNullsLast:
		return "DESC_NULLS_LAST"
	}
	return "INVALID"
}

// IsAscending reports whether the direction encodes an ASC ordering.
// Used by ordering-property analysis to determine whether the
// produced bytes preserve or invert the underlying value's natural
// ordering.
func (d OrderedBytesDirection) IsAscending() bool {
	return d == OrderedBytesAscNullsFirst || d == OrderedBytesAscNullsLast
}

// tupleDirection is the TupleOrdering.Direction the bytes are packed in.
func (d OrderedBytesDirection) tupleDirection() tupleordering.Direction {
	switch d {
	case OrderedBytesAscNullsLast:
		return tupleordering.AscNullsLast
	case OrderedBytesDescNullsFirst:
		return tupleordering.DescNullsFirst
	case OrderedBytesDescNullsLast:
		return tupleordering.DescNullsLast
	}
	return tupleordering.AscNullsFirst
}

// ToOrderedBytesValue encodes its child Value's evaluation as a
// FoundationDB-compatible ordered-bytes blob suitable for use as
// part of an index key. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ToOrderedBytesValue`.
//
// Used by index-key construction: the planner lowers a SQL
// ORDER-BY-expression-as-index-prefix to a chain of ToOrderedBytes
// applications — each column's ordering direction baked into the
// produced bytes so a forward FDB scan over the index produces rows
// in the requested SQL order.
//
// Result type: NotNullBytes. Even when the child is NULL, the
// encoding produces a sentinel byte sequence, so the byte output is
// always populated.
type ToOrderedBytesValue struct {
	Child     Value
	Direction OrderedBytesDirection
}

// NewToOrderedBytesValue constructs the encoder.
func NewToOrderedBytesValue(child Value, direction OrderedBytesDirection) *ToOrderedBytesValue {
	return &ToOrderedBytesValue{Child: child, Direction: direction}
}

// Children returns the single child Value.
func (v *ToOrderedBytesValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the SQL function name.
func (*ToOrderedBytesValue) Name() string { return "to_ordered_bytes" }

// Type is nullable BYTES, Java's primitiveType(BYTES), though the encoder
// produces bytes for every input.
func (*ToOrderedBytesValue) Type() Type { return NullableBytes }

// Evaluate packs the child as a one-element tuple in the direction
// (TupleOrdering.pack(Key.Evaluated.scalar(child).toTuple(), direction)).
// The row domain widens FLOAT to float64, so a FLOAT child is narrowed back
// to the float32 element Java packs, or the bytes would not match the index.
func (v *ToOrderedBytesValue) Evaluate(evalCtx any) (any, error) {
	if v.Child == nil {
		return nil, fmt.Errorf("to_ordered_bytes: no child")
	}
	value, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	element, err := rowValueToTupleElement(value, v.Child.Type())
	if err != nil {
		return nil, fmt.Errorf("to_ordered_bytes: %w", err)
	}
	return tupleordering.Pack(tuple.Tuple{element}, v.Direction.tupleDirection()), nil
}

// rowValueToTupleElement is the inverse of TupleElementToRowValue for one
// scalar: the element Java's Key.Evaluated.scalar packs.
func rowValueToTupleElement(value any, typ Type) (any, error) {
	switch tv := value.(type) {
	case nil, int64, string, []byte, bool, float32:
		return value, nil
	case int32:
		return int64(tv), nil
	case int:
		return int64(tv), nil
	case float64:
		if typ != nil && typ.Code() == TypeCodeFloat {
			return float32(tv), nil
		}
		return tv, nil
	case [16]byte:
		return tuple.UUID(tv), nil
	case tuple.UUID:
		return tv, nil
	}
	return nil, fmt.Errorf("value %T has no tuple encoding", value)
}

// CreateInverse returns the FromOrderedBytesValue that decodes the
// ordered-bytes form back to the original value. Java's
// createInverseValueMaybe always returns Optional.of — the encoding
// is always invertible. We expose the inverse as a method for
// matchers / rules that need to canonicalise To→From chains.
//
// Per Java's signature, the inverse takes a NEW child (the
// ordered-bytes input the inverse decodes) and the ORIGINAL
// child's result type (so the inverse knows what type to decode
// to).
func (v *ToOrderedBytesValue) CreateInverse(newChild Value, originalType Type) *FromOrderedBytesValue {
	return NewFromOrderedBytesValue(newChild, v.Direction, originalType)
}

// FromOrderedBytesValue decodes an ordered-bytes blob (the output
// of ToOrderedBytesValue) back to the original typed value.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.FromOrderedBytesValue`.
//
// The decoder needs the encoded direction (so it knows whether to
// undo the DESC inversion) and the target type (so it knows what
// Tuple element type to extract). Java's class carries both fields.
//
// As the inverse of ToOrderedBytesValue, this Value typically
// appears in covering-index / index-only access plans where the
// planner has rewritten a SQL projection to read the encoded form
// from an index entry.
type FromOrderedBytesValue struct {
	Child      Value
	Direction  OrderedBytesDirection
	TargetType Type
}

// NewFromOrderedBytesValue constructs the decoder.
func NewFromOrderedBytesValue(child Value, direction OrderedBytesDirection, targetType Type) *FromOrderedBytesValue {
	if targetType == nil {
		targetType = UnknownType
	}
	return &FromOrderedBytesValue{
		Child:      child,
		Direction:  direction,
		TargetType: targetType,
	}
}

// Children returns the single child Value.
func (v *FromOrderedBytesValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the SQL function name.
func (*FromOrderedBytesValue) Name() string { return "from_ordered_bytes" }

// Type returns the target type — the decoded value's natural type.
// Note: Java's getResultType() returns the target type made nullable,
// since decoding may produce NULL for the NULL-sentinel byte
// sequence. Go mirrors that — wraps target in nullable.
func (v *FromOrderedBytesValue) Type() Type {
	if v.TargetType == nil {
		return UnknownType
	}
	return WithNullability(v.TargetType, true)
}

// Evaluate decodes the child's bytes and answers the first element in the
// row domain (TupleOrdering.unpack(bytes, direction).get(0), then
// tupleValueToRuntimeValue). Java requires the bytes non-null.
func (v *FromOrderedBytesValue) Evaluate(evalCtx any) (any, error) {
	if v.Child == nil {
		return nil, fmt.Errorf("from_ordered_bytes: no child")
	}
	value, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	packed, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("from_ordered_bytes: child produced %T, not bytes", value)
	}
	t, err := tupleordering.Unpack(packed, v.Direction.tupleDirection())
	if err != nil {
		return nil, fmt.Errorf("from_ordered_bytes: %w", err)
	}
	if len(t) == 0 {
		return nil, fmt.Errorf("from_ordered_bytes: no element in %x", packed)
	}
	return TupleElementToRowValue(t[0]), nil
}
