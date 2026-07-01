package executor

import (
	"encoding/binary"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	Datum any
	// Positional is the RFC-173 ordinal-model sibling of Datum: the same row as
	// a typed PositionalRow (field values indexed by ordinal). Non-nil marks the
	// row as being on the NON-JOIN FRONTIER (scans, covering scans, projection/
	// map over the frontier emit it; join producers mergeRows/qualifyOuterRow do
	// NOT), and since Slice 1 it is what FieldValue resolution READS there —
	// authoritative, by ordinal, loud on a miss. The name-keyed Datum is still
	// emitted alongside for coexistence (downstream name-model consumers, final
	// materialization) until Slice 4 retires it; a shadow test pins that the two
	// mirror each other field-for-field.
	Positional *PositionalRow
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
		Positional: protoToPositional(rec.Record),
		Record:     rec,
		PrimaryKey: rec.PrimaryKey,
	}
}

// positionalTypeCache caches the row-invariant PositionalRow.Type per message
// descriptor. The RecordType depends only on the descriptor (field names in
// declaration order), never the row, and rebuilding it per scanned row made
// protoToPositional cost more than the sparse protoToMap itself
// (BenchmarkProtoToPositional_Order). The cached type is shared across rows and
// goroutines — read-only after construction (FieldIndex / shadow reads only).
// Keyed by the descriptor (a per-message-type singleton for generated code;
// dynamicpb descriptors miss and rebuild, which is correct, just uncached). A
// racy duplicate Store is harmless: both values are structurally equal.
var positionalTypeCache sync.Map // protoreflect.MessageDescriptor -> *values.RecordType

// protoToPositional is the RFC-173 ordinal-model counterpart of protoToMap: it
// builds a PositionalRow from a proto message, one slot per descriptor field in
// declaration order (the field's ordinal), with an UPPER-cased field name and a
// dark UnknownType (type refinement comes with the later slices). An unset field
// is a nil slot — matching protoToMap omitting the key (SQL NULL) — so the
// positional row and the map agree field-for-field (pinned by the shadow test).
// Since Slice 1 this row is what the non-join frontier RESOLVES against; the
// name-keyed map is still emitted for coexistence (retired in Slice 4).
func protoToPositional(msg proto.Message) *PositionalRow {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	n := fields.Len()
	var rt *values.RecordType
	if v, ok := positionalTypeCache.Load(desc); ok {
		rt = v.(*values.RecordType)
	} else {
		rtFields := make([]values.Field, n)
		for i := 0; i < n; i++ {
			rtFields[i] = values.Field{Name: strings.ToUpper(string(fields.Get(i).Name())), FieldType: values.UnknownType, Ordinal: i}
		}
		rt = values.NewRecordType("", false, rtFields)
		positionalTypeCache.Store(desc, rt)
	}
	slots := make([]any, n)
	for i := 0; i < n; i++ {
		fd := fields.Get(i)
		if refl.Has(fd) {
			slots[i] = protoFieldToGo(fd, refl.Get(fd))
		}
	}
	return &PositionalRow{Type: rt, Slots: slots}
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
