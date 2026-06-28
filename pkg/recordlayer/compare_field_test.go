package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("compareKeys", func() {
	// --- Part C: Unit tests for compareKeys ---

	It("returns 0 for both empty tuples", func() {
		r, err := compareKeys(tuple.Tuple{}, tuple.Tuple{})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns 0 for same-length equal keys", func() {
		r, err := compareKeys(tuple.Tuple{int64(1), "a"}, tuple.Tuple{int64(1), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns negative when first element is less", func() {
		r, err := compareKeys(tuple.Tuple{int64(1), "a"}, tuple.Tuple{int64(2), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("uses second element as tiebreaker", func() {
		r, err := compareKeys(tuple.Tuple{int64(1), "a"}, tuple.Tuple{int64(1), "b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns negative for shorter tuple", func() {
		r, err := compareKeys(tuple.Tuple{int64(1)}, tuple.Tuple{int64(1), "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns positive for longer tuple", func() {
		r, err := compareKeys(tuple.Tuple{int64(1), "a"}, tuple.Tuple{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically(">", 0))
	})

	It("returns 0 for nil vs nil", func() {
		r, err := compareKeys(nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("returns negative for nil vs non-empty", func() {
		r, err := compareKeys(nil, tuple.Tuple{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("returns positive for non-empty vs nil", func() {
		r, err := compareKeys(tuple.Tuple{int64(1)}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically(">", 0))
	})

	It("returns error for unsupported types in keys", func() {
		type custom struct{ x int }
		_, err := compareKeys(tuple.Tuple{custom{x: 1}}, tuple.Tuple{custom{x: 2}})
		Expect(err).To(HaveOccurred())
	})

	It("handles multi-element keys with mixed types", func() {
		a := tuple.Tuple{int64(1), "hello", float64(3.14), true, []byte{0x42}}
		b := tuple.Tuple{int64(1), "hello", float64(3.14), true, []byte{0x42}}
		r, err := compareKeys(a, b)
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("orders nil element before non-nil within a tuple", func() {
		// nil as a tuple element sorts before any typed value per FDB tuple encoding
		r, err := compareKeys(tuple.Tuple{nil}, tuple.Tuple{int64(0)})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})

	It("cross-type: int64 vs string produces non-zero result", func() {
		// Different FDB tuple type codes produce different byte sequences.
		r, err := compareKeys(tuple.Tuple{int64(1)}, tuple.Tuple{"1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).NotTo(Equal(0))
	})

	It("returns 0 for equal versionstamp tuples", func() {
		vs := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 5}}
		r, err := compareKeys(tuple.Tuple{vs}, tuple.Tuple{vs})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(0))
	})

	It("orders versionstamps by TransactionVersion", func() {
		vs1 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}
		vs2 := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 2}}
		r, err := compareKeys(tuple.Tuple{vs1}, tuple.Tuple{vs2})
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(BeNumerically("<", 0))
	})
})

var _ = Describe("Union/Intersection with compareKeys", func() {
	// --- Part D: Integration tests with UnionCursor ---

	Describe("Union with int64 comparison keys", func() {
		It("merges and deduplicates sorted int64 streams", func() {
			ctx := context.Background()

			c1 := FromList([]int64{1, 3, 5})
			c2 := FromList([]int64{2, 3, 4})
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v string) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v string) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v string) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
			compKey := func(v int64) tuple.Tuple { return tuple.Tuple{v} }
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
