package wire

import (
	"testing"
)

// --- Writer round-trip tests (Writer → Reader) ---

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
