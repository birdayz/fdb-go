package values

// Shape pins for the plan-time descriptor emitter (proto_type.go).
//
// Every assertion here is a choice copied from a named Java method, and each
// is asserted on the EMITTED DescriptorProto rather than on a round-trip
// through a built message. That is deliberate: a round-trip proves the
// descriptor is self-consistent, which it would be with the label, the field
// number, or the type/type_name split all wrong. The shape is what has to
// match Java, so the shape is what is checked.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer/protoname"
)

// findMsg returns the emitted message with the given name.
func findMsg(t *testing.T, p *TypeProtoRepository, name string) *descriptorpb.DescriptorProto {
	t.Helper()
	for _, m := range p.FileDescriptorProtoForTest().MessageType {
		if m.GetName() == name {
			return m
		}
	}
	t.Fatalf("no emitted message %q; have %v", name, msgNames(p))
	return nil
}

func msgNames(p *TypeProtoRepository) []string {
	var out []string
	for _, m := range p.FileDescriptorProtoForTest().MessageType {
		out = append(out, m.GetName())
	}
	return out
}

// A COMPUTED record — the anonymous *RecordType RecordConstructorValue.Type()
// returns for `(1, 1.0, 'a', true)` — is the whole reason this emitter exists,
// so it is the first thing pinned: ordinal field names, field numbers at
// ordinal+1, and LABEL_OPTIONAL on every field (Type.java:2339-2356 passes
// LABEL_OPTIONAL unconditionally).
func TestProtoType_AnonymousComputedRecord(t *testing.T) {
	t.Parallel()
	rt := &RecordType{Nullable: true, Fields: []Field{
		{Name: "_0", FieldType: NotNullLong, Ordinal: 0},
		{Name: "_1", FieldType: NotNullDouble, Ordinal: 1},
		{Name: "_2", FieldType: NotNullString, Ordinal: 2},
		{Name: "_3", FieldType: NotNullBoolean, Ordinal: 3},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(rt)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	if md.Fields().Len() != 4 {
		t.Fatalf("want 4 fields, got %d", md.Fields().Len())
	}

	// The synthetic name is Java's "__type__" shape in the namespace no user
	// identifier can reach (see the file header), with a deterministic suffix.
	if got := string(md.Name()); got != "__0type__1" {
		t.Errorf("synthetic type name = %q, want %q", got, "__0type__1")
	}

	msg := findMsg(t, p, "__0type__1")
	wantTypes := []descriptorpb.FieldDescriptorProto_Type{
		descriptorpb.FieldDescriptorProto_TYPE_INT64,
		descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
		descriptorpb.FieldDescriptorProto_TYPE_STRING,
		descriptorpb.FieldDescriptorProto_TYPE_BOOL,
	}
	for i, f := range msg.Field {
		if want := OrdinalFieldName(i); f.GetName() != want {
			t.Errorf("field %d name = %q, want %q", i, f.GetName(), want)
		}
		// Java's normalizeFields: fieldIndex = ordinal + 1.
		if f.GetNumber() != int32(i+1) {
			t.Errorf("field %d number = %d, want %d", i, f.GetNumber(), i+1)
		}
		if f.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL {
			t.Errorf("field %d label = %v, want LABEL_OPTIONAL", i, f.GetLabel())
		}
		if f.GetType() != wantTypes[i] {
			t.Errorf("field %d type = %v, want %v", i, f.GetType(), wantTypes[i])
		}
		// A primitive sets `type` and NEVER `type_name` (Type.java:1067-1080).
		if f.TypeName != nil {
			t.Errorf("field %d carries type_name %q; a primitive must not", i, f.GetTypeName())
		}
	}
}

// A record field whose type is itself a record: the outer field must carry
// ONLY type_name, with no explicit TYPE_MESSAGE. That is Java's
// Record.addProtoField (Type.java:2374-2388), and it is the same shape the
// stored-schema descriptors use — a message field named, never typed.
func TestProtoType_NestedRecordFieldCarriesOnlyTypeName(t *testing.T) {
	t.Parallel()
	inner := &RecordType{Nullable: true, Fields: []Field{
		{Name: "X", FieldType: NotNullLong, Ordinal: 0},
	}}
	outer := &RecordType{Nullable: true, Fields: []Field{
		{Name: "N", FieldType: inner, Ordinal: 0},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(outer)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	f := md.Fields().Get(0)
	if f.Kind() != 11 /* MessageKind */ {
		t.Fatalf("nested field kind = %v, want message", f.Kind())
	}
	if f.Message().Fields().Len() != 1 || string(f.Message().Fields().Get(0).Name()) != "X" {
		t.Errorf("nested message shape wrong: %v", f.Message().FullName())
	}

	outerMsg := findMsg(t, p, string(md.Name()))
	raw := outerMsg.Field[0]
	if raw.Type != nil {
		t.Errorf("nested record field sets type %v; Java sets only type_name", raw.GetType())
	}
	if raw.GetTypeName() == "" {
		t.Error("nested record field has no type_name")
	}
}

// The NullableArrayWrapper identity (Type.java:3251-3291). A NULLABLE array
// cannot be a bare repeated field, because repeated cannot distinguish an
// empty array from NULL; Java wraps it in a synthetic single-field message
// `{ repeated E values = 1; }` and points an OPTIONAL field at that. A
// NON-nullable array flattens straight into the parent as LABEL_REPEATED.
// Both arms are checked together because it is the CONTRAST that is the rule.
func TestProtoType_ArrayNullabilityDecidesWrapperVsFlat(t *testing.T) {
	t.Parallel()
	rt := &RecordType{Nullable: true, Fields: []Field{
		{Name: "NULLABLE_ARR", FieldType: &ArrayType{Nullable: true, ElementType: NotNullLong}, Ordinal: 0},
		{Name: "FLAT_ARR", FieldType: &ArrayType{Nullable: false, ElementType: NotNullLong}, Ordinal: 1},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(rt)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	msg := findMsg(t, p, string(md.Name()))

	nullableField := msg.Field[0]
	if nullableField.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL {
		t.Errorf("nullable array field label = %v, want LABEL_OPTIONAL (it points at the wrapper)",
			nullableField.GetLabel())
	}
	if nullableField.GetTypeName() == "" {
		t.Fatal("nullable array field has no type_name; the wrapper message was not referenced")
	}
	wrapper := findMsg(t, p, nullableField.GetTypeName())
	if len(wrapper.Field) != 1 {
		t.Fatalf("wrapper has %d fields, want exactly 1", len(wrapper.Field))
	}
	wf := wrapper.Field[0]
	// The LITERAL "values", not the constant: Java's
	// NullableArrayTypeUtils.getRepeatedFieldName() is this exact string, and
	// every reader of a wrapped array identifies it STRUCTURALLY by this field
	// name (the message name is a random UUID in Java, so the name cannot be
	// the key). Asserting against wrappedArrayFieldName instead would compare
	// the constant to itself and pass with any value in it.
	if wf.GetName() != "values" {
		t.Errorf("wrapper field name = %q, want %q — the wrapper identity is STRUCTURAL, "+
			"so this name is the thing readers key on", wf.GetName(), "values")
	}
	if wrappedArrayFieldName != "values" {
		t.Errorf("wrappedArrayFieldName = %q, want %q (Java's "+
			"NullableArrayTypeUtils.getRepeatedFieldName)", wrappedArrayFieldName, "values")
	}
	if wf.GetNumber() != 1 {
		t.Errorf("wrapper field number = %d, want 1", wf.GetNumber())
	}
	if wf.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		t.Errorf("wrapper field label = %v, want LABEL_REPEATED", wf.GetLabel())
	}

	flatField := msg.Field[1]
	if flatField.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		t.Errorf("non-nullable array field label = %v, want LABEL_REPEATED (flattened into the parent)",
			flatField.GetLabel())
	}
	if flatField.TypeName != nil {
		t.Errorf("flat array field carries type_name %q; its element is a primitive", flatField.GetTypeName())
	}
	if flatField.GetType() != descriptorpb.FieldDescriptorProto_TYPE_INT64 {
		t.Errorf("flat array field type = %v, want TYPE_INT64", flatField.GetType())
	}
}

// A UUID field carries the fully-qualified tuple_fields message name and no
// explicit TYPE_MESSAGE (Type.java:3531-3540). The dependency on
// tuple_fields.proto is what makes it resolve.
func TestProtoType_UuidFieldNamesTheTupleFieldsMessage(t *testing.T) {
	t.Parallel()
	rt := &RecordType{Nullable: true, Fields: []Field{
		{Name: "U", FieldType: NullableUuid, Ordinal: 0},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(rt)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	if got := string(md.Fields().Get(0).Message().FullName()); got != uuidProtoTypeName {
		t.Errorf("UUID field resolves to %q, want %q", got, uuidProtoTypeName)
	}
	raw := findMsg(t, p, string(md.Name())).Field[0]
	if raw.Type != nil {
		t.Errorf("UUID field sets type %v; Java sets only type_name", raw.GetType())
	}
	if raw.GetTypeName() != uuidProtoTypeName {
		t.Errorf("UUID type_name = %q, want %q", raw.GetTypeName(), uuidProtoTypeName)
	}
}

// Java's addTypeIfNeeded dedups on the CANONICAL-nullability form
// (TypeRepository.java:292-298, :498-503): a field's nullability lives on the
// referencing field's label, never on the referenced message, so a nullable
// and a non-nullable use of one shape share ONE descriptor. Without the
// canonicalization each use would mint its own message — the cache would
// still "work", so nothing but this test distinguishes the two.
func TestProtoType_DedupsAcrossNullabilityAndRepeatedCalls(t *testing.T) {
	t.Parallel()
	mk := func(nullable bool) *RecordType {
		return &RecordType{Nullable: nullable, Fields: []Field{
			{Name: "A", FieldType: NotNullLong, Ordinal: 0},
		}}
	}
	p := NewTypeProtoRepository()
	first, err := p.MessageDescriptorFor(mk(true))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := p.MessageDescriptorFor(mk(true))
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	if first != again {
		t.Error("an identical type minted a second descriptor; addTypeIfNeeded must dedup")
	}
	flipped, err := p.MessageDescriptorFor(mk(false))
	if err != nil {
		t.Fatalf("flipped: %v", err)
	}
	if first != flipped {
		t.Error("the non-nullable variant minted its own descriptor; " +
			"canonicalizeNullability must collapse the two")
	}
	if n := len(msgNames(p)); n != 1 {
		t.Errorf("emitted %d messages (%v), want exactly 1", n, msgNames(p))
	}
}

// Names reaching the descriptor go through the SAME escaping as the stored
// schema (pkg/recordlayer/protoname, Java's ProtoUtils). The witnesses are the
// live-JVM measured ones the stored-schema tests already use, so a divergence
// between the two emitters shows up as this test failing rather than as two
// spellings of one name.
func TestProtoType_EscapesNamesLikeTheStoredSchemaEmitter(t *testing.T) {
	t.Parallel()
	rt := &RecordType{RecordName: "x$$", Nullable: true, Fields: []Field{
		{Name: "foo.tableA", FieldType: NotNullLong, Ordinal: 0},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(rt)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	if got := string(md.Name()); got != "x__1__1" {
		t.Errorf("record name escaped to %q, want %q", got, "x__1__1")
	}
	if got := string(md.Fields().Get(0).Name()); got != "foo__2tableA" {
		t.Errorf("field name escaped to %q, want %q", got, "foo__2tableA")
	}
}

// One shape reached TWICE must be defined once and referenced twice — Java's
// addTypeIfNeeded seeing the type already in its BiMap on the second visit.
// This is the reachable half of the register-before-recurse ordering; the
// unreachable half is the subject of the negative result below.
func TestProtoType_RepeatedNestedShapeIsDefinedOnce(t *testing.T) {
	t.Parallel()
	inner := func() *RecordType {
		return &RecordType{Nullable: true, Fields: []Field{
			{Name: "X", FieldType: NotNullLong, Ordinal: 0},
		}}
	}
	outer := &RecordType{Nullable: true, Fields: []Field{
		{Name: "L", FieldType: inner(), Ordinal: 0},
		{Name: "R", FieldType: inner(), Ordinal: 1},
	}}
	p := NewTypeProtoRepository()
	md, err := p.MessageDescriptorFor(outer)
	if err != nil {
		t.Fatalf("MessageDescriptorFor: %v", err)
	}
	l, r := md.Fields().Get(0).Message(), md.Fields().Get(1).Message()
	if l.FullName() != r.FullName() {
		t.Errorf("the same shape got two descriptors (%q, %q); addTypeIfNeeded must dedup",
			l.FullName(), r.FullName())
	}
	// outer + the single shared inner.
	if n := len(msgNames(p)); n != 2 {
		t.Errorf("emitted %d messages (%v), want 2", n, msgNames(p))
	}
}

// NEGATIVE RESULT, pinned at the only depth that can be probed without
// crashing the process: RecordType.Equals has NO identity short-circuit, so it
// descends structurally even when both sides are the SAME pointer.
//
// That is the fact which makes a cyclic record type fatal rather than merely
// unsupported — and it is why this emitter has no cycle guard. Java is
// identical (Record.equals is `Objects.requireNonNull(fields).equals(
// otherType.fields)` → Field.equals → getFieldType().equals, with no visited
// set), so a type that reaches itself blows the stack in BOTH engines the
// moment anything compares it, long before a descriptor is asked for. A
// Go-only guard here would be a divergence dressed as a fix.
//
// If Equals ever gains a fast path, this test fails, and that is the signal to
// re-examine whether cyclic types have become constructible — at which point
// TypeProtoRepository's Equals-based cache lookup is one of the places that
// needs a guard. The cyclic type itself is deliberately NOT constructed here:
// a Go stack overflow is a fatal error, not a recoverable panic, so a probe
// that built one would take the whole test binary with it.
func TestProtoType_RecordEqualsHasNoIdentityShortCircuit(t *testing.T) {
	t.Parallel()
	// A sentinel element type that COUNTS the comparisons Equals performs.
	// If Equals short-circuited on pointer identity, the count would be 0.
	counter := &countingType{}
	rt := &RecordType{RecordName: "R", Nullable: true, Fields: []Field{
		{Name: "A", FieldType: counter, Ordinal: 0},
	}}
	if !rt.Equals(rt) {
		t.Fatal("a record type is not equal to itself")
	}
	if counter.n == 0 {
		t.Fatal("RecordType.Equals short-circuited on pointer identity. That is a behaviour " +
			"change vs Java's Record.equals, which always descends — re-check whether cyclic " +
			"types are now constructible, and whether TypeProtoRepository's cache lookup " +
			"needs a cycle guard.")
	}
}

// countingType records how many times Equals descended into it.
type countingType struct{ n int }

func (c *countingType) Code() TypeCode   { return TypeCodeLong }
func (c *countingType) IsNullable() bool { return false }
func (c *countingType) String() string   { return "COUNTING" }
func (c *countingType) Equals(other Type) bool {
	c.n++
	_, ok := other.(*countingType)
	return ok
}

// A type with no protobuf form is LOUD. The alternative — emitting a field
// with a guessed kind — is a silently wrong descriptor, and a silently wrong
// descriptor produces silently wrong rows.
func TestProtoType_TypesWithNoProtoFormAreLoud(t *testing.T) {
	t.Parallel()
	p := NewTypeProtoRepository()
	if _, err := p.MessageDescriptorFor(nil); err == nil {
		t.Error("nil type: want an error")
	}
	// A primitive has no MESSAGE form at all.
	if _, err := p.MessageDescriptorFor(NotNullLong); err == nil {
		t.Error("primitive: want an error, a scalar has no message descriptor")
	}
	// An erased array has no element type to emit.
	if _, err := p.MessageDescriptorFor(&RecordType{Nullable: true, Fields: []Field{
		{Name: "A", FieldType: &ArrayType{Nullable: true}, Ordinal: 0},
	}}); err == nil {
		t.Error("erased array: want an error")
	}
}

// TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier pins the property the
// register-time guard rests on: a synthetic name can never be the escaped form
// of a user identifier, so an entry already holding a name is always another
// DECLARED shape and never an anonymous squatter.
//
// The witnesses are the collision that existed while the synthetic prefix was
// `__type__`: `__type$` escapes to exactly `__type__1`, and with an anonymous
// record minted first that query was refused as a name clash it is not (Java
// names its anonymous types by random UUID and runs it), while with the
// declared struct first the file failed to validate and EVERY record in the
// plan fell back to a map.
func TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier(t *testing.T) {
	t.Parallel()

	// No accepted identifier escapes into the synthetic namespace...
	for _, identifier := range []string{"__type$", "__type.", "__type$0", "__type__x", "_$", "a.b", "x$$"} {
		escaped, err := protoname.ToProtoBufCompliantName(identifier)
		if err != nil {
			t.Fatalf("%q: escaping refused it: %v", identifier, err)
		}
		if strings.HasPrefix(escaped, syntheticTypeNamePrefix) {
			t.Fatalf("%q escapes to %q, inside the synthetic namespace %q", identifier, escaped, syntheticTypeNamePrefix)
		}
	}
	// ...because an identifier that would have to start with one is refused.
	if _, err := protoname.ToProtoBufCompliantName(syntheticTypeNamePrefix + "1"); err == nil {
		t.Fatalf("the identifier %q was accepted; the synthetic namespace is reachable after all", syntheticTypeNamePrefix+"1")
	}

	// The witness, both orders, in one repository: an anonymous record and a
	// struct declared `__type$` both get a descriptor, under distinct names.
	// `__type$` is the witness: it ESCAPES to `__type__1`, the name the counter
	// minted under the old prefix. A record declared `__type__1` escapes to
	// `__type__01` and never collided, so it would make this arm vacuous.
	declared := NewRecordType("__type$", false, []Field{{Name: "A", FieldType: NullableLong, Ordinal: 0}})
	anonymous := NewRecordType("", false, []Field{{Name: "B", FieldType: NullableLong, Ordinal: 0}})
	for _, order := range [][2]Type{{anonymous, declared}, {declared, anonymous}} {
		repo := NewTypeProtoRepository()
		first, err := repo.MessageDescriptorFor(order[0])
		if err != nil {
			t.Fatalf("first type %v: %v", order[0], err)
		}
		second, err := repo.MessageDescriptorFor(order[1])
		if err != nil {
			t.Fatalf("second type %v after %v: %v", order[1], order[0], err)
		}
		if first.Name() == second.Name() {
			t.Fatalf("both types were named %q", first.Name())
		}
	}

	// The genuine clash is still refused: one DECLARED name, two shapes.
	repo := NewTypeProtoRepository()
	one := NewRecordType("FOO", false, []Field{{Name: "P", FieldType: NullableLong, Ordinal: 0}})
	two := NewRecordType("FOO", false, []Field{
		{Name: "P", FieldType: NullableLong, Ordinal: 0},
		{Name: "Q", FieldType: NullableLong, Ordinal: 1},
	})
	if _, err := repo.MessageDescriptorFor(one); err != nil {
		t.Fatalf("first FOO: %v", err)
	}
	var clash *DeclaredNameClashError
	if _, err := repo.MessageDescriptorFor(two); !errors.As(err, &clash) || clash.Name != "FOO" {
		t.Fatalf("second FOO shape = %v, want a DeclaredNameClashError for FOO", err)
	}
}

// A failed root must not publish partial names or invalidate already-bound
// descriptors; later valid roots must still be admissible.
func TestDuplicateFieldNameRowRollsBackRegistration(t *testing.T) {
	t.Parallel()
	repo := NewTypeProtoRepository()
	early := NewRecordType("", false, []Field{{Name: "A", FieldType: NullableLong}})
	earlyDesc, err := repo.MessageDescriptorFor(early)
	if err != nil {
		t.Fatal(err)
	}
	before := repo.FileDescriptorProtoForTest()
	twice := &RecordType{RecordName: "FailedRoot", Fields: []Field{
		{Name: "ID", FieldType: NullableLong}, {Name: "ID", FieldType: NullableLong, Ordinal: 1},
	}}
	if err := repo.RegisterType(twice); err == nil {
		t.Fatal("duplicate field names must fail validation")
	}
	if got := repo.FileDescriptorProtoForTest(); !proto.Equal(before, got) {
		t.Fatal("failed closure changed repository definitions")
	}
	again, err := repo.MessageDescriptorFor(early)
	if err != nil || again != earlyDesc {
		t.Fatalf("failed registration invalidated bound descriptor: %v", err)
	}
	valid := NewRecordType("FailedRoot", false, []Field{{Name: "B", FieldType: NullableString}})
	if err := repo.RegisterType(valid); err != nil {
		t.Fatalf("partial name survived rollback: %v", err)
	}
	if err := repo.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MessageDescriptorFor(valid); err != nil {
		t.Fatalf("valid root after failed root: %v", err)
	}
}

func TestProtoTypeSealedGraphIdentity(t *testing.T) {
	t.Parallel()
	for _, childFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(childFirst), func(t *testing.T) {
			t.Parallel()
			child := NewRecordType("Child", false, []Field{{Name: "N", FieldType: NotNullLong}})
			enum := NewEnumType("Color", true, []EnumValue{{Name: "RED", Number: 7}, {Name: "BLUE", Number: 12}})
			parent := NewRecordType("Parent", false, []Field{
				{Name: "C", FieldType: child},
				{Name: "A", FieldType: &ArrayType{Nullable: true, ElementType: child}},
				{Name: "E", FieldType: enum},
			})
			other := NewRecordType("Other", false, []Field{{Name: "S", FieldType: NotNullString}})
			repo := NewTypeProtoRepository()
			order := []Type{parent, other, child}
			if childFirst {
				order = []Type{child, other, parent}
			}
			for _, typ := range order {
				if err := repo.RegisterType(typ); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.Seal(); err != nil {
				t.Fatal(err)
			}
			pd, err := repo.MessageDescriptorFor(parent)
			if err != nil {
				t.Fatal(err)
			}
			cd, err := repo.MessageDescriptorFor(child)
			if err != nil {
				t.Fatal(err)
			}
			if pd.Fields().Get(0).Message() != cd {
				t.Fatal("child and parent's field use different descriptors")
			}
			wrapper := pd.Fields().Get(1).Message()
			if wrapper.Fields().Get(0).Message() != cd {
				t.Fatal("nullable wrapper lost child descriptor identity")
			}
			ad, err := repo.MessageDescriptorFor(&ArrayType{Nullable: true, ElementType: child})
			if err != nil || ad != wrapper {
				t.Fatalf("array lookup differs from parent wrapper: %v", err)
			}
			ed := pd.Fields().Get(2).Enum()
			if ed == nil || ed.Values().ByNumber(7).Name() != "RED" {
				t.Fatal("enum definition lost declared numbers")
			}
			value, err := rowScalarToProtoValue(pd.Fields().Get(2), int64(12))
			if err != nil || value.Enum() != 12 {
				t.Fatalf("enum row rebinding: %v", err)
			}
			late := NewRecordType("Late", false, []Field{{Name: "L", FieldType: NotNullDouble}})
			var sealed *SealedTypeRepositoryError
			if err := repo.RegisterType(late); !errors.As(err, &sealed) {
				t.Fatalf("late registration = %v", err)
			}
			for i := 0; i < 4; i++ {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					t.Parallel()
					again, err := repo.MessageDescriptorFor(parent)
					if err != nil || again != pd {
						t.Fatalf("sealed lookup changed descriptor: %v", err)
					}
				})
			}
		})
	}
}

func TestProtoTypeIncrementalInvalidation(t *testing.T) {
	t.Parallel()
	child := NewRecordType("Child", false, []Field{{Name: "N", FieldType: NotNullLong}})
	parent := NewRecordType("Parent", false, []Field{{Name: "C", FieldType: child}})
	repo := NewTypeProtoRepository()
	old, err := repo.MessageDescriptorFor(child)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := repo.MessageDescriptorFor(parent)
	if err != nil {
		t.Fatal(err)
	}
	cd, err := repo.MessageDescriptorFor(child)
	if err != nil {
		t.Fatal(err)
	}
	if pd.Fields().Get(0).Message() != cd || old == cd {
		t.Fatal("incremental registration retained a stale child descriptor")
	}
}

func TestProtoTypeFailedLookupPreservesGraph(t *testing.T) {
	t.Parallel()
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			t.Parallel()
			repo := NewTypeProtoRepository()
			valid := NewRecordType("Valid", false, []Field{{Name: "N", FieldType: NotNullLong}})
			if err := repo.RegisterType(valid); err != nil {
				t.Fatal(err)
			}
			unknownArray := NewArrayType(true, UnknownType)
			// A successful registration promises that every descriptor needed
			// by lookup is already representable. Continue after a violation to
			// exercise the failed lookup on precisely that incomplete closure.
			var invalid *ProtoTypeError
			if err := repo.RegisterType(unknownArray); !errors.As(err, &invalid) {
				t.Errorf("register ARRAY<UNKNOWN> = %v, want ProtoTypeError", err)
			}
			if sealed {
				if err := repo.Seal(); err != nil {
					t.Fatal(err)
				}
			}
			desc, err := repo.MessageDescriptorFor(valid)
			if err != nil {
				t.Fatal(err)
			}
			before := proto.Clone(repo.FileDescriptorProtoForTest())
			counter, entries, compiled := repo.counter, len(repo.entries), repo.compiled
			for _, bad := range []Type{unknownArray, NewArrayType(false, UnknownType), UnknownType, NotNullLong, (*RecordType)(nil), nil} {
				if got, err := repo.MessageDescriptorFor(bad); got != nil || err == nil {
					t.Errorf("lookup %T = %v, %v; want no descriptor and an error", bad, got, err)
				}
				if repo.counter != counter || len(repo.entries) != entries || repo.compiled != compiled || !proto.Equal(before, repo.FileDescriptorProtoForTest()) {
					t.Fatalf("failed lookup of %T changed repository state (sealed=%v)", bad, sealed)
				}
				again, err := repo.MessageDescriptorFor(valid)
				if err != nil || again != desc {
					t.Fatalf("failed lookup of %T changed descriptor identity: %v", bad, err)
				}
			}
		})
	}
}
