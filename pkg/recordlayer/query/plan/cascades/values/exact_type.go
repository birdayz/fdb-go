package values

import (
	"encoding/binary"
	"hash/fnv"
)

// ExactTypeHandle is an immutable read view of a checked type snapshot.
// Type returns a fresh ordinary graph and CanonicalBytes returns a defensive
// copy, so no caller can mutate the identity held by a QOV or memo boundary.
type ExactTypeHandle interface {
	Type() Type
	CanonicalBytes() []byte
	RelationInner() (ExactTypeHandle, bool)
	isExactTypeHandleView()
}

type exactField struct {
	name    string
	ordinal int
	typ     *exactType
}

type exactType struct {
	code       TypeCode
	nullable   bool
	anyRecord  bool
	name       string
	fields     []exactField
	element    *exactType
	enumValues []EnumValue
	canonical  []byte
	hash       uint64
}

func (*exactType) isExactTypeHandleView() {}

func (e *exactType) Type() Type {
	if e == nil {
		return nil
	}
	return e.thaw()
}

func (e *exactType) CanonicalBytes() []byte {
	if e == nil {
		return nil
	}
	return append([]byte(nil), e.canonical...)
}

func (e *exactType) RelationInner() (ExactTypeHandle, bool) {
	if e == nil || e.code != TypeCodeRelation || e.element == nil {
		return nil, false
	}
	return e.element, true
}

func (e *exactType) thaw() Type {
	switch e.code {
	case TypeCodeRecord:
		if e.anyRecord {
			return anyRecordType{nullable: e.nullable}
		}
		fields := make([]Field, len(e.fields))
		for i, field := range e.fields {
			fields[i] = Field{
				Name:      field.name,
				Ordinal:   field.ordinal,
				FieldType: field.typ.thaw(),
			}
		}
		// Do not route through NewRecordType: exact snapshots intentionally
		// admit duplicate names because ordinal access remains unambiguous.
		return &RecordType{RecordName: e.name, Nullable: e.nullable, Fields: fields}
	case TypeCodeArray:
		return &ArrayType{Nullable: e.nullable, ElementType: e.element.thaw()}
	case TypeCodeEnum:
		return &EnumType{
			EnumName: e.name,
			Nullable: e.nullable,
			Values:   append([]EnumValue(nil), e.enumValues...),
		}
	case TypeCodeRelation:
		return &RelationType{InnerType: e.element.thaw()}
	default:
		return &PrimitiveType{TypeCode: e.code, Nullable: e.nullable}
	}
}

// SnapshotExactType checks and freezes an ordinary Type graph.
func SnapshotExactType(typ Type) (ExactTypeHandle, error) {
	exact, err := snapshotExactType(typ, "type", make(map[any]struct{}))
	if err != nil {
		return nil, err
	}
	return exact, nil
}

// ExactRelationOf snapshots object and wraps it in exactly one RELATION layer.
func ExactRelationOf(object Type) (ExactTypeHandle, error) {
	handle, err := SnapshotExactType(object)
	if err != nil {
		return nil, err
	}
	inner := handle.(*exactType)
	relation := &exactType{code: TypeCodeRelation, element: inner}
	relation.finishCanonical()
	return relation, nil
}

// AsExactTypeHandle exact-recognizes the package-owned immutable handle. An
// embedded interface or a nil embedded view is not admitted.
func AsExactTypeHandle(value any) (ExactTypeHandle, bool) {
	exact, ok := value.(*exactType)
	if !ok || exact == nil {
		return nil, false
	}
	return exact, true
}

func snapshotExactType(typ Type, path string, active map[any]struct{}) (*exactType, error) {
	if typ == nil {
		return nil, resolutionError(TypeNil, path, "type is nil")
	}

	var identity any
	switch typed := typ.(type) {
	case anyRecordType:
		identity = typed
	case *PrimitiveType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "primitive type is typed nil")
		}
		identity = typed
	case *RecordType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "record type is typed nil")
		}
		identity = typed
	case *ArrayType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "array type is typed nil")
		}
		identity = typed
	case *EnumType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "enum type is typed nil")
		}
		identity = typed
	case *RelationType:
		if typed == nil {
			return nil, resolutionError(TypeTypedNil, path, "relation type is typed nil")
		}
		identity = typed
	default:
		return nil, resolutionError(TypeMalformedCode, path, "unsupported concrete Type implementation")
	}
	if _, cyclic := active[identity]; cyclic {
		return nil, resolutionError(TypeCycle, path, "type graph contains a cycle")
	}
	active[identity] = struct{}{}
	defer delete(active, identity)

	var exact *exactType
	switch typed := typ.(type) {
	case anyRecordType:
		exact = &exactType{code: TypeCodeRecord, nullable: typed.nullable, anyRecord: true}
	case *PrimitiveType:
		switch typed.TypeCode {
		case TypeCodeUnknown, TypeCodeAny, TypeCodeNone:
			return nil, resolutionError(TypeUnresolved, path, "placeholder type is not exact")
		case TypeCodeNull, TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
			TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBytes,
			TypeCodeVersion, TypeCodeUuid, TypeCodeDate, TypeCodeTimestamp:
			// Admitted below.
		default:
			return nil, resolutionError(TypeMalformedCode, path, "primitive carries a structured or unknown code")
		}
		exact = &exactType{code: typed.TypeCode, nullable: typed.Nullable}
	case *RecordType:
		fields := make([]exactField, len(typed.Fields))
		for i, field := range typed.Fields {
			if field.Ordinal != i {
				return nil, resolutionError(TypeMalformedOrdinal, path, "record field ordinal does not equal its position")
			}
			fieldType, err := snapshotExactType(field.FieldType, path+".field["+uitoa(uint64(i))+"]", active)
			if err != nil {
				return nil, err
			}
			fields[i] = exactField{name: field.Name, ordinal: i, typ: fieldType}
		}
		exact = &exactType{
			code:     TypeCodeRecord,
			nullable: typed.Nullable,
			name:     typed.RecordName,
			fields:   fields,
		}
	case *ArrayType:
		if typed.ElementType == nil {
			return nil, resolutionError(TypeErased, path, "array element type is erased")
		}
		element, err := snapshotExactType(typed.ElementType, path+".element", active)
		if err != nil {
			return nil, err
		}
		exact = &exactType{code: TypeCodeArray, nullable: typed.Nullable, element: element}
	case *EnumType:
		values := append([]EnumValue(nil), typed.Values...)
		seenNames := make(map[string]struct{}, len(values))
		seenNumbers := make(map[int32]struct{}, len(values))
		for _, value := range values {
			if _, duplicate := seenNames[value.Name]; duplicate {
				return nil, resolutionError(TypeMalformedCode, path, "duplicate enum value name")
			}
			if _, duplicate := seenNumbers[value.Number]; duplicate {
				return nil, resolutionError(TypeMalformedCode, path, "duplicate enum value number")
			}
			seenNames[value.Name] = struct{}{}
			seenNumbers[value.Number] = struct{}{}
		}
		exact = &exactType{code: TypeCodeEnum, nullable: typed.Nullable, name: typed.EnumName, enumValues: values}
	case *RelationType:
		if typed.InnerType == nil {
			return nil, resolutionError(TypeErased, path, "relation inner type is erased")
		}
		inner, err := snapshotExactType(typed.InnerType, path+".inner", active)
		if err != nil {
			return nil, err
		}
		exact = &exactType{code: TypeCodeRelation, element: inner}
	}
	exact.finishCanonical()
	return exact, nil
}

func (e *exactType) finishCanonical() {
	encoded := make([]byte, 0, 32)
	encoded = binary.AppendUvarint(encoded, uint64(e.code))
	if e.nullable {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	// TypeCodeRecord alone cannot distinguish Java's erased AnyRecord from
	// the concrete zero-field unit record. Keep that concrete-kind bit in the
	// immutable identity so equality and hashing cannot collapse the two.
	if e.anyRecord {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	encoded = appendCanonicalString(encoded, e.name)
	encoded = binary.AppendUvarint(encoded, uint64(len(e.fields)))
	for _, field := range e.fields {
		encoded = appendCanonicalString(encoded, field.name)
		encoded = binary.AppendVarint(encoded, int64(field.ordinal))
		encoded = appendCanonicalBytes(encoded, field.typ.canonical)
	}
	if e.element == nil {
		encoded = append(encoded, 0)
	} else {
		encoded = append(encoded, 1)
		encoded = appendCanonicalBytes(encoded, e.element.canonical)
	}
	encoded = binary.AppendUvarint(encoded, uint64(len(e.enumValues)))
	for _, value := range e.enumValues {
		encoded = appendCanonicalString(encoded, value.Name)
		encoded = binary.AppendVarint(encoded, int64(value.Number))
	}
	e.canonical = encoded
	h := fnv.New64a()
	_, _ = h.Write(encoded)
	e.hash = h.Sum64()
}

func appendCanonicalString(dst []byte, value string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendCanonicalBytes(dst, value []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func exactTypesEqual(left, right *exactType) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.hash != right.hash || len(left.canonical) != len(right.canonical) {
		return false
	}
	for i := range left.canonical {
		if left.canonical[i] != right.canonical[i] {
			return false
		}
	}
	return true
}
