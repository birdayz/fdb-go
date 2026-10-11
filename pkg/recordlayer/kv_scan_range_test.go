package recordlayer

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestKVScanBoundsPrefixLength(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("idx", int64(3))
	sl := len(ss.Bytes())
	for _, tc := range []struct {
		name string
		rng  TupleRange
		want int
	}{
		{"all", TupleRangeAll, sl},
		{"allOf", TupleRangeAllOf(tuple.Tuple{int64(11)}), len(ss.Pack(tuple.Tuple{int64(11)}))},
		{"between shares a type code", TupleRangeBetween(tuple.Tuple{int64(5)}, tuple.Tuple{int64(9)}), sl + 1},
		{"open high", TupleRange{Low: tuple.Tuple{int64(5)}, LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeTreeEnd}, sl},
		{"prefix string drops the terminator", TupleRangePrefixString("ab"), len(ss.Pack(tuple.Tuple{"ab"})) - 1},
		{"one prefix-string endpoint keeps it", TupleRange{Low: tuple.Tuple{"ab"}, High: tuple.Tuple{"ab"}, LowEndpoint: EndpointTypePrefixString, HighEndpoint: EndpointTypeRangeInclusive}, len(ss.Pack(tuple.Tuple{"ab"}))},
	} {
		b := newKVScanBounds(ss, tc.rng.Low, tc.rng.High, tc.rng.LowEndpoint, tc.rng.HighEndpoint)
		if b.prefixLength != tc.want {
			t.Errorf("%s: prefixLength %d, want %d", tc.name, b.prefixLength, tc.want)
		}
	}
}

// TestKVScanBoundsKeyRange pins TupleRange.toRange's bytes and the resume key
// lowBytes[:prefixLength]+inner (KeyValueCursorBase.java:322-340).
func TestKVScanBoundsKeyRange(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("idx")
	p := func(v int64) []byte { return ss.Pack(tuple.Tuple{v}) }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	allOf := TupleRangeAllOf(tuple.Tuple{int64(11)})
	inner := tuple.Tuple{int64(2), int64(2)}.Pack()
	token, err := wrapContinuation(inner)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		rng        TupleRange
		cont       []byte
		reverse    bool
		begin, end []byte
	}{
		{"all", TupleRangeAll, nil, false, ss.Bytes(), cat(ss.Bytes(), []byte{0xFF})},
		{"allOf", allOf, nil, false, p(11), cat(p(11), []byte{0xFF})},
		{"exclusive high", TupleRangeBetween(tuple.Tuple{int64(1)}, tuple.Tuple{int64(4)}), nil, false, p(1), p(4)},
		{"forward resume", allOf, token, false, cat(p(11), inner, []byte{0x00}), cat(p(11), []byte{0xFF})},
		{"reverse resume", allOf, token, true, p(11), cat(p(11), inner)},
		{"prefix string", TupleRangePrefixString("ab"), nil, false, cat(ss.Bytes(), []byte{0x02, 'a', 'b'}), cat(ss.Bytes(), []byte{0x02, 'a', 'c'})},
	} {
		b := newKVScanBounds(ss, tc.rng.Low, tc.rng.High, tc.rng.LowEndpoint, tc.rng.HighEndpoint)
		rng, err := b.keyRange(tc.cont, tc.reverse)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if begin, end := rng.Begin.FDBKey(), rng.End.FDBKey(); !bytes.Equal(begin, tc.begin) || !bytes.Equal(end, tc.end) {
			t.Errorf("%s: [%x, %x), want [%x, %x)", tc.name, rng.Begin, rng.End, tc.begin, tc.end)
		}
	}
}

func TestKVScanBoundsRejectsInvalidEndpoints(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("idx")
	for _, rng := range []TupleRange{
		{LowEndpoint: EndpointTypeTreeEnd, HighEndpoint: EndpointTypeTreeEnd},
		{LowEndpoint: EndpointTypeTreeStart, HighEndpoint: EndpointTypeTreeStart},
		{Low: tuple.Tuple{"a"}, High: tuple.Tuple{"b"}, LowEndpoint: EndpointTypePrefixString, HighEndpoint: EndpointTypePrefixString},
		{Low: tuple.Tuple{int64(1)}, High: tuple.Tuple{int64(1)}, LowEndpoint: EndpointTypePrefixString, HighEndpoint: EndpointTypePrefixString},
	} {
		b := newKVScanBounds(ss, rng.Low, rng.High, rng.LowEndpoint, rng.HighEndpoint)
		if _, err := b.keyRange(nil, false); err == nil {
			t.Errorf("%+v: want error", rng)
		}
	}
}
