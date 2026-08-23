package values

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/protoname"
)

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
//   - Nullability follows the value the row materializer can actually emit:
//     optional/presence-bearing fields may be nil, while proto3 scalars,
//     required fields, and flat repeated fields always have a value.
//   - Repeated fields are exact ARRAYs. ProtoFieldToRowValue materializes them
//     as []any, so returning the element scalar (the old bug) or Unknown (the
//     old workaround) both misdescribe the executable slot.
//   - Proto maps remain UnknownType: ProtoFieldToRowValue intentionally keeps
//     their native protoreflect.Map carrier and the Type algebra has no map
//     kind. An executable exact-type boundary therefore rejects map-bearing
//     rows instead of laundering them as a record or array.
//   - The tuple_fields.UUID wrapper message is UUID — it materializes as a
//     neutral [16]byte, not as a raw message (see protoScalarToRowValue).
//     Any OTHER message field is UnknownType: its slot stays a raw
//     proto.Message, and a recursive message has no finite structural type.
//   - Known message descriptors become structural RECORDs. Cyclic back-edges
//     become exact AnyRecord leaves: the carrier is still a record, but a
//     finite FieldValue path may not descend through the erased recursive
//     edge. UUID keeps its dedicated scalar representation.
//   - Enums keep their descriptor identity when it is representable by the
//     Type algebra. Aliased enums and unsigned integers use LONG, matching the
//     int64 carrier emitted by ProtoScalarKindToRowValue.
func FieldTypeForProtoField(fd protoreflect.FieldDescriptor) Type {
	return fieldTypeForProtoField(fd, make(map[protoreflect.FullName]struct{}))
}

func fieldTypeForProtoField(fd protoreflect.FieldDescriptor, active map[protoreflect.FullName]struct{}) Type {
	if fd == nil || fd.IsMap() {
		return UnknownType
	}
	if list, wrapped, ok := EffectiveListField(fd); ok {
		element := scalarTypeForProtoField(list, active)
		if IsUnresolved(element) {
			return UnknownType
		}
		// A flat repeated field materializes as an empty (never nil) list.
		// A nullable-array wrapper preserves absent wrapper as SQL NULL.
		return NewArrayType(wrapped, withNullability(element, false))
	}
	return withNullability(scalarTypeForProtoField(fd, active), protoFieldNullable(fd))
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
	return scalarTypeForProtoField(fd, make(map[protoreflect.FullName]struct{}))
}

// ScalarCodeForProtoKind is the ALLOCATION-FREE half of the scalar mapping: the
// TypeCode a descriptor's kind carries, without building the Type that names it.
// ok=false for a kind with no scalar answer (a record, or one this engine does
// not map), which the caller must then handle structurally.
//
// It exists because two callers want different halves of one decision, and
// splitting the decision is the only way to keep it a single authority.
// ScalarTypeForProtoKind needs the Type. The exact-type compatibility check
// needs only the code, and it runs per FIELD READ per ROW — building a Type
// there (and, for an enum, its value slice and number set) allocated
// O(rows × accesses × width) of garbage to answer a question about an int.
//
// The enum arm is the reason this is not a plain switch on Kind: a
// number-ALIASING enum maps to LONG rather than to an enum type, and that is
// only knowable by scanning the descriptor's values. The scan allocates
// nothing, unlike constructing the EnumType it is deciding against.
func ScalarCodeForProtoKind(fd protoreflect.FieldDescriptor) (TypeCode, bool) {
	if fd == nil {
		return TypeCodeUnknown, false
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return TypeCodeBoolean, true
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return TypeCodeInt, true
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return TypeCodeLong, true
	case protoreflect.FloatKind:
		return TypeCodeFloat, true
	case protoreflect.DoubleKind:
		return TypeCodeDouble, true
	case protoreflect.StringKind:
		// DATE and TIMESTAMP columns also store as STRING proto fields; the
		// SQL-level type is not recoverable from the descriptor alone, and the
		// slot genuinely holds a Go string either way.
		return TypeCodeString, true
	case protoreflect.BytesKind:
		return TypeCodeBytes, true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return TypeCodeLong, true
	case protoreflect.EnumKind:
		if protoEnumHasNumberAlias(fd.Enum()) {
			return TypeCodeLong, true
		}
		if fd.Enum() == nil {
			return TypeCodeUnknown, false
		}
		return TypeCodeEnum, true
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return TypeCodeUuid, true
		}
		return TypeCodeRecord, true
	}
	return TypeCodeUnknown, false
}

// protoEnumHasNumberAlias reports the condition that forces an enum onto the
// LONG carrier: the engine's EnumType rejects number aliases while the runtime
// carrier is the number itself.
func protoEnumHasNumberAlias(ed protoreflect.EnumDescriptor) bool {
	if ed == nil {
		return false
	}
	declared := ed.Values()
	for i := 0; i < declared.Len(); i++ {
		number := declared.Get(i).Number()
		for j := 0; j < i; j++ {
			if declared.Get(j).Number() == number {
				return true
			}
		}
	}
	return false
}

func scalarTypeForProtoField(fd protoreflect.FieldDescriptor, active map[protoreflect.FullName]struct{}) Type {
	if fd == nil {
		return UnknownType
	}
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return enumTypeForProto(fd.Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return NewPrimitiveType(TypeCodeUuid, true)
		}
		return recordTypeForProtoMessage(fd.Message(), active)
	}
	// Every remaining kind is a plain scalar whose Type is exactly its code.
	// Taken from the shared authority so the two cannot drift — which is the
	// whole reason the compatibility check stopped restating this switch.
	code, ok := ScalarCodeForProtoKind(fd)
	if !ok {
		return UnknownType
	}
	return NewPrimitiveType(code, true)
}

func protoFieldNullable(fd protoreflect.FieldDescriptor) bool {
	return fd != nil && fd.HasPresence() && fd.Cardinality() != protoreflect.Required
}

func recordTypeForProtoMessage(md protoreflect.MessageDescriptor, active map[protoreflect.FullName]struct{}) Type {
	if md == nil {
		return UnknownType
	}
	name := md.FullName()
	if _, recursive := active[name]; recursive {
		return NewAnyRecordType(true)
	}
	active[name] = struct{}{}
	defer delete(active, name)

	fields := md.Fields()
	recordFields := make([]Field, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		recordFields[i] = Field{
			Name:      FieldNameForProtoField(field),
			FieldType: fieldTypeForProtoField(field, active),
			Ordinal:   i,
		}
	}
	return NewRecordType(string(name), true, recordFields)
}

func enumTypeForProto(ed protoreflect.EnumDescriptor) Type {
	if ed == nil {
		return UnknownType
	}
	declared := ed.Values()
	values := make([]EnumValue, 0, declared.Len())
	seenNumbers := make(map[protoreflect.EnumNumber]struct{}, declared.Len())
	for i := 0; i < declared.Len(); i++ {
		value := declared.Get(i)
		if _, alias := seenNumbers[value.Number()]; alias {
			// The engine's EnumType deliberately rejects number aliases while
			// the runtime carrier is the number. LONG is the exact executable
			// carrier until the enum algebra grows alias support.
			return NewPrimitiveType(TypeCodeLong, true)
		}
		seenNumbers[value.Number()] = struct{}{}
		values = append(values, EnumValue{Name: string(value.Name()), Number: int32(value.Number())})
	}
	return NewEnumType(string(ed.FullName()), true, values)
}

// FieldNameForProtoField is THE single authority for the NAME a stored
// column's slot carries in a values.Type: the user identifier, VERBATIM.
//
// A protobuf field name is the STORAGE spelling — `$`, `.` and `__` are escaped
// (`a$b` is stored as `A__1B`). Java keeps the two apart on the type itself:
// `Type.Record.fromDescriptorPreservingName` records
// `ProtoUtils.toUserIdentifier(descriptor.getName())` as the name and the raw
// descriptor name as the storage name, and `Type.Record.Field` does the same
// per column — the wire keeps the escaped spelling, the SQL surface sees the
// user's.
//
// Go's SQL catalog already un-escapes (rlcatalog), so a Value-tier type built
// straight off the descriptor states a DIFFERENT name for the same column than
// the reference that reads it. Under RFC-232 those two are compared as exact
// types, so the disagreement is not cosmetic: it is a hard
// "type disagrees with declared binding" at evaluation, and every query over a
// table with an escaped identifier fails.
//
// It does not FOLD, for the same reason Java does not: the only normalization
// in the language is at the parse boundary (SemanticAnalyzer.normalizeString —
// quoted keeps its case, unquoted folds UPPER), and every catalog comparison
// after it is exact. A fold here was invisible for a DDL-created schema, whose
// descriptor names are already the normalized spelling, and wrong for
// everything else — it named a column something no reference could spell, and
// it made the index-matching bridge miss SILENTLY (RFC-237 §2.1).
func FieldNameForProtoField(field protoreflect.FieldDescriptor) string {
	return protoname.ToUserIdentifier(string(field.Name()))
}

// RecordNameForDescriptor is FieldNameForProtoField's twin for the record's own
// name, un-escaped by the same rule.
func RecordNameForDescriptor(descriptor protoreflect.MessageDescriptor) string {
	return protoname.ToUserIdentifier(string(descriptor.Name()))
}
