package rtree

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestPersistedHilbertNumbers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"zero", int64(0), "0"},
		{"negative", int64(-1), "-1"},
		{"min int64", int64(math.MinInt64), "-9223372036854775808"},
		{"max int64", int64(math.MaxInt64), "9223372036854775807"},
		{"int64 above float precision", int64(9007199254740993), "9007199254740993"},
		{"uint64 zero", uint64(0), "0"},
		{"uint64 above float precision", uint64(9007199254740993), "9007199254740993"},
		{"uint64 max signed", uint64(math.MaxInt64), "9223372036854775807"},
		{"uint64 high bit", uint64(1) << 63, "9223372036854775808"},
		{"uint64 high bit plus one", uint64(1)<<63 + 1, "9223372036854775809"},
		{"max uint64", uint64(math.MaxUint64), "18446744073709551615"},
		{"big pointer small", big.NewInt(17), "17"},
		{"big pointer high bit", new(big.Int).Lsh(big.NewInt(1), 63), "9223372036854775808"},
		{"big pointer above uint64", new(big.Int).Lsh(big.NewInt(1), 64), "18446744073709551616"},
		{"big pointer above uint64 plus one", new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1)), "18446744073709551617"},
		{"big value above uint64", *new(big.Int).Lsh(big.NewInt(1), 64), "18446744073709551616"},
		{"big value above uint64 plus one", *new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1)), "18446744073709551617"},
		{"big negative below int64", new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 64)), "-18446744073709551616"},
		{"big negative value below int64 minus one", *new(big.Int).Sub(new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 64)), big.NewInt(1)), "-18446744073709551617"},
		// The bit patterns pin adjacent representations at both long limits.
		{"float64 positive zero", float64(0), "0"},
		{"float64 negative zero", math.Float64frombits(0x8000000000000000), "0"},
		{"float64 positive fraction", float64(1.75), "1"},
		{"float64 negative fraction", float64(-1.75), "-1"},
		{"float64 positive subunit", float64(0.75), "0"},
		{"float64 negative subunit", float64(-0.75), "0"},
		{"float64 below two to 63", math.Float64frombits(0x43dfffffffffffff), "9223372036854774784"},
		{"float64 two to 63", float64(0x1p63), "9223372036854775807"},
		{"float64 above two to 63", math.Float64frombits(0x43e0000000000001), "9223372036854775807"},
		{"float64 above negative two to 63", math.Float64frombits(0xc3dfffffffffffff), "-9223372036854774784"},
		{"float64 negative two to 63", float64(-0x1p63), "-9223372036854775808"},
		{"float64 below negative two to 63", math.Float64frombits(0xc3e0000000000001), "-9223372036854775808"},
		{"float64 max finite", math.Float64frombits(0x7fefffffffffffff), "9223372036854775807"},
		{"float64 min finite", math.Float64frombits(0xffefffffffffffff), "-9223372036854775808"},
		{"float64 NaN", math.Float64frombits(0x7ff8000000000042), "0"},
		{"float64 negative NaN", math.Float64frombits(0xfff8000000000042), "0"},
		{"float64 positive infinity", math.Inf(1), "9223372036854775807"},
		{"float64 negative infinity", math.Inf(-1), "-9223372036854775808"},
		{"float32 positive zero", float32(0), "0"},
		{"float32 negative zero", math.Float32frombits(0x80000000), "0"},
		{"float32 positive fraction", float32(1.75), "1"},
		{"float32 negative fraction", float32(-1.75), "-1"},
		{"float32 positive subunit", float32(0.75), "0"},
		{"float32 negative subunit", float32(-0.75), "0"},
		{"float32 below two to 63", math.Float32frombits(0x5effffff), "9223371487098961920"},
		{"float32 two to 63", float32(0x1p63), "9223372036854775807"},
		{"float32 above two to 63", math.Float32frombits(0x5f000001), "9223372036854775807"},
		{"float32 above negative two to 63", math.Float32frombits(0xdeffffff), "-9223371487098961920"},
		{"float32 negative two to 63", float32(-0x1p63), "-9223372036854775808"},
		{"float32 below negative two to 63", math.Float32frombits(0xdf000001), "-9223372036854775808"},
		{"float32 max finite", math.Float32frombits(0x7f7fffff), "9223372036854775807"},
		{"float32 min finite", math.Float32frombits(0xff7fffff), "-9223372036854775808"},
		{"float32 NaN", math.Float32frombits(0x7fc00042), "0"},
		{"float32 negative NaN", math.Float32frombits(0xffc00042), "0"},
		{"float32 positive infinity", float32(math.Inf(1)), "9223372036854775807"},
		{"float32 negative infinity", float32(math.Inf(-1)), "-9223372036854775808"},
	}
	storage := &StorageAdapter{config: DefaultRTreeConfig(1)}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Exercise the numeric decoder and both readers before and after unpacking.
			// A stored item value must not be silently recomputed from point 5.
			for _, packed := range []bool{false, true} {
				itemList := tuple.Tuple{hilbertItemTuple(tc.value)}
				childList := tuple.Tuple{hilbertChildTuple(tc.value, tc.value)}
				if packed {
					var err error
					itemList, err = tuple.Unpack(itemList.Pack())
					if err != nil {
						t.Fatal(err)
					}
					childList, err = tuple.Unpack(childList.Pack())
					if err != nil {
						t.Fatal(err)
					}
				}
				value := itemList[0].(tuple.Tuple)[0]
				got, err := decodeHilbertValue(value)
				if err != nil || got == nil || got.String() != tc.want {
					t.Fatalf("decodeHilbertValue(%T %v) (packed %t) = %v, %v; want %s", value, value, packed, got, err, tc.want)
				}
				items, err := storage.deserializeItemSlots(itemList)
				if err != nil {
					t.Fatalf("item decode (packed %t): %v", packed, err)
				}
				if len(items) != 1 || items[0].HilbertValue == nil || items[0].HilbertValue.String() != tc.want {
					t.Fatalf("item decode (packed %t) = %+v; want Hilbert value %s", packed, items, tc.want)
				}
				children, err := storage.deserializeChildSlots(childList)
				if err != nil {
					t.Fatalf("child decode (packed %t): %v", packed, err)
				}
				if len(children) != 1 || children[0].SmallestHV == nil || children[0].LargestHV == nil ||
					children[0].SmallestHV.String() != tc.want || children[0].LargestHV.String() != tc.want {
					t.Fatalf("child decode (packed %t) = %+v; want both Hilbert bounds %s", packed, children, tc.want)
				}
			}
		})
	}
}

func TestPersistedHilbertInvalidTypes(t *testing.T) {
	t.Parallel()

	storage := &StorageAdapter{config: DefaultRTreeConfig(1)}
	for _, tc := range []struct {
		name        string
		value       any
		wantType    string
		packedError bool
	}{
		{"string", "9223372036854775808", "string", true},
		{"bytes", []byte{1}, "[]uint8", true},
		{"bool", true, "bool", true},
		{"tuple", tuple.Tuple{int64(1)}, "tuple.Tuple", true},
		// Native integers normalize to int64/uint64; a nil big pointer cannot pack.
		{"int", int(1), "int", false},
		{"uint", uint(1), "uint", false},
		{"nil big pointer", (*big.Int)(nil), "", false},
		{"nil", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkError := func(err error) {
				t.Helper()
				if tc.wantType == "" {
					var target *NullPointerError
					if !errors.As(err, &target) {
						t.Fatalf("got %v, want NullPointerError", err)
					}
				} else {
					var target *ClassCastError
					if !errors.As(err, &target) || target.ValueType != tc.wantType {
						t.Fatalf("got %v, want ClassCastError with value type %s", err, tc.wantType)
					}
				}
			}
			for _, packed := range []bool{false, true} {
				if packed && !tc.packedError {
					continue
				}
				values := tuple.Tuple{tc.value}
				if packed {
					var err error
					values, err = tuple.Unpack(values.Pack())
					if err != nil {
						t.Fatal(err)
					}
				}
				_, err := decodeHilbertValue(values[0])
				checkError(err)
				if tc.value != nil {
					itemList := tuple.Tuple{hilbertItemTuple(int64(1)), hilbertItemTuple(tc.value)}
					if packed {
						itemList, err = tuple.Unpack(itemList.Pack())
						if err != nil {
							t.Fatal(err)
						}
					}
					items, err := storage.deserializeItemSlots(itemList)
					checkError(err)
					if items != nil || !strings.Contains(err.Error(), "item slot 1 Hilbert value") {
						t.Fatalf("invalid item (packed %t) = %+v, %v; want nil slots and field context", packed, items, err)
					}
				}
				for _, index := range []int{0, 2} {
					child := hilbertChildTuple(int64(1), int64(2))
					child[index] = tc.value
					childList := tuple.Tuple{hilbertChildTuple(int64(1), int64(2)), child}
					if packed {
						childList, err = tuple.Unpack(childList.Pack())
						if err != nil {
							t.Fatal(err)
						}
					}
					children, err := storage.deserializeChildSlots(childList)
					checkError(err)
					field := "smallest"
					if index == 2 {
						field = "largest"
					}
					if children != nil || !strings.Contains(err.Error(), "child slot 1 "+field+" Hilbert value") {
						t.Fatalf("invalid child (packed %t) = %+v, %v; want nil slots and field context", packed, children, err)
					}
				}
			}
		})
	}
}

func TestPersistedHilbertBigIntegerCopies(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"value", "pointer"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := new(big.Int).Lsh(big.NewInt(1), 64)
			source.SetBit(source, 0, 1)
			var value any = *source
			if name == "pointer" {
				value = source
			}
			storage := &StorageAdapter{config: DefaultRTreeConfig(1)}
			got, err := decodeHilbertValue(value)
			if err != nil {
				t.Fatal(err)
			}
			items, err := storage.deserializeItemSlots(tuple.Tuple{hilbertItemTuple(value)})
			if err != nil {
				t.Fatal(err)
			}
			children, err := storage.deserializeChildSlots(tuple.Tuple{hilbertChildTuple(value, value)})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || len(children) != 1 {
				t.Fatalf("got %d items and %d children; want one each", len(items), len(children))
			}
			numbers := []*big.Int{got, items[0].HilbertValue, children[0].SmallestHV, children[0].LargestHV}
			// Mutate existing words so a shallow copy of big.Int cannot pass.
			source.SetBit(source, 0, 0)
			for i, number := range numbers {
				if number == nil || number.String() != "18446744073709551617" {
					t.Fatalf("decoded value %d = %v after source mutation; want 18446744073709551617", i, number)
				}
				number.SetBit(number, 1, 1)
				if source.String() != "18446744073709551616" {
					t.Fatalf("source = %s after decoded value %d mutation; want 18446744073709551616", source, i)
				}
				for j, other := range numbers[i+1:] {
					if other == nil || other.String() != "18446744073709551617" {
						t.Fatalf("decoded value %d = %v after value %d mutation; want independent copies", i+1+j, other, i)
					}
				}
			}
		})
	}
}

func TestPersistedHilbertNullItemRecomputes(t *testing.T) {
	t.Parallel()

	storage := &StorageAdapter{config: DefaultRTreeConfig(1)}
	for _, packed := range []bool{false, true} {
		itemList := tuple.Tuple{hilbertItemTuple(nil)}
		if packed {
			var err error
			itemList, err = tuple.Unpack(itemList.Pack())
			if err != nil {
				t.Fatal(err)
			}
		}
		items, err := storage.deserializeItemSlots(itemList)
		if err != nil {
			t.Fatal(err)
		}
		// One-dimensional Hilbert values are the coordinate with its sign bit flipped.
		if len(items) != 1 || items[0].HilbertValue == nil || items[0].HilbertValue.String() != "9223372036854775813" {
			t.Fatalf("null item decode (packed %t) = %+v; want 2^63 + 5", packed, items)
		}
	}
}

func hilbertItemTuple(hv any) tuple.Tuple {
	return tuple.Tuple{hv, tuple.Tuple{tuple.Tuple{int64(5)}, tuple.Tuple{int64(7)}}, tuple.Tuple{int64(9)}}
}

func hilbertChildTuple(smallest, largest any) tuple.Tuple {
	key := tuple.Tuple{tuple.Tuple{int64(5)}, tuple.Tuple{int64(7)}}
	return tuple.Tuple{smallest, key, largest, key, make([]byte, 16), tuple.Tuple{int64(5), int64(5)}}
}
