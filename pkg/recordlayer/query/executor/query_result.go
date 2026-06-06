package executor

import (
	"encoding/binary"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
	// UUID columns are stored as the tuple_fields.UUID message; the SQL layer
	// surfaces them as the canonical 36-char string (matches Java's
	// getString(uuidColumn)), not the raw proto message.
	if fd.Kind() == protoreflect.MessageKind {
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return uuidMessageToString(v.Message())
		}
	}
	return scalarProtoToGo(fd.Kind(), v)
}

// uuidMessageToString renders a tuple_fields.UUID message (most/least
// _significant_bits) as the canonical 36-char lower-case UUID string.
func uuidMessageToString(msg protoreflect.Message) string {
	fields := msg.Descriptor().Fields()
	mostFD := fields.ByName("most_significant_bits")
	leastFD := fields.ByName("least_significant_bits")
	if mostFD == nil || leastFD == nil {
		return ""
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(msg.Get(mostFD).Int()))   //nolint:gosec
	binary.BigEndian.PutUint64(b[8:16], uint64(msg.Get(leastFD).Int())) //nolint:gosec
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
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
