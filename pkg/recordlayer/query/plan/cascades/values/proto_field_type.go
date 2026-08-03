package values

import "google.golang.org/protobuf/reflect/protoreflect"

// FieldTypeForProtoField maps a proto field descriptor to the logical column
// Type the engine gives that field's slot. It is THE single authority for
// "what type is this stored column", and every layout describing a stored
// record's row must derive its field types from it.
//
// Having exactly one of these is the point, not a tidiness preference. The
// layout is derived independently on two paths — the plain full-scan leaf
// (cascades_translator's tableColumns) and the sargable match candidate
// (executor.PositionalTypeForDescriptor) — and those two paths feed the SAME
// planner decisions about the same table. When one of them typed its fields
// and the other stamped UnknownType on all of them, a type-directed rule
// silently reached opposite conclusions depending on which access path was
// under consideration: the ordering-claim predicate could prove a column was a
// DOUBLE on the scan leaf and could not prove it on the index candidate, so a
// predicated query kept the unsound sort elision that the unpredicated form of
// the same query had already lost. A second copy of this switch is the same
// bug waiting to be reintroduced.
//
// Conventions, all deliberate:
//
//   - Every type is NULLABLE. A row layout carries no per-column NOT NULL
//     constraint, and an unset proto field materializes as a nil slot.
//   - Repeated and map fields are UnknownType: their slot holds a list, not
//     the element scalar, so naming the element type would misdescribe the
//     slot.
//   - The tuple_fields.UUID wrapper message is UUID — it materializes as a
//     neutral [16]byte, not as a raw message (see protoScalarToRowValue).
//     Any OTHER message field is UnknownType: its slot stays a raw
//     proto.Message, and a recursive message has no finite structural type.
//   - Enums, groups and the UNSIGNED integer kinds are UnknownType. They have
//     no faithful column type here — an enum slot is a bare int64 ordinal with
//     no enum descriptor attached, and an unsigned 64-bit value is reinterpreted
//     as a signed int64 on the way in.
func FieldTypeForProtoField(fd protoreflect.FieldDescriptor) Type {
	if fd == nil || fd.IsList() || fd.IsMap() {
		return UnknownType
	}
	return ScalarTypeForProtoKind(fd)
}

// ScalarTypeForProtoKind is FieldTypeForProtoField's kind switch with
// repetition ignored: it answers "what type does ONE value of this field
// descriptor have", so applied to a repeated field it states the ELEMENT
// type rather than UnknownType.
//
// The seam exists because a caller that has already accounted for repetition
// — one typing an array's element, where the repeatedness belongs to the
// enclosing array type — must not have the collapse applied a second time.
// Every convention listed on FieldTypeForProtoField except the
// repeated/map collapse applies here unchanged, and both functions remain
// the same single copy of the mapping: FieldTypeForProtoField is this
// switch plus the collapse, never a parallel transcription of it.
func ScalarTypeForProtoKind(fd protoreflect.FieldDescriptor) Type {
	if fd == nil {
		return UnknownType
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return NewPrimitiveType(TypeCodeBoolean, true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return NewPrimitiveType(TypeCodeInt, true)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return NewPrimitiveType(TypeCodeLong, true)
	case protoreflect.FloatKind:
		return NewPrimitiveType(TypeCodeFloat, true)
	case protoreflect.DoubleKind:
		return NewPrimitiveType(TypeCodeDouble, true)
	case protoreflect.StringKind:
		// DATE and TIMESTAMP columns also store as STRING proto fields; the
		// SQL-level type is not recoverable from the descriptor alone, and the
		// slot genuinely holds a Go string either way.
		return NewPrimitiveType(TypeCodeString, true)
	case protoreflect.BytesKind:
		return NewPrimitiveType(TypeCodeBytes, true)
	case protoreflect.MessageKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return NewPrimitiveType(TypeCodeUuid, true)
		}
		return UnknownType
	}
	return UnknownType
}
