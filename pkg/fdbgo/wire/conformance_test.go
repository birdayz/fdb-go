package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// Conformance tests verify two things:
// 1. Our Reader can parse bytes produced by FDB's C++ flat_buffers.h (ground truth)
// 2. Our Writer + Reader round-trip produces correct field values
//
// We do NOT compare raw bytes between Go and C++ because vtable ordering
// depends on std::set<const VTable*> pointer values (non-deterministic).
// Instead, we parse both and verify field values match.

type testVector struct {
	Name           string `json:"name"`
	FileIdentifier uint32 `json:"file_identifier"`
	Size           int    `json:"size"`
	Hex            string `json:"hex"`
}

func loadTestVector(t *testing.T, name string) *testVector {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Skipf("test vector %s not found: %v", name, err)
		return nil
	}
	var v testVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse testdata/%s.json: %v", name, err)
	}
	return &v
}

func mustParseCpp(t *testing.T, v *testVector) *Reader {
	t.Helper()
	data, err := hex.DecodeString(v.Hex)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader on C++ bytes: %v", err)
	}
	if got := r.FileIdentifier(); got != v.FileIdentifier {
		t.Errorf("C++ file_id: got %d, want %d", got, v.FileIdentifier)
	}
	return r
}

// --- MsgSingleInt32: int32 x = 42 ---

func TestConformance_MsgSingleInt32(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgSingleInt32")
	if v == nil {
		return
	}

	// Parse C++ bytes.
	r := mustParseCpp(t, v)
	if got := r.ReadInt32(0); got != 42 {
		t.Errorf("C++ ReadInt32(0): got %d, want 42", got)
	}

	// Writer round-trip.
	msgVT := GenerateVTable([]uint32{4}, []uint32{4})
	w := NewWriter(nil)
	buf := w.WriteMessage(v.FileIdentifier, msgVT, 4, func(obj *ObjectWriter) {
		obj.WriteInt32(int(msgVT[2]), 42)
	})
	r2, err := NewReader(buf)
	if err != nil {
		t.Fatalf("round-trip NewReader: %v", err)
	}
	if got := r2.ReadInt32(0); got != 42 {
		t.Errorf("round-trip ReadInt32(0): got %d, want 42", got)
	}
}

// --- MsgMultiScalar: uint8 a=0xAA, uint8 b=0xBB, int32 c=100, int64 d=200, int32 e=300 ---

func TestConformance_MsgMultiScalar(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgMultiScalar")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadUint8(0); got != 0xAA {
		t.Errorf("C++ a: got %#x, want 0xAA", got)
	}
	if got := r.ReadUint8(1); got != 0xBB {
		t.Errorf("C++ b: got %#x, want 0xBB", got)
	}
	if got := r.ReadInt32(2); got != 100 {
		t.Errorf("C++ c: got %d, want 100", got)
	}
	if got := r.ReadInt64(3); got != 200 {
		t.Errorf("C++ d: got %d, want 200", got)
	}
	if got := r.ReadInt32(4); got != 300 {
		t.Errorf("C++ e: got %d, want 300", got)
	}
}

// --- MsgWithString: int64 version=0x1234567890ABCDEF, string name="hello, fdb!" ---

func TestConformance_MsgWithString(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgWithString")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadInt64(0); got != 0x1234567890ABCDEF {
		t.Errorf("C++ version: got %#x, want 0x1234567890ABCDEF", got)
	}
	if got := r.ReadString(1); got != "hello, fdb!" {
		t.Errorf("C++ name: got %q, want %q", got, "hello, fdb!")
	}
}

// --- MsgBoolDouble: bool flag=true, double value=3.14159 ---

func TestConformance_MsgBoolDouble(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgBoolDouble")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadBool(0); got != true {
		t.Errorf("C++ flag: got %v, want true", got)
	}
	if got := r.ReadFloat64(1); got != 3.14159 {
		t.Errorf("C++ value: got %v, want 3.14159", got)
	}
}

// --- MsgVectorInt32: int32 id=99, vector<int32> values=[10,20,30] ---

func TestConformance_MsgVectorInt32(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgVectorInt32")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadInt32(0); got != 99 {
		t.Errorf("C++ id: got %d, want 99", got)
	}
	got := r.ReadVectorInt32(1)
	if len(got) != 3 || got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Errorf("C++ values: got %v, want [10 20 30]", got)
	}
}

// --- MsgEmptyVector: vector<int32> values=[] ---

func TestConformance_MsgEmptyVector(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgEmptyVector")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	got := r.ReadVectorInt32(0)
	if len(got) != 0 {
		t.Errorf("C++ values: got %v, want []", got)
	}
}

// --- MsgOptionalPresent: int32 id=42, Optional<int32> value=123 ---

func TestConformance_MsgOptionalPresent(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgOptionalPresent")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadInt32(0); got != 42 {
		t.Errorf("C++ id: got %d, want 42", got)
	}
	val, ok := r.ReadOptionalInt32(1, 2)
	if !ok || val != 123 {
		t.Errorf("C++ value: got (%d, %v), want (123, true)", val, ok)
	}
}

// --- MsgOptionalAbsent: int32 id=42, Optional<int32> value=absent ---

func TestConformance_MsgOptionalAbsent(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgOptionalAbsent")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	if got := r.ReadInt32(0); got != 42 {
		t.Errorf("C++ id: got %d, want 42", got)
	}
	val, ok := r.ReadOptionalInt32(1, 2)
	if ok || val != 0 {
		t.Errorf("C++ value: got (%d, %v), want (0, false)", val, ok)
	}
}

// --- MsgOptionalString: Optional<string> name="test" ---

func TestConformance_MsgOptionalString(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgOptionalString")
	if v == nil {
		return
	}
	r := mustParseCpp(t, v)
	val, ok := r.ReadOptionalString(0, 1)
	if !ok || val != "test" {
		t.Errorf("C++ name: got (%q, %v), want (\"test\", true)", val, ok)
	}
}

// --- MsgVectorString: vector<string> names=["abc","def","ghi"] ---

func TestConformance_MsgVectorString(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgVectorString")
	if v == nil {
		return
	}
	// Vector of strings: Reader reads RelativeOffsets to each string.
	// Just verify we can parse the C++ bytes without error.
	data, _ := hex.DecodeString(v.Hex)
	_, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader on C++ VectorString bytes: %v", err)
	}
}

// --- MsgNested: int64 id=1000, Inner{x=42, y=99} ---

func TestConformance_MsgNested(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgNested")
	if v == nil {
		return
	}
	// Nested struct: our Reader reads the outer message.
	// Inner struct is a RelativeOffset — Reader sees it as struct field.
	r := mustParseCpp(t, v)
	if got := r.ReadInt64(0); got != 1000 {
		t.Errorf("C++ id: got %d, want 1000", got)
	}
	// Field 1 is a RelativeOffset to the Inner struct — Reader can't auto-navigate.
	// Verify we parsed without error (file_id + outer fields correct).
}

// --- MsgNestedString: InnerWithString{code=200, label="hello"}, int32 version=7 ---

func TestConformance_MsgNestedString(t *testing.T) {
	t.Parallel()
	v := loadTestVector(t, "MsgNestedString")
	if v == nil {
		return
	}
	data, _ := hex.DecodeString(v.Hex)
	_, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader on C++ NestedString bytes: %v", err)
	}
}
