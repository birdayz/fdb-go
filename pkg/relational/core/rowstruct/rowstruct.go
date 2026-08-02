// Package rowstruct materializes a STRUCT column value for SQL clients:
// the Go analogue of Java's RowStruct family, where a struct column read
// off a record surfaces as a RelationalStruct rather than as the raw
// protobuf message.
//
// Java's chain is RowStruct.getObject → getStruct →
// `new ImmutableRowStruct(new MessageTuple((Message) obj),
// metaData.getStructMetaData(col))` (RowStruct.java:184-197, :281-302),
// with MessageTuple reading the message POSITIONALLY over its descriptor's
// fields (MessageTuple.java:53-78) and RelationalStructMetaData backed by
// the struct's DataType (RelationalStructMetaData.java:40-135). This
// package is the same three pieces: MessageStruct (the struct), its
// positional accessor over the descriptor, and StructMetaData over an
// api.StructType.
package rowstruct

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
)

// MessageStruct is an api.Struct over a proto message — Java's
// ImmutableRowStruct wrapping a MessageTuple.
//
// Attributes are read POSITIONALLY off the descriptor, which is Java's
// contract (MessageTuple.getObject's `descriptor.getFields().get(position)`)
// and the only sound one: two same-named fields cannot occur in a
// descriptor, but position is what the metadata's ordinals mean.
type MessageStruct struct {
	msg protoreflect.Message
	md  api.StructMetaData
}

// New builds the struct view of a proto message. The metadata is derived
// from the message's own descriptor, so a struct read back through it
// reports the DECLARED struct type name (Java: RelationalStructMetaData
// .getTypeName, :62-65).
func New(msg protoreflect.Message) (*MessageStruct, error) {
	st, err := metadata.StructTypeFromDescriptor(msg.Descriptor(), true)
	if err != nil {
		return nil, err
	}
	return &MessageStruct{msg: msg, md: &StructMetaData{typ: st}}, nil
}

// MetaData returns the struct's metadata.
func (s *MessageStruct) MetaData() api.StructMetaData { return s.md }

// AttributeCount returns the number of attributes.
func (s *MessageStruct) AttributeCount() int { return s.msg.Descriptor().Fields().Len() }

// Attribute returns the attribute at oneBasedIndex (JDBC convention).
func (s *MessageStruct) Attribute(oneBasedIndex int) (any, error) {
	fields := s.msg.Descriptor().Fields()
	if oneBasedIndex < 1 || oneBasedIndex > fields.Len() {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"attribute index %d out of range for struct %q with %d attributes",
			oneBasedIndex, s.md.TypeName(), fields.Len())
	}
	return s.fieldValue(fields.Get(oneBasedIndex - 1))
}

// AttributeByName looks the attribute up by name, case-insensitively —
// Java resolves a struct attribute name the same way
// (RelationalStruct.getOneBasedPosition:156-164, equalsIgnoreCase).
func (s *MessageStruct) AttributeByName(name string) (any, error) {
	fields := s.msg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if equalFoldASCII(string(fields.Get(i).Name()), name) {
			return s.fieldValue(fields.Get(i))
		}
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
		"struct %q has no attribute %q", s.md.TypeName(), name)
}

// Attributes returns every attribute in declared order. An attribute that
// cannot be materialized surfaces as nil rather than an error, because the
// interface has no error channel here; every caller that needs the failure
// uses Attribute.
func (s *MessageStruct) Attributes() []any {
	fields := s.msg.Descriptor().Fields()
	out := make([]any, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		v, err := s.fieldValue(fields.Get(i))
		if err == nil {
			out[i] = v
		}
	}
	return out
}

// fieldValue materializes one field the way the driver materializes a
// top-level column, so a value nested in a struct and the same value as a
// column are indistinguishable to a client: an ABSENT field is SQL NULL
// (Java: MessageTuple's `!message.hasField` → null, :70-72), a nested
// non-UUID message is another struct (Java: getStruct's Message arm), an
// array is a []any of materialized elements, and every scalar goes through
// the one proto→driver conversion (UUID as its canonical string included).
func (s *MessageStruct) fieldValue(fd protoreflect.FieldDescriptor) (any, error) {
	if list, _, ok := values.EffectiveListField(fd); ok {
		// A NULLABLE array is the wrapper message: ABSENT wrapper is NULL,
		// a present one with an empty list is the empty array.
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !s.msg.Has(fd) {
			return nil, nil
		}
		var lv protoreflect.List
		if fd.IsList() {
			lv = s.msg.Get(fd).List()
		} else {
			lv = s.msg.Get(fd).Message().Get(list).List()
		}
		out := make([]any, lv.Len())
		for i := 0; i < lv.Len(); i++ {
			v, err := elementValue(list, lv.Get(i))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	if !s.msg.Has(fd) {
		return nil, nil
	}
	return elementValue(fd, s.msg.Get(fd))
}

// elementValue materializes a non-repeated proto value against the field
// descriptor that types it (for an array, the repeated field — its ELEMENT
// type).
func elementValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) (any, error) {
	if fd.Kind() == protoreflect.MessageKind && !isUUIDField(fd) {
		return New(v.Message())
	}
	return functions.ProtoValueToDriver(fd, v), nil
}

func isUUIDField(fd protoreflect.FieldDescriptor) bool {
	msg := fd.Message()
	return msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName
}

// equalFoldASCII is strings.EqualFold restricted to the ASCII case folding
// Java's equalsIgnoreCase applies to identifiers.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// StructMetaData is api.StructMetaData backed by an api.StructType —
// Java's RelationalStructMetaData (RelationalStructMetaData.java:40-135).
type StructMetaData struct {
	typ *api.StructType
}

// NewStructMetaData wraps a struct type as struct metadata.
func NewStructMetaData(t *api.StructType) *StructMetaData { return &StructMetaData{typ: t} }

// TypeName returns the declared struct type name.
func (m *StructMetaData) TypeName() string { return m.typ.Name() }

// AttributeCount returns the number of attributes.
func (m *StructMetaData) AttributeCount() int { return m.typ.NumFields() }

func (m *StructMetaData) field(oneBasedIndex int) (api.StructField, error) {
	if oneBasedIndex < 1 || oneBasedIndex > m.typ.NumFields() {
		return api.StructField{}, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"attribute index %d out of range for struct %q with %d attributes",
			oneBasedIndex, m.typ.Name(), m.typ.NumFields())
	}
	return m.typ.Field(oneBasedIndex - 1), nil
}

// AttributeName returns the attribute's declared name.
func (m *StructMetaData) AttributeName(oneBasedIndex int) (string, error) {
	f, err := m.field(oneBasedIndex)
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

// AttributeType returns the attribute's JDBC type code.
func (m *StructMetaData) AttributeType(oneBasedIndex int) (int, error) {
	f, err := m.field(oneBasedIndex)
	if err != nil {
		return 0, err
	}
	return api.JDBCType(f.Type().Code()), nil
}

// AttributeTypeName returns the attribute type's display name.
func (m *StructMetaData) AttributeTypeName(oneBasedIndex int) (string, error) {
	f, err := m.field(oneBasedIndex)
	if err != nil {
		return "", err
	}
	if st, ok := f.Type().(*api.StructType); ok {
		return st.Name(), nil
	}
	return f.Type().Code().String(), nil
}

// AttributeDataType returns the attribute's full DataType.
func (m *StructMetaData) AttributeDataType(oneBasedIndex int) (api.DataType, error) {
	f, err := m.field(oneBasedIndex)
	if err != nil {
		return nil, err
	}
	return f.Type(), nil
}

// AttributeNullable reports whether the attribute admits NULL.
func (m *StructMetaData) AttributeNullable(oneBasedIndex int) (int, error) {
	f, err := m.field(oneBasedIndex)
	if err != nil {
		return 0, err
	}
	if f.Type().IsNullable() {
		return api.ColumnNullable, nil
	}
	return api.ColumnNoNulls, nil
}

var (
	_ api.Struct         = (*MessageStruct)(nil)
	_ api.StructMetaData = (*StructMetaData)(nil)
)
