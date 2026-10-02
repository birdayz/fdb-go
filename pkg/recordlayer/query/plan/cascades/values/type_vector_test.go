package values

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestVectorTypeIdentityAndProto(t *testing.T) {
	t.Parallel()
	v := NewVectorType(false, 16, 3)
	h, err := SnapshotExactType(v)
	if err != nil {
		t.Fatal(err)
	}
	if h.Code() != TypeCodeVector || !h.Type().Equals(v) {
		t.Fatalf("snapshot lost vector shape: %v", h.Type())
	}
	widened := exactWithNullability(h.(*exactType), true)
	if !widened.Type().Equals(NewVectorType(true, 16, 3)) {
		t.Fatalf("widened: %v", widened.Type())
	}
	for _, other := range []*VectorType{NewVectorType(false, 32, 3), NewVectorType(false, 16, 4), NewVectorType(true, 16, 3)} {
		oh, err := SnapshotExactType(other)
		if err != nil {
			t.Fatal(err)
		}
		if v.Equals(other) || bytes.Equal(h.CanonicalBytes(), oh.CanonicalBytes()) {
			t.Fatalf("collapsed %v and %+v", v, other)
		}
	}
	c := NewSerializationContext()
	p, err := c.TypeToProto(v)
	if err != nil {
		t.Fatal(err)
	}
	want := &gen.PType{SpecificType: &gen.PType_VectorType{VectorType: &gen.PType_PVectorType{Precision: proto.Int32(16), Dimensions: proto.Int32(3), IsNullable: proto.Bool(false)}}}
	if !proto.Equal(p, want) {
		t.Fatalf("proto=%v, want %v", p, want)
	}
	back, err := c.TypeFromProto(p)
	if err != nil || !back.Equals(v) {
		t.Fatalf("decoded %v, %v", back, err)
	}
	repo := NewTypeProtoRepository()
	md, err := repo.MessageDescriptorFor(NewRecordType("", false, []Field{{Name: "V", FieldType: v}}))
	if err != nil {
		t.Fatal(err)
	}
	fd := md.Fields().Get(0)
	if fd.Kind() != protoreflect.BytesKind {
		t.Fatalf("vector field kind: %v", fd.Kind())
	}
	opts := proto.GetExtension(fd.Options(), gen.E_Field).(*gen.FieldOptions).GetVectorOptions()
	if opts.GetPrecision() != 16 || opts.GetDimensions() != 3 {
		t.Fatalf("vector options: %v", opts)
	}
	if !FieldTypeForProtoField(fd).Equals(NewVectorType(true, 16, 3)) {
		t.Fatalf("descriptor lost shape: %v", FieldTypeForProtoField(fd))
	}
	if protoScalarShapeCompatible(fd, mustVectorExact(t, 32, 3)) || protoScalarShapeCompatible(fd, mustVectorExact(t, 16, 4)) {
		t.Fatal("descriptor accepted different vector shape")
	}
}

func mustVectorExact(t *testing.T, precision, dimensions int) *exactType {
	t.Helper()
	h, err := SnapshotExactType(NewVectorType(true, precision, dimensions))
	if err != nil {
		t.Fatal(err)
	}
	return h.(*exactType)
}

func TestVectorCastJavaSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		source                Type
		input                 any
		precision, dimensions int
		want                  []byte
		invalid               bool
	}{
		{"vector identity", NewVectorType(false, 16, 1), []byte{0, 0x3c, 0}, 16, 1, []byte{0, 0x3c, 0}, false},
		{"vector precision", NewVectorType(false, 16, 1), []byte{0, 0x3c, 0}, 32, 1, nil, true},
		{"vector dimensions", NewVectorType(false, 16, 1), []byte{0, 0x3c, 0}, 16, 2, nil, true},
		{"null vector incompatible shape", NewVectorType(true, 16, 1), nil, 32, 1, nil, true},
		{"half truncation", NewArrayType(false, NotNullDouble), []any{1.00146484375}, 16, 1, []byte{0, 0x3c, 1}, false},
		{"float", NewArrayType(false, NotNullLong), []any{int64(2)}, 32, 1, []byte{1, 0x40, 0, 0, 0}, false},
		{"double", NewArrayType(false, NotNullInt), []any{int64(2)}, 64, 1, []byte{2, 0x40, 0, 0, 0, 0, 0, 0, 0}, false},
		{"null", NullType, nil, 16, 3, nil, false},
		{"dimensions", NewArrayType(false, NotNullDouble), []any{1.0}, 16, 2, nil, true},
		{"string", NewArrayType(false, NotNullString), []any{"1"}, 16, 1, nil, true},
		{"float to double undefined", NewArrayType(false, NotNullFloat), []any{1.0}, 64, 1, nil, true},
		{"null component", NewArrayType(false, NullableDouble), []any{nil}, 16, 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cast := NewCastValue(&ConstantValue{Value: tc.input, Typ: tc.source}, NewVectorType(true, tc.precision, tc.dimensions))
			got, err := cast.Evaluate(nil)
			if tc.invalid {
				var invalid *InvalidCastError
				if !errors.As(err, &invalid) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.input == nil {
				if got != nil {
					t.Fatalf("NULL=%v", got)
				}
				return
			}
			b, ok := got.([]byte)
			if !ok || !bytes.Equal(b, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVectorPreparedPromotion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		source, target Type
		value          any
		invalid        bool
	}{
		{"null", NullType, NewVectorType(true, 16, 3), nil, false},
		{"identity", NewVectorType(false, 16, 3), NewVectorType(true, 16, 3), []byte{0, 0, 0, 0, 0, 0, 0}, false},
		{"precision", NewVectorType(false, 16, 3), NewVectorType(true, 32, 3), nil, true},
		{"dimensions", NewVectorType(false, 16, 3), NewVectorType(true, 16, 4), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := NewPromoteValueChecked(&ConstantValue{Value: tc.value, Typ: tc.source}, tc.target)
			if tc.invalid {
				var incompatible *PromotionError
				if !errors.As(err, &incompatible) {
					t.Fatalf("got %v, want incompatible promotion", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Evaluate(nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.value == nil {
				if got != nil {
					t.Fatalf("null promotion = %v", got)
				}
			} else if !bytes.Equal(got.([]byte), tc.value.([]byte)) {
				t.Fatalf("identity changed bytes: %v", got)
			}
		})
	}
}

func TestVectorMaximumTypeAndOwnership(t *testing.T) {
	t.Parallel()
	left, right := NewVectorType(false, 16, 3), NewVectorType(true, 32, 4)
	// Java Type.maximumType's primitive arm is left-biased even for vectors.
	// Shape incompatibility is rejected by PromoteValue, not MaximumType.
	maximum := MaximumType(left, right)
	if !maximum.Equals(NewVectorType(true, 16, 3)) {
		t.Fatalf("maximum = %v", maximum)
	}
	cloned, err := copyPromotionType(left, map[Type]bool{})
	if err != nil {
		t.Fatal(err)
	}
	left.Dimensions = 9
	if !cloned.Equals(NewVectorType(false, 16, 3)) || !maximum.Equals(NewVectorType(true, 16, 3)) {
		t.Fatal("vector copy retained mutable source shape")
	}
}

func TestVectorRecordSerializationIdentity(t *testing.T) {
	t.Parallel()
	encoder, decoder := NewSerializationContext(), NewSerializationContext()
	for _, shape := range [][2]int{{16, 3}, {32, 3}, {16, 4}, {16, 3}} {
		row := NewRecordType("R", false, []Field{{Name: "V", FieldType: NewVectorType(true, shape[0], shape[1])}})
		wire, err := encoder.TypeToProto(row)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decoder.TypeFromProto(wire)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equals(row) {
			t.Fatalf("vector shape %v reused another record's reference: got %v, want %v", shape, got, row)
		}
	}
}

func TestVectorProtoRequiresNullability(t *testing.T) {
	t.Parallel()
	wire := &gen.PType{SpecificType: &gen.PType_VectorType{VectorType: &gen.PType_PVectorType{Precision: proto.Int32(16), Dimensions: proto.Int32(3)}}}
	if _, err := NewSerializationContext().TypeFromProto(wire); err == nil {
		t.Fatal("Java requires vector isNullable presence")
	}
}

func TestVectorArrayCastShape(t *testing.T) {
	t.Parallel()
	source := NewArrayType(true, NewVectorType(false, 32, 2))
	for _, tc := range []struct {
		name   string
		target Type
		valid  bool
	}{
		{"same", NewArrayType(true, NewVectorType(false, 32, 2)), true},
		{"precision", NewArrayType(true, NewVectorType(false, 64, 2)), false},
		{"dimensions", NewArrayType(true, NewVectorType(false, 32, 3)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if CastTypesDefined(source, tc.target) != tc.valid {
				t.Fatal("incorrect array cast admission")
			}
			// Even an empty or NULL array must not hide an invalid element cast.
			for _, input := range []any{nil, []any{}} {
				_, err := CastEvaluated(input, source, tc.target)
				if (err == nil) != tc.valid {
					t.Fatalf("input %v: %v", input, err)
				}
			}
		})
	}
}
