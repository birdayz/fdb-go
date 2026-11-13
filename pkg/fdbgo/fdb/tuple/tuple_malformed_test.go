package tuple

import (
	"bytes"
	"math/big"
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
			for name, unpack := range map[string]func([]byte) (Tuple, error){"Unpack": Unpack, "UnpackStrict": UnpackStrict} {
				tup, err := unpack(c.in)
				if err == nil {
					t.Fatalf("%s(%x) = %v, want error", name, c.in, tup)
				}
			}
		})
	}
}

func TestUnpackStrictMalformed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty unterminated nested", []byte{0x05}},
		{"unterminated nested integer", []byte{0x05, 0x15, 0x01}},
		{"unterminated outer nested", []byte{0x05, 0x05, 0x00}},
		{"unterminated inner and outer", []byte{0x05, 0x05, 0x15, 0x01}},
		{"escaped null is not nested terminator", []byte{0x05, 0x00, 0xff}},
		{"invalid UTF-8", []byte{0x02, 0xff, 0x00}},
		{"nested invalid UTF-8", []byte{0x05, 0x02, 0xff, 0x00, 0x00}},
		{"overlong UTF-8", []byte{0x02, 0xc0, 0xaf, 0x00}},
		{"UTF-8 surrogate", []byte{0x02, 0xed, 0xa0, 0x80, 0x00}},
		{"truncated UTF-8 codepoint", []byte{0x02, 0xe2, 0x82, 0x00}},
		{"UTF-8 above Unicode maximum", []byte{0x02, 0xf4, 0x90, 0x80, 0x80, 0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := UnpackStrict(tc.in); err == nil {
				t.Fatalf("UnpackStrict(%x) = %v, want error", tc.in, got)
			}
			// Strict restoration must not change existing Unpack callers.
			if _, err := Unpack(tc.in); err != nil {
				t.Fatalf("legacy Unpack(%x) changed: %v", tc.in, err)
			}
		})
	}
}

func TestUnpackStrictValid(t *testing.T) {
	t.Parallel()
	allTypes := Tuple{
		[]byte{0xff, 0x00, 0xfe},
		"a\x00é😀", int64(-42), int64(42),
		new(big.Int).Lsh(big.NewInt(1), 80), float32(1.25), float64(-2.5),
		true, false,
		UUID{1, 2, 3},
		Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 2},
		nil,
		Tuple{nil, Tuple{int64(1)}, "nested\x00é"},
	}
	for _, tc := range []struct {
		name string
		in   []byte
		want Tuple
	}{
		{"empty", nil, nil},
		{"empty nested", []byte{0x05, 0x00}, Tuple{Tuple(nil)}},
		{"nested null", []byte{0x05, 0x00, 0xff, 0x00}, Tuple{Tuple{nil}}},
		{"nested integer", []byte{0x05, 0x15, 0x01, 0x00}, Tuple{Tuple{int64(1)}}},
		{"noncanonical integer", []byte{0x16, 0x00, 0x01}, Tuple{int64(1)}},
		{"all types", allTypes.Pack(), allTypes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnpackStrict(tc.in)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("UnpackStrict(%x) = %v, %v; want %v", tc.in, got, err, tc.want)
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
