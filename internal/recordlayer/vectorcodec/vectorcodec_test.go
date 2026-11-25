package vectorcodec

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestSerializeDeserialize_DoubleRoundTrip(t *testing.T) {
	t.Parallel()
	want := []float64{1.5, -2.5, 0, 3.25, -0.125}
	got, err := Deserialize(Serialize(want))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDeserialize_Single(t *testing.T) {
	t.Parallel()
	// Hand-build a SINGLE (float32, ordinal 1) payload: [1.5, -2.0].
	src := []float32{1.5, -2.0}
	buf := make([]byte, 1+4*len(src))
	buf[0] = typeSingle
	for i, f := range src {
		binary.BigEndian.PutUint32(buf[1+i*4:], math.Float32bits(f))
	}
	got, err := Deserialize(buf)
	if err != nil {
		t.Fatalf("deserialize single: %v", err)
	}
	if len(got) != 2 || got[0] != 1.5 || got[1] != -2.0 {
		t.Errorf("got %v, want [1.5 -2]", got)
	}
}

func TestDeserialize_Half(t *testing.T) {
	t.Parallel()
	// HALF (ordinal 0): 1.0 = 0x3C00, 2.0 = 0x4000, -0.5 = 0xB800.
	bits := []uint16{0x3C00, 0x4000, 0xB800}
	want := []float64{1.0, 2.0, -0.5}
	buf := make([]byte, 1+2*len(bits))
	buf[0] = typeHalf
	for i, b := range bits {
		binary.BigEndian.PutUint16(buf[1+i*2:], b)
	}
	got, err := Deserialize(buf)
	if err != nil {
		t.Fatalf("deserialize half: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDeserialize_Errors(t *testing.T) {
	t.Parallel()
	if _, err := Deserialize(nil); err == nil {
		t.Error("empty data should error")
	}
	if _, err := Deserialize([]byte{typeRaBitQ, 0x01}); err == nil {
		t.Error("RaBitQ ordinal should error (needs the quantizer)")
	}
	if _, err := Deserialize([]byte{99}); err == nil {
		t.Error("unknown type ordinal should error")
	}
}

// A serialized vector whose length Java's RealVector.fromBytes cannot read is
// rejected, exactly where Java rejects it. Each Java subtype sizes the vector
// as vectorBytes.length >> log2(stride), type byte included
// (HalfRealVector.java:284, FloatRealVector.java:323,
// DoubleRealVector.java:303), so a total length that is a multiple of the
// stride reads one component past the payload (BufferUnderflowException);
// every other trailing partial component is floored away in both. Before, Go
// floored every case and the VECTOR index maintainer indexed a truncated
// vector Java refuses to write.
func TestDeserialize_TruncatedPayloadMatchesJava(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		data    []byte
		wantErr bool
		wantLen int
	}{
		{"half one trailing byte", []byte{typeHalf, 0x3c, 0x00, 0x3c}, true, 0},
		{"half empty", []byte{typeHalf}, false, 0},
		{"single three trailing bytes", []byte{typeSingle, 0, 0, 0}, true, 0},
		{"single two trailing bytes", []byte{typeSingle, 0, 0}, false, 0},
		{"double seven trailing bytes", append(Serialize([]float64{1}), 0, 0, 0, 0, 0, 0, 0), true, 0},
		{"double one trailing byte", []byte{typeDouble, 0}, false, 0},
		{"double six trailing bytes", append(Serialize([]float64{1}), 0, 0, 0, 0, 0, 0), false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Deserialize(c.data)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Deserialize(%x) = %v, want an error (Java underflows)", c.data, got)
				}
				if _, _, _, ok := Payload(c.data); ok {
					t.Fatalf("Payload(%x) ok, want !ok (Java underflows)", c.data)
				}
				return
			}
			if err != nil || len(got) != c.wantLen {
				t.Fatalf("Deserialize(%x) = %v, %v; want %d components (Java floors)", c.data, got, err, c.wantLen)
			}
			if _, _, _, ok := Payload(c.data); !ok {
				t.Fatalf("Payload(%x) !ok, want ok", c.data)
			}
		})
	}
}
