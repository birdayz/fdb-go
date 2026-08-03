package values

// Plan-time protobuf descriptor synthesis for a values.Type.
//
// This is the port of Java's Type.defineProtoType / Type.addProtoField
// dispatch (Type.java:324-341 for the interface, :2339-2356 Record,
// :3251-3291 Array, :1067-1080 Primitive, :3531-3540 Uuid) driven by
// TypeRepository.Builder.addTypeIfNeeded / defineAndResolveType
// (TypeRepository.java:498-503, :551-555).
//
// WHY IT HAS TO EXIST. A COMPUTED record — `(1, 1.0, 'a', true)`, the result
// of RecordConstructorValue.Type() — is an ANONYMOUS *RecordType. It has no
// target column and therefore no stored descriptor, so the DDL-time emitter
// (pkg/relational/core/metadata) cannot serve it: that one works from
// api.DataType and rejects an empty struct name, which is exactly what a
// computed record has. Java has the same split and resolves it the same way:
// the stored schema comes from RecordMetaData, and anything computed gets a
// descriptor SYNTHESISED on demand from its Type. Without this, a computed
// record can only be a bare map, which is not a message, so nothing
// downstream can describe it as a struct.
//
// WIRE STATUS: NONE. Every descriptor built here is synthesised per query and
// thrown away; it never reaches FDB. The stored-schema emitter in
// pkg/relational/core/metadata is the one whose bytes are wire, and it is
// deliberately NOT reused here — its inputs and its error rules are the DDL's.
// What the two share is the escaping (pkg/recordlayer/protoname) and the
// structural choices, which are copied from the same Java methods.
//
// THE ONE DELIBERATE DIVERGENCE — synthetic type names. Java names an
// anonymous type ProtoUtils.uniqueTypeName() = "__type__" + a RANDOM
// UUID (ProtoUtils.java:85-95), fresh on every call. Go keeps the exact
// FORMAT but derives the name from a per-repository counter, so identical
// input types produce identical descriptors and a test can assert on one.
// This cannot break compatibility with Java, and the reason is not a judgement
// call: Java's own name is random, so no Java reader can be keyed on it and
// no two Java runs agree on it either. Every consumer is structural.

import (
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/protoname"
)

// wrappedArrayFieldName is Java's NullableArrayTypeUtils.getRepeatedFieldName()
// — the single repeated field inside a NullableArrayWrapper message. Kept
// equal to the DDL emitter's constant of the same name
// (pkg/relational/core/metadata/proto_types.go): the wrapper IDENTITY is
// structural (readers check the shape, never the message name), and the shape
// is this field name.
const wrappedArrayFieldName = "values"

// uuidProtoTypeName is the fully-qualified tuple_fields UUID message. A UUID
// field carries only this type_name, no explicit TYPE_MESSAGE — Java's
// Uuid.addProtoField (Type.java:3531-3540).
const uuidProtoTypeName = "com.apple.foundationdb.record.UUID"

// ProtoTypeError reports a type that cannot be given a protobuf descriptor.
// Java throws RecordCoreException from the addProtoField of the types that
// have no protobuf form (FUNCTION at Type.java:3414, and the erased/unresolved
// shapes that fail their Objects.requireNonNull).
type ProtoTypeError struct {
	// TypeName is the offending type rendered via Type.String().
	TypeName string
	// Reason states what about it has no protobuf form.
	Reason string
}

func (e *ProtoTypeError) Error() string {
	return fmt.Sprintf("cannot synthesise a protobuf descriptor for %s: %s", e.TypeName, e.Reason)
}

// canonicalizeNullability is Java's TypeRepository.canonicalizeNullability
// (TypeRepository.java:292-298): the descriptor cache is keyed on a form that
// ignores the nullability of the type ITSELF, because a field's nullability
// lives on the REFERENCING field's label, never on the referenced message. So
// a nullable and a non-nullable use of one record shape share one descriptor.
// RELATION and NONE canonicalize non-nullable, everything else nullable.
func canonicalizeNullability(t Type) Type {
	if t.Code() == TypeCodeRelation || t.Code() == TypeCodeNone {
		return withNullability(t, false)
	}
	return withNullability(t, true)
}

// withNullability returns t with its nullability set, sharing t when it
// already matches. Java has this on the Type interface; Go's Type interface
// carries only Code/IsNullable/Equals/String, so the switch lives here.
func withNullability(t Type, nullable bool) Type {
	if t == nil || t.IsNullable() == nullable {
		return t
	}
	switch v := t.(type) {
	case *PrimitiveType:
		return &PrimitiveType{TypeCode: v.TypeCode, Nullable: nullable}
	case *RecordType:
		// Legs is layout metadata, not shape, and is carried across
		// unchanged for the same reason identity ignores it.
		return &RecordType{RecordName: v.RecordName, Nullable: nullable, Fields: v.Fields, Legs: v.Legs}
	case *ArrayType:
		return &ArrayType{Nullable: nullable, ElementType: v.ElementType}
	case *EnumType:
		return &EnumType{EnumName: v.EnumName, Nullable: nullable, Values: v.Values}
	default:
		// RelationType and any future impl: nullability is not expressible,
		// so the type is already canonical.
		return t
	}
}

// TypeProtoRepository is the protobuf surface of a type repository: Java's
// TypeRepository.Builder plus the built TypeRepository, folded into one
// object because Go has no need for the two-phase split (Java's Builder
// exists to accumulate a FileDescriptorProto that is validated once; here the
// file is recompiled on demand and memoised).
//
// It is Java's addTypeIfNeeded dedup: a Type already defined is never defined
// twice, and its descriptor is handed back from the cache. Safe for
// concurrent use — a single repository is shared by every row of a query, and
// rows are produced concurrently.
type TypeProtoRepository struct {
	mu sync.Mutex
	// messages accumulates the synthesised DescriptorProtos in definition
	// order — Java's fileDescProtoBuilder.addMessageType.
	messages []*descriptorpb.DescriptorProto
	// entries is Java's typeToNameMap BiMap. A LINEAR SCAN over Type.Equals,
	// not a hash map over a derived key: Equals is the authoritative
	// structural comparison this package already defines, and an invented
	// string key would be a second, weaker definition of type identity whose
	// collisions would silently merge two shapes into one descriptor.
	// Repositories hold a handful of entries — one per distinct computed
	// shape in one query.
	entries []typeNameEntry
	// counter names anonymous types deterministically (see the file header).
	counter int
	// compiled memoises the built FileDescriptor; invalidated by any define.
	compiled protoreflect.FileDescriptor
}

type typeNameEntry struct {
	// typ is the CANONICAL-nullability form (canonicalizeNullability).
	typ Type
	// name is the synthesised or derived protobuf message name.
	name string
	// desc memoises the compiled descriptor for this entry.
	desc protoreflect.MessageDescriptor
}

// NewTypeProtoRepository returns an empty repository.
func NewTypeProtoRepository() *TypeProtoRepository {
	return &TypeProtoRepository{}
}

// protoRepo lazily attaches a protobuf surface to the named-type repository,
// so a caller holding a TypeRepository can synthesise descriptors for the
// types it resolves. Java's TypeRepository IS the protobuf surface; Go's grew
// as a name->Type map first, so the two are stitched here rather than merged,
// which would churn every existing Register/Lookup caller for no behavioural
// gain.
func (r *TypeRepository) protoRepo() *TypeProtoRepository {
	r.protoOnce.Do(func() { r.proto = NewTypeProtoRepository() })
	return r.proto
}

// MessageDescriptorFor returns the synthetic protobuf message descriptor for
// t, defining it (and its whole type closure) on first use. Java:
// TypeRepository.newMessageBuilder(Type) → getProtoTypeName → the descriptor
// (TypeRepository.java:160-176).
//
// Only a type with a MESSAGE form has one: a RECORD, or an array whose
// nullable-wrapper is a message. Anything else is a *ProtoTypeError.
func (r *TypeRepository) MessageDescriptorFor(t Type) (protoreflect.MessageDescriptor, error) {
	return r.protoRepo().MessageDescriptorFor(t)
}

// MessageDescriptorFor returns the synthetic descriptor for t, defining it on
// first use and serving it from the cache afterwards.
func (p *TypeProtoRepository) MessageDescriptorFor(t Type) (protoreflect.MessageDescriptor, error) {
	if t == nil {
		return nil, &ProtoTypeError{TypeName: "<nil>", Reason: "nil type"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	name, err := p.defineAndResolveLocked(t)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, &ProtoTypeError{TypeName: t.String(), Reason: "type has no message form"}
	}
	idx := p.indexOfLocked(t)
	if idx >= 0 && p.entries[idx].desc != nil {
		return p.entries[idx].desc, nil
	}
	fd, err := p.compileLocked()
	if err != nil {
		return nil, err
	}
	md := fd.Messages().ByName(protoreflect.Name(name))
	if md == nil {
		return nil, &ProtoTypeError{
			TypeName: t.String(),
			Reason:   fmt.Sprintf("synthesised message %q missing from the compiled file", name),
		}
	}
	if idx >= 0 {
		p.entries[idx].desc = md
	}
	return md, nil
}

// indexOfLocked finds t's cache entry by CANONICAL structural equality.
func (p *TypeProtoRepository) indexOfLocked(t Type) int {
	c := canonicalizeNullability(t)
	for i := range p.entries {
		if p.entries[i].typ.Equals(c) {
			return i
		}
	}
	return -1
}

// defineAndResolveLocked is Java's Builder.defineAndResolveType
// (TypeRepository.java:551-555): addTypeIfNeeded, then return the registered
// name. The empty string is Java's Optional.empty — a type with no message
// form (every primitive), which is not an error: addProtoField ignores the
// type name for those.
func (p *TypeProtoRepository) defineAndResolveLocked(t Type) (string, error) {
	if idx := p.indexOfLocked(t); idx >= 0 {
		return p.entries[idx].name, nil
	}
	if err := p.defineProtoTypeLocked(t); err != nil {
		return "", err
	}
	if idx := p.indexOfLocked(t); idx >= 0 {
		return p.entries[idx].name, nil
	}
	return "", nil
}

// registerLocked is Java's Builder.registerTypeToTypeNameMapping.
func (p *TypeProtoRepository) registerLocked(t Type, name string) {
	p.entries = append(p.entries, typeNameEntry{typ: canonicalizeNullability(t), name: name})
	p.compiled = nil
}

// uniqueTypeNameLocked is Java's ProtoUtils.uniqueTypeName with a
// deterministic suffix — see the file header for why the divergence is safe.
func (p *TypeProtoRepository) uniqueTypeNameLocked() string {
	p.counter++
	return fmt.Sprintf("__type__%d", p.counter)
}

// defineProtoTypeLocked is Java's Type.defineProtoType dispatch. The default
// arm builds nothing (Type.java:324-326) — a primitive has no message.
func (p *TypeProtoRepository) defineProtoTypeLocked(t Type) error {
	switch v := t.(type) {
	case *RecordType:
		return p.defineRecordLocked(v)
	case *ArrayType:
		return p.defineArrayLocked(v)
	default:
		return nil
	}
}

// defineRecordLocked ports Type.Record.defineProtoType (Type.java:2339-2356):
// one DescriptorProto named after the record's storage name (or a synthetic
// name when anonymous), one field per record field at index ordinal+1, every
// field LABEL_OPTIONAL, each field's own type defined recursively first.
func (p *TypeProtoRepository) defineRecordLocked(rt *RecordType) error {
	typeName := ""
	if rt.RecordName != "" {
		escaped, err := protoname.ToProtoBufCompliantName(rt.RecordName)
		if err != nil {
			return &ProtoTypeError{
				TypeName: rt.String(),
				Reason:   fmt.Sprintf("record name %q: %v", rt.RecordName, err),
			}
		}
		typeName = escaped
	} else {
		typeName = p.uniqueTypeNameLocked()
	}
	msg := &descriptorpb.DescriptorProto{Name: proto.String(typeName)}
	// Register BEFORE walking the fields. Java gets the same protection from
	// its recursion terminating on the BiMap, and without it a record type
	// that reaches itself recurses until the stack ends.
	p.registerLocked(rt, typeName)

	for i, f := range rt.Fields {
		ft := f.FieldType
		if ft == nil {
			ft = UnknownType
		}
		// Java's normalizeFields (Type.java:2617-2680): an anonymous field is
		// named "_<ordinal>" and every field index is ordinal+1.
		fieldName := f.Name
		if fieldName == "" {
			fieldName = OrdinalFieldName(i)
		}
		storageName, err := protoname.ToProtoBufCompliantName(fieldName)
		if err != nil {
			return &ProtoTypeError{
				TypeName: rt.String(),
				Reason:   fmt.Sprintf("field name %q: %v", fieldName, err),
			}
		}
		nested, err := p.defineAndResolveLocked(ft)
		if err != nil {
			return err
		}
		if err := p.addProtoFieldLocked(ft, msg, int32(i+1), storageName, nested, //nolint:gosec
			descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL); err != nil {
			return err
		}
	}
	p.messages = append(p.messages, msg)
	p.compiled = nil
	return nil
}

// defineArrayLocked ports Type.Array.defineProtoType (Type.java:3251-3262).
// A NULLABLE array of a known element type is represented by a synthetic
// single-field wrapper message `{ repeated E values = 1; }` — the
// NullableArrayWrapper identity — because a bare repeated field cannot
// distinguish "empty array" from NULL. A non-nullable array flattens into its
// parent as a repeated field and defines only its element type.
func (p *TypeProtoRepository) defineArrayLocked(at *ArrayType) error {
	if at.ElementType == nil {
		return &ProtoTypeError{TypeName: at.String(), Reason: "erased array (no element type)"}
	}
	// Java registers the array's OWN unique name first, unconditionally, and
	// then defines whatever the array's representation needs.
	p.registerLocked(at, p.uniqueTypeNameLocked())
	if at.Nullable && at.ElementType.Code() != TypeCodeUnknown {
		_, err := p.defineAndResolveLocked(nullableArrayWrapperType(at.ElementType))
		return err
	}
	_, err := p.defineAndResolveLocked(at.ElementType)
	return err
}

// nullableArrayWrapperType builds Java's wrapper record
// `Record.fromFields(List.of(Field.of(new Array(elementType), "values")))`
// (Type.java:3256, :3273). The wrapper's single field is a NON-nullable array
// so it emits as the flat repeated field the wrapper exists to hold.
func nullableArrayWrapperType(elem Type) *RecordType {
	return &RecordType{
		Nullable: true,
		Fields: []Field{{
			Name:      wrappedArrayFieldName,
			FieldType: &ArrayType{Nullable: false, ElementType: elem},
			Ordinal:   0,
		}},
	}
}

// addProtoFieldLocked is Java's Type.addProtoField dispatch: it appends ONE
// FieldDescriptorProto to descriptorBuilder describing a field OF type t.
//
// The three arms differ in exactly the way Java's do:
//   - a primitive sets `type` and never `type_name` (Type.java:1067-1080);
//   - a record or UUID sets `type_name` and never an explicit TYPE_MESSAGE
//     (Type.java:2374-2388, :3531-3540) — matching the stored-schema
//     descriptors Java writes, where a message field carries only its name;
//   - an array either flattens to LABEL_REPEATED (non-nullable) or becomes an
//     OPTIONAL reference to the wrapper message (nullable) (Type.java:3267-3291).
func (p *TypeProtoRepository) addProtoFieldLocked(
	t Type,
	msg *descriptorpb.DescriptorProto,
	number int32,
	fieldName string,
	typeName string,
	label descriptorpb.FieldDescriptorProto_Label,
) error {
	f := &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(fieldName),
		Number: proto.Int32(number),
		Label:  label.Enum(),
	}

	if at, isArray := t.(*ArrayType); isArray {
		if at.ElementType == nil {
			return &ProtoTypeError{TypeName: at.String(), Reason: "erased array (no element type)"}
		}
		if at.Nullable && at.ElementType.Code() != TypeCodeUnknown {
			wrapper := nullableArrayWrapperType(at.ElementType)
			wrapperName, err := p.defineAndResolveLocked(wrapper)
			if err != nil {
				return err
			}
			// The wrapper reference is always OPTIONAL, whatever label the
			// caller asked for — Java passes LABEL_OPTIONAL explicitly here.
			return p.addProtoFieldLocked(wrapper, msg, number, fieldName, wrapperName,
				descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
		}
		// Flat: the element's own field shape, repeated, straight into the parent.
		elemName, err := p.defineAndResolveLocked(at.ElementType)
		if err != nil {
			return err
		}
		return p.addProtoFieldLocked(at.ElementType, msg, number, fieldName, elemName,
			descriptorpb.FieldDescriptorProto_LABEL_REPEATED)
	}

	switch t.Code() {
	case TypeCodeRecord:
		if typeName == "" {
			return &ProtoTypeError{TypeName: t.String(), Reason: "record type was not defined"}
		}
		f.TypeName = proto.String(typeName)
	case TypeCodeUuid:
		f.TypeName = proto.String(uuidProtoTypeName)
	case TypeCodeEnum:
		// Java emits an EnumDescriptorProto here. Go's plan-time computed
		// records cannot carry one: no expression in this engine produces a
		// values.EnumType (enums arrive only from a STORED descriptor, which
		// already has its own). Loud rather than silently emitting a wrong
		// shape — if an expression ever does produce one, this is where the
		// EnumDescriptorProto arm goes.
		return &ProtoTypeError{
			TypeName: t.String(),
			Reason:   "enum types have no synthesised form; they come from a stored descriptor",
		}
	default:
		pt, ok := primitiveProtoType(t.Code())
		if !ok {
			return &ProtoTypeError{TypeName: t.String(), Reason: "no protobuf representation"}
		}
		f.Type = pt.Enum()
	}
	msg.Field = append(msg.Field, f)
	return nil
}

// primitiveProtoType is Java's TypeCode.getProtoType table. The codes with no
// protobuf form (RELATION, FUNCTION, ANY, UNKNOWN) report !ok so the caller
// raises a ProtoTypeError naming the type, rather than emitting a field whose
// shape is a guess.
//
// NULL and NONE map to a placeholder INT64: they are the types of the literal
// NULL and of the untyped empty array, and a field of either is only ever
// ABSENT in the built message, so the declared scalar kind is never read. They
// still need a kind, because a field with neither `type` nor `type_name` does
// not compile.
func primitiveProtoType(code TypeCode) (descriptorpb.FieldDescriptorProto_Type, bool) {
	switch code {
	case TypeCodeBoolean:
		return descriptorpb.FieldDescriptorProto_TYPE_BOOL, true
	case TypeCodeInt:
		return descriptorpb.FieldDescriptorProto_TYPE_INT32, true
	case TypeCodeLong:
		return descriptorpb.FieldDescriptorProto_TYPE_INT64, true
	case TypeCodeFloat:
		return descriptorpb.FieldDescriptorProto_TYPE_FLOAT, true
	case TypeCodeDouble:
		return descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, true
	case TypeCodeString:
		return descriptorpb.FieldDescriptorProto_TYPE_STRING, true
	case TypeCodeBytes:
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES, true
	case TypeCodeVersion:
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES, true
	case TypeCodeNull, TypeCodeNone:
		return descriptorpb.FieldDescriptorProto_TYPE_INT64, true
	default:
		return 0, false
	}
}

// syntheticFileName names the synthesised file. It is never persisted and
// never referenced by another file, so any stable name serves; a distinctive
// one makes it obvious in a descriptor dump where a message came from.
const syntheticFileName = "__computed_types__.proto"

// compileLocked turns the accumulated DescriptorProtos into a live
// FileDescriptor — Java's Builder.build() plus its DescriptorValidationException
// handling. The tuple_fields dependency is what makes a UUID field's
// type_name resolvable.
func (p *TypeProtoRepository) compileLocked() (protoreflect.FileDescriptor, error) {
	if p.compiled != nil {
		return p.compiled, nil
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String(syntheticFileName),
		Dependency:  []string{gen.File_tuple_fields_proto.Path()},
		MessageType: p.messages,
	}
	resolver := &protoregistry.Files{}
	// Duplicate registration is not an error worth surfacing: the global
	// registry already holds this file and all we need is local visibility.
	_ = resolver.RegisterFile(gen.File_tuple_fields_proto)

	// The synthesised names are RELATIVE, exactly as Java writes them; the
	// protodesc resolver wants absolute ones. Absolutizing a CLONE keeps
	// p.messages in the form the emitter (and any test asserting on it) sees.
	buildable := proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	absolutizeSyntheticTypeNames(buildable)
	fd, err := protodesc.NewFile(buildable, resolver)
	if err != nil {
		return nil, &ProtoTypeError{
			TypeName: syntheticFileName,
			Reason:   fmt.Sprintf("descriptor validation failed: %v", err),
		}
	}
	p.compiled = fd
	return fd, nil
}

// absolutizeSyntheticTypeNames rewrites each field's relative type_name to the
// leading-dot absolute form protodesc requires. A name that already resolves
// against a dependency (the fully-qualified UUID message) is absolutized in
// place; a name that is one of THIS file's own messages gets no package
// prefix, because the synthesised file declares none.
func absolutizeSyntheticTypeNames(fdp *descriptorpb.FileDescriptorProto) {
	for _, msg := range fdp.MessageType {
		for _, f := range msg.Field {
			if f.TypeName == nil || len(*f.TypeName) == 0 || (*f.TypeName)[0] == '.' {
				continue
			}
			f.TypeName = proto.String("." + *f.TypeName)
		}
	}
}

// FileDescriptorProtoForTest exposes the accumulated file in its RELATIVE,
// pre-compilation form. It exists so the descriptor SHAPE — labels, field
// numbers, the presence of type vs type_name, the wrapper's structure — can
// be asserted directly rather than inferred from a round-trip, which is where
// a shape divergence from Java would hide.
func (p *TypeProtoRepository) FileDescriptorProtoForTest() *descriptorpb.FileDescriptorProto {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &descriptorpb.FileDescriptorProto{
		Name:        proto.String(syntheticFileName),
		Dependency:  []string{gen.File_tuple_fields_proto.Path()},
		MessageType: p.messages,
	}
}
