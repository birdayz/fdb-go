package wire

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestReaderReuseClearsFault(t *testing.T) {
	t.Parallel()
	// ErrorOr<uint32 table>: tag 2, followed by a table whose slot 0 is 7.
	union := make([]byte, 40)
	le := binary.LittleEndian
	le.PutUint32(union, 16)
	le.PutUint16(union[8:], 8)
	le.PutUint16(union[10:], 12)
	le.PutUint16(union[12:], 8)
	le.PutUint16(union[14:], 4)
	le.PutUint32(union[16:], 8)
	le.PutUint32(union[20:], 12)
	union[24] = 2
	le.PutUint16(union[26:], 6)
	le.PutUint16(union[28:], 8)
	le.PutUint16(union[30:], 4)
	le.PutUint32(union[32:], 6)
	le.PutUint32(union[36:], 7)
	root, err := ReaderAtRootObject(union)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		init func(*Reader) error
		want uint32
	}{
		{"InitReader", func(r *Reader) error { return InitReader(scalarMessage(1), r) }, 7},
		{"root object", func(r *Reader) error { return initReaderAtRootObject(union, r) }, 2},
		{"nested", func(r *Reader) error { return root.readNestedInto(1, r) }, 7},
		{"ReadErrorOrInto", func(r *Reader) error { return ReadErrorOrInto(union, r) }, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := scalarMessage(1)
			le.PutUint16(bad[28:], 0xFFF0)
			var r Reader
			if err := InitReader(bad, &r); err != nil {
				t.Fatal(err)
			}
			r.ReadUint32(0)
			var bounds *FieldBoundsError
			if !errors.As(r.Err(), &bounds) {
				t.Fatalf("fault setup: %v, want FieldBoundsError", r.Err())
			}
			if err := tc.init(&r); err != nil {
				t.Fatal(err)
			}
			if got := uint32(r.ReadUint8(0)); got != tc.want || r.Err() != nil {
				t.Fatalf("reused Reader: value %d, error %v; want %d, nil", got, r.Err(), tc.want)
			}
		})
	}
}
