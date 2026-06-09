package tuple

import (
	"bytes"
	"reflect"
	"testing"
)

// TestUnpackMalformedReturnsError pins Unpack's documented contract — "or an
// error if the key does not correctly encode a FoundationDB tuple" — on
// malformed inputs found by fuzzing the SPFresh codec layer built on top of
// Unpack. Most of these used to PANIC (slice out of range); two were silent
// WRONG-ACCEPTS (an unterminated bytes element whose payload happened to end
// the input decoded as a shorter value instead of erroring). Library code must
// never panic on data bytes (design principle #4), and malformed encodings
// must error, not decode to something plausible.
func TestUnpackMalformedReturnsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
	}{
		{"string code, no terminator, empty payload", []byte{0x02}},
		{"string code, no terminator", []byte{0x02, 'a', 'b'}},
		{"bytes code, no terminator", []byte{0x01, 0xff}},
		{"bytes code, escaped-00 then no terminator", []byte{0x01, 0x00, 0xff, 'x'}},
		{"8-byte int, truncated payload", []byte{0x1c, 0x01}},
		{"4-byte int, truncated payload", []byte{0x18, 0x01, 0x02}},
		{"neg 8-byte int discriminator at end of input", []byte{0x0c}},
		{"neg 8-byte int, truncated payload", []byte{0x0c, 0x80, 0x01}},
		{"posIntEnd bigint, missing length byte", []byte{0x1d}},
		{"posIntEnd bigint, truncated payload", []byte{0x1d, 0x09, 0x01}},
		{"negIntStart bigint, missing length byte", []byte{0x0b}},
		{"negIntStart bigint, truncated payload", []byte{0x0b, 0xf6, 0x01}},
		{"nested tuple with truncated string", []byte{0x05, 0x02, 'a'}},
		{"valid element then truncated second", []byte{0x14, 0x02}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tup, err := Unpack(c.in)
			if err == nil {
				t.Fatalf("Unpack(%x) = %v, want error", c.in, tup)
			}
		})
	}
}

// FuzzUnpack: Unpack must never panic on arbitrary bytes, and anything it
// accepts must re-pack and re-unpack to an equal tuple (decode/encode
// coherence — Pack can legitimately normalize, so we compare the second
// decode against the first).
func FuzzUnpack(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x02})
	f.Add([]byte{0x1c, 0x01})
	f.Add(Tuple{int64(42), "hello", []byte{0, 1}, 3.14, true, nil}.Pack())
	f.Add(Tuple{Tuple{int64(-5), "nested"}, int64(1) << 60}.Pack())
	f.Fuzz(func(t *testing.T, data []byte) {
		tup, err := Unpack(data)
		if err != nil {
			return
		}
		repacked := tup.Pack()
		tup2, err := Unpack(repacked)
		if err != nil {
			t.Fatalf("re-unpack of accepted tuple failed: %v (orig %x, repacked %x)", err, data, repacked)
		}
		// Element-wise equality, not just length: decoded element types are
		// canonical, so DeepEqual is exact — except NaN (never equal to
		// itself), which we exempt by comparing through a re-pack instead.
		if !reflect.DeepEqual(tup2, tup) {
			if !bytes.Equal(tup2.Pack(), repacked) {
				t.Fatalf("re-unpack element mismatch: %v vs %v (orig %x)", tup2, tup, data)
			}
		}
	})
}
