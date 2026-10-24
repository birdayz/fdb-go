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

	cases := []struct {
		name string
		edit func(d []byte)
		read func(r *Reader)
		slot int
	}{
		{"scalar", func(d []byte) { binary.LittleEndian.PutUint16(d[28:], 0xFFF0) }, func(r *Reader) { r.ReadUint32(0) }, 0},
		{"bytes target", func(d []byte) { binary.LittleEndian.PutUint32(d[20:], 1<<20) }, func(r *Reader) { r.ReadBytes(0) }, 0},
		{"bare out-of-line scalar", func(d []byte) { binary.LittleEndian.PutUint32(d[20:], 1<<20) }, func(r *Reader) { r.ReadRelOffUint64(0) }, 0},
		{"vtable longer than the buffer", func(d []byte) { binary.LittleEndian.PutUint16(d[24:], 400) }, func(r *Reader) { r.FieldPresent(100) }, 100},
	}
	for _, tc := range cases {
		d := scalarMessage(2)
		tc.edit(d)
		err := read(d, func(r *Reader) {
			tc.read(r)
			r.ReadUint32(1) // a later absent read keeps the first fault
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
