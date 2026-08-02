package functions

import (
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
		// our cross-engine plandiff harness's expectation.
		if isUUIDMessageField(fd) {
			return uuidProtoMessageToString(v.Message())
		}
		// A NullableArrayWrapper column reads back as the []any it stores.
		if inner, wrapped, ok := values.EffectiveListField(fd); ok && wrapped {
			list := v.Message().Get(inner).List()
			out := make([]any, list.Len())
			for i := 0; i < list.Len(); i++ {
				out[i] = ProtoValueToDriver(inner, list.Get(i))
			}
			return out
		}
		// Other MessageKind fields are not supported as SQL columns.
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
//
// An ARRAY column takes the evaluated array literal — the []any an
// ArrayConstructorValue produces (Java's LightArrayConstructorValue
// evals to a List) — and converts each element through the same
// scalar lanes, mirroring Java's MessageHelpers.coerceArray
// element-wise coercion. A NON-nullable array is a flat repeated
// field; a NULLABLE array is stored through the NullableArrayWrapper
// (`message W { repeated E values = 1; }`), where a present wrapper
// with an empty list keeps [] distinct from NULL — Java's exact wire
// shape.
func ConvertToProtoValue(fd protoreflect.FieldDescriptor, val any) (protoreflect.Value, error) {
	if inner, wrapped, ok := values.EffectiveListField(fd); ok {
		elems, elemsOK := val.([]any)
		if !elemsOK {
			// A scalar (or anything that is not an evaluated array
			// literal) into an ARRAY column: same verbatim 22000 the
			// scalar lanes end in.
			return protoreflect.Value{}, cannotConvertTypeError()
		}
		if wrapped {
			// NULLABLE array: build the NullableArrayWrapper message —
			// a PRESENT wrapper with an empty list keeps [] distinct
			// from NULL (the absent field), exactly Java's stored shape.
			wrapperMsg, list := values.NewWrappedArrayMessage(fd)
			if err := appendArrayElements(list, inner, elems); err != nil {
				return protoreflect.Value{}, err
			}
			return protoreflect.ValueOfMessage(wrapperMsg), nil
		}
		parent := dynamicpb.NewMessage(fd.ContainingMessage())
		lv := parent.NewField(fd)
		if err := appendArrayElements(lv.List(), inner, elems); err != nil {
			return protoreflect.Value{}, err
		}
		return lv, nil
	}
	return convertScalarProtoValue(fd, val)
}

// appendArrayElements converts each evaluated array-literal element through
// the scalar lanes against the (effective) repeated field descriptor.
func appendArrayElements(list protoreflect.List, elemFD protoreflect.FieldDescriptor, elems []any) error {
	for _, e := range elems {
		if e == nil {
			// Java forbids NULL elements in collections
			// (MessageHelpers.coerceArray, SemanticException
			// UNSUPPORTED — surfaces as an internal error;
			// tracked upstream as fdb-record-layer#3646).
			return api.NewErrorf(api.ErrCodeInternalError,
				"NULL as elements of a collection are currently not supported")
		}
		pv, err := convertScalarProtoValue(elemFD, e)
		if err != nil {
			return err
		}
		list.Append(pv)
	}
	return nil
}

func convertScalarProtoValue(fd protoreflect.FieldDescriptor, val any) (protoreflect.Value, error) {
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
			// No range check, deliberately: Java's PromoteValue.LONG_TO_FLOAT
			// is the plain widening cast Float.valueOf((Long)in) —
			// precision-lossy above 2^24, never ±Inf (MaxInt64 ≈ 9.2e18 <
			// MaxFloat32 ≈ 3.4e38, so overflow is unreachable from an
			// integer; the float64 arm above range-checks because float64
			// CAN exceed float32 range).
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
		// (most_significant_bits, least_significant_bits). Convert here
		// at the proto-write boundary from either carrier: the canonical
		// 36-char string, or the neutral [16]byte the Cascades value
		// layer works with (RFC-162 — CAST('…' AS UUID) folded at plan
		// time arrives as [16]byte). Mirrors the executor's
		// goToProtoValue UUID arm so a UUID written via INSERT … VALUES
		// is byte-identical to one written via UPDATE / INSERT … SELECT.
		if isUUIDMessageField(fd) {
			switch v := val.(type) {
			case string:
				return uuidStringToProtoMessage(fd, v)
			case [16]byte:
				return uuidStringToProtoMessage(fd, uuid.UUID(v).String())
			}
		}
		// A STRUCT cell. The typed row constructor the DML target-type
		// push-down builds (Java: RecordConstructorValue, typed by
		// ExpressionVisitor.parseRecordField's recursion,
		// ExpressionVisitor.java:967-1008) evaluates to a map keyed by the
		// TARGET field names — the names the push-down adopted from the
		// column's Type.Record — so the nested message is built by walking
		// the TARGET descriptor's fields, which is Java's own pairing:
		// RecordConstructorValue.eval sets child i on
		// `fieldDescriptors.get(i)` (RecordConstructorValue.java:113-139).
		if m, ok := val.(map[string]any); ok {
			return structToProtoValue(fd.Message(), m)
		}
	}
	return protoreflect.Value{}, cannotConvertTypeError()
}

// structToProtoValue builds the nested dynamic message for a STRUCT cell
// from the evaluated record constructor's field map.
//
// A field whose value is NULL is left ABSENT rather than set — Java's
// RecordConstructorValue.eval takes the same branch (a null child is not
// set on the builder at all, RecordConstructorValue.java:135), and absence
// is what makes a nullable struct field read back as NULL. A NULL at a
// NON-nullable field is rejected HERE and not one level up, because here is
// where Java rejects it too: eval's `Verify.verify(fieldType.isNullable(),
// "Cannot set a non-nullable field to the NULL value")` (:135) sits at the
// message build, which is the one point every writer reaches — INSERT …
// VALUES, INSERT … SELECT and UPDATE alike. The visitor-level gate
// (ExpressionVisitor.java:1068) covers only the column a statement did not
// mention at all.
//
// Each field is converted through ConvertToProtoValue, not the scalar
// lanes, so a struct field that is itself an ARRAY takes the list arm —
// including the NullableArrayWrapper build, which Java performs at this
// same point in eval (RecordConstructorValue.java:127-131).
func structToProtoValue(md protoreflect.MessageDescriptor, fields map[string]any) (protoreflect.Value, error) {
	pv, err := values.BuildStructMessage(md, fields, ConvertToProtoValue)
	if err != nil {
		return protoreflect.Value{}, structBuildError(err)
	}
	return pv, nil
}

// structBuildError states the shared builder's typed failures in the SQL
// layer's vocabulary: a NULL at a non-nullable field is the same 23502 the
// top-level column gate reports, and a name the target struct does not
// declare is unreachable from the DML push-down (every name is adopted FROM
// the target descriptor), so it is loud rather than a silently dropped value.
func structBuildError(err error) error {
	var nn *values.NonNullableFieldError
	if errors.As(err, &nn) {
		return api.NewErrorf(api.ErrCodeNotNullViolation, "%s", nn.Error())
	}
	var ud *values.UndeclaredStructFieldError
	if errors.As(err, &ud) {
		return api.NewErrorf(api.ErrCodeInternalError, "%s", ud.Error())
	}
	return err
}

// cannotConvertTypeError is the Java-verbatim 22000: 'A value cannot
// be assigned to a variable because the type of the value does not
// match the type of the variable and cannot be promoted to the type
// of the variable.' — the same SemanticException Java emits at
// INSERT / UPDATE type mismatch.
func cannotConvertTypeError() *api.Error {
	return api.NewErrorf(api.ErrCodeCannotConvertType,
		"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
}
