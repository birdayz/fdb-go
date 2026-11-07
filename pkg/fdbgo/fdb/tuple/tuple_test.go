package tuple

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	. "github.com/onsi/gomega"
)

// TestPackWithPrefix verifies PackWithPrefix produces identical bytes to
// prefix + Pack.
func TestPackWithPrefix(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	cases := []struct {
		name   string
		prefix []byte
		tuple  Tuple
	}{
		{"empty tuple", []byte{0x01, 0x02}, Tuple{}},
		{"single int", []byte{0xAB}, Tuple{int64(42)}},
		{"single string", []byte{0x01}, Tuple{"hello"}},
		{"multi element", []byte{0x01, 0x02, 0x03}, Tuple{int64(1), "two", int64(3)}},
		{"nil prefix", nil, Tuple{int64(99)}},
		{"empty prefix", []byte{}, Tuple{"test"}},
		{"bytes element", []byte{0xFF}, Tuple{[]byte{0xDE, 0xAD}}},
		{"nil element", []byte{0x01}, Tuple{nil}},
		{"negative int", []byte{0x01}, Tuple{int64(-42)}},
		{"large int", []byte{0x01}, Tuple{int64(math.MaxInt64)}},
		{"float", []byte{0x01}, Tuple{3.14}},
		{"bool true", []byte{0x01}, Tuple{true}},
		{"bool false", []byte{0x01}, Tuple{false}},
		{"nested tuple", []byte{0x01}, Tuple{Tuple{int64(1), int64(2)}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expected := append(append([]byte{}, tc.prefix...), tc.tuple.Pack()...)
			got := tc.tuple.PackWithPrefix(tc.prefix)
			g.Expect(got).To(Equal(expected), "PackWithPrefix mismatch for %s", tc.name)
		})
	}
}

// TestPackConcatWithPrefix verifies concatenated tuple packing.
func TestPackConcatWithPrefix(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	cases := []struct {
		name   string
		prefix []byte
		tuples []Tuple
	}{
		{"two tuples", []byte{0x01}, []Tuple{{int64(1)}, {int64(2)}}},
		{"three tuples", []byte{0xAB}, []Tuple{{int64(1)}, {"two"}, {int64(3)}}},
		{"empty first", []byte{0x01}, []Tuple{{}, {int64(1)}}},
		{"empty second", []byte{0x01}, []Tuple{{int64(1)}, {}}},
		{"both empty", []byte{0x01}, []Tuple{{}, {}}},
		{"single tuple", []byte{0x01}, []Tuple{{int64(42), "hello"}}},
		{"no tuples", []byte{0x01}, []Tuple{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Expected: prefix + Pack(t1) + Pack(t2) + ...
			var expected []byte
			expected = append(expected, tc.prefix...)
			for _, tup := range tc.tuples {
				expected = append(expected, tup.Pack()...)
			}
			got := PackConcatWithPrefix(tc.prefix, tc.tuples...)
			g.Expect(got).To(Equal(expected), "PackConcatWithPrefix mismatch for %s", tc.name)
		})
	}
}

// TestPack1WithPrefix verifies single-element packing.
func TestPack1WithPrefix(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	elems := []TupleElement{
		int64(42), int64(-1), int64(0), int64(math.MaxInt64), int64(math.MinInt64),
		"hello", "", "a",
		[]byte{0xDE, 0xAD},
		[]byte{},
		nil, true, false, 3.14, float32(2.5),
	}

	prefix := []byte{0x01, 0x02}
	for _, elem := range elems {
		expected := append(append([]byte{}, prefix...), Tuple{elem}.Pack()...)
		got := Pack1WithPrefix(prefix, elem)
		g.Expect(got).To(Equal(expected), "Pack1WithPrefix mismatch for %v", elem)
	}
}

// AppendString is Pack of the string element, NUL bytes escaped, appended in place.
func TestAppendString(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	for _, s := range []string{"", "token", "\x00", "a\x00b", "\x00lead", "trail\x00", "a\x00\x00b\x00", "\xff\x00\xff"} {
		prefix := []byte("pre")
		dst := append(make([]byte, 0, 64), prefix...)
		got := AppendString(dst, s)
		g.Expect(got).To(Equal(append([]byte("pre"), Tuple{s}.Pack()...)), "%q", s)
		g.Expect(&got[0]).To(BeIdenticalTo(&dst[0]), "%q: reallocated a buffer with room", s)
		decoded, err := Unpack(got[len(prefix):])
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(decoded).To(Equal(Tuple{s}))
	}
	g.Expect(AppendString(nil, "a\x00b\x00")).To(Equal([]byte{0x02, 'a', 0x00, 0xff, 'b', 0x00, 0xff, 0x00}))
}

// TestPack1ConcatWithPrefix verifies single element + suffix tuple.
func TestPack1ConcatWithPrefix(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0xAB, 0xCD}
	elem := int64(42)
	suffix := Tuple{"pk1", int64(99)}

	expected := append(append([]byte{}, prefix...), Tuple{elem}.Pack()...)
	expected = append(expected, suffix.Pack()...)

	got := Pack1ConcatWithPrefix(prefix, elem, suffix)
	g.Expect(got).To(Equal(expected))
}

// TestPackerPoolRoundtrip verifies the public Packer API produces correct output.
func TestPackerPoolRoundtrip(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	pk := GetPacker()
	defer PutPacker(pk)

	pk.EncodeInt(42)
	pk.EncodeElement("hello")
	pk.EncodeTuple(Tuple{int64(99)})

	prefix := []byte{0x01}
	var buf []byte
	got := pk.AppendInto(&buf, prefix)

	// Expected: prefix + Pack(42) + Pack("hello") + Pack(Tuple{99})
	expected := append([]byte{}, prefix...)
	expected = append(expected, Tuple{int64(42)}.Pack()...)
	expected = append(expected, Tuple{"hello"}.Pack()...)
	expected = append(expected, Tuple{int64(99)}.Pack()...)
	g.Expect(got).To(Equal(expected))
}

// TestPack1_IncompleteVersionstampPanics pins that the single-element APIs (Pack1WithPrefix,
// Pack1ConcatWithPrefix, Packer.EncodeElement) reject an INCOMPLETE versionstamp exactly as the
// vanilla Tuple.Pack path does (encodeTuple guard, tuple.go:455) — failing loud instead of silently
// encoding an unresolved 0xFF×10 versionstamp that would never be filled in at commit. A COMPLETE
// versionstamp encodes fine and matches Tuple.Pack byte-for-byte.
func TestPack1_IncompleteVersionstampPanics(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	prefix := []byte{0x01, 0x02}
	incomplete := IncompleteVersionstamp(7)

	// Incomplete → panic on every single-element entry point (matching Tuple.Pack).
	g.Expect(func() { Tuple{incomplete}.Pack() }).To(Panic(), "Tuple.Pack baseline")
	g.Expect(func() { Pack1WithPrefix(prefix, incomplete) }).To(Panic(), "Pack1WithPrefix")
	g.Expect(func() { Pack1ConcatWithPrefix(prefix, incomplete, Tuple{int64(1)}) }).To(Panic(), "Pack1ConcatWithPrefix")
	// GetPacker() (not a bare Packer{}, whose nil p would nil-deref) so the panic is the
	// versionstamp guard, not an uninitialized-packer crash.
	g.Expect(func() { GetPacker().EncodeElement(incomplete) }).To(Panic(), "Packer.EncodeElement")

	// Complete versionstamp → encodes, byte-identical to Tuple.Pack.
	var tv [10]byte
	for i := range tv {
		tv[i] = byte(i + 1)
	}
	complete := Versionstamp{TransactionVersion: tv, UserVersion: 9}
	expected := append(append([]byte{}, prefix...), Tuple{complete}.Pack()...)
	g.Expect(Pack1WithPrefix(prefix, complete)).To(Equal(expected), "complete versionstamp must encode")
}

// decodeIntBinaryRead is decodeInt as it decoded before the stack array:
// binary.Read of the right-aligned bytes.
func decodeIntBinaryRead(b []byte) (any, int, error) {
	if b[0] == intZeroCode {
		return int64(0), 1, nil
	}
	var neg bool
	n := int(b[0]) - intZeroCode
	if n < 0 {
		n = -n
		neg = true
	}
	if n+1 > len(b) {
		return nil, 0, fmt.Errorf("insufficient bytes to decode %d-byte integer", n)
	}
	bp := make([]byte, 8)
	copy(bp[8-n:], b[1:n+1])
	var ret int64
	binary.Read(bytes.NewBuffer(bp), binary.BigEndian, &ret)
	if neg {
		return ret - int64(sizeLimits[n]), n + 1, nil
	}
	if ret > 0 {
		return ret, n + 1, nil
	}
	return uint64(ret), n + 1, nil
}

// decodeInt decodes every integer type code and length exactly as the
// binary.Read version did, including truncated input, zero bytes, the
// sign bit and non-minimal encodings, and allocates at most the boxing.
func TestDecodeIntMatchesBinaryRead(t *testing.T) {
	rng := rand.New(rand.NewPCG(283, 7))
	patterns := [][]byte{{}, {0}, {0xff}, {0x80}, {0x7f}, {0, 0, 0, 0, 0, 0, 0, 0}, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, {0x80, 0, 0, 0, 0, 0, 0, 1}, {1, 2, 3, 4, 5, 6, 7, 8}}
	for range 2000 {
		p := make([]byte, rng.IntN(10))
		for i := range p {
			p[i] = byte(rng.UintN(256))
		}
		patterns = append(patterns, p)
	}
	for code := negIntStart + 1; code < posIntEnd; code++ {
		for _, p := range patterns {
			b := append([]byte{byte(code)}, p...)
			got, goff, gerr := decodeInt(b)
			want, woff, werr := decodeIntBinaryRead(b)
			if goff != woff || (gerr == nil) != (werr == nil) || fmt.Sprintf("%T %v", got, got) != fmt.Sprintf("%T %v", want, want) {
				t.Fatalf("%x: decodeInt = %T %v, %d, %v; binary.Read = %T %v, %d, %v", b, got, got, goff, gerr, want, want, woff, werr)
			}
		}
	}
	b := Tuple{int64(1) << 40}.Pack()
	allocs := testing.AllocsPerRun(100, func() { _, _, _ = decodeInt(b) })
	before := testing.AllocsPerRun(100, func() { _, _, _ = decodeIntBinaryRead(b) })
	t.Logf("decodeInt allocations: %v, with binary.Read %v", allocs, before)
	if allocs > 1 {
		t.Fatalf("decodeInt allocates %v times", allocs)
	}
}
