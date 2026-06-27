package functions

import (
	"database/sql/driver"
	"encoding/binary"
	"math"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/relational/api"
)

// UUIDProtoMessageName is the fully-qualified name of the
// tuple_fields.UUID proto message that fdb-relational uses to store
// UUID column values (matches Java's TupleFieldsProto.UUID).
const UUIDProtoMessageName = "com.apple.foundationdb.record.UUID"

// isUUIDMessageField reports whether fd is a UUID-typed field — a
// MessageKind whose message-descriptor's full name matches the
// tuple_fields.UUID type. Used by ConvertToProtoValue / ProtoValueToDriver
// / jdbcTypeNameForFD to recognise UUID fields without taking a
// dependency on the gen package's typed message.
func isUUIDMessageField(fd protoreflect.FieldDescriptor) bool {
	if fd == nil || fd.Kind() != protoreflect.MessageKind {
		return false
	}
	if msg := fd.Message(); msg != nil {
		return string(msg.FullName()) == UUIDProtoMessageName
	}
	return false
}

// uuidStringToProtoMessage parses a canonical UUID string and builds
// a dynamicpb message matching the tuple_fields.UUID descriptor.
// Returns the Go uuid.UUID value too, for callers that want both.
func uuidStringToProtoMessage(fd protoreflect.FieldDescriptor, s string) (protoreflect.Value, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInvalidCast,
			"cannot CAST %q to UUID: %v", s, err)
	}
	msgDesc := fd.Message()
	dynMsg := dynamicpb.NewMessage(msgDesc)
	mostFD := msgDesc.Fields().ByName("most_significant_bits")
	leastFD := msgDesc.Fields().ByName("least_significant_bits")
	if mostFD == nil || leastFD == nil {
		return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInternalError,
			"UUID message descriptor missing most/least_significant_bits fields")
	}
	dynMsg.Set(mostFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(u[0:8]))))   //nolint:gosec
	dynMsg.Set(leastFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(u[8:16])))) //nolint:gosec
	return protoreflect.ValueOfMessage(dynMsg), nil
}

// uuidProtoMessageToString reads a UUID proto message and returns the
// canonical 36-char string form. The message is identified by its
// most/least_significant_bits fields; other shapes panic.
func uuidProtoMessageToString(msg protoreflect.Message) string {
	mostFD := msg.Descriptor().Fields().ByName("most_significant_bits")
	leastFD := msg.Descriptor().Fields().ByName("least_significant_bits")
	most := uint64(msg.Get(mostFD).Int())   //nolint:gosec
	least := uint64(msg.Get(leastFD).Int()) //nolint:gosec
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[0:8], most)
	binary.BigEndian.PutUint64(u[8:16], least)
	return u.String()
}

// LiteralMatchesPKKind reports whether a driver-value literal is a
// safe tuple element for a column of the given proto kind. Only
// numeric / string / bytes kinds are in scope — booleans and enums
// can be columns in theory but are unusual and left to the scan
// path for now.
func LiteralMatchesPKKind(val any, kind protoreflect.Kind) bool {
	switch kind {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		switch val.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		}
		return false
	case protoreflect.StringKind:
		_, ok := val.(string)
		return ok
	case protoreflect.BytesKind:
		_, ok := val.([]byte)
		return ok
	}
	return false
}

// ProtoValueToDriver maps a protoreflect.Value (read off a record)
// into a driver.Value for SQL-level consumption. Widens all integer
// kinds to int64 so the SQL evaluator doesn't need per-kind fan-out.
func ProtoValueToDriver(fd protoreflect.FieldDescriptor, v protoreflect.Value) driver.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint()) //nolint:gosec
	case protoreflect.FloatKind:
		return float64(v.Float())
	case protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return []byte(v.Bytes())
	case protoreflect.MessageKind:
		// UUID columns return the canonical 36-char string form for
		// SQL consumption — matches Java's getString(uuidColumn) and
		// our cross-engine plandiff harness's expectation. Other
		// MessageKind fields are not supported as SQL columns.
		if isUUIDMessageField(fd) {
			return uuidProtoMessageToString(v.Message())
		}
		return v.Interface()
	default:
		return v.Interface()
	}
}

// ConvertToProtoValue converts a SQL-level driver.Value (int64,
// float64, string, bool, []byte) to a protoreflect.Value matching
// the field descriptor's kind. Range checks match Java's CastValue
// behaviour (LONG_TO_INT / DOUBLE_TO_LONG / DOUBLE_TO_FLOAT etc.);
// NaN/Inf in float columns are rejected as silent-data-corruption
// vectors.
func ConvertToProtoValue(fd protoreflect.FieldDescriptor, val any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		switch v := val.(type) {
		case bool:
			return protoreflect.ValueOfBool(v), nil
		case int64:
			return protoreflect.ValueOfBool(v != 0), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		if v, ok := val.(int64); ok {
			// Java CastValue.LONG_TO_INT range-checks before narrowing. Go
			// used to silently wrap via int32() which could turn an
			// INSERT of 2147483648 into -2147483648 — a value-corrupting
			// divergence. Reject cleanly.
			if v < math.MinInt32 || v > math.MaxInt32 {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
					"value %d out of range for %s column %q", v, fd.Kind(), fd.Name())
			}
			return protoreflect.ValueOfInt32(int32(v)), nil
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfInt64(v), nil
		}
		// A float64 (an AVG result, or a DOUBLE literal/expression) into a
		// BIGINT column is NOT coerced: DOUBLE→LONG has no edge in Java's
		// promotion lattice, so it falls through to the verbatim 22000
		// SemanticException below, matching Java's plan-time PromoteValue
		// rejection. (The former whole-valued-float→int64 coercion silently
		// accepted DOUBLE→BIGINT, a divergence; aggregate INSERT…SELECT now
		// rejects at plan time — see checkInsertSelectPromotable.)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		if v, ok := val.(int64); ok {
			if v < 0 || v > math.MaxUint32 {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
					"value %d out of range for %s column %q", v, fd.Kind(), fd.Name())
			}
			return protoreflect.ValueOfUint32(uint32(v)), nil
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if v, ok := val.(int64); ok {
			if v < 0 {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
					"negative value %d cannot be stored in unsigned %s column %q", v, fd.Kind(), fd.Name())
			}
			return protoreflect.ValueOfUint64(uint64(v)), nil
		}
	case protoreflect.FloatKind:
		switch v := val.(type) {
		case float64:
			// Java CastValue.DOUBLE_TO_FLOAT range-checks against ±MaxFloat
			// and rejects NaN/Inf. Reject here too — silent +Inf from
			// overflow is a value corruption.
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInvalidParameter,
					"cannot store NaN or Infinity in FLOAT column %q", fd.Name())
			}
			if v > math.MaxFloat32 || v < -math.MaxFloat32 {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
					"value %v out of range for FLOAT column %q", v, fd.Name())
			}
			return protoreflect.ValueOfFloat32(float32(v)), nil
		case int64:
			return protoreflect.ValueOfFloat32(float32(v)), nil
		}
	case protoreflect.DoubleKind:
		switch v := val.(type) {
		case float64:
			// NaN/Inf are silent data corruption vectors — a later read
			// via ProtoValueToDriver would pass them through and confuse
			// comparisons / aggregates.
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInvalidParameter,
					"cannot store NaN or Infinity in DOUBLE column %q", fd.Name())
			}
			return protoreflect.ValueOfFloat64(v), nil
		case int64:
			return protoreflect.ValueOfFloat64(float64(v)), nil
		}
	case protoreflect.StringKind:
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfString(v), nil
		}
		if v, ok := val.(time.Time); ok {
			return protoreflect.ValueOfString(FormatTimestamp(v)), nil
		}
	case protoreflect.BytesKind:
		if v, ok := val.([]byte); ok {
			return protoreflect.ValueOfBytes(v), nil
		}
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfBytes([]byte(v)), nil
		}
	case protoreflect.MessageKind:
		// UUID columns are stored as the tuple_fields.UUID message
		// (most_significant_bits, least_significant_bits). The SQL
		// layer carries the value as the canonical 36-char string;
		// convert here at the proto-write boundary.
		if isUUIDMessageField(fd) {
			if s, ok := val.(string); ok {
				return uuidStringToProtoMessage(fd, s)
			}
		}
	}
	// Java verbatim: 'A value cannot be assigned to a variable because
	// the type of the value does not match the type of the variable
	// and cannot be promoted to the type of the variable.' — same
	// SemanticException Java emits at INSERT / UPDATE type mismatch.
	//
	return protoreflect.Value{}, api.NewErrorf(api.ErrCodeCannotConvertType,
		"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
}
