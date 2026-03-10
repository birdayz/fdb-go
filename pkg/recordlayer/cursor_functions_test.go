package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cursor standalone functions", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("First", func() {
		It("returns first element", func() {
			cursor := FromList([]int{10, 20, 30})
			val, err := First(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).NotTo(BeNil())
			Expect(*val).To(Equal(10))
		})

		It("returns nil for empty cursor", func() {
			cursor := Empty[int]()
			val, err := First(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(BeNil())
		})
	})

	Describe("GetCount", func() {
		It("counts elements", func() {
			cursor := FromList([]string{"a", "b", "c", "d"})
			count, err := GetCount(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(4))
		})

		It("returns 0 for empty cursor", func() {
			cursor := Empty[string]()
			count, err := GetCount(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(0))
		})
	})

	Describe("Reduce", func() {
		It("sums integers", func() {
			cursor := FromList([]int{1, 2, 3, 4, 5})
			sum, err := Reduce(ctx, cursor, 0, func(acc, val int) int { return acc + val })
			Expect(err).NotTo(HaveOccurred())
			Expect(sum).To(Equal(15))
		})

		It("returns initial for empty cursor", func() {
			cursor := Empty[int]()
			result, err := Reduce(ctx, cursor, 42, func(acc, val int) int { return acc + val })
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(42))
		})

		It("concatenates strings", func() {
			cursor := FromList([]string{"a", "b", "c"})
			result, err := Reduce(ctx, cursor, "", func(acc string, val string) string { return acc + val })
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("abc"))
		})
	})

	Describe("SkipCursor", func() {
		It("skips first n elements", func() {
			cursor := SkipCursor(FromList([]int{1, 2, 3, 4, 5}), 2)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{3, 4, 5}))
		})

		It("returns empty when skip >= length", func() {
			cursor := SkipCursor(FromList([]int{1, 2}), 5)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeEmpty())
		})

		It("skip 0 is identity", func() {
			inner := FromList([]int{1, 2, 3})
			cursor := SkipCursor(inner, 0)
			// skip 0 returns the inner cursor directly
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{1, 2, 3}))
		})

		It("works with Seq2", func() {
			cursor := SkipCursor(FromList([]int{10, 20, 30, 40}), 2)
			var items []int
			for v, err := range Seq2(cursor, ctx) {
				Expect(err).NotTo(HaveOccurred())
				items = append(items, v)
			}
			Expect(items).To(Equal([]int{30, 40}))
		})
	})

	Describe("LimitRowsCursor", func() {
		It("limits to n elements", func() {
			cursor := LimitRowsCursor(FromList([]int{1, 2, 3, 4, 5}), 3)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{1, 2, 3}))
		})

		It("returns all when limit > length", func() {
			cursor := LimitRowsCursor(FromList([]int{1, 2}), 10)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{1, 2}))
		})

		It("limit 0 returns empty", func() {
			cursor := LimitRowsCursor(FromList([]int{1, 2, 3}), 0)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeEmpty())
		})

		It("returns ReturnLimitReached when exhausted by limit", func() {
			cursor := LimitRowsCursor(FromList([]int{1, 2, 3}), 2)
			// Consume 2 elements
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeTrue())
			// Third call should return no next with limit reason
			r3, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r3.HasNext()).To(BeFalse())
			Expect(r3.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})

		It("works with Seq2", func() {
			cursor := LimitRowsCursor(FromList([]int{10, 20, 30, 40}), 2)
			var items []int
			for v, err := range Seq2(cursor, ctx) {
				Expect(err).NotTo(HaveOccurred())
				items = append(items, v)
			}
			Expect(items).To(Equal([]int{10, 20}))
		})
	})

	Describe("SkipCursor + LimitRowsCursor combined", func() {
		It("skip then limit (pagination)", func() {
			data := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
			// Page 2: skip 3, take 3
			cursor := LimitRowsCursor(SkipCursor(FromList(data), 3), 3)
			items, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{4, 5, 6}))
		})
	})
})
