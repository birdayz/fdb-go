package tuple

import (
	"bytes"
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

// TestPackInt64Into verifies int64 packing into shared buffer.
func TestPackInt64Into(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01, 0x02}
	vals := []int64{0, 1, -1, 42, -42, math.MaxInt64, math.MinInt64, 255, 256, 65535}

	for _, val := range vals {
		var buf []byte
		got := PackInt64Into(&buf, prefix, val)

		expected := Tuple{val}.PackWithPrefix(prefix)
		g.Expect(got).To(Equal(expected), "PackInt64Into mismatch for %d", val)
	}
}

// TestPackInt64ConcatInto verifies int64 + suffix packing into shared buffer.
func TestPackInt64ConcatInto(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01}
	val := int64(42)
	suffix := Tuple{"pk"}

	var buf []byte
	got := PackInt64ConcatInto(&buf, prefix, val, suffix)

	expected := append(append([]byte{}, prefix...), Tuple{val}.Pack()...)
	expected = append(expected, suffix.Pack()...)
	g.Expect(got).To(Equal(expected))
}

// TestIntoVariantsSharedBuffer verifies that multiple Into calls on the same
// buffer produce independent, correct results. This is the core safety property:
// buffer growth must not corrupt previously-returned sub-slices.
func TestIntoVariantsSharedBuffer(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01}
	var buf []byte

	// Pack 100 different keys into the same buffer.
	results := make([][]byte, 100)
	for i := 0; i < 100; i++ {
		results[i] = PackInt64Into(&buf, prefix, int64(i))
	}

	// Verify each result is still correct (not corrupted by later packs).
	for i := 0; i < 100; i++ {
		expected := Tuple{int64(i)}.PackWithPrefix(prefix)
		g.Expect(results[i]).To(Equal(expected),
			"shared buffer corruption at index %d", i)
	}
}

// TestPackWithPrefixInto verifies the Into variant matches PackWithPrefix.
func TestPackWithPrefixInto(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0xAB, 0xCD, 0xEF}
	tuples := []Tuple{
		{int64(1), "two", int64(3)},
		{nil},
		{},
		{int64(math.MinInt64), true, []byte{0xFF}},
	}

	for _, tup := range tuples {
		var buf []byte
		got := tup.PackWithPrefixInto(&buf, prefix)
		expected := tup.PackWithPrefix(prefix)
		g.Expect(got).To(Equal(expected))
	}
}

// TestPackConcatInto verifies the Into variant of PackConcatWithPrefix.
func TestPackConcatInto(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01}
	t1 := Tuple{int64(1), "hello"}
	t2 := Tuple{int64(2)}

	var buf []byte
	got := PackConcatInto(&buf, prefix, t1, t2)
	expected := PackConcatWithPrefix(prefix, t1, t2)
	g.Expect(got).To(Equal(expected))
}

// TestPack1Into verifies the Into variant of Pack1WithPrefix.
func TestPack1Into(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01, 0x02}
	elem := int64(42)

	var buf []byte
	got := Pack1Into(&buf, prefix, elem)
	expected := Pack1WithPrefix(prefix, elem)
	g.Expect(got).To(Equal(expected))
}

// TestPack1ConcatInto verifies the Into variant of Pack1ConcatWithPrefix.
func TestPack1ConcatInto(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := []byte{0x01}
	elem := "hello"
	suffix := Tuple{int64(99)}

	var buf []byte
	got := Pack1ConcatInto(&buf, prefix, elem, suffix)
	expected := Pack1ConcatWithPrefix(prefix, elem, suffix)
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

// TestAppendPackedGrowth verifies appendPacked handles buffer growth correctly
// by filling a small initial buffer then packing larger entries.
func TestAppendPackedGrowth(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	prefix := bytes.Repeat([]byte{0xAB}, 50)
	var buf []byte

	// Pack progressively larger tuples to force multiple buffer growths.
	for i := 0; i < 50; i++ {
		key := Tuple{int64(i), string(bytes.Repeat([]byte("x"), i*10))}
		result := key.PackWithPrefixInto(&buf, prefix)
		expected := key.PackWithPrefix(prefix)
		g.Expect(result).To(Equal(expected), "growth corruption at iteration %d", i)
	}
}

// FuzzPackIntoEquivalence fuzzes the Into variants against the allocating versions.
// Seeds a tuple from fuzz input, packs via both paths, and checks byte equality.
// This catches buffer corruption, off-by-one errors, and growth bugs.
func FuzzPackIntoEquivalence(f *testing.F) {
	f.Add(int64(0), "hello", []byte{0x01, 0x02}, int64(42))
	f.Add(int64(-1), "", []byte{}, int64(0))
	f.Add(int64(math.MaxInt64), "test", []byte{0xFF}, int64(math.MinInt64))
	f.Add(int64(255), "x", []byte{0xAB, 0xCD, 0xEF}, int64(256))

	f.Fuzz(func(t *testing.T, a int64, s string, prefix []byte, b int64) {
		tup := Tuple{a, s, b}

		// Test PackWithPrefixInto
		var buf1 []byte
		got1 := tup.PackWithPrefixInto(&buf1, prefix)
		exp1 := tup.PackWithPrefix(prefix)
		if !bytes.Equal(got1, exp1) {
			t.Errorf("PackWithPrefixInto mismatch: got %x, want %x", got1, exp1)
		}

		// Test Pack1Into (single element)
		var buf2 []byte
		got2 := Pack1Into(&buf2, prefix, a)
		exp2 := Pack1WithPrefix(prefix, a)
		if !bytes.Equal(got2, exp2) {
			t.Errorf("Pack1Into mismatch: got %x, want %x", got2, exp2)
		}

		// Test PackInt64Into
		var buf3 []byte
		got3 := PackInt64Into(&buf3, prefix, a)
		exp3 := Tuple{a}.PackWithPrefix(prefix)
		if !bytes.Equal(got3, exp3) {
			t.Errorf("PackInt64Into mismatch: got %x, want %x", got3, exp3)
		}

		// Test PackConcatInto (two tuples)
		t1 := Tuple{a, s}
		t2 := Tuple{b}
		var buf4 []byte
		got4 := PackConcatInto(&buf4, prefix, t1, t2)
		exp4 := PackConcatWithPrefix(prefix, t1, t2)
		if !bytes.Equal(got4, exp4) {
			t.Errorf("PackConcatInto mismatch: got %x, want %x", got4, exp4)
		}

		// Test shared buffer with multiple packs (growth safety)
		var shared []byte
		results := make([][]byte, 10)
		for i := 0; i < 10; i++ {
			results[i] = PackInt64Into(&shared, prefix, a+int64(i))
		}
		for i := 0; i < 10; i++ {
			exp := Tuple{a + int64(i)}.PackWithPrefix(prefix)
			if !bytes.Equal(results[i], exp) {
				t.Errorf("shared buffer corruption at %d: got %x, want %x", i, results[i], exp)
			}
		}
	})
}
