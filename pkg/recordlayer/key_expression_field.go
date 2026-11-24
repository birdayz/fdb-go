// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fieldDescCache is an atomically-swapped cache entry for FieldKeyExpression.
// FieldKeyExpressions are shared across goroutines (via RecordMetaData), so
// the cache must be safe for concurrent reads/writes. Using atomic.Pointer
// ensures no torn reads (unlike bare struct fields which would race).
// The cache is keyed on the MessageDescriptor IDENTITY, never on its full
// name. A full name does not identify a descriptor: two distinct descriptors
// (a generated type and a dynamicpb type built from stored metadata, or two
// evolutions of one message) legitimately share "pkg.Rec" while numbering
// their fields differently. Keying on the name hands back a FieldDescriptor
// belonging to the OTHER descriptor, and protoreflect rejects that with a
// panic ("field descriptor does not belong to this message") on the index-key
// path.
type fieldDescCache struct {
	desc protoreflect.MessageDescriptor
	fd   protoreflect.FieldDescriptor
}

// FieldKeyExpression extracts a single field value from a record for use as a key component.
// For repeated fields, use FanOut() or Concatenate() to control multi-value handling.
type FieldKeyExpression struct {
	fieldName string
	fanType   FanType
	// nullStandin is Java's Key.Evaluated.NullStandin (Key.java:394-421): what
	// an absent field evaluates to, and whether its null takes part in a unique
	// index. Java's toProto writes it EXPLICITLY even at its default, so the
	// stored bytes carry it (the RFC-204 template byte-goldens).
	nullStandin NullStandin
	// fdCache caches the protoreflect.FieldDescriptor for this field name,
	// keyed by the message full name. Avoids ByName() map lookup per Evaluate.
	// Uses atomic.Pointer for goroutine safety (metadata is shared across txns).
	fdCache atomic.Pointer[fieldDescCache]
}

// FieldWithNullStandin is Java's Key.Expressions.field(name, fanType,
// nullStandin).
func FieldWithNullStandin(name string, fanType FanType, standin NullStandin) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: fanType, nullStandin: standin}
}

// absent is Java's test for a singular field that yields its null result:
// neither NOT_NULL nor present (FieldKeyExpression.java:208). Has is
// protobuf-java's hasField for either kind of presence: an implicit-presence
// (proto3) field at its default value is absent.
func (f *FieldKeyExpression) absent(m protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	return f.nullStandin != NullStandinNotNull && !m.Has(fd)
}

// Field creates a key expression that extracts a single (non-repeated) field.
func Field(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeNone}
}

// resolveFieldDescriptor returns the field descriptor this expression names on
// the given message, resolving and caching it atomically. Thread-safe.
//
// Resolution is BY NAME, and that is correct: Java resolves the same step by
// name on every evaluation — FieldKeyExpression.evaluateMessage does
// recordDescriptor.findFieldByName(fieldName) (FieldKeyExpression.java:183),
// and validate() does the same (:140). A key expression is declared against a
// field NAME in stored metadata, so the name is the identity here; the field's
// declaration index is not, and must never be substituted for it.
//
// An absent field is a DELIBERATE divergence from Java and must stay an error.
// Java folds it into getNullResult() — its Javadoc lists "the fieldDescriptor
// is null (meaning the field does not exist, i.e., incorrect metadata)" as one
// of that method's three callers (FieldKeyExpression.java:220-231), reached
// from evaluateMessage's final else (:214-216), so Java writes a null index
// entry. Go refuses instead, because the arm is only reachable through
// metadata Java itself calls incorrect and a null entry files every record of
// the type under one key with nothing recording that the field was missing.
// Refusing writes no bytes rather than different bytes, so stored data stays
// wire-identical; it is the write that never happens, loudly. The strictness
// is pinned by TestKeyExpressionFastPath_UnknownFieldErrorsEverywhere.
func (f *FieldKeyExpression) resolveFieldDescriptor(m protoreflect.Message) (protoreflect.FieldDescriptor, error) {
	md := m.Descriptor()
	if cached := f.fdCache.Load(); cached != nil && cached.desc == md {
		return cached.fd, nil
	}
	fd := md.Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s not found in message", f.fieldName)}
	}
	f.fdCache.Store(&fieldDescCache{desc: md, fd: fd})
	return fd, nil
}

// FanOut creates a key expression for a repeated field that produces one index entry
// per repeated value. Matches Java's Key.Expressions.field("name", FanType.FanOut).
func FanOut(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeFanOut}
}

// FieldConcatenate creates a key expression for a repeated field that collects all
// repeated values into a single tuple element. Matches Java's
// Key.Expressions.field("name", FanType.Concatenate). This is the argument shape a
// CARDINALITY() index uses: the whole array is materialised into one Key.Evaluated
// so its element count can be taken.
func FieldConcatenate(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeConcatenate}
}

// Evaluate extracts the field value(s) from the message.
// For non-repeated fields, returns one tuple with the single value.
// For repeated fields with FanOut, returns one tuple per value.
// For repeated fields with Concatenate, returns one tuple containing a nested tuple of all values.
func (f *FieldKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg == nil {
		return f.getNullResult(), nil
	}
	m := msg.ProtoReflect()

	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}

	if fd.IsList() {
		return f.evaluateRepeated(m, fd)
	}

	// Scalar field: an absent one is the null result, and under NOT_NULL an
	// unset field reads as its default (Get returns the default).
	if f.absent(m, fd) {
		return f.getNullResult(), nil
	}
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	// Return single-element result. Inner slice is 1 alloc; outer reuses fixed capacity.
	return [][]any{{result}}, nil
}

// EvaluateFlat returns the scalar value directly as a single-element []any.
func (f *FieldKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if msg == nil {
		v, err := f.nilMessageScalar()
		if err != nil {
			return nil, err
		}
		return []any{v}, nil
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		// Repeated fields can't be flattened — signal caller to fall through.
		return nil, fmt.Errorf("EvaluateFlat: repeated field %s", f.fieldName)
	}
	if f.absent(m, fd) {
		return []any{nil}, nil
	}
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	return []any{result}, nil
}

// nilMessageScalar is getNullResult's one value, for a nil message. FanOut's
// result is no entry, so it is an error, and the scalar and flat evaluators
// defer to Evaluate as they do for a repeated field.
func (f *FieldKeyExpression) nilMessageScalar() (any, error) {
	switch f.fanType {
	case FanTypeFanOut:
		return nil, fmt.Errorf("fan-out field %s of a nil message has no entry", f.fieldName)
	case FanTypeConcatenate:
		if f.nullStandin == NullStandinNotNull {
			return tuple.Tuple{}, nil
		}
	}
	return nil, nil
}

// EvaluateScalar returns the single scalar value directly — zero allocs for the return.
func (f *FieldKeyExpression) EvaluateScalar(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, error) {
	if msg == nil {
		return f.nilMessageScalar()
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		return nil, fmt.Errorf("EvaluateScalar on repeated field")
	}
	if f.absent(m, fd) {
		return nil, nil
	}
	return scalarToInterface(fd, m.Get(fd))
}

// EvaluateInt64 returns the field value as int64 without boxing.
// Returns (0, false, nil) for nil/unset fields, (val, true, nil) for integer fields.
func (f *FieldKeyExpression) EvaluateInt64(record *FDBStoredRecord[proto.Message], msg proto.Message) (int64, bool, error) {
	if msg == nil {
		return 0, false, nil
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return 0, false, err
	}
	if fd.IsList() {
		// Repeated integer fields must use Evaluate so FanOut and Concatenate
		// preserve their list semantics. Calling Int or Uint on a list value
		// panics in protoreflect.
		return 0, false, nil
	}
	if f.absent(m, fd) {
		return 0, false, nil
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return m.Get(fd).Int(), true, nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		// Java reads a 32-bit unsigned field as a SIGNED Integer (protobuf-java), so a
		// value >= 2^31 is negative there. Record metadata refuses unsigned fields
		// (validateRecordDataTypes, as Java's validateRecords), so this arm only keeps
		// the bytes identical if one is ever read.
		return int64(int32(uint32(m.Get(fd).Uint()))), true, nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(m.Get(fd).Uint()), true, nil
	case protoreflect.EnumKind:
		return int64(m.Get(fd).Enum()), true, nil
	default:
		return 0, false, nil // not an integer field
	}
}

// PackDirect encodes the field value directly into a Packer without boxing into any.
// Returns false if the field is nil/unset or not a directly-packable type.
func (f *FieldKeyExpression) PackDirect(pk *tuple.Packer, _ *FDBStoredRecord[proto.Message], msg proto.Message) bool {
	if msg == nil {
		return false
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil || fd.IsList() {
		return false
	}
	if f.absent(m, fd) {
		return false
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		pk.EncodeInt(m.Get(fd).Int())
		return true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		// Signed, as Java reads it (see the evaluator's arm above).
		pk.EncodeInt(int64(int32(uint32(m.Get(fd).Uint()))))
		return true
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		pk.EncodeInt(int64(m.Get(fd).Uint()))
		return true
	case protoreflect.EnumKind:
		pk.EncodeInt(int64(m.Get(fd).Enum()))
		return true
	case protoreflect.StringKind:
		pk.EncodeString(m.Get(fd).String())
		return true
	default:
		return false
	}
}

// getNullResult is Java's FieldKeyExpression.getNullResult
// (FieldKeyExpression.java:229-240), the result for an absent singular field
// or a nil message:
//   - FanOut → no entries (Collections.emptyList());
//   - Concatenate → one entry holding the empty list under NOT_NULL (the
//     empty NESTED tuple, 0x05 0x00), else one holding the null standin
//     (tuple null 0x00);
//   - None → one entry holding the null standin (tuple null).
//
// The standin packs as a tuple null whichever it is; which one it was is read
// from the key expression where it matters (a unique index, COUNT_NOT_NULL:
// keyContainsNonUniqueNull).
func (f *FieldKeyExpression) getNullResult() [][]any {
	switch f.fanType {
	case FanTypeFanOut:
		return nil
	case FanTypeConcatenate:
		if f.nullStandin == NullStandinNotNull {
			return [][]any{{tuple.Tuple{}}}
		}
		return [][]any{{nil}}
	default:
		return [][]any{{nil}}
	}
}

// evaluateRepeated handles repeated proto fields according to FanType.
func (f *FieldKeyExpression) evaluateRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]any, error) {
	list := m.Get(fd).List()
	count := list.Len()

	switch f.fanType {
	case FanTypeFanOut:
		if count == 0 {
			return nil, nil // Empty list → no index entries (matches Java)
		}
		result := make([][]any, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			result[i] = []any{val}
		}
		return result, nil

	case FanTypeConcatenate:
		// The repeated field's values become a single index-key element encoded as a
		// NESTED TUPLE, matching Java's Tuple.addObject(List) → nested Tuple. It must
		// be a tuple.Tuple, not a raw []any: the FDB tuple packer has no case for a
		// bare []any and would panic ("unencodable element") on every save of a record
		// with this index (index_maintainer pack path).
		nested := make(tuple.Tuple, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			nested[i] = val
		}
		return [][]any{{nested}}, nil

	default: // FanTypeNone
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is repeated with FanType.None", f.fieldName)}
	}
}

// scalarToInterface converts a protoreflect.Value to a Go any suitable for FDB tuples.
// All integer types → int64, all floats → float64 (FDB tuple layer constraint).
func scalarToInterface(fd protoreflect.FieldDescriptor, value protoreflect.Value) (any, error) {
	kind := fd.Kind()
	// For list elements, get the element kind from the field descriptor
	if fd.IsList() {
		kind = fd.Kind()
	}
	switch kind {
	case protoreflect.StringKind:
		return value.String(), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return value.Int(), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		// Signed, as Java reads it (see the evaluator's arm above).
		return int64(int32(uint32(value.Uint()))), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(value.Uint()), nil
	case protoreflect.FloatKind:
		// Must return float32 so FDB tuple encodes as 0x20 (4 bytes).
		// Java protobuf returns java.lang.Float → Tuple.add(Float) → 0x20.
		// value.Float() returns float64; narrowing to float32 matches Java.
		return float32(value.Float()), nil
	case protoreflect.DoubleKind:
		return value.Float(), nil
	case protoreflect.BoolKind:
		return value.Bool(), nil
	case protoreflect.BytesKind:
		return value.Bytes(), nil
	case protoreflect.EnumKind:
		return int64(value.Enum()), nil
	case protoreflect.MessageKind:
		// A tuple_fields.UUID message is a SCALAR tuple element (a 16-byte UUID),
		// not a nested message — Java's TupleFieldsHelper treats it the same way
		// (isTupleField → fromProto → java.util.UUID). Reproduce Java's encoding
		// exactly so a Go-written UUID index entry is byte-identical to Java's.
		if msg := value.Message(); msg.Descriptor().FullName() == uuidProtoFullName {
			return uuidMessageToTuple(msg), nil
		}
		return nil, &KeyExpressionError{Message: fmt.Sprintf("unsupported field type %s for key expression", kind)}
	default:
		return nil, &KeyExpressionError{Message: fmt.Sprintf("unsupported field type %s for key expression", kind)}
	}
}

// uuidMessageToTuple converts a tuple_fields.UUID proto message (sfixed64
// most_significant_bits = field 1, least_significant_bits = field 2) into a
// 16-byte tuple.UUID, MSB||LSB big-endian. This reproduces Java's
// TupleFieldsHelper.fromProto → new java.util.UUID(msb, lsb), which the tuple
// layer encodes as 0x30 + msb(8B BE) + lsb(8B BE) (TupleUtil.UUID_CODE) — so a
// Go-written UUID index entry / primary key is byte-identical to Java's.
func uuidMessageToTuple(msg protoreflect.Message) tuple.UUID {
	fields := msg.Descriptor().Fields()
	msb := msg.Get(fields.ByNumber(1)).Int()
	lsb := msg.Get(fields.ByNumber(2)).Int()
	var u tuple.UUID
	binary.BigEndian.PutUint64(u[0:8], uint64(msb))
	binary.BigEndian.PutUint64(u[8:16], uint64(lsb))
	return u
}

// FieldNames returns the field name accessed by this expression
func (f *FieldKeyExpression) FieldNames() []string {
	return []string{f.fieldName}
}

// ColumnSize returns 1 — a field expression produces a single tuple element.
func (f *FieldKeyExpression) ColumnSize() int {
	return 1
}

// FieldName is Java's FieldKeyExpression.getFieldName().
func (f *FieldKeyExpression) FieldName() string { return f.fieldName }

// FanType is Java's FieldKeyExpression.getFanType().
func (f *FieldKeyExpression) FanType() FanType { return f.fanType }
