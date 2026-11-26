package rlcollections

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

type bloomAliasKey []byte

func (k bloomAliasKey) FDBKey() fdb.Key { return fdb.Key(k) }

func TestBloomFunnelNormalizesUnsignedAndByteKeyAliases(t *testing.T) {
	t.Parallel()
	for _, values := range [][]any{
		{int64(7), uint(7), uint64(7)},
		{uint(math.MaxUint64), uint64(math.MaxUint64)},
		{[]byte{0, 1, 255}, fdb.Key{0, 1, 255}, bloomAliasKey{0, 1, 255}},
	} {
		want, err := bloomKeyFunnel(tuple.Tuple{values[0]})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values[1:] {
			got, err := bloomKeyFunnel(tuple.Tuple{value})
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("alias %T: %x want %x / %v", value, got, want, err)
			}
		}
	}
	nested := tuple.Tuple{"word"}
	got, err := bloomKeyFunnel(tuple.Tuple{nested})
	if err != nil {
		t.Fatal(err)
	}
	want := tuple.Tuple{nested}.Pack()
	if !bytes.Equal(got, want) || bytes.Equal(got, nested.Pack()) {
		t.Fatalf("nested tuple was flattened: %x, nested want %x", got, want)
	}
	raw, err := bloomKeyFunnel(tuple.Tuple{fdb.Key(nested.Pack())})
	if err != nil || bytes.Equal(got, raw) {
		t.Fatalf("nested tuple confused with byte-key alias: %x / %v", raw, err)
	}
}

func TestGuavaBloomFilterHashCountBoundary(t *testing.T) {
	t.Parallel()
	for _, hashes := range []int{1, 255} {
		f, err := NewGuavaBloomFilter(500, math.Ldexp(1, -hashes))
		if err != nil {
			t.Fatalf("create %d hashes: %v", hashes, err)
		}
		if f.numHashFunctions != hashes {
			t.Fatalf("hash count = %d, want %d", f.numHashFunctions, hashes)
		}
		key := tuple.Tuple{int64(42)}
		if err := f.Put(key); err != nil {
			t.Fatal(err)
		}
		encoded := f.WriteTo()
		if int(encoded[1]) != hashes {
			t.Fatalf("encoded hash count = %d, want %d", encoded[1], hashes)
		}
		restored, err := ReadGuavaBloomFilter(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if restored.numHashFunctions != hashes || !bytes.Equal(restored.WriteTo(), encoded) {
			t.Fatalf("%d-hash filter changed on round trip", hashes)
		}
		if contains, err := restored.MightContain(key); err != nil || !contains {
			t.Fatalf("inserted key: contains=%v, err=%v", contains, err)
		}
		if contains, err := restored.MightContain(tuple.Tuple{int64(43)}); err != nil || contains {
			t.Fatalf("absent key: contains=%v, err=%v", contains, err)
		}
	}

	for _, hashes := range []int{256, 1074} {
		f, err := NewGuavaBloomFilter(500, math.Ldexp(1, -hashes))
		var argumentErr *BloomArgumentError
		if f != nil || !errors.As(err, &argumentErr) {
			t.Fatalf("%d hashes: filter=%v, want BloomArgumentError, got %v", hashes, f, err)
		}
		for _, context := range []string{"numHashFunctions", "255", "expectedInsertions=500", "fpp="} {
			if !strings.Contains(argumentErr.Message, context) {
				t.Fatalf("missing %q context in %q", context, argumentErr.Message)
			}
		}
	}
}

func TestGuavaBloomFilterRejectsZeroHashCount(t *testing.T) {
	t.Parallel()
	for _, strategy := range []byte{BloomStrategyMitz32, BloomStrategyMitz64} {
		// A complete one-word filter, not a truncated-header/bit-array failure.
		encoded := []byte{strategy, 0, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		f, err := ReadGuavaBloomFilter(encoded)
		var coreErr *BloomCoreError
		var argumentErr *BloomArgumentError
		if f != nil || !errors.As(err, &coreErr) || !errors.As(err, &argumentErr) {
			t.Fatalf("strategy %d: filter=%v, want typed decode error and argument cause, got %v", strategy, f, err)
		}
		if coreErr.Message != "unable to deserialize bloom filter" || !strings.Contains(argumentErr.Message, "numHashFunctions (0)") {
			t.Fatalf("strategy %d: decode error=%v, cause=%v", strategy, coreErr, argumentErr)
		}
	}
}

func TestGuavaBloomFilterSizingAndRoundTrip(t *testing.T) {
	t.Parallel()
	f, err := NewGuavaBloomFilter(500, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	// Guava 33: floor(-500 ln 0.01 / ln²2) = 4792 bits in 75 words, round(-log2 0.01) = 7 hashes.
	if len(f.data) != 75 || f.numHashFunctions != 7 {
		t.Fatalf("words=%d hashes=%d, want 75/7", len(f.data), f.numHashFunctions)
	}
	keys := []tuple.Tuple{{int64(1)}, {"a", int64(-7)}, {[]byte{1, 2}}, {nil}, {true, 2.5}}
	for _, k := range keys {
		if err := f.Put(k); err != nil {
			t.Fatal(err)
		}
	}
	b := f.WriteTo()
	if !bytes.Equal(b[:6], []byte{1, 7, 0, 0, 0, 75}) {
		t.Fatalf("header = %v", b[:6])
	}
	r, err := ReadGuavaBloomFilter(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.WriteTo(), b) {
		t.Fatal("WriteTo mismatch after round-trip")
	}
	for _, k := range keys {
		ok, err := r.MightContain(k)
		if err != nil || !ok {
			t.Fatalf("MightContain(%v) = %v, %v", k, ok, err)
		}
	}
	if _, err := ReadGuavaBloomFilter([]byte{}); err == nil {
		t.Fatal("expected error for empty bytes")
	}
	if _, err := NewGuavaBloomFilter(10, 1.0); err == nil {
		t.Fatal("expected error for fpp=1.0")
	}
}

func TestMurmur3EmptyIsZero(t *testing.T) {
	t.Parallel()
	h1, h2 := murmur3x64128(nil)
	if h1 != 0 || h2 != 0 {
		t.Fatalf("murmur3x64128(nil) = (%d, %d), want (0, 0)", h1, h2)
	}
}
