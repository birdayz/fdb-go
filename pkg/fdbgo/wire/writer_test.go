package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// --- C++ conformance tests ---
// These compare Go writer output byte-for-byte against test vectors generated
// by FDB's actual flat_buffers.h serialization code (gen_vectors.cpp).

type testVector struct {
	Name           string `json:"name"`
	FileIdentifier uint32 `json:"file_identifier"`
	Size           int    `json:"size"`
	Hex            string `json:"hex"`
}

func loadTestVectors(t *testing.T) []testVector {
	t.Helper()
	data, err := os.ReadFile("testdata/test_vectors.json")
	if err != nil {
		t.Skipf("test vectors not generated: %v (run: bazelisk run //pkg/fdbgo/wire/testdata:gen_vectors > pkg/fdbgo/wire/testdata/test_vectors.json)", err)
	}
	var vectors []testVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("parse test_vectors.json: %v", err)
	}
	return vectors
}

func findVector(vectors []testVector, name string) *testVector {
	for i := range vectors {
		if vectors[i].Name == name {
			return &vectors[i]
		}
	}
	return nil
}

func TestConformance_MsgSingleInt32(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgSingleInt32")
	if v == nil {
		t.Skip("MsgSingleInt32 not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// VTable for int32: [6, 8, 4]
	msgVT := GenerateVTable([]uint32{4}, []uint32{4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgMultiScalar(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgMultiScalar")
	if v == nil {
		t.Skip("MsgMultiScalar not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// VTable: sizes=[1,1,4,8,4], aligns=[1,1,4,8,4] → [14, 22, 20, 21, 12, 4, 16]
	msgVT := GenerateVTable(
		[]uint32{1, 1, 4, 8, 4},
		[]uint32{1, 1, 4, 8, 4},
	)

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 8, func(obj *ObjectWriter) {
		obj.WriteUint8(int(msgVT[2]), 0xAA)
		obj.WriteUint8(int(msgVT[3]), 0xBB)
		obj.WriteInt32(int(msgVT[4]), 100)
		obj.WriteInt64(int(msgVT[5]), 200)
		obj.WriteInt32(int(msgVT[6]), 300)
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgWithString(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgWithString")
	if v == nil {
		t.Skip("MsgWithString not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// VTable: sizes=[8, 4], aligns=[8, 4] → [8, 16, 4, 12]
	msgVT := GenerateVTable([]uint32{8, 4}, []uint32{8, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 8, func(obj *ObjectWriter) {
		obj.WriteInt64(int(msgVT[2]), 0x1234567890ABCDEF)
		obj.WriteString(int(msgVT[3]), "hello, fdb!")
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgBoolDouble(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgBoolDouble")
	if v == nil {
		t.Skip("MsgBoolDouble not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// VTable: sizes=[1, 8], aligns=[1, 8] → [8, 13, 12, 4]
	msgVT := GenerateVTable([]uint32{1, 8}, []uint32{1, 8})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 8, func(obj *ObjectWriter) {
		obj.WriteBool(int(msgVT[2]), true)
		obj.WriteFloat64(int(msgVT[3]), 3.14159)
	})

	assertBytesEqual(t, expected, got)
}

// --- Vector conformance tests ---

func TestConformance_MsgVectorInt32(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgVectorInt32")
	if v == nil {
		t.Skip("MsgVectorInt32 not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Fields: int32(4,4) + vector<int32> as RelativeOffset(4,4)
	// VTable: [8, 12, 4, 8]
	msgVT := GenerateVTable([]uint32{4, 4}, []uint32{4, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 99)
		obj.WriteVectorInt32(int(msgVT[3]), []int32{10, 20, 30})
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgEmptyVector(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgEmptyVector")
	if v == nil {
		t.Skip("MsgEmptyVector not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Fields: vector<int32> as RelativeOffset(4,4)
	// VTable: [6, 8, 4]
	msgVT := GenerateVTable([]uint32{4}, []uint32{4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteVectorInt32(int(msgVT[2]), []int32{})
	})

	assertBytesEqual(t, expected, got)
}

// --- Optional conformance tests ---

func TestConformance_MsgOptionalPresent(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgOptionalPresent")
	if v == nil {
		t.Skip("MsgOptionalPresent not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Fields: int32(4,4) + Optional<int32> → union_like [uint8(1,1), uint32(4,4)]
	// Total: 3 vtable fields: sizes=[4, 1, 4], aligns=[4, 1, 4]
	// VTable: [10, 13, 4, 12, 8]
	msgVT := GenerateVTable([]uint32{4, 1, 4}, []uint32{4, 1, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
		obj.WriteOptionalInt32Present(int(msgVT[3]), int(msgVT[4]), 123)
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgOptionalAbsent(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgOptionalAbsent")
	if v == nil {
		t.Skip("MsgOptionalAbsent not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Same vtable as MsgOptionalPresent
	msgVT := GenerateVTable([]uint32{4, 1, 4}, []uint32{4, 1, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
		obj.WriteOptionalAbsent(int(msgVT[3]), int(msgVT[4]))
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgOptionalString(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgOptionalString")
	if v == nil {
		t.Skip("MsgOptionalString not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Fields: Optional<string> → union_like [uint8(1,1), uint32(4,4)]
	// VTable: sizes=[1, 4], aligns=[1, 4]
	// Sorted: [(1,4), (0,1)] → vtable = [8, 9, 8, 4]
	// Wait: result[0]=2*2+4=8, (1,4) at 0→result[3]=4, (0,1) at 4→result[2]=8
	// result[1] = 5+4 = 9
	msgVT := GenerateVTable([]uint32{1, 4}, []uint32{1, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteOptionalStringPresent(int(msgVT[2]), int(msgVT[3]), "test")
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgVectorString(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgVectorString")
	if v == nil {
		t.Skip("MsgVectorString not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// Fields: vector<string> → RelativeOffset(4,4)
	msgVT := GenerateVTable([]uint32{4}, []uint32{4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteVectorStrings(int(msgVT[2]), []string{"abc", "def", "ghi"})
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgNested(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgNested")
	if v == nil {
		t.Skip("MsgNested not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// MsgNested: int64(8,8) + Inner(RelativeOffset, 4,4)
	// Inner: int32(4,4) + int32(4,4)
	msgVT := GenerateVTable([]uint32{8, 4}, []uint32{8, 4})
	innerVT := GenerateVTable([]uint32{4, 4}, []uint32{4, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 8, func(obj *ObjectWriter) {
		obj.WriteInt64(int(msgVT[2]), 1000)
		obj.WriteStruct(int(msgVT[3]), innerVT, 4, func(inner *ObjectWriter) {
			inner.WriteInt32(int(innerVT[2]), 42)
			inner.WriteInt32(int(innerVT[3]), 99)
		})
	})

	assertBytesEqual(t, expected, got)
}

func TestConformance_MsgNestedString(t *testing.T) {
	t.Parallel()
	vectors := loadTestVectors(t)
	v := findVector(vectors, "MsgNestedString")
	if v == nil {
		t.Skip("MsgNestedString not in test vectors")
	}
	expected, _ := hex.DecodeString(v.Hex)

	// MsgNestedString: InnerWithString(RelOff, 4,4) + int32(4,4)
	// InnerWithString: int32(4,4) + string(RelOff, 4,4)
	msgVT := GenerateVTable([]uint32{4, 4}, []uint32{4, 4})
	innerVT := GenerateVTable([]uint32{4, 4}, []uint32{4, 4})

	w := NewWriter(nil)
	got := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteStruct(int(msgVT[2]), innerVT, 4, func(inner *ObjectWriter) {
			inner.WriteInt32(int(innerVT[2]), 200)
			inner.WriteString(int(innerVT[3]), "hello")
		})
		obj.WriteInt32(int(msgVT[3]), 7)
	})

	assertBytesEqual(t, expected, got)
}

// --- Round-trip tests (writer → reader) ---

func TestRoundTrip_SingleInt32(t *testing.T) {
	t.Parallel()
	msgVT := GenerateVTable([]uint32{4}, []uint32{4})

	w := NewWriter(nil)
	buf := w.WriteMessage(12345, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
	})

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := r.FileIdentifier(); got != 12345 {
		t.Errorf("FileIdentifier: got %d, want 12345", got)
	}
	if got := r.ReadInt32(0); got != 42 {
		t.Errorf("ReadInt32(0): got %d, want 42", got)
	}
}

func TestRoundTrip_BytesField(t *testing.T) {
	t.Parallel()
	msgVT := GenerateVTable([]uint32{8, 4}, []uint32{8, 4})

	w := NewWriter(nil)
	buf := w.WriteMessage(55555, msgVT, 8, func(obj *ObjectWriter) {
		obj.WriteInt64(int(msgVT[2]), 0x1234567890ABCDEF)
		obj.WriteBytes(int(msgVT[3]), []byte("hello, fdb!"))
	})

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := r.FileIdentifier(); got != 55555 {
		t.Errorf("FileIdentifier: got %d, want 55555", got)
	}
	if got := r.ReadInt64(0); got != 0x1234567890ABCDEF {
		t.Errorf("ReadInt64(0): got %#x, want 0x1234567890ABCDEF", got)
	}
	if got := r.ReadBytes(1); string(got) != "hello, fdb!" {
		t.Errorf("ReadBytes(1): got %q, want %q", got, "hello, fdb!")
	}
}

func TestRoundTrip_VectorInt32(t *testing.T) {
	t.Parallel()
	msgVT := GenerateVTable([]uint32{4, 4}, []uint32{4, 4})

	w := NewWriter(nil)
	buf := w.WriteMessage(100005, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 99)
		obj.WriteVectorInt32(int(msgVT[3]), []int32{10, 20, 30})
	})

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := r.ReadInt32(0); got != 99 {
		t.Errorf("ReadInt32(0): got %d, want 99", got)
	}
	got := r.ReadVectorInt32(1)
	if len(got) != 3 || got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Errorf("ReadVectorInt32(1): got %v, want [10 20 30]", got)
	}
}

func TestRoundTrip_OptionalPresent(t *testing.T) {
	t.Parallel()
	msgVT := GenerateVTable([]uint32{4, 1, 4}, []uint32{4, 1, 4})

	w := NewWriter(nil)
	buf := w.WriteMessage(100007, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
		obj.WriteOptionalInt32Present(int(msgVT[3]), int(msgVT[4]), 123)
	})

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// type tag at slot 1, value at slot 2
	val, ok := r.ReadOptionalInt32(1, 2)
	if !ok || val != 123 {
		t.Errorf("ReadOptionalInt32: got (%d, %v), want (123, true)", val, ok)
	}
}

func TestRoundTrip_OptionalAbsent(t *testing.T) {
	t.Parallel()
	msgVT := GenerateVTable([]uint32{4, 1, 4}, []uint32{4, 1, 4})

	w := NewWriter(nil)
	buf := w.WriteMessage(100008, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
		obj.WriteOptionalAbsent(int(msgVT[3]), int(msgVT[4]))
	})

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	val, ok := r.ReadOptionalInt32(1, 2)
	if ok || val != 0 {
		t.Errorf("ReadOptionalInt32: got (%d, %v), want (0, false)", val, ok)
	}
}

func TestReader_InvalidBuffer(t *testing.T) {
	t.Parallel()
	_, err := NewReader([]byte{0, 0, 0})
	if err == nil {
		t.Error("expected error for short buffer")
	}
}

// --- Helpers ---

func assertBytesEqual(t *testing.T, expected, got []byte) {
	t.Helper()
	if len(got) != len(expected) {
		t.Errorf("length mismatch: got %d, want %d", len(got), len(expected))
		t.Logf("got:  %s", hex.EncodeToString(got))
		t.Logf("want: %s", hex.EncodeToString(expected))
		return
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Errorf("byte mismatch at offset %d: got %02x, want %02x", i, got[i], expected[i])
			t.Logf("got:  %s", hex.EncodeToString(got))
			t.Logf("want: %s", hex.EncodeToString(expected))
			// Show diff region.
			start := i - 4
			if start < 0 {
				start = 0
			}
			end := i + 8
			if end > len(got) {
				end = len(got)
			}
			t.Logf("got  [%d:%d]: %s", start, end, hex.EncodeToString(got[start:end]))
			t.Logf("want [%d:%d]: %s", start, end, hex.EncodeToString(expected[start:end]))
			return
		}
	}
}
