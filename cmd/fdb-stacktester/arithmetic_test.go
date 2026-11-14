//go:build bazelrunfiles

package main

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func TestSubHandlesUnsignedTupleIntegersWithoutTruncation(t *testing.T) {
	t.Parallel()
	wide := new(big.Int).Lsh(big.NewInt(1), 65)
	negativeWide := new(big.Int).Neg(new(big.Int).Set(wide))
	for _, tc := range []struct {
		name string
		a, b any
		want string
	}{
		{"unsigned minus signed", uint64(1 << 63), int64(1), "9223372036854775807"},
		{"unsigned minus larger", uint64(math.MaxUint64), wide, "-18446744073709551617"},
		{"signed minus unsigned", int64(-1), uint64(math.MaxUint64), "-18446744073709551616"},
		{"unsigned minus unsigned", uint64(math.MaxUint64), uint64(1 << 63), "9223372036854775807"},
		{"larger minus unsigned", wide, uint64(math.MaxUint64), "18446744073709551617"},
		{"unsigned minus negative", uint64(math.MaxUint64), negativeWide, "55340232221128654847"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := &StackMachine{}
			unsigned := false
			// Decode real PUSH instructions: the Apple tuple decoder returns
			// uint64, rather than *big.Int, for positive values above MaxInt64.
			for i, value := range []any{tc.b, tc.a} {
				instruction, err := tuple.Unpack(tuple.Tuple{"PUSH", value}.Pack())
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := instruction[1].(uint64); ok {
					unsigned = true
				}
				if err := sm.execute(context.Background(), i, instruction[0].(string), instruction[1]); err != nil {
					t.Fatal(err)
				}
			}
			if !unsigned {
				t.Fatal("fixture did not reach the unsigned tuple decoding boundary")
			}
			if err := sm.execute(context.Background(), 42, "SUB", nil); err != nil {
				t.Fatal(err)
			}
			if len(sm.stack) != 1 || sm.stack[0].idx != 42 {
				t.Fatalf("SUB stack shape=%v", sm.stack)
			}
			got, ok := sm.pop().value.(*big.Int)
			if !ok || got.String() != tc.want {
				t.Fatalf("SUB result=%v want %s", got, tc.want)
			}
		})
	}
	if wide.String() != "36893488147419103232" || negativeWide.String() != "-36893488147419103232" {
		t.Fatal("SUB mutated its input integers")
	}
}
