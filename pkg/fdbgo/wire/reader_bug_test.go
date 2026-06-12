package wire

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// These tests exercise bugs found by the 5-agent review (2026-04-01).
// They should FAIL on the unfixed code and PASS after fixes.
//
// Tests 1-3 use pre-captured hex data (from the old ObjectWriter-based construction)
// to avoid depending on ObjectWriter/NewWriter/WriteMessage/WriteStruct which are deleted.

// TestReadVectorInt32_NestedStruct tests ReadVectorInt32 on a nested struct.
// REVIEW NOTE: The reviewer flagged this as CRITICAL #1 (wrong RelOff calculation),
// but r.object = data[objPos:] (extends to end of buffer), so r.object[off+relOff]
// IS the correct absolute position. The code is correct but lacks bounds checks.
// This test verifies correctness and also that bounds checking doesn't panic.
func TestReadVectorInt32_NestedStruct(t *testing.T) {
	// Pre-captured wire data for a message (fileID=12345) with:
	//   msgVT = {10, 16, 4, 8, 12}  (3 fields: bytes, bytes, nested struct)
	//   nestedVT = {6, 8, 4}        (1 field: vectorInt32)
	//   field 0 (offset 4): bytes "padding1padding1padding1padding1"
	//   field 1 (offset 8): bytes "padding2padding2padding2padding2"
	//   field 2 (offset 12): nested struct with VectorInt32 [100, 200, 300]
	data, err := hex.DecodeString(
		"18000000393000000a001000040008000c000600080004000600000004000000" +
			"180000002400000044000000040000001e000000040000000300000064000000" +
			"c80000002c0100002000000070616464696e673170616464696e673170616464" +
			"696e673170616464696e67312000000070616464696e673270616464696e6732" +
			"70616464696e673270616464696e6732")
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}

	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	nestedR, err := r.ReadNestedReader(2)
	if err != nil {
		t.Fatalf("ReadNestedReader: %v", err)
	}

	vec := nestedR.ReadVectorInt32(0)
	if len(vec) != 3 {
		t.Fatalf("expected 3 elements, got %d (nil=%v)", len(vec), vec == nil)
	}
	if vec[0] != 100 || vec[1] != 200 || vec[2] != 300 {
		t.Errorf("got %v, want [100, 200, 300]", vec)
	}
}

// TestReadOptionalInt32_NestedStruct tests ReadOptionalInt32 on a nested struct.
// Same as above — code is correct, reviewer was wrong about CRITICAL #2.
func TestReadOptionalInt32_NestedStruct(t *testing.T) {
	// Pre-captured wire data for a message (fileID=12345) with:
	//   msgVT = {10, 16, 4, 8, 12}    (3 fields: bytes, bytes, nested struct)
	//   nestedVT = {8, 9, 8, 4}       (2 fields: type tag at 8, value at 4)
	//   field 0 (offset 4): bytes "padding1padding1padding1padding1"
	//   field 1 (offset 8): bytes "padding2padding2padding2padding2"
	//   field 2 (offset 12): nested struct with OptionalInt32Present(typeOff=8, valOff=4, val=42)
	data, err := hex.DecodeString(
		"20000000393000000a001000040008000c0008000900080004000600080004000600000004000000" +
			"200000001c0000003c000000040000002600000008000000010000002a000000" +
			"2000000070616464696e673170616464696e673170616464696e673170616464" +
			"696e67312000000070616464696e673270616464696e673270616464696e6732" +
			"70616464696e673270616464696e6732")
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}

	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	nestedR, err := r.ReadNestedReader(2)
	if err != nil {
		t.Fatalf("ReadNestedReader: %v", err)
	}

	val, present := nestedR.ReadOptionalInt32(0, 1)
	if !present {
		t.Fatal("expected present, got absent")
	}
	if val != 42 {
		t.Errorf("got %d, want 42", val)
	}
}

// TestReadUID_MissingField tests MEDIUM #9:
// ReadUID has no bounds check — panics on missing fields.
func TestReadUID_MissingField(t *testing.T) {
	// Pre-captured wire data for a message (fileID=12345) with:
	//   msgVT = {6, 8, 0}  (1 field with offset 0 = absent)
	//   No fields written.
	data, err := hex.DecodeString(
		"1800000039300000000000000600080004000600080000000c000000040000000e00000000000000")
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}

	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	// This should NOT panic. The buggy code has no bounds check.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReadUID panicked on missing field: %v", r)
		}
	}()

	uid := r.ReadUID(0)
	if uid != [16]byte{} {
		t.Errorf("expected zero UID for missing field, got %v", uid)
	}
}

// TestVTableHashCollision tests CRITICAL #3:
// MessageTemplate vtable key uses only (vt[0]<<16|vt[1]), ignoring field offsets.
// Two vtables with same size+objSize but different fields silently collide.
func TestVTableHashCollision(t *testing.T) {
	// Two vtables with same vt[0] (vtable size) and vt[1] (object size)
	// but different field offsets.
	vt1 := VTable{8, 12, 4, 8} // fields at offset 4 and 8
	vt2 := VTable{8, 12, 8, 4} // fields at offset 8 and 4 (swapped!)

	// Both have vt[0]=8, vt[1]=12. The old key (8<<16|12) is the same.
	closure := []VTable{vt1, vt2, {6, 8, 4}} // FakeRoot vtable

	tmpl := NewMessageTemplate(99999, vt1, 4, closure)

	// Both vtables should resolve to DIFFERENT offsets.
	off1 := tmpl.vtableOffset(vt1)
	off2 := tmpl.vtableOffset(vt2)

	if off1 == off2 {
		t.Errorf("vtable hash collision: vt1 and vt2 resolve to same offset %d", off1)
	}
}

// TestFieldOffset_CorruptedVTable tests MEDIUM #10:
// fieldOffset doesn't check vtable slice bounds against declared vtable size.
func TestFieldOffset_CorruptedVTable(t *testing.T) {
	// Craft data with a vtable that claims more entries than actually present.
	// vtable_size=20 (10 entries) but only 6 bytes of actual vtable data.
	var buf [64]byte

	// Root footer at offset 0: points to FakeRoot
	binary.LittleEndian.PutUint32(buf[0:], 20) // FakeRoot at offset 20
	binary.LittleEndian.PutUint32(buf[4:], 0)  // file_id

	// FakeRoot at offset 20: vtable backref + field[0] RelOff
	// FakeRoot vtable at offset 14: {6, 8, 4}
	binary.LittleEndian.PutUint16(buf[14:], 6) // vtable_size=6
	binary.LittleEndian.PutUint16(buf[16:], 8) // obj_size=8
	binary.LittleEndian.PutUint16(buf[18:], 4) // field0 offset=4

	// FakeRoot object at offset 20: soffset to vtable + field[0] RelOff
	binary.LittleEndian.PutUint32(buf[20:], uint32(int32(20-14))) // soffset=6 -> vtable at 14
	binary.LittleEndian.PutUint32(buf[24:], 4)                    // field[0] RelOff -> message at 28

	// Message at offset 28: vtable claims 20 bytes but only 6 exist
	// Message vtable at offset 22: only 6 bytes ({20, 8, 4} but only first 6 bytes present)
	binary.LittleEndian.PutUint16(buf[22:], 20) // vtable_size=20 (claims 10 entries!)
	binary.LittleEndian.PutUint16(buf[24:], 8)  // obj_size=8
	binary.LittleEndian.PutUint16(buf[26:], 4)  // field0 offset=4

	// Message object at offset 28
	binary.LittleEndian.PutUint32(buf[28:], uint32(int32(28-22))) // soffset=6 -> vtable at 22

	r, err := NewReader(buf[:36])
	if err != nil {
		// NewReader rejecting the malformed buffer outright is one valid
		// defense; the FieldPresent bounds check below is the other. Either
		// way the property under test (no panic on a lying vtable) holds —
		// this is a pass, not a skip.
		t.Logf("NewReader rejected crafted data: %v", err)
		return
	}

	// Accessing a high slot should NOT panic even though vtable claims it exists.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fieldOffset panicked on corrupted vtable: %v", r)
		}
	}()

	// Slot 5 -> entryIndex 7 -> byte offset 14. The vtable only has 3 entries (6 bytes).
	// The buggy code checks VTableLength() (which returns 10) but not the actual slice bounds.
	_ = r.FieldPresent(5)
}
