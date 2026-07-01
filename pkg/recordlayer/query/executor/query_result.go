package executor

import (
	"encoding/binary"
	"strings"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
)

// uuidProtoMessageName is the fully-qualified tuple_fields.UUID message that
// fdb-relational stores UUID column values in. (Canonical copy lives in
// pkg/relational/core/functions/proto_value.go; duplicated here because the
// record-layer executor cannot depend on the relational layer.)
const uuidProtoMessageName = "com.apple.foundationdb.record.UUID"

// QueryResult is the row type flowing through plan execution cursors.
// Wraps a datum (the computed/flowed row), an optional stored record
// (when the row originated from a scan), and an optional primary key.
// Mirrors Java's QueryResult.
type QueryResult struct {
	Datum      any
	Record     *recordlayer.FDBStoredRecord[proto.Message]
	PrimaryKey tuple.Tuple
	// Complete marks a computed/synthetic row whose Datum key set is
	// authoritative — every legal column is present (nil-valued for SQL NULL),
	// with no proto-style optional-field omissions. Set by aggregate output
	// (finalizeGroup/emptyScalarResult). Consumers use it to enable the RFC-048
	// W1 strict unresolved-reference check: against such a row, a referenced
	// name that is absent is a bug, not a NULL. Raw stored-record rows
	// (FromStoredRecord) leave it false, because they legitimately omit unset
	// optional fields.
	Complete bool
}

// FromStoredRecord builds a QueryResult from a stored record. The
// datum is set to a map[string]any extracted from the proto message's
// fields, keyed by UPPER-case field name (matching the identifier
// folding convention).
func FromStoredRecord(rec *recordlayer.FDBStoredRecord[proto.Message]) QueryResult {
	datum := protoToMap(rec.Record)
	return QueryResult{
		Datum:      datum,
		Record:     rec,
		PrimaryKey: rec.PrimaryKey,
	}
}

// protoToMap converts a proto.Message to map[string]any with
// UPPER-case keys. Only set fields are included; unset fields are
// omitted (NULL semantics — FieldValue.Evaluate returns nil for
// missing keys).
//
// An EMPTY repeated field is omitted too (protoreflect's Has()
// reports false for an empty list) and so reads back as SQL NULL.
// Go writes a plain repeated field with no nullable-array wrapper,
// so a NULL array and an empty array are wire-indistinguishable;
// both materialize as NULL here. CARDINALITY of such a column is
// therefore NULL, not 0, for an empty/unset array — an instance of
// the RFC-143 §3a nullable-array-wrapper divergence (Java's wrapper
// distinguishes the two), out of scope for the Phase 1 function. The
// function itself is correct: nil array → nil, populated array →
// len.
func protoToMap(msg proto.Message) map[string]any {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	m := make(map[string]any, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !refl.Has(fd) {
			continue
		}
		key := strings.ToUpper(string(fd.Name()))
		m[key] = protoFieldToGo(fd, refl.Get(fd))
	}
	return m
}

// protoFieldToGo converts a protoreflect.Value to a native Go value
// suitable for Value.Evaluate consumption.
func protoFieldToGo(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if fd.IsList() {
		list := v.List()
		out := make([]any, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = scalarProtoToGo(fd.Kind(), list.Get(i))
		}
		return out
	}
	if fd.IsMap() {
		return v.Interface()
	}
	// UUID columns are stored as the tuple_fields.UUID message. Surface the
	// value as a neutral 16-byte array ([16]byte, msb‖lsb big-endian —
	// matching Java's java.util.UUID and the tuple.UUID wire layout) rather
	// than the canonical string. This lets the filter path compare
	// [16]byte==[16]byte (predicates.cmpAny) and the index-scan-range packer
	// seek the exact 0x30 entry; the [16]byte → canonical-string conversion
	// happens once, at the result-materialization boundary. Returning a string
	// here instead would make `WHERE v = '<uuid>'` and INL UUID join keys pack
	// a 0x02 string that never matches the 0x30 index entry.
	if fd.Kind() == protoreflect.MessageKind {
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return uuidMessageToBytes(v.Message())
		}
	}
	return scalarProtoToGo(fd.Kind(), v)
}

// uuidMessageToBytes reads a tuple_fields.UUID message (most/least
// _significant_bits) into a neutral 16-byte array, msb‖lsb big-endian — the
// same layout tuple.UUID and Java's java.util.UUID use.
func uuidMessageToBytes(msg protoreflect.Message) [16]byte {
	fields := msg.Descriptor().Fields()
	mostFD := fields.ByName("most_significant_bits")
	leastFD := fields.ByName("least_significant_bits")
	var b [16]byte
	if mostFD == nil || leastFD == nil {
		return b
	}
	binary.BigEndian.PutUint64(b[0:8], uint64(msg.Get(mostFD).Int()))   //nolint:gosec
	binary.BigEndian.PutUint64(b[8:16], uint64(msg.Get(leastFD).Int())) //nolint:gosec
	return b
}

func scalarProtoToGo(kind protoreflect.Kind, v protoreflect.Value) any {
	switch kind {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return int64(v.Int())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return int64(v.Uint())
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint())
	case protoreflect.FloatKind:
		return v.Float()
	case protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return v.Bytes()
	case protoreflect.EnumKind:
		return int64(v.Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return v.Message().Interface()
	default:
		return v.Interface()
	}
}
