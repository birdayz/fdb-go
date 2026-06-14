package recordlayer

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("cursor_util", func() {
	ctx := context.Background()

	Describe("ForEach", func() {
		It("applies function to each element", func() {
			var collected []int
			err := ForEach(ctx, FromList([]int{1, 2, 3}), func(v int) error {
				collected = append(collected, v)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(collected).To(Equal([]int{1, 2, 3}))
		})

		It("returns nil for empty cursor", func() {
			var collected []int
			err := ForEach(ctx, FromList([]int{}), func(v int) error {
				collected = append(collected, v)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(collected).To(BeEmpty())
		})

		It("propagates function errors", func() {
			sentinel := fmt.Errorf("stop")
			err := ForEach(ctx, FromList([]int{1, 2, 3}), func(v int) error {
				if v == 2 {
					return sentinel
				}
				return nil
			})
			Expect(err).To(MatchError("stop"))
		})

		It("propagates cursor errors", func() {
			cursor := &errorCursor[int]{err: fmt.Errorf("cursor broken")}
			err := ForEach(ctx, cursor, func(_ int) error { return nil })
			Expect(err).To(MatchError("cursor broken"))
		})
	})

	Describe("AsList", func() {
		It("collects all elements", func() {
			items, err := AsList(ctx, FromList([]string{"a", "b", "c"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]string{"a", "b", "c"}))
		})

		It("returns nil slice for empty cursor", func() {
			items, err := AsList(ctx, FromList([]int{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())
		})

		It("propagates cursor errors", func() {
			_, err := AsList(ctx, &errorCursor[int]{err: fmt.Errorf("fail")})
			Expect(err).To(MatchError("fail"))
		})
	})

	Describe("AsListWithContinuation", func() {
		It("returns all items and nil continuation when source exhausted", func() {
			items, cont, err := AsListWithContinuation(ctx, FromList([]int{10, 20, 30}))
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{10, 20, 30}))
			Expect(cont).To(BeNil())
		})

		It("returns empty and nil continuation for empty cursor", func() {
			items, cont, err := AsListWithContinuation(ctx, FromList([]int{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())
			Expect(cont).To(BeNil())
		})

		It("returns continuation when cursor stops before end", func() {
			// Use a LimitRows cursor to simulate stopping before end
			inner := FromList([]int{1, 2, 3, 4, 5})
			limited := LimitRowsCursor(inner, 3)
			items, cont, err := AsListWithContinuation(ctx, limited)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{1, 2, 3}))
			Expect(cont).NotTo(BeNil()) // Should have a continuation
		})

		It("propagates cursor errors", func() {
			_, _, err := AsListWithContinuation(ctx, &errorCursor[int]{err: fmt.Errorf("fail")})
			Expect(err).To(MatchError("fail"))
		})
	})

	Describe("First", func() {
		It("returns first element", func() {
			v, err := First(ctx, FromList([]int{42, 99}))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).NotTo(BeNil())
			Expect(*v).To(Equal(42))
		})

		It("returns nil for empty cursor", func() {
			v, err := First(ctx, FromList([]int{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
		})

		It("propagates cursor errors", func() {
			_, err := First(ctx, &errorCursor[int]{err: fmt.Errorf("fail")})
			Expect(err).To(MatchError("fail"))
		})
	})

	Describe("GetCount", func() {
		It("counts all elements", func() {
			count, err := GetCount(ctx, FromList([]string{"a", "b", "c", "d"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(4))
		})

		It("returns 0 for empty cursor", func() {
			count, err := GetCount(ctx, FromList([]int{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(0))
		})

		It("returns 1 for single element", func() {
			count, err := GetCount(ctx, FromList([]int{42}))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(1))
		})

		It("propagates cursor errors", func() {
			_, err := GetCount(ctx, &errorCursor[int]{err: fmt.Errorf("fail")})
			Expect(err).To(MatchError("fail"))
		})
	})

	Describe("Reduce", func() {
		It("sums integers", func() {
			result, err := Reduce(ctx, FromList([]int{1, 2, 3, 4}), 0, func(acc, v int) int {
				return acc + v
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(10))
		})

		It("concatenates strings", func() {
			result, err := Reduce(ctx, FromList([]string{"a", "b", "c"}), "", func(acc, v string) string {
				return acc + v
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("abc"))
		})

		It("returns initial value for empty cursor", func() {
			result, err := Reduce(ctx, FromList([]int{}), 42, func(acc, v int) int {
				return acc + v
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(42))
		})

		It("propagates cursor errors", func() {
			_, err := Reduce(ctx, &errorCursor[int]{err: fmt.Errorf("fail")}, 0, func(acc, v int) int {
				return acc + v
			})
			Expect(err).To(MatchError("fail"))
		})
	})

	Describe("Filter (iter.Seq)", func() {
		It("filters elements matching predicate", func() {
			seq := mustSeq(FromList([]int{1, 2, 3, 4, 5, 6}), ctx)
			evens := Filter(seq, func(v int) bool { return v%2 == 0 })
			var collected []int
			for v := range evens {
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]int{2, 4, 6}))
		})

		It("returns empty for no matches", func() {
			seq := mustSeq(FromList([]int{1, 3, 5}), ctx)
			evens := Filter(seq, func(v int) bool { return v%2 == 0 })
			var collected []int
			for v := range evens {
				collected = append(collected, v)
			}
			Expect(collected).To(BeEmpty())
		})
	})

	Describe("Map (iter.Seq)", func() {
		It("transforms elements", func() {
			seq := mustSeq(FromList([]int{1, 2, 3}), ctx)
			doubled := Map(seq, func(v int) int { return v * 2 })
			var collected []int
			for v := range doubled {
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]int{2, 4, 6}))
		})

		It("handles type conversion", func() {
			seq := mustSeq(FromList([]int{1, 2, 3}), ctx)
			strs := Map(seq, func(v int) string { return fmt.Sprintf("%d", v) })
			var collected []string
			for v := range strs {
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]string{"1", "2", "3"}))
		})
	})

	Describe("Filter2 (iter.Seq2)", func() {
		It("filters value-error pairs", func() {
			seq := Seq2(FromList([]int{1, 2, 3, 4, 5}), ctx)
			evens := Filter2(seq, func(v int) bool { return v%2 == 0 })
			var collected []int
			for v, err := range evens {
				Expect(err).NotTo(HaveOccurred())
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]int{2, 4}))
		})

		It("passes through errors without filtering", func() {
			cursor := &errorCursor[int]{err: fmt.Errorf("oops")}
			seq := Seq2[int](cursor, ctx)
			evens := Filter2(seq, func(v int) bool { return v%2 == 0 })
			var gotErr error
			for _, err := range evens {
				if err != nil {
					gotErr = err
					break
				}
			}
			Expect(gotErr).To(MatchError("oops"))
		})
	})

	Describe("Limit (iter.Seq)", func() {
		It("limits to n elements", func() {
			seq := mustSeq(FromList([]int{1, 2, 3, 4, 5}), ctx)
			limited := Limit(seq, 3)
			var collected []int
			for v := range limited {
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]int{1, 2, 3}))
		})

		It("returns all if n >= length", func() {
			seq := mustSeq(FromList([]int{1, 2}), ctx)
			limited := Limit(seq, 10)
			var collected []int
			for v := range limited {
				collected = append(collected, v)
			}
			Expect(collected).To(Equal([]int{1, 2}))
		})

		It("returns empty for n=0", func() {
			seq := mustSeq(FromList([]int{1, 2, 3}), ctx)
			limited := Limit(seq, 0)
			var collected []int
			for v := range limited {
				collected = append(collected, v)
			}
			Expect(collected).To(BeEmpty())
		})
	})
})
