package fdb

import (
	"bytes"
	"testing"
)

// TestTuplePackInt64_FDBVectors pins tuplePackInt64 to FDB's canonical minimal-width
// tuple integer encoding (Tuple.cpp Tuple::append(int64_t)). tenant nameIndex and lastId
// are TupleCodec<int64_t>; libfdb_c/Java write these vectors, so a Go client sharing the
// cluster must produce byte-identical output (and decode them). The previous fixed 9-byte
// form failed every small-ID case here — which is exactly why a Go client could not open a
// tenant created by libfdb_c. Vectors are hand-derived from the FDB tuple spec:
//
//	0      -> 0x14
//	1..255 -> 0x15 <1 byte>
//	256..  -> 0x16 <2 bytes>, etc. (type code 0x14+n, n = significant bytes)
//	<0     -> 0x14-n <n bytes one's-complement>
func TestTuplePackInt64_FDBVectors(t *testing.T) {
	cases := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x14}},
		{1, []byte{0x15, 0x01}},
		{5, []byte{0x15, 0x05}},
		{255, []byte{0x15, 0xFF}},
		{256, []byte{0x16, 0x01, 0x00}},
		{258, []byte{0x16, 0x01, 0x02}},
		{65535, []byte{0x16, 0xFF, 0xFF}},
		{65536, []byte{0x17, 0x01, 0x00, 0x00}},
		{0xFFFFFFFFFF, []byte{0x19, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}}, // 5 bytes
		{1 << 56, []byte{0x1C, 0x01, 0, 0, 0, 0, 0, 0, 0}},         // first value needing the full 8 bytes
		{-1, []byte{0x13, 0xFE}},                                   // negative: 0x14-1, one's complement
		{-256, []byte{0x12, 0xFE, 0xFF}},
	}
	for _, c := range cases {
		got := tuplePackInt64(c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("tuplePackInt64(%d) = % x, want % x", c.v, got, c.want)
		}
		// Round-trip.
		back, err := tupleUnpackInt64(got)
		if err != nil {
			t.Errorf("tupleUnpackInt64(% x): %v", got, err)
			continue
		}
		if back != c.v {
			t.Errorf("round-trip %d -> % x -> %d", c.v, got, back)
		}
	}
}

// TestTupleUnpackInt64_LegacyWideForm verifies the decoder still reads the non-canonical
// fixed 9-byte form a PRIOR Go client may have written to a cluster (n == 8: 0x1C + 8
// big-endian bytes), so the fix is backward-compatible with already-persisted metadata.
func TestTupleUnpackInt64_LegacyWideForm(t *testing.T) {
	cases := []struct {
		data []byte
		want int64
	}{
		{[]byte{0x1C, 0, 0, 0, 0, 0, 0, 0, 5}, 5},
		{[]byte{0x1C, 0, 0, 0, 0, 0, 0, 0, 1}, 1},
		{[]byte{0x1C, 0, 0, 0, 0, 0, 0, 0, 0}, 0}, // wide-form zero
	}
	for _, c := range cases {
		got, err := tupleUnpackInt64(c.data)
		if err != nil {
			t.Errorf("tupleUnpackInt64(% x): %v", c.data, err)
			continue
		}
		if got != c.want {
			t.Errorf("tupleUnpackInt64(% x) = %d, want %d", c.data, got, c.want)
		}
	}
}

// TestTupleUnpackInt64_Errors pins rejection of malformed encodings.
func TestTupleUnpackInt64_Errors(t *testing.T) {
	cases := [][]byte{
		nil,                // empty
		{},                 // empty
		{0x15},             // claims 1 trailing byte, has 0
		{0x16, 0x01},       // claims 2 trailing bytes, has 1
		{0x15, 0x01, 0x02}, // claims 1 trailing byte, has 2 (trailing garbage)
		{0x14, 0x00},       // zero code with trailing byte
		{0x00},             // type code 0x00 is not an integer
		{0xFF},             // type code out of integer range
	}
	for _, c := range cases {
		if _, err := tupleUnpackInt64(c); err == nil {
			t.Errorf("tupleUnpackInt64(% x): expected error, got nil", c)
		}
	}
}
