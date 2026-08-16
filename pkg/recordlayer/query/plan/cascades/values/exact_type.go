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
	// The record/enum NAME is deliberately absent from the identity: it is
	// provenance, not shape, and Java's Type.Record/Type.Enum equals+hashCode
	// exclude it too. Keeping it here would make the exact channel's identity
	// STRICTER than the Type equality it exists to make checkable, so one row
	// reached by two routes would carry two "exact" identities.
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

// exactRowShapesAgree is the exact-handle twin of QuantifiedRowShapesAgree —
// same rule, same reason (see that doc): equality of the bound row's SHAPE, with
// the top-level nullable bit excluded because it states that the row may be
// ABSENT, which every binder carries structurally instead. Nested nullability is
// compared byte-for-byte with the children's canonical forms, so only the one
// outermost bit is ever ignored, and only at a lookup.
func exactRowShapesAgree(left, right *exactType) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.nullable == right.nullable {
		return exactTypesEqual(left, right)
	}
	if left.code != right.code || left.anyRecord != right.anyRecord {
		return false
	}
	if len(left.fields) != len(right.fields) || len(left.enumValues) != len(right.enumValues) {
		return false
	}
	for i := range left.fields {
		if left.fields[i].name != right.fields[i].name ||
			left.fields[i].ordinal != right.fields[i].ordinal ||
			!exactTypesEqual(left.fields[i].typ, right.fields[i].typ) {
			return false
		}
	}
	for i := range left.enumValues {
		if left.enumValues[i] != right.enumValues[i] {
			return false
		}
	}
	return exactTypesEqual(left.element, right.element)
}

// describeExactType renders a compact, comparable shape for a diagnostic:
// `RECORD(LID:LONG,VAL:LONG?)`, `ARRAY<INT>`, `LONG?`. A trailing `?` marks
// nullable.
//
// It exists because a type-conflict error that names neither the correlation nor
// how the two types differ makes every occurrence a fresh investigation — and
// these conflicts arrive in clusters, where the ONE thing worth knowing first is
// whether the disagreement is arity, nullability, or field naming.
//
// Every attribute exact equality compares is rendered, and only the ones that
// can differ are ever spelled out: a field ORDINAL as `#n` when it is not the
// field's own position. Rendering less than equality compares makes two
// provably-unequal types print identically, which reads as "the diagnostic is
// wrong" and sends the next reader down the wrong path — it already cost one
// investigation a full lap.
//
// The record/enum NAME is rendered too — as `RECORD@Name(…)` — even though
// equality does NOT compare it. It is the provenance of the shape, and when a
// conflict IS elsewhere it is usually the fastest way to see which two routes
// produced the two sides.
func describeExactType(e *exactType) string {
	if e == nil {
		return "<nil>"
	}
	out := e.code.String()
	switch {
	case e.anyRecord:
		out = "RECORD(*)"
	case e.code == TypeCodeRecord:
		if e.name != "" {
			out += "@" + e.name
		}
		out += "("
		for i, field := range e.fields {
			if i > 0 {
				out += ","
			}
			out += field.name
			if field.ordinal != i {
				out += "#" + uitoa(uint64(field.ordinal))
			}
			out += ":" + describeExactType(field.typ)
		}
		out += ")"
	case e.code == TypeCodeEnum:
		if e.name != "" {
			out += "@" + e.name
		}
	case e.code == TypeCodeArray || e.code == TypeCodeRelation:
		out += "<" + describeExactType(e.element) + ">"
	}
	if e.nullable {
		out += "?"
	}
	return out
}

// exactTypeConflict is the shared rendering for the "two exact types were
// expected to be the same one" family. Every arm reports WHICH correlation and
// BOTH shapes, in that order, so the reader can tell a nullability-only
// disagreement from a different row.
func exactTypeConflict(path, what string, correlation CorrelationIdentifier, have, want *exactType) error {
	return resolutionError(CorrelationTypeConflict, path+"["+correlation.Name()+"]",
		what+": bound "+describeExactType(have)+" but read as "+describeExactType(want))
}

// QuantifiedRowShapesAgree reports whether two exact QOV flowed types denote the
// SAME BOUND ROW. It is the one comparison every runtime QOV *lookup* uses, and
// it deliberately excludes the top-level nullable bit.
//
// That bit is not part of a row's shape: on a quantifier it declares that the
// quantifier's row may be ABSENT, and absence is carried structurally by the
// binding itself — a nil row, or a positive explicit-absence proof — never by
// the row's type. Evaluating FieldValue(qov, i) against a present row is
// bit-identical whichever way the bit is set, and against an absent one both
// yield NULL, so the bit can decide nothing at a lookup.
//
// One alias therefore legitimately carries two QOVs differing only there. That
// pairing is Java's, not a Go accident: an outer join's OUTPUT columns are
// pulled up through Quantifier.pullUpResultColumnsWithNullability(true), which
// mints QuantifiedObjectValue.of(alias, rowType.withNullability(true)), while
// the same alias's ON/WHERE predicates keep reading getFlowedObjectValue() —
// the leg's own, non-nullable row. Java binds both by alias alone; Go's exact
// channel keeps the strictly stronger check on everything that IS shape.
//
// Field names, ordinals, arity, record names and every NESTED nullability still
// have to match exactly, so a foreign owner spelled with the same alias is
// still refused. Type DERIVATION sites (layout window construction, the
// null-supplying proofs, metadata nullability) must keep comparing with Equals:
// there the bit is the whole point.
func QuantifiedRowShapesAgree(left, right Type) bool {
	if left == nil || right == nil {
		return false
	}
	if left.IsNullable() == right.IsNullable() {
		return left.Equals(right)
	}
	return left.Equals(WithNullability(right, left.IsNullable()))
}

// DescribeType renders a Type as a compact, comparable shape for a diagnostic:
// `RECORD(LID:LONG,VAL:LONG?)`, `ARRAY<INT>`, `LONG?`. A trailing `?` marks
// nullable. It is the public twin of the rendering the exact-type conflicts use,
// for callers outside this package that hold a Type rather than a handle.
func DescribeType(typ Type) string {
	handle, err := SnapshotExactType(typ)
	if err != nil {
		if typ == nil {
			return "<nil>"
		}
		return typ.Code().String() + "(inexact)"
	}
	return describeExactType(handle.(*exactType))
}
