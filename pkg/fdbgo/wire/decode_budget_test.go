package wire

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestDecodeBudget(t *testing.T) {
	t.Parallel()
	b := NewDecodeBudget(make([]byte, 40))
	if b.Left() != 10 {
		t.Fatalf("budget for 40 bytes = %d, want 10", b.Left())
	}
	for _, n := range []int{0, 4, 6} {
		if err := b.Take(n); err != nil {
			t.Fatalf("Take(%d): %v", n, err)
		}
	}
	err := b.Take(1)
	var budgetErr *DecodeBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Elements != 1 || budgetErr.Left != 0 || b.Left() != 0 {
		t.Fatalf("Take past the budget: %v, %d left", err, b.Left())
	}
	if err := b.Take(0); err != nil {
		t.Fatalf("Take(0) on an exhausted budget: %v", err)
	}
}

// scalarMessage builds a message whose vtable declares slots slots; slot 0 is a
// uint32 at object offset 4, and the rest are absent.
func scalarMessage(slots int) []byte {
	d := make([]byte, 28+2*slots)
	le := binary.LittleEndian
	le.PutUint32(d[0:], 8) // root offset: FakeRoot at 8
	le.PutUint32(d[4:], 1) // file identifier
	le.PutUint32(d[12:], 4)
	le.PutUint32(d[16:], uint32(0xFFFFFFF8)) // message at 16, vtable at 24
	le.PutUint32(d[20:], 7)                  // slot 0
	le.PutUint16(d[24:], uint16(4+2*slots))
	le.PutUint16(d[26:], 8)
	le.PutUint16(d[28:], 4)
	return d
}

// A present field outside the buffer reads as zero and is recorded; an absent one is not.
func TestReaderRecordsFieldOutsideBuffer(t *testing.T) {
	t.Parallel()
	read := func(d []byte, read func(r *Reader)) error {
		t.Helper()
		r, err := NewReader(d)
		if err != nil {
			t.Fatal(err)
		}
		read(r)
		return r.Err()
	}
	d := scalarMessage(2)
	if err := read(d, func(r *Reader) {
		if v := r.ReadUint32(0); v != 7 {
			t.Errorf("slot 0 = %d, want 7", v)
		}
		if v := r.ReadUint32(1); v != 0 {
			t.Errorf("absent slot 1 = %d, want 0", v)
		}
		r.ReadUint32(5) // beyond the vtable: absent
	}); err != nil {
		t.Fatalf("well-formed reads: %v", err)
	}

	type readCase struct {
		name string
		edit func(d []byte)
		read func(r *Reader)
		slot int
	}
	fieldPast := func(d []byte) { binary.LittleEndian.PutUint16(d[28:], 0xFFF0) } // slot 0 past the object
	targetPast := func(d []byte) { binary.LittleEndian.PutUint32(d[20:], 1<<20) } // its RelativeOffset past the buffer
	cases := []readCase{
		{"vtable longer than the buffer", func(d []byte) { binary.LittleEndian.PutUint16(d[24:], 400) }, func(r *Reader) { r.FieldPresent(100) }, 100},
	}
	// Every leaf read, with the field outside the object.
	leaves := map[string]func(r *Reader){
		"ReadInt8": func(r *Reader) { r.ReadInt8(0) }, "ReadUint8": func(r *Reader) { r.ReadUint8(0) },
		"ReadInt16": func(r *Reader) { r.ReadInt16(0) }, "ReadUint16": func(r *Reader) { r.ReadUint16(0) },
		"ReadInt32": func(r *Reader) { r.ReadInt32(0) }, "ReadUint32": func(r *Reader) { r.ReadUint32(0) },
		"ReadInt64": func(r *Reader) { r.ReadInt64(0) }, "ReadUint64": func(r *Reader) { r.ReadUint64(0) },
		"ReadFloat64": func(r *Reader) { r.ReadFloat64(0) }, "ReadBool": func(r *Reader) { r.ReadBool(0) },
		"ReadUID": func(r *Reader) { r.ReadUID(0) }, "ReadUIDPair": func(r *Reader) { r.ReadUIDPair(0) },
	}
	// Reads through a RelativeOffset fail both with the field and with its target outside.
	outOfLine := map[string]func(r *Reader){
		"ReadBytes": func(r *Reader) { r.ReadBytes(0) }, "ReadVectorInt32": func(r *Reader) { r.ReadVectorInt32(0) },
		"ReadVectorUint64": func(r *Reader) { r.ReadVectorUint64(0) }, "ReadRelOffRaw": func(r *Reader) { r.ReadRelOffRaw(0, 4) },
		"ReadRelOffUint32": func(r *Reader) { r.ReadRelOffUint32(0) }, "ReadRelOffUint64": func(r *Reader) { r.ReadRelOffUint64(0) },
		"ReadIPv4": func(r *Reader) { r.ReadIPv4(0) },
	}
	for name, read := range leaves {
		cases = append(cases, readCase{name, fieldPast, read, 0})
	}
	for name, read := range outOfLine {
		cases = append(cases, readCase{name + " field", fieldPast, read, 0}, readCase{name + " target", targetPast, read, 0})
	}
	for _, tc := range cases {
		d := scalarMessage(2)
		tc.edit(d)
		binary.LittleEndian.PutUint16(d[30:], 0xFFF0) // slot 1, also past the buffer
		err := read(d, func(r *Reader) {
			tc.read(r)
			r.ReadUint32(1) // a later fault does not replace the first
		})
		var bounds *FieldBoundsError
		if !errors.As(err, &bounds) || bounds.Slot != tc.slot {
			t.Errorf("%s: %v, want a FieldBoundsError for slot %d", tc.name, err, tc.slot)
		}
	}
}

// A vector field whose offset lies past the buffer is an error, not an absent vector.
func TestReadVectorCountFieldPastBuffer(t *testing.T) {
	t.Parallel()
	d := vectorMessage(0, 0)
	binary.LittleEndian.PutUint16(d[28:], 0xFFF0)
	r, err := NewReader(d)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadVectorCount(0); err == nil {
		t.Fatalf("ReadVectorCount = %d, nil; want an error", n)
	}
}

// optionalMessage builds a message whose slots 0 and 1 are an Optional: a tag at
// object offset 4 and a RelativeOffset at 8 to a value of 4 + len(tail) bytes.
func optionalMessage(tag byte, rel uint32, value uint32, tail string) []byte {
	d := make([]byte, 40+len(tail))
	le := binary.LittleEndian
	le.PutUint32(d[0:], 8)
	le.PutUint32(d[4:], 1)
	le.PutUint32(d[12:], 4)                  // message at 16
	le.PutUint32(d[16:], uint32(0xFFFFFFF4)) // vtable at 28
	d[20] = tag
	le.PutUint32(d[24:], rel) // value at 36 when rel is 12
	le.PutUint16(d[28:], 8)
	le.PutUint16(d[30:], 12)
	le.PutUint16(d[32:], 4)
	le.PutUint16(d[34:], 8)
	le.PutUint32(d[36:], value)
	copy(d[40:], tail)
	return d
}

// The Optional helpers follow C++: only tag 1 is present, and a value outside the
// buffer is recorded.
func TestReadOptionalTagsAndBounds(t *testing.T) {
	t.Parallel()
	read := func(d []byte) (int32, bool, string, bool, error) {
		t.Helper()
		r, err := NewReader(d)
		if err != nil {
			t.Fatal(err)
		}
		i, iok := r.ReadOptionalInt32(0, 1)
		s, sok := r.ReadOptionalString(0, 1)
		return i, iok, s, sok, r.Err()
	}
	if i, iok, s, sok, err := read(optionalMessage(1, 12, 2, "hi")); i != 2 || !iok || s != "hi" || !sok || err != nil {
		t.Fatalf("tag 1: %d %t %q %t %v", i, iok, s, sok, err)
	}
	if _, iok, _, sok, err := read(optionalMessage(2, 12, 2, "hi")); iok || sok || err != nil {
		t.Fatalf("tag 2: present %t/%t, %v; want absent", iok, sok, err)
	}
	// Each helper records the fault on a reader of its own.
	for name, readOne := range map[string]func(r *Reader) bool{
		"int32":  func(r *Reader) bool { _, ok := r.ReadOptionalInt32(0, 1); return ok },
		"string": func(r *Reader) bool { _, ok := r.ReadOptionalString(0, 1); return ok },
	} {
		r, err := NewReader(optionalMessage(1, 1<<20, 2, "hi"))
		if err != nil {
			t.Fatal(err)
		}
		present := readOne(r)
		var bounds *FieldBoundsError
		if err := r.Err(); present || !errors.As(err, &bounds) || bounds.Slot != 1 {
			t.Fatalf("%s value past the buffer: present %t, %v; want a FieldBoundsError for slot 1", name, present, err)
		}
	}
}
