package rowstruct

import (
	"fmt"
	"reflect"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TypedOrdinalRow is the exact, positional representation accepted by
// NewOrdinal. executor.PositionalRow implements it without making rowstruct
// depend on the executor package (which is a higher layer).
type TypedOrdinalRow interface {
	values.OrdinalRow
	OrdinalRecordType() *values.RecordType
	OrdinalRowKind() values.OrdinalCarrierKind
}

// OrdinalStruct is the SQL-client view of a record-valued ordinal row. The
// executor keeps nested records as positional rows so further FieldValues can
// address them by baked ordinal; the public boundary exposes the same value as
// api.Struct, just as MessageStruct does for a protobuf-backed record.
type OrdinalStruct struct {
	row TypedOrdinalRow
	typ *values.RecordType
	md  api.StructMetaData
}

// NewOrdinal validates and wraps one exact positional record. It deliberately
// has no width/name recovery: a malformed row is an executor bug, not a struct
// whose missing attributes should quietly read as NULL.
func NewOrdinal(row TypedOrdinalRow) (*OrdinalStruct, error) {
	if row == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "ordinal struct requires a non-nil row")
	}
	typ := row.OrdinalRecordType()
	if typ == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "ordinal struct requires an exact record type")
	}
	// A field-name/width test cannot distinguish the executor's one-slot scalar
	// envelope from an authored anonymous record whose sole field is `_0`.
	// Admit only the producer-stated record kind; the scalar purpose constructor
	// marks its private wrapper explicitly.
	if row.OrdinalRowKind() != values.OrdinalCarrierRecord {
		return nil, api.NewError(api.ErrCodeInternalError, "ordinal value is not a SQL struct record")
	}
	for i, field := range typ.Fields {
		if field.Ordinal != i {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"ordinal struct field %q declares ordinal %d, want %d", field.Name, field.Ordinal, i)
		}
		if field.FieldType == nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"ordinal struct field %q has no exact type", field.Name)
		}
		if _, ok := row.Get(i); !ok {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"ordinal struct row has no slot %d for field %q", i, field.Name)
		}
	}
	if _, extra := row.Get(len(typ.Fields)); extra {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"ordinal struct row has more than %d declared slots", len(typ.Fields))
	}
	structType, err := ordinalStructType(typ)
	if err != nil {
		return nil, err
	}
	return &OrdinalStruct{
		row: row,
		typ: typ,
		md:  NewStructMetaData(structType),
	}, nil
}

// MetaData returns metadata derived from the exact planner RecordType.
func (s *OrdinalStruct) MetaData() api.StructMetaData { return s.md }

// AttributeCount returns the number of positional record fields.
func (s *OrdinalStruct) AttributeCount() int { return len(s.typ.Fields) }

// Attribute reads the one-based attribute and recursively materializes nested
// ordinal/protobuf records and arrays into their public representations.
func (s *OrdinalStruct) Attribute(oneBasedIndex int) (any, error) {
	if oneBasedIndex < 1 || oneBasedIndex > len(s.typ.Fields) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"attribute index %d out of range for struct %q with %d attributes",
			oneBasedIndex, s.typ.RecordName, len(s.typ.Fields))
	}
	ordinal := oneBasedIndex - 1
	v, ok := s.row.Get(ordinal)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"ordinal struct row lost slot %d after construction", ordinal)
	}
	return materializeOrdinalValue(v, s.typ.Fields[ordinal].FieldType)
}

// AttributeByName resolves a unique field case-insensitively. Duplicate exact
// record labels remain ambiguous; ordinal access is always available.
func (s *OrdinalStruct) AttributeByName(name string) (any, error) {
	ordinal := -1
	for i, field := range s.typ.Fields {
		if !equalFoldASCII(field.Name, name) {
			continue
		}
		if ordinal >= 0 {
			return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"struct %q has more than one attribute named %q", s.typ.RecordName, name)
		}
		ordinal = i
	}
	if ordinal < 0 {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"struct %q has no attribute %q", s.typ.RecordName, name)
	}
	return s.Attribute(ordinal + 1)
}

// Attributes returns all attributes in declared order. As with MessageStruct,
// the interface has no error channel, so an individually malformed attribute
// is represented as nil; callers needing the error use Attribute.
func (s *OrdinalStruct) Attributes() []any {
	out := make([]any, len(s.typ.Fields))
	for i := range out {
		v, err := s.Attribute(i + 1)
		if err == nil {
			out[i] = v
		}
	}
	return out
}

func ordinalStructType(record *values.RecordType) (*api.StructType, error) {
	if record == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "nil ordinal record type")
	}
	fields := make([]api.StructField, len(record.Fields))
	for i, field := range record.Fields {
		if field.Ordinal != i || field.FieldType == nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"ordinal record field %q is not exactly typed at slot %d", field.Name, i)
		}
		dt, err := ordinalDataType(field.FieldType)
		if err != nil {
			return nil, fmt.Errorf("ordinal struct field %q: %w", field.Name, err)
		}
		fields[i] = api.NewStructField(field.Name, dt, i)
	}
	return api.NewStructType(publicOrdinalTypeName(record.RecordName, "RECORD"), fields, record.Nullable), nil
}

func ordinalDataType(typ values.Type) (api.DataType, error) {
	if typ == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "missing exact ordinal type")
	}
	nullable := typ.IsNullable()
	switch typ.Code() {
	case values.TypeCodeBoolean:
		return api.NewBooleanType(nullable), nil
	case values.TypeCodeInt:
		return api.NewIntegerType(nullable), nil
	case values.TypeCodeLong:
		return api.NewLongType(nullable), nil
	case values.TypeCodeFloat:
		return api.NewFloatType(nullable), nil
	case values.TypeCodeDouble:
		return api.NewDoubleType(nullable), nil
	case values.TypeCodeString:
		return api.NewStringType(nullable), nil
	case values.TypeCodeBytes:
		return api.NewBytesType(nullable), nil
	case values.TypeCodeVersion:
		return api.NewVersionType(nullable), nil
	case values.TypeCodeUuid:
		return api.NewUUIDType(nullable), nil
	case values.TypeCodeDate:
		return api.NewDateType(nullable), nil
	case values.TypeCodeTimestamp:
		return api.NewTimestampType(nullable), nil
	case values.TypeCodeNull:
		return api.NewNullType(), nil
	case values.TypeCodeRecord:
		record, ok := typ.(*values.RecordType)
		if !ok {
			return nil, api.NewError(api.ErrCodeInternalError, "record code has no exact record type")
		}
		return ordinalStructType(record)
	case values.TypeCodeArray:
		array, ok := typ.(*values.ArrayType)
		if !ok || array.ElementType == nil {
			return nil, api.NewError(api.ErrCodeInternalError, "array has no exact element type")
		}
		element, err := ordinalDataType(array.ElementType)
		if err != nil {
			return nil, err
		}
		return api.NewArrayType(element, nullable), nil
	case values.TypeCodeEnum:
		enum, ok := typ.(*values.EnumType)
		if !ok || enum.EnumName == "" || len(enum.Values) == 0 {
			return nil, api.NewError(api.ErrCodeInternalError, "enum has no exact public declaration")
		}
		members := make([]api.EnumValue, len(enum.Values))
		for i, member := range enum.Values {
			members[i] = api.NewEnumValue(member.Name, int(member.Number))
		}
		return api.NewEnumType(publicOrdinalTypeName(enum.EnumName, "ENUM"), members, nullable), nil
	default:
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"ordinal type %s has no exact public data type", typ.Code())
	}
}

func publicOrdinalTypeName(name, anonymous string) string {
	if name == "" {
		return anonymous
	}
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

func materializeOrdinalValue(v any, expected values.Type) (any, error) {
	if v == nil {
		return nil, nil
	}
	if expected == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "ordinal attribute has no exact type")
	}
	switch expected.Code() {
	case values.TypeCodeRecord:
		expectedRecord, ok := expected.(*values.RecordType)
		if !ok {
			return nil, api.NewError(api.ErrCodeInternalError, "record attribute has no exact record type")
		}
		if row, ok := v.(TypedOrdinalRow); ok {
			actual := row.OrdinalRecordType()
			if actual == nil || !actual.Equals(expectedRecord) {
				return nil, api.NewErrorf(api.ErrCodeInternalError,
					"ordinal struct attribute type %v disagrees with %v", actual, expectedRecord)
			}
			return NewOrdinal(row)
		}
		if msg, ok := v.(protoreflect.ProtoMessage); ok {
			return New(msg.ProtoReflect())
		}
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"record attribute carries %T, want an ordinal row or protobuf message", v)
	case values.TypeCodeArray:
		array, ok := expected.(*values.ArrayType)
		if !ok || array.ElementType == nil {
			return nil, api.NewError(api.ErrCodeInternalError, "array attribute has no exact element type")
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"array attribute carries %T, want a slice or array", v)
		}
		out := make([]any, rv.Len())
		for i := range out {
			materialized, err := materializeOrdinalValue(rv.Index(i).Interface(), array.ElementType)
			if err != nil {
				return nil, fmt.Errorf("array element %d: %w", i, err)
			}
			out[i] = materialized
		}
		return out, nil
	case values.TypeCodeUuid:
		switch id := v.(type) {
		case [16]byte:
			return tuple.UUID(id).String(), nil
		case tuple.UUID:
			return id.String(), nil
		}
	}
	return v, nil
}

var _ api.Struct = (*OrdinalStruct)(nil)
