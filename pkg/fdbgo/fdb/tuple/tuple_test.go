package tuple

import (
	"math"
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
