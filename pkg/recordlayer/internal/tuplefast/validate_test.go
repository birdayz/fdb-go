package tuplefast

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Validate must fail exactly where Unpack fails: on every packed tuple, every
// truncation of one, every one-byte mutation, and random bytes.
func TestValidateMatchesUnpack(t *testing.T) {
	t.Parallel()
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	tuples := []tuple.Tuple{
		{},
		{nil},
		{int64(0), int64(1), int64(-1), int64(255), int64(-256), int64(1 << 40), int64(-1 << 40)},
		{int64(math.MaxInt64), int64(math.MinInt64), uint64(math.MaxUint64)},
		{huge, new(big.Int).Neg(huge)},
		{"", "a\x00b", []byte{0, 0xff, 0}, true, false},
		{float32(-1.5), float64(2.5), tuple.UUID{1, 2, 3}},
		{tuple.Versionstamp{TransactionVersion: [10]byte{1, 2, 3}, UserVersion: 7}},
		{tuple.Tuple{nil, "x", tuple.Tuple{int64(3), nil}}, int64(9)},
		{"writer-0042", int64(1004), int64(0)},
	}
	check := func(b []byte) {
		t.Helper()
		_, unpackErr := Unpack(b)
		if got := Validate(b); (got == nil) != (unpackErr == nil) {
			t.Fatalf("%x: Validate=%v, Unpack=%v", b, got, unpackErr)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, tup := range tuples {
		packed := tup.Pack()
		check(packed)
		for n := range len(packed) {
			check(packed[:n])
			for range 8 {
				mutated := append([]byte(nil), packed...)
				mutated[n] = byte(rng.IntN(256))
				check(mutated)
			}
		}
	}
	for range 20000 {
		b := make([]byte, rng.IntN(24))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		check(b)
	}
}
