package recordlayer

import (
	"bytes"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Java's element order is the order of the packed bytes for every tuple Pack
// accepts; compareTuples must agree with it without packing.
func TestCompareTuplesMatchesPackedOrder(t *testing.T) {
	t.Parallel()
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	elements := []tuple.TupleElement{
		nil, int64(math.MinInt64), int64(-70000), int64(-1), int64(0), int64(1), int64(255), int64(256),
		int64(math.MaxInt64), uint64(math.MaxUint64), huge, new(big.Int).Neg(huge), int(7), uint(9),
		"", "a", "a\x00", "a\x00\xff", "ab", "é", "\U0001F600",
		[]byte{},
		[]byte{0},
		[]byte{0, 0xff},
		[]byte{1},
		fdb.Key("k"),
		float32(math.Inf(-1)), float32(-1), float32(math.Copysign(0, -1)), float32(0), float32(2), float32(math.NaN()),
		math.Inf(-1), -2.0, math.Copysign(0, -1), 0.0, 3.5, math.Inf(1), math.NaN(),
		false, true,
		tuple.UUID{},
		tuple.UUID{0xff},
		tuple.UUID{0, 1},
		tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 2},
		tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 3},
		tuple.Tuple{},
		tuple.Tuple{nil},
		tuple.Tuple{int64(1)},
		tuple.Tuple{int64(1), nil},
		tuple.Tuple{tuple.Tuple{}},
	}
	rng := rand.New(rand.NewPCG(3, 4))
	tuples := []tuple.Tuple{{}}
	for _, e := range elements {
		tuples = append(tuples, tuple.Tuple{e})
	}
	for range 400 {
		n := 1 + rng.IntN(3)
		tup := make(tuple.Tuple, n)
		for i := range tup {
			tup[i] = elements[rng.IntN(len(elements))]
		}
		tuples = append(tuples, tup)
	}
	for _, a := range tuples {
		pa := a.Pack()
		for _, b := range tuples {
			want := bytes.Compare(pa, b.Pack())
			if got := compareTuples(a, b); got != want {
				t.Fatalf("compareTuples(%v, %v) = %d, packed order %d", a, b, got, want)
			}
		}
	}
}

// Integers compare by value whatever their Go type, as Java's BigInteger
// fallback does; incomplete versionstamps, which Pack refuses, follow every
// complete one and order by user version (Versionstamp.compareTo).
func TestCompareTuplesBeyondPack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b tuple.TupleElement
		want int
	}{
		{int(5), int64(5), 0},
		{uint64(5), big.NewInt(5), 0},
		{uint(math.MaxUint64), int64(math.MaxInt64), 1},
		{uint64(math.MaxUint64), new(big.Int).SetUint64(math.MaxUint64), 0},
		{big.NewInt(-1), uint64(0), -1},
		{*big.NewInt(3), int64(2), 1},
		{fdb.Key("ab"), []byte("ab"), 0},
		{tuple.IncompleteVersionstamp(0), tuple.Versionstamp{TransactionVersion: [10]byte{0xff, 0xff}, UserVersion: 9}, 1},
		{tuple.IncompleteVersionstamp(1), tuple.IncompleteVersionstamp(2), -1},
		{tuple.IncompleteVersionstamp(4), tuple.IncompleteVersionstamp(4), 0},
		{tuple.Tuple{tuple.IncompleteVersionstamp(1)}, tuple.Tuple{tuple.IncompleteVersionstamp(0)}, 1},
	} {
		if got := compareTuples(tuple.Tuple{c.a}, tuple.Tuple{c.b}); got != c.want {
			t.Errorf("compareTuples(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareTuples(tuple.Tuple{c.b}, tuple.Tuple{c.a}); got != -c.want {
			t.Errorf("compareTuples(%v, %v) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestCompareTuplesDoesNotAllocate(t *testing.T) {
	a, b := tuple.Tuple{"writer-0042", int64(1004), int64(0)}, tuple.Tuple{"writer-0042", int64(1004), int64(1)}
	if n := testing.AllocsPerRun(100, func() { compareTuples(a, b) }); n != 0 {
		t.Fatalf("compareTuples allocates %v times", n)
	}
}
