package wire

import (
	"encoding/binary"
	"errors"
	"testing"
)

// vectorMessage builds a message whose only field (slot 0) is a vector declaring
// count elements, followed by room for exactly room element offsets.
func vectorMessage(count uint32, room int) []byte {
	d := make([]byte, 36+4*room)
	put := binary.LittleEndian.PutUint32
	put(d[0:], 8)  // root offset: FakeRoot at 8
	put(d[4:], 1)  // file identifier
	put(d[12:], 4) // FakeRoot field 0: message at 16
	put(d[16:], uint32(0xFFFFFFF8))
	put(d[20:], 12)                          // slot 0: vector at 32
	binary.LittleEndian.PutUint16(d[24:], 6) // message vtable at 24 (soffset -8)
	binary.LittleEndian.PutUint16(d[26:], 8)
	binary.LittleEndian.PutUint16(d[28:], 4)
	put(d[32:], count)
	return d
}

// A count the buffer cannot hold is malformed; ReadVectorCount used to cap it
// and let callers read the bytes after the vector as element offsets.
func TestReadVectorCount_RejectsOversizedCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		count uint32
		room  int
		ok    bool
	}{
		{count: 0, room: 0, ok: true},
		{count: 3, room: 3, ok: true},
		{count: 2, room: 3, ok: true},
		{count: 4, room: 3},
		{count: 1, room: 0},
		{count: 0xFFFFFFFF, room: 3},
	} {
		r, err := NewReader(vectorMessage(tc.count, tc.room))
		if err != nil {
			t.Fatal(err)
		}
		n, err := r.ReadVectorCount(0)
		if tc.ok {
			if err != nil || n != int(tc.count) {
				t.Fatalf("count %d, room %d: got (%d, %v), want (%d, nil)", tc.count, tc.room, n, err, tc.count)
			}
			continue
		}
		var countErr *VectorCountError
		if !errors.As(err, &countErr) || countErr.Count != tc.count || countErr.Capacity != tc.room || countErr.Slot != 0 {
			t.Fatalf("count %d, room %d: got (%d, %v), want a VectorCountError", tc.count, tc.room, n, err)
		}
	}
}
