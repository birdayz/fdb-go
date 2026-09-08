package values

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Java NullableArrayTypeUtils.unwrapIfArray leaves an existing List alone
// under either declaration, but only unwraps a Message for a nullable ARRAY.
// Read through FieldValue too: output admission and field descent must agree.
func TestProtoRecordAdmissionArrayStorage(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{0, 2} {
		for _, wrapped := range []bool{false, true} {
			for _, nullable := range []bool{false, true} {
				for _, payload := range []string{"absent", "empty", "populated"} {
					t.Run(fmt.Sprintf("depth%d/wrapped%v/nullable%v/%s", depth, wrapped, nullable, payload), func(t *testing.T) {
						t.Parallel()
						message, declared, path := protoArrayAdmissionFixture(t, depth, wrapped, nullable, payload)
						handle, err := SnapshotExactType(declared)
						if err != nil {
							t.Fatal(err)
						}
						wantCompatible := !wrapped || nullable
						for range 2 { // exercise both the initial verdict and its cache hit
							if got := ProtoRecordDescriptorCompatible(message.Descriptor(), handle); got != wantCompatible {
								t.Fatalf("descriptor admission = %v, want %v", got, wantCompatible)
							}
						}
						qov := mustQOV(t, NamedCorrelationIdentifier("array_storage"), declared)
						field, err := ResolveFieldOrdinals(qov, path)
						if err != nil {
							t.Fatal(err)
						}
						got, err := field.Evaluate(&RowEvalContext{Correlations: &ordEvalBinder{id: qov.Correlation(), bound: message.Interface()}})
						if !wantCompatible {
							var resolution *ResolutionError
							if !errors.As(err, &resolution) || resolution.ErrorCode != LayoutRuntimeShape {
								t.Fatalf("non-nullable ARRAY read a wrapper: value=%v error=%v", got, err)
							}
							return
						}
						var want any = []any{}
						if payload == "populated" {
							want = []any{int64(9)}
						}
						if payload == "absent" && wrapped {
							want = nil
						}
						if err != nil || !reflect.DeepEqual(got, want) {
							t.Fatalf("resolved nested ARRAY = (%#v, %v), want %#v", got, err, want)
						}
					})
				}
			}
		}
	}
}

func protoArrayAdmissionFixture(t testing.TB, depth int, wrapped, nullable bool, payload string) (protoreflect.Message, *RecordType, []int) {
	t.Helper()
	physical := NewRecordType("Stored", false, []Field{
		{Name: "id", FieldType: NotNullLong, Ordinal: 0},
		{Name: "items", FieldType: NewArrayType(wrapped, NotNullLong), Ordinal: 1},
	})
	declared := NewRecordType("Logical", true, []Field{
		{Name: "LOGICAL_ID", FieldType: NullableLong, Ordinal: 0},
		{Name: "LOGICAL_ITEMS", FieldType: NewArrayType(nullable, NullableLong), Ordinal: 1},
	})
	for range depth {
		physical = NewRecordType("", false, []Field{{Name: "child", FieldType: physical, Ordinal: 0}})
		declared = NewRecordType("", true, []Field{{Name: "LOGICAL_CHILD", FieldType: declared, Ordinal: 0}})
	}
	descriptor, err := NewTypeProtoRepository().MessageDescriptorFor(physical)
	if err != nil {
		t.Fatal(err)
	}
	message := dynamicpb.NewMessage(descriptor)
	var leaf protoreflect.Message = message
	path := make([]int, depth+1)
	path[depth] = 1
	for range depth {
		leaf = leaf.Mutable(leaf.Descriptor().Fields().Get(0)).Message()
	}
	leaf.Set(leaf.Descriptor().Fields().Get(0), protoreflect.ValueOfInt64(7))
	items := leaf.Descriptor().Fields().Get(1)
	_, gotWrapped, ok := EffectiveListField(items)
	if !ok || gotWrapped != wrapped {
		t.Fatalf("fixture array storage = (%v, %v), want wrapped=%v", gotWrapped, ok, wrapped)
	}
	if payload != "absent" {
		var list protoreflect.List
		if wrapped {
			wrapper, wrappedList := NewWrappedArrayMessage(items)
			leaf.Set(items, protoreflect.ValueOfMessage(wrapper))
			list = wrappedList
		} else {
			list = leaf.Mutable(items).List()
		}
		if payload == "populated" {
			list.Append(protoreflect.ValueOfInt64(9))
		}
	}
	return message, declared, path
}

func TestProtoRecordDescriptorAdmissionRejectsMalformedDeclarations(t *testing.T) {
	t.Parallel()
	message, declared, _ := protoArrayAdmissionFixture(t, 0, false, true, "populated")
	for _, shape := range []string{"nil", "scalar", "width", "leaf", "element", "duplicate", "nested_duplicate"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			candidate := &RecordType{Fields: append([]Field(nil), declared.Fields...), Nullable: true}
			descriptor := message.Descriptor()
			var typ Type = candidate
			switch shape {
			case "nil":
				typ = nil
			case "scalar":
				typ = NotNullLong
			case "width":
				candidate.Fields = candidate.Fields[:1]
			case "leaf":
				candidate.Fields[0].FieldType = NullableString
			case "element":
				candidate.Fields[1].FieldType = NewArrayType(true, NullableString)
			case "duplicate":
				candidate.Fields[1].Name = candidate.Fields[0].Name
			case "nested_duplicate":
				candidate.Fields[1].Name = candidate.Fields[0].Name
				typ = &RecordType{Fields: []Field{{Name: "child", FieldType: candidate, Ordinal: 0}}}
				physical := NewRecordType("", false, []Field{{Name: "child", FieldType: declared, Ordinal: 0}})
				var err error
				descriptor, err = NewTypeProtoRepository().MessageDescriptorFor(physical)
				if err != nil {
					t.Fatal(err)
				}
			}
			var handle ExactTypeHandle
			if typ != nil {
				var err error
				handle, err = SnapshotExactType(typ)
				if err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if ProtoRecordDescriptorCompatible(descriptor, handle) {
					t.Fatalf("malformed %s protobuf declaration admitted", shape)
				}
			}
		})
	}
	handle, err := SnapshotExactType(declared)
	if err != nil {
		t.Fatal(err)
	}
	if ProtoRecordDescriptorCompatible(nil, handle) {
		t.Fatal("nil descriptor admitted")
	}
	if !ProtoRecordDescriptorCompatible(message.Descriptor(), handle) {
		t.Fatal("valid renamed declaration rejected")
	}
	measured := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = ProtoRecordDescriptorCompatible(message.Descriptor(), handle)
		}
	})
	if measured.N == 0 || measured.AllocsPerOp() != 0 {
		t.Fatalf("warm descriptor admission: %d iterations, %d allocations per row", measured.N, measured.AllocsPerOp())
	}
}

func TestProtoRecordAdmissionUsesScalarStorageAuthority(t *testing.T) {
	t.Parallel()
	descriptor := scalarShapeDescriptor(t)
	fields := make([]Field, descriptor.Fields().Len())
	for i := range fields {
		fields[i] = Field{Name: fmt.Sprintf("ALIAS_%d", i), Ordinal: i, FieldType: FieldTypeForProtoField(descriptor.Fields().Get(i))}
	}
	if len(fields) != 8 {
		t.Fatalf("scalar/enum fixture has %d fields, want 8", len(fields))
	}
	handle, err := SnapshotExactType(NewRecordType("", true, fields))
	if err != nil {
		t.Fatal(err)
	}
	if !ProtoRecordDescriptorCompatible(descriptor, handle) {
		t.Fatal("storage scalar/enum aliases rejected by output admission")
	}
	fields[6].FieldType = NullableLong // plain ENUM is not the alias-bearing enum's LONG carrier
	handle, err = SnapshotExactType(NewRecordType("", true, fields))
	if err != nil {
		t.Fatal(err)
	}
	if ProtoRecordDescriptorCompatible(descriptor, handle) {
		t.Fatal("plain ENUM admitted as LONG")
	}
}

func FuzzProtoRecordDescriptorAdmission(f *testing.F) {
	// Each descriptor pair has independent identity but identical storage.
	// Alternation exercises replacement of the single-entry immutable verdict.
	type fixture struct {
		descriptors [2]protoreflect.MessageDescriptor
		handle      ExactTypeHandle
		compatible  bool
	}
	var fixtures [8]fixture
	for i := range fixtures {
		wrapped, nullable := i&1 != 0, i&2 != 0
		for j := range fixtures[i].descriptors {
			message, declared, _ := protoArrayAdmissionFixture(f, 2*(i>>2), wrapped, nullable, "populated")
			var err error
			fixtures[i].handle, err = SnapshotExactType(declared)
			if err != nil {
				f.Fatal(err)
			}
			fixtures[i].descriptors[j] = message.Descriptor()
		}
		fixtures[i].compatible = !wrapped || nullable
		f.Add(byte(i), false)
		f.Add(byte(i), true)
	}
	f.Fuzz(func(t *testing.T, shape byte, reverse bool) {
		t.Parallel()
		entry := fixtures[int(shape)%len(fixtures)]
		for i := range 2 {
			if reverse {
				i = 1 - i
			}
			if got := ProtoRecordDescriptorCompatible(entry.descriptors[i], entry.handle); got != entry.compatible {
				t.Fatalf("shape %d descriptor %d: compatible=%v, want %v", shape, i, got, entry.compatible)
			}
		}
	})
}
