// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// RecordTypeKeyExpression represents the special record type key prefix.
// Matches Java's RecordTypeKeyExpression: evaluates to the record type key
// (an integer derived from the union descriptor field number).
// It carries NO state. Java's RecordTypeKeyExpression is a singleton with no
// fields whose evaluateMessage reads the key off the record being evaluated:
//
//	record != null ? Key.Evaluated.scalar(record.getRecordType().getRecordTypeKey())
//	               : Key.Evaluated.NULL
//
// Go previously bound a metadata-derived map onto the expression instead. That
// made the expression object metadata-scoped state while the object itself is
// shared: RecordMetaDataBuilder does not copy key expressions, so two builds
// from one expression graph both wrote into the same slot, and whichever bound
// LAST decided the record type key for BOTH — the first metadata's stores then
// wrote index entries and primary keys under the second metadata's key. No
// amount of invalidation fixes that, because the two bindings are both live at
// once; the state has to go. Resolving from the record makes the key a
// function of the record's own resolved type, which is what it always meant.
type RecordTypeKeyExpression struct{}

// RecordTypeKey creates a key expression that prefixes with the record type
func RecordTypeKey() *RecordTypeKeyExpression {
	return &RecordTypeKeyExpression{}
}

// RecordTypeKeyUnresolvedError reports an evaluation of a record type key
// against a record whose type was never resolved. Java cannot reach this: its
// FDBRecord always carries a RecordType, and the save path evaluates the
// primary key against an FDBStoredRecordBuilder that has been given the type
// before any key is computed (FDBRecordStore.saveTypedRecord). Go's callers
// must do the same. It is an ERROR rather than a fallback to the message's
// type NAME because that fallback wrote a string where the integer type key
// belongs — bytes Java reads as a different key entirely — and did it
// silently.
type RecordTypeKeyUnresolvedError struct {
	// MessageType is the proto message name that was being evaluated, empty
	// when there was no message either.
	MessageType string
}

func (e *RecordTypeKeyUnresolvedError) Error() string {
	return fmt.Sprintf("record type key evaluated without a resolved record type (message type %q): "+
		"the caller must supply the FDBStoredRecord's RecordType", e.MessageType)
}

// recordTypeKeyOf resolves the record type key exactly as Java does: off the
// record's own resolved type. A nil record is Java's NULL case.
func recordTypeKeyOf(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, bool, error) {
	if record == nil {
		return nil, false, nil // Java: Key.Evaluated.NULL
	}
	if record.RecordType == nil {
		var name string
		if msg != nil {
			name = string(msg.ProtoReflect().Descriptor().Name())
		}
		return nil, false, &RecordTypeKeyUnresolvedError{MessageType: name}
	}
	return record.RecordType.GetRecordTypeKey(), true, nil
}

// Nest is Concat(RecordTypeKey(), expr), Java's concat(recordType(), expr).
// Java's RecordTypeKeyExpression has no children, so this is a Go spelling of
// that Then, not a second shape: it builds the Then (flattened as Concat
// flattens) and leaves the receiver unchanged.
func (r *RecordTypeKeyExpression) Nest(expr KeyExpression) KeyExpression {
	return Concat(r, expr)
}

// Evaluate returns the record type key (integer).
// Matches Java's RecordTypeKeyExpression.evaluateMessage() which returns
// record.getRecordType().getRecordTypeKey() — the union descriptor field number.
func (r *RecordTypeKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg == nil {
		return nilKeyResult, nil
	}
	typeKey, ok, err := recordTypeKeyOf(record, msg)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Java's record == null arm: Key.Evaluated.NULL.
		return nilKeyResult, nil
	}
	return [][]any{{typeKey}}, nil
}

// EvaluateScalar returns the type key directly — zero alloc.
func (r *RecordTypeKeyExpression) EvaluateScalar(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, error) {
	if msg == nil {
		return nil, nil
	}
	typeKey, ok, err := recordTypeKeyOf(record, msg)
	if err != nil || !ok {
		return nil, err
	}
	return typeKey, nil
}

// EvaluateFlat returns the type key as a single-element []any.
func (r *RecordTypeKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if msg == nil {
		return []any{nil}, nil
	}
	typeKey, ok, err := recordTypeKeyOf(record, msg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []any{nil}, nil
	}
	return []any{typeKey}, nil
}

// PackDirect encodes the record type key directly into a Packer. Reports false
// when the type cannot be resolved from the record, which routes the caller to
// the erroring Evaluate path rather than packing a guess.
func (r *RecordTypeKeyExpression) PackDirect(pk *tuple.Packer, record *FDBStoredRecord[proto.Message], msg proto.Message) bool {
	if msg == nil {
		return false
	}
	typeKey, ok, err := recordTypeKeyOf(record, msg)
	if err != nil || !ok {
		return false
	}
	pk.EncodeElement(typeKey)
	return true
}

// FieldNames returns no field names: the record type key reads no field.
func (r *RecordTypeKeyExpression) FieldNames() []string {
	return []string{}
}

// ColumnSize is 1, Java's getColumnSize.
func (r *RecordTypeKeyExpression) ColumnSize() int {
	return 1
}

// IsRecordTypeExpression checks if a key expression starts with record type
func IsRecordTypeExpression(expr KeyExpression) bool {
	_, ok := expr.(*RecordTypeKeyExpression)
	return ok
}

// EmptyKeyExpression produces an empty tuple — used for ungrouped record counting.
// When used as a recordCountKey, a single total count is maintained.
type EmptyKeyExpression struct{}

// EmptyKey creates a key expression that produces an empty tuple.
func EmptyKey() KeyExpression {
	return &EmptyKeyExpression{}
}

// Evaluate returns one empty tuple (no key components).
func (e *EmptyKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	return emptyKeyResult, nil
}

// EvaluateFlat returns empty (no elements to append).
func (e *EmptyKeyExpression) EvaluateFlat(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([]any, error) {
	return nil, nil
}

// FieldNames returns no field names.
func (e *EmptyKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 0 — an empty expression produces no tuple elements.
func (e *EmptyKeyExpression) ColumnSize() int {
	return 0
}

// LiteralKeyExpression represents a static constant value in a key expression.
// The value does not depend on the record — it evaluates to the same fixed value for every record.
// Primary use case: passing static arguments to function key expressions, or creating
// composite indexes with a fixed prefix.
// Matches Java's LiteralKeyExpression.
type LiteralKeyExpression struct {
	value any
}

// Literal creates a key expression that always evaluates to the given constant value.
// Supported types: nil, int, int32, int64, float32, float64, bool, string, []byte.
// Matches Java's Key.Expressions.value(Object).
func Literal(value any) *LiteralKeyExpression {
	// Validate type at construction time — unsupported types would silently
	// produce nil proto fields in ToKeyExpression() and wrong equality results.
	switch value.(type) {
	case nil, int, int32, int64, float32, float64, bool, string, []byte:
		// supported
	default:
		panic(fmt.Sprintf("Literal: unsupported value type %T", value))
	}
	return &LiteralKeyExpression{value: value}
}

// Evaluate returns the constant value regardless of the record.
// Matches Java's LiteralKeyExpression.evaluateMessage() which ignores the record parameter.
//
// The value is the one the tuple layer encodes: Java's Key.Evaluated.scalar holds
// the literal's Integer, which Tuple packs as the integer it is, while Go's tuple
// encoder takes integers as int64 and refuses an int32. The literal keeps its
// carrier (an int32 is written as int_value, the target's width) for ToProto.
func (l *LiteralKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	switch v := l.value.(type) {
	case int32:
		return [][]any{{int64(v)}}, nil
	case int:
		return [][]any{{int64(v)}}, nil
	}
	return [][]any{{l.value}}, nil
}

// FieldNames returns an empty slice — literal expressions don't access any fields.
func (l *LiteralKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 1 — a literal expression produces a single tuple element.
func (l *LiteralKeyExpression) ColumnSize() int {
	return 1
}

// GetValue returns the constant value held by this expression.
func (l *LiteralKeyExpression) GetValue() any {
	return l.value
}

// VersionKeyExpression extracts the record version as a Versionstamp tuple element.
// Used by VERSION indexes to index records by their commit version.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.VersionKeyExpression.
type VersionKeyExpression struct{}

// VersionKey creates a key expression that evaluates to the record's version.
// Matches Java's Key.Expressions.version().
func VersionKey() *VersionKeyExpression {
	return &VersionKeyExpression{}
}

// Evaluate returns the record's version as a Versionstamp.
// For complete versions, returns a complete tuple.Versionstamp.
// For incomplete versions, returns an incomplete tuple.Versionstamp (with placeholder bytes).
// If record or version is nil, returns [[nil]].
// Matches Java's VersionKeyExpression.evaluateMessage().
func (v *VersionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	if record == nil || record.Version == nil {
		return [][]any{{nil}}, nil
	}

	ver := record.Version
	vs := tuple.Versionstamp{
		UserVersion: uint16(ver.GetLocalVersion()),
	}
	copy(vs.TransactionVersion[:], ver.ToBytes()[:GlobalVersionBytes])

	return [][]any{{vs}}, nil
}

// FieldNames returns an empty slice — version expressions don't access proto fields.
func (v *VersionKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 1 — a version expression produces a single tuple element.
func (v *VersionKeyExpression) ColumnSize() int {
	return 1
}
