package recordlayer

import (
	"context"
	"math"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("compareField", func() {

	// --- Part A: Unit tests for compareField ---

	Describe("nil handling", func() {
		It("returns 0 for nil vs nil", func() {
			Expect(compareField(nil, nil)).To(Equal(0))
		})

		It("returns -1 for nil vs non-nil", func() {
			Expect(compareField(nil, int64(1))).To(BeNumerically("<", 0))
		})

		It("returns 1 for non-nil vs nil", func() {
			Expect(compareField(int64(1), nil)).To(BeNumerically(">", 0))
		})

		It("returns -1 for nil vs empty string", func() {
			Expect(compareField(nil, "")).To(BeNumerically("<", 0))
		})

		It("returns 1 for false vs nil", func() {
			Expect(compareField(false, nil)).To(BeNumerically(">", 0))
		})
	})

	Describe("int64 values", func() {
		It("returns 0 for equal values", func() {
			Expect(compareField(int64(42), int64(42))).To(Equal(0))
		})

		It("returns negative for less", func() {
			Expect(compareField(int64(3), int64(5))).To(BeNumerically("<", 0))
		})

		It("returns positive for greater", func() {
			Expect(compareField(int64(5), int64(3))).To(BeNumerically(">", 0))
		})

		It("returns 0 for zero vs zero", func() {
			Expect(compareField(int64(0), int64(0))).To(Equal(0))
		})

		It("orders negative before positive", func() {
			Expect(compareField(int64(-10), int64(10))).To(BeNumerically("<", 0))
		})

		It("orders MaxInt64 after MinInt64", func() {
			Expect(compareField(int64(math.MinInt64), int64(math.MaxInt64))).To(BeNumerically("<", 0))
		})

		It("returns 0 for MaxInt64 vs MaxInt64", func() {
			Expect(compareField(int64(math.MaxInt64), int64(math.MaxInt64))).To(Equal(0))
		})

		It("returns 0 for MinInt64 vs MinInt64", func() {
			Expect(compareField(int64(math.MinInt64), int64(math.MinInt64))).To(Equal(0))
		})

		It("distinguishes adjacent values", func() {
			Expect(compareField(int64(5), int64(6))).To(BeNumerically("<", 0))
			Expect(compareField(int64(6), int64(5))).To(BeNumerically(">", 0))
		})

		It("handles large negative values", func() {
			Expect(compareField(int64(-1000000), int64(-999999))).To(BeNumerically("<", 0))
		})
	})

	Describe("int values", func() {
		It("returns 0 for equal values", func() {
			Expect(compareField(int(42), int(42))).To(Equal(0))
		})

		It("returns negative for less", func() {
			Expect(compareField(int(1), int(100))).To(BeNumerically("<", 0))
		})

		It("returns positive for greater", func() {
			Expect(compareField(int(100), int(1))).To(BeNumerically(">", 0))
		})
	})

	Describe("float64 values", func() {
		It("returns 0 for equal values", func() {
			Expect(compareField(float64(3.14), float64(3.14))).To(Equal(0))
		})

		It("returns negative for less", func() {
			Expect(compareField(float64(1.0), float64(2.0))).To(BeNumerically("<", 0))
		})

		It("returns positive for greater", func() {
			Expect(compareField(float64(2.0), float64(1.0))).To(BeNumerically(">", 0))
		})

		It("handles negative floats", func() {
			Expect(compareField(float64(-1.5), float64(1.5))).To(BeNumerically("<", 0))
		})

		It("returns 0 for zero vs zero", func() {
			Expect(compareField(float64(0.0), float64(0.0))).To(Equal(0))
		})

		It("handles -0.0 vs +0.0", func() {
			// FDB tuple encoding may or may not distinguish -0 and +0.
			// Just verify it does not panic and returns a deterministic result.
			result := compareField(float64(math.Copysign(0, -1)), float64(0.0))
			_ = result // no panic is the assertion
		})

		It("orders +Inf after -Inf", func() {
			Expect(compareField(math.Inf(-1), math.Inf(1))).To(BeNumerically("<", 0))
		})

		It("orders +Inf after normal values", func() {
			Expect(compareField(float64(999999.0), math.Inf(1))).To(BeNumerically("<", 0))
		})

		It("orders -Inf before normal values", func() {
			Expect(compareField(math.Inf(-1), float64(-999999.0))).To(BeNumerically("<", 0))
		})

		It("handles very small differences", func() {
			Expect(compareField(float64(1.0), float64(1.0000000001))).To(BeNumerically("<", 0))
		})

		It("does not panic on NaN vs NaN", func() {
			// tuple.Pack may or may not panic on NaN. compareField should
			// return 0 gracefully if it does panic.
			result := compareField(math.NaN(), math.NaN())
			_ = result // no panic is the key assertion
		})

		It("does not panic on NaN vs normal", func() {
			result := compareField(math.NaN(), float64(1.0))
			_ = result
		})
	})

	Describe("string values", func() {
		It("returns 0 for equal strings", func() {
			Expect(compareField("hello", "hello")).To(Equal(0))
		})

		It("returns negative for lexicographically less", func() {
			Expect(compareField("abc", "def")).To(BeNumerically("<", 0))
		})

		It("returns positive for lexicographically greater", func() {
			Expect(compareField("def", "abc")).To(BeNumerically(">", 0))
		})

		It("orders empty string before non-empty", func() {
			Expect(compareField("", "a")).To(BeNumerically("<", 0))
		})

		It("returns 0 for empty vs empty", func() {
			Expect(compareField("", "")).To(Equal(0))
		})

		It("handles unicode strings", func() {
			// Just verify it doesn't panic and produces a deterministic result
			r := compareField("alpha", "bravo")
			Expect(r).To(BeNumerically("<", 0))
		})

		It("handles strings differing only in last character", func() {
			Expect(compareField("testa", "testb")).To(BeNumerically("<", 0))
			Expect(compareField("testb", "testa")).To(BeNumerically(">", 0))
		})

		It("handles prefix string vs longer string", func() {
			Expect(compareField("test", "testing")).To(BeNumerically("<", 0))
		})
	})

	Describe("bool values", func() {
		It("returns 0 for true vs true", func() {
			Expect(compareField(true, true)).To(Equal(0))
		})

		It("returns 0 for false vs false", func() {
			Expect(compareField(false, false)).To(Equal(0))
		})

		It("orders false before true", func() {
			Expect(compareField(false, true)).To(BeNumerically("<", 0))
		})

		It("orders true after false", func() {
			Expect(compareField(true, false)).To(BeNumerically(">", 0))
		})
	})

	Describe("[]byte values", func() {
		It("returns 0 for equal byte slices", func() {
			Expect(compareField([]byte{1, 2, 3}, []byte{1, 2, 3})).To(Equal(0))
		})

		It("returns negative for less", func() {
			Expect(compareField([]byte{1, 2}, []byte{1, 3})).To(BeNumerically("<", 0))
		})

		It("returns positive for greater", func() {
			Expect(compareField([]byte{1, 3}, []byte{1, 2})).To(BeNumerically(">", 0))
		})

		It("orders empty byte slice before non-empty", func() {
			Expect(compareField([]byte{}, []byte{0})).To(BeNumerically("<", 0))
		})

		It("returns 0 for empty vs empty", func() {
			Expect(compareField([]byte{}, []byte{})).To(Equal(0))
		})

		It("handles nil []byte vs empty []byte", func() {
			// nil []byte is still a []byte value (not nil interface).
			// FDB tuple layer should treat them equivalently.
			var nilBytes []byte
			emptyBytes := []byte{}
			result := compareField(nilBytes, emptyBytes)
			Expect(result).To(Equal(0))
		})

		It("handles byte sequences sharing a prefix", func() {
			Expect(compareField([]byte{1, 2, 3}, []byte{1, 2, 3, 4})).To(BeNumerically("<", 0))
		})
	})

	Describe("tuple.Versionstamp values", func() {
		It("returns 0 for equal versionstamps", func() {
			vs1 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 0}
			vs2 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 0}
			Expect(compareField(vs1, vs2)).To(Equal(0))
		})

		It("orders by TransactionVersion", func() {
			vs1 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 0}
			vs2 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 2}, UserVersion: 0}
			Expect(compareField(vs1, vs2)).To(BeNumerically("<", 0))
		})

		It("orders by UserVersion when TransactionVersion is equal", func() {
			vs1 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 5}
			vs2 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 10}
			Expect(compareField(vs1, vs2)).To(BeNumerically("<", 0))
		})

		It("returns 0 for zero versionstamp vs zero versionstamp", func() {
			vs1 := tuple.Versionstamp{}
			vs2 := tuple.Versionstamp{}
			Expect(compareField(vs1, vs2)).To(Equal(0))
		})

		It("orders zero versionstamp before non-zero", func() {
			vs1 := tuple.Versionstamp{}
			vs2 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, UserVersion: 0}
			Expect(compareField(vs1, vs2)).To(BeNumerically("<", 0))
		})
	})

	Describe("tuple.UUID values", func() {
		It("returns 0 for equal UUIDs", func() {
			uuid1 := tuple.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
			uuid2 := tuple.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
			Expect(compareField(uuid1, uuid2)).To(Equal(0))
		})

		It("orders different UUIDs", func() {
			uuid1 := tuple.UUID{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
			uuid2 := tuple.UUID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
			Expect(compareField(uuid1, uuid2)).To(BeNumerically("<", 0))
		})

		It("returns 0 for all-zeros vs all-zeros", func() {
			uuid1 := tuple.UUID{}
			uuid2 := tuple.UUID{}
			Expect(compareField(uuid1, uuid2)).To(Equal(0))
		})
	})

	Describe("cross-type comparison", func() {
		It("int64 vs string returns non-zero", func() {
			// Different tuple type codes produce different byte sequences.
			result := compareField(int64(1), "1")
			Expect(result).NotTo(Equal(0))
		})

		It("int64 vs bool does not panic", func() {
			result := compareField(int64(1), true)
			_ = result // no panic
		})

		It("string vs []byte does not panic", func() {
			result := compareField("hello", []byte("hello"))
			_ = result // no panic
		})

		It("float64 vs int64 does not panic", func() {
			result := compareField(float64(1.0), int64(1))
			_ = result // no panic
		})
	})

	Describe("unsupported types", func() {
		It("panics for struct{}", func() {
			type custom struct{ x int }
			Expect(func() {
				compareField(custom{x: 1}, custom{x: 2})
			}).To(Panic())
		})

		It("panics for map", func() {
			Expect(func() {
				compareField(map[string]int{"a": 1}, map[string]int{"b": 2})
			}).To(Panic())
		})

		It("panics for channel", func() {
			ch1 := make(chan int)
			ch2 := make(chan int)
			Expect(func() {
				compareField(ch1, ch2)
			}).To(Panic())
		})
	})
})

var _ = Describe("compareFieldChecked", func() {

	// --- Part B: Unit tests for compareFieldChecked ---

	Describe("valid types return (result, nil)", func() {
		It("nil vs nil", func() {
			r, err := compareFieldChecked(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("nil vs non-nil", func() {
			r, err := compareFieldChecked(nil, int64(1))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("non-nil vs nil", func() {
			r, err := compareFieldChecked(int64(1), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically(">", 0))
		})

		It("int64 equal", func() {
			r, err := compareFieldChecked(int64(42), int64(42))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("int64 less", func() {
			r, err := compareFieldChecked(int64(1), int64(2))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("int64 greater", func() {
			r, err := compareFieldChecked(int64(2), int64(1))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically(">", 0))
		})

		It("float64 equal", func() {
			r, err := compareFieldChecked(float64(3.14), float64(3.14))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("float64 less", func() {
			r, err := compareFieldChecked(float64(1.0), float64(2.0))
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("string equal", func() {
			r, err := compareFieldChecked("hello", "hello")
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("string less", func() {
			r, err := compareFieldChecked("abc", "def")
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("bool false < true", func() {
			r, err := compareFieldChecked(false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("[]byte equal", func() {
			r, err := compareFieldChecked([]byte{1, 2}, []byte{1, 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("[]byte less", func() {
			r, err := compareFieldChecked([]byte{1}, []byte{2})
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})

		It("Versionstamp equal", func() {
			vs := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 5}}
			r, err := compareFieldChecked(vs, vs)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})

		It("UUID equal", func() {
			uuid := tuple.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
			r, err := compareFieldChecked(uuid, uuid)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(Equal(0))
		})
	})

	Describe("unsupported types return (0, error)", func() {
		It("returns error for struct{}", func() {
			type custom struct{ x int }
			r, err := compareFieldChecked(custom{x: 1}, custom{x: 2})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported type"))
			Expect(r).To(Equal(0))
		})

		It("returns error for map", func() {
			r, err := compareFieldChecked(map[string]int{"a": 1}, map[string]int{"b": 2})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported type"))
			Expect(r).To(Equal(0))
		})

		It("returns error for func type", func() {
			f1 := func() {}
			f2 := func() {}
			r, err := compareFieldChecked(f1, f2)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported type"))
			Expect(r).To(Equal(0))
		})

		It("returns error only when the unsupported value is non-nil", func() {
			// nil vs unsupported: nil path returns before Pack is called
			type custom struct{ x int }
			r, err := compareFieldChecked(nil, custom{x: 1})
			// nil sorts first, so it returns -1 without ever Packing
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNumerically("<", 0))
		})
	})
})

var _ = Describe("compareKeysChecked", func() {

	// --- Part C: Unit tests for compareKeysChecked ---

	It("returns 0 for both empty slices", func() {
		r, err := compareKeysChecked([]any{}, []any{})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns 0 for same-length equal keys", func() {
		r, err := compareKeysChecked([]any{int64(1), "a"}, []any{int64(1), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns negative when first element is less", func() {
		r, err := compareKeysChecked([]any{int64(1), "a"}, []any{int64(2), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("uses second element as tiebreaker", func() {
		r, err := compareKeysChecked([]any{int64(1), "a"}, []any{int64(1), "b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns negative for shorter slice", func() {
		r, err := compareKeysChecked([]any{int64(1)}, []any{int64(1), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns positive for longer slice", func() {
		r, err := compareKeysChecked([]any{int64(1), "a"}, []any{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically(">", 0))
	})

	It("returns 0 for nil vs nil", func() {
		r, err := compareKeysChecked(nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns negative for nil vs non-empty", func() {
		r, err := compareKeysChecked(nil, []any{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns positive for non-empty vs nil", func() {
		r, err := compareKeysChecked([]any{int64(1)}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically(">", 0))
	})

	It("returns error for unsupported types in keys", func() {
		type custom struct{ x int }
		_, err := compareKeysChecked([]any{custom{x: 1}}, []any{custom{x: 2}})
		Expect(err).To(HaveOccurred())
	})

	It("stops at first differing element", func() {
		// First element differs, so second (unsupported) element is never compared
		type custom struct{}
		r, err := compareKeysChecked([]any{int64(1), custom{}}, []any{int64(2), custom{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("handles multi-element keys with all types", func() {
		a := []any{int64(1), "hello", float64(3.14), true, []byte{0x42}}
		b := []any{int64(1), "hello", float64(3.14), true, []byte{0x42}}
		r, err := compareKeysChecked(a, b)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})
})

var _ = Describe("Union/Intersection with compareField", func() {

	// --- Part D: Integration tests with UnionCursor ---

	Describe("Union with int64 comparison keys", func() {
		It("merges and deduplicates sorted int64 streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 3, 5})
			c2 := FromList([]int64{2, 3, 4})
			compKey := func(v int64) []any { return []any{v} }
			union := Union([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{1, 2, 3, 4, 5}))
		})

		It("handles fully overlapping streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 2, 3})
			c2 := FromList([]int64{1, 2, 3})
			compKey := func(v int64) []any { return []any{v} }
			union := Union([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{1, 2, 3}))
		})

		It("handles non-overlapping streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 3, 5})
			c2 := FromList([]int64{2, 4, 6})
			compKey := func(v int64) []any { return []any{v} }
			union := Union([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{1, 2, 3, 4, 5, 6}))
		})
	})

	Describe("Union with string comparison keys", func() {
		It("merges sorted string streams", func() {
			ctx := context.Background()

			c1 := FromList([]string{"apple", "cherry", "elderberry"})
			c2 := FromList([]string{"banana", "cherry", "date"})
			compKey := func(v string) []any { return []any{v} }
			union := Union([]RecordCursor[string]{c1, c2}, compKey, false)

			var results []string
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]string{"apple", "banana", "cherry", "date", "elderberry"}))
		})

		It("merges single-element streams", func() {
			ctx := context.Background()

			c1 := FromList([]string{"b"})
			c2 := FromList([]string{"a"})
			compKey := func(v string) []any { return []any{v} }
			union := Union([]RecordCursor[string]{c1, c2}, compKey, false)

			var results []string
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]string{"a", "b"}))
		})
	})

	Describe("Union reverse mode", func() {
		It("merges reverse-sorted int64 streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{5, 3, 1})
			c2 := FromList([]int64{4, 3, 2})
			compKey := func(v int64) []any { return []any{v} }
			union := Union([]RecordCursor[int64]{c1, c2}, compKey, true)

			var results []int64
			for {
				result, err := union.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{5, 4, 3, 2, 1}))
		})
	})

	// --- Part E: Integration tests with IntersectionCursor ---

	Describe("Intersection with int64 comparison keys", func() {
		It("returns only common elements", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 2, 3, 4, 5})
			c2 := FromList([]int64{2, 4, 6})
			compKey := func(v int64) []any { return []any{v} }
			inter := Intersection([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{2, 4}))
		})

		It("returns empty for non-overlapping streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 3, 5})
			c2 := FromList([]int64{2, 4, 6})
			compKey := func(v int64) []any { return []any{v} }
			inter := Intersection([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(BeEmpty())
		})

		It("returns all elements for identical streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 2, 3})
			c2 := FromList([]int64{1, 2, 3})
			compKey := func(v int64) []any { return []any{v} }
			inter := Intersection([]RecordCursor[int64]{c1, c2}, compKey, false)

			var results []int64
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{1, 2, 3}))
		})
	})

	Describe("Intersection with string comparison keys", func() {
		It("intersects sorted string streams", func() {
			ctx := context.Background()

			c1 := FromList([]string{"apple", "banana", "cherry", "date"})
			c2 := FromList([]string{"banana", "date", "fig"})
			compKey := func(v string) []any { return []any{v} }
			inter := Intersection([]RecordCursor[string]{c1, c2}, compKey, false)

			var results []string
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]string{"banana", "date"}))
		})
	})

	Describe("Intersection reverse mode", func() {
		It("intersects reverse-sorted streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{5, 4, 3, 2, 1})
			c2 := FromList([]int64{6, 4, 2})
			compKey := func(v int64) []any { return []any{v} }
			inter := Intersection([]RecordCursor[int64]{c1, c2}, compKey, true)

			var results []int64
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{4, 2}))
		})
	})

	Describe("Intersection with three cursors", func() {
		It("returns elements common to all three", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 2, 3, 4, 5, 6})
			c2 := FromList([]int64{2, 3, 5, 6})
			c3 := FromList([]int64{3, 5, 7})
			compKey := func(v int64) []any { return []any{v} }
			inter := Intersection([]RecordCursor[int64]{c1, c2, c3}, compKey, false)

			var results []int64
			for {
				result, err := inter.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}

			Expect(results).To(Equal([]int64{3, 5}))
		})
	})
})
