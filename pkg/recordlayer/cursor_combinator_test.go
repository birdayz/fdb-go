package recordlayer

import (
	"context"
	"fmt"
	"slices"

	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// fakeOutOfBandCursor wraps a cursor and returns TimeLimitReached after limit
// items. Matches Java's FakeOutOfBandCursor used in RecordCursorTest.
func fakeOutOfBandCursor[T any](inner RecordCursor[T], limit int) RecordCursor[T] {
	return &outOfBandCursor[T]{inner: inner, limit: limit}
}

type outOfBandCursor[T any] struct {
	inner   RecordCursor[T]
	limit   int
	count   int
	lastCon RecordCursorContinuation
}

func (c *outOfBandCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.count >= c.limit {
		cont := c.lastCon
		if cont == nil {
			cont = &BytesContinuation{}
		}
		return NewResultNoNext[T](TimeLimitReached, cont), nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return result, err
	}
	if result.HasNext() {
		c.count++
		c.lastCon = result.GetContinuation()
	}
	return result, nil
}

func (c *outOfBandCursor[T]) Close() error   { return c.inner.Close() }
func (c *outOfBandCursor[T]) IsClosed() bool { return c.inner.IsClosed() }

// collectUntilStop drains a cursor until it stops (exhaustion or limit),
// returning items collected and the final continuation.
func collectUntilStop(ctx context.Context, cursor RecordCursor[int]) ([]int, RecordCursorContinuation) {
	var items []int
	var lastCont RecordCursorContinuation
	for {
		r, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		if !r.HasNext() {
			lastCont = r.GetContinuation()
			break
		}
		items = append(items, r.GetValue())
	}
	cursor.Close()
	return items, lastCont
}

// iterateGrid drives a FlatMap cursor through multiple continuation cycles
// until SOURCE_EXHAUSTED, counting results and verifying monotonic ordering.
// Port of Java's iterateGrid helper from RecordCursorTest.
func iterateGrid(cursorFunc func(cont []byte) RecordCursor[[2]int]) int {
	results := 0
	leftSoFar := -1
	rightSoFar := -1
	var continuation []byte
	for {
		cursor := cursorFunc(continuation)
		for {
			r, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			if !r.HasNext() {
				reason := r.GetNoNextReason()
				if reason.IsSourceExhausted() {
					cursor.Close()
					return results
				}
				contBytes, contErr := r.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				Expect(contBytes).NotTo(BeEmpty())
				continuation = contBytes
				break
			}
			val := r.GetValue()
			Expect(val[0]).To(BeNumerically(">", val[1]))
			Expect(val[0]).To(BeNumerically(">=", leftSoFar))
			if val[0] == leftSoFar {
				Expect(val[1]).To(BeNumerically(">", rightSoFar))
			} else {
				leftSoFar = val[0]
			}
			rightSoFar = val[1]
			results++
		}
		cursor.Close()
	}
}

// populate10Orders saves 10 orders (IDs 1-10, prices 10-100) into the given subspace.
func populate10Orders(ctx context.Context, metaData *RecordMetaData) {
	ks := specSubspace()
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := range int64(10) {
			order := &gen.Order{
				OrderId: proto.Int64(i + 1),
				Price:   proto.Int32(int32((i + 1) * 10)),
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("CursorCombinators", func() {
	var (
		metaData *RecordMetaData
		ctx      context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("FilterEliminatesAll", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			// Filter where price > 1000 -- no such records
			filtered := Filter(
				Seq(cursor, ctx),
				func(rec *FDBStoredRecord[proto.Message]) bool {
					return rec.Record.(*gen.Order).GetPrice() > 1000
				},
			)

			results := slices.Collect(filtered)
			Expect(results).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ChainedFilterMapLimit", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())

			// Filter: price > 30 (orders 4-10)
			// Map: extract order ID
			// Limit: take 3
			chain := Limit(
				Map(
					Filter(
						Seq(cursor, ctx),
						func(rec *FDBStoredRecord[proto.Message]) bool {
							return rec.Record.(*gen.Order).GetPrice() > 30
						},
					),
					func(rec *FDBStoredRecord[proto.Message]) int64 {
						return rec.Record.(*gen.Order).GetOrderId()
					},
				),
				3,
			)

			ids := slices.Collect(chain)
			Expect(ids).To(Equal([]int64{4, 5, 6}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitZero", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			limited := Limit(Seq(cursor, ctx), 0)
			results := slices.Collect(limited)
			Expect(results).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Filter2EmptyResult", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			cursor := store.ScanRecords(nil, ForwardScan())
			filtered := Filter2(
				Seq2(cursor, ctx),
				func(rec *FDBStoredRecord[proto.Message]) bool {
					return false // eliminate everything
				},
			)

			count := 0
			for _, err := range filtered {
				Expect(err).NotTo(HaveOccurred())
				count++
			}
			Expect(count).To(Equal(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("EmptyCursor", func() {
		cursor := Empty[int]()
		result, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(result.GetContinuation().IsEnd()).To(BeTrue())

		// Seq should produce nothing
		count := 0
		for range Seq(cursor, ctx) {
			count++
		}
		Expect(count).To(Equal(0))
	})

	It("ListCursor", func() {
		items := []string{"alice", "bob", "charlie"}
		cursor := FromList(items)

		collected, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(collected).To(Equal([]string{"alice", "bob", "charlie"}))
	})

	It("ListCursor empty", func() {
		cursor := FromList([]int{})
		result, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
	})

	It("HasStoppedBeforeEnd", func() {
		// Source exhausted → has NOT stopped before end
		r1 := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
		Expect(r1.HasStoppedBeforeEnd()).To(BeFalse())

		// Return limit reached → HAS stopped before end
		r2 := NewResultNoNext[int](ReturnLimitReached, &BytesContinuation{bytes: []byte{1}})
		Expect(r2.HasStoppedBeforeEnd()).To(BeTrue())

		// Byte limit reached → HAS stopped before end
		r3 := NewResultNoNext[int](ByteLimitReached, &BytesContinuation{bytes: []byte{1}})
		Expect(r3.HasStoppedBeforeEnd()).To(BeTrue())

		// Result with value → HasStoppedBeforeEnd is false (it has a value)
		r4 := NewResultWithValue(42, &StartContinuation{})
		Expect(r4.HasStoppedBeforeEnd()).To(BeFalse())
	})

	It("ForEachAndAsList", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Test AsList
			cursor := store.ScanRecords(nil, ForwardScan())
			records, err := AsList(ctx, cursor)
			if err != nil {
				return nil, err
			}
			Expect(records).To(HaveLen(10))

			// Test ForEach
			cursor2 := store.ScanRecords(nil, ForwardScan())
			var sum int32
			err = ForEach(ctx, cursor2, func(rec *FDBStoredRecord[proto.Message]) error {
				sum += rec.Record.(*gen.Order).GetPrice()
				return nil
			})
			if err != nil {
				return nil, err
			}
			// Sum of 10+20+...+100 = 550
			Expect(sum).To(Equal(int32(550)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Skip records", func() {
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range int64(10) {
				order := &gen.Order{OrderId: proto.Int64(i + 1), Price: proto.Int32(int32((i + 1) * 10))}
				if _, saveErr := store.SaveRecord(order); saveErr != nil {
					return nil, saveErr
				}
			}

			// Skip first 3 records, return remaining 7
			scan := ForwardScan()
			scan.ExecuteProperties.Skip = 3
			records, scanErr := AsList(ctx, store.ScanRecords(nil, scan))
			if scanErr != nil {
				return nil, scanErr
			}
			Expect(records).To(HaveLen(7))
			// First returned record should be order_id=4
			Expect(records[0].Record.(*gen.Order).GetOrderId()).To(Equal(int64(4)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Skip with row limit", func() {
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range int64(10) {
				order := &gen.Order{OrderId: proto.Int64(i + 1), Price: proto.Int32(int32((i + 1) * 10))}
				if _, saveErr := store.SaveRecord(order); saveErr != nil {
					return nil, saveErr
				}
			}

			// Skip 3, then return at most 2
			scan := ForwardScan()
			scan.ExecuteProperties.Skip = 3
			scan.ExecuteProperties.ReturnedRowLimit = 2
			records, scanErr := AsList(ctx, store.ScanRecords(nil, scan))
			if scanErr != nil {
				return nil, scanErr
			}
			Expect(records).To(HaveLen(2))
			Expect(records[0].Record.(*gen.Order).GetOrderId()).To(Equal(int64(4)))
			Expect(records[1].Record.(*gen.Order).GetOrderId()).To(Equal(int64(5)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ConcatCursors basic", func() {
		first := FromList([]int{1, 2, 3})
		second := FromList([]int{4, 5, 6})

		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return first },
			func(_ []byte) RecordCursor[int] { return second },
			nil,
		)
		result, err := AsList(ctx, concat)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{1, 2, 3, 4, 5, 6}))
	})

	It("ConcatCursors first empty", func() {
		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return FromList([]int{}) },
			func(_ []byte) RecordCursor[int] { return FromList([]int{7, 8}) },
			nil,
		)
		result, err := AsList(ctx, concat)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{7, 8}))
	})

	It("ConcatCursors second empty", func() {
		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
			func(_ []byte) RecordCursor[int] { return FromList([]int{}) },
			nil,
		)
		result, err := AsList(ctx, concat)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{1, 2}))
	})

	It("ConcatCursors both empty", func() {
		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return Empty[int]() },
			func(_ []byte) RecordCursor[int] { return Empty[int]() },
			nil,
		)
		result, err := AsList(ctx, concat)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("ConcatCursors continuation wraps inner", func() {
		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
			func(_ []byte) RecordCursor[int] { return FromList([]int{3, 4}) },
			nil,
		)

		// Read one record and check continuation is non-nil
		r, err := concat.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(1))
		Expect(r.GetContinuation()).NotTo(BeNil())
		Expect(r.GetContinuation().IsEnd()).To(BeFalse())
		Expect(concat.Close()).To(Succeed())
	})

	It("ConcatCursors exhaustion returns SourceExhausted", func() {
		concat := ConcatCursors(
			func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
			func(_ []byte) RecordCursor[int] { return FromList([]int{2}) },
			nil,
		)
		// Drain
		r1, _ := concat.OnNext(ctx)
		Expect(r1.HasNext()).To(BeTrue())
		r2, _ := concat.OnNext(ctx)
		Expect(r2.HasNext()).To(BeTrue())
		r3, _ := concat.OnNext(ctx)
		Expect(r3.HasNext()).To(BeFalse())
		Expect(r3.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(concat.Close()).To(Succeed())
	})

	It("MapCursor transforms values", func() {
		inner := FromList([]int{1, 2, 3})
		mapped := MapCursor(inner, func(n int) string {
			return fmt.Sprintf("item_%d", n)
		})
		result, err := AsList(ctx, mapped)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]string{"item_1", "item_2", "item_3"}))
	})

	It("MapCursor empty", func() {
		mapped := MapCursor(Empty[int](), func(n int) string { return "" })
		result, err := AsList(ctx, mapped)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("MapCursor preserves continuation", func() {
		inner := FromList([]int{10, 20, 30})
		mapped := MapCursor(inner, func(n int) int { return n * 2 })

		r, err := mapped.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(20))
		Expect(r.GetContinuation()).NotTo(BeNil())
		Expect(mapped.Close()).To(Succeed())
	})

	It("MapCursor exhaustion", func() {
		inner := FromList([]int{1})
		mapped := MapCursor(inner, func(n int) int { return n + 100 })

		r1, _ := mapped.OnNext(ctx)
		Expect(r1.HasNext()).To(BeTrue())
		Expect(r1.GetValue()).To(Equal(101))

		r2, _ := mapped.OnNext(ctx)
		Expect(r2.HasNext()).To(BeFalse())
		Expect(r2.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(mapped.Close()).To(Succeed())
	})

	It("MapErrCursor transforms values", func() {
		inner := FromList([]int{1, 2, 3})
		mapped := MapErrCursor(inner, func(n int) (string, error) {
			return fmt.Sprintf("item_%d", n), nil
		})
		result, err := AsList(ctx, mapped)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]string{"item_1", "item_2", "item_3"}))
	})

	It("MapErrCursor propagates transform error", func() {
		inner := FromList([]int{1, 2, 3})
		mapped := MapErrCursor(inner, func(n int) (string, error) {
			if n == 2 {
				return "", fmt.Errorf("transform error at %d", n)
			}
			return fmt.Sprintf("item_%d", n), nil
		})
		// First item succeeds
		r1, err := mapped.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r1.HasNext()).To(BeTrue())
		Expect(r1.GetValue()).To(Equal("item_1"))

		// Second item fails
		_, err = mapped.OnNext(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transform error at 2"))
		Expect(mapped.Close()).To(Succeed())
	})

	It("MapErrCursor empty", func() {
		mapped := MapErrCursor(Empty[int](), func(n int) (string, error) { return "", nil })
		result, err := AsList(ctx, mapped)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("AsListWithContinuation collects all from exhausted cursor", func() {
		inner := FromList([]int{10, 20, 30})
		items, cont, err := AsListWithContinuation(ctx, inner)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(Equal([]int{10, 20, 30}))
		Expect(cont).To(BeNil()) // source exhausted = no continuation
	})

	It("AsListWithContinuation returns continuation from limited cursor", func() {
		inner := LimitRowsCursor(FromList([]int{10, 20, 30, 40, 50}), 3)
		items, cont, err := AsListWithContinuation(ctx, inner)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(Equal([]int{10, 20, 30}))
		Expect(cont).NotTo(BeNil()) // limit reached = has continuation
	})

	It("AsListWithContinuation empty cursor", func() {
		items, cont, err := AsListWithContinuation(ctx, Empty[int]())
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(BeEmpty())
		Expect(cont).To(BeNil())
	})

	It("FlatMapPipelined basic", func() {
		// Outer: [1, 2, 3], Inner: for each outer x → [x*10, x*10+1]
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[int] { return FromList([]int{1, 2, 3}) },
			func(outer int, cont []byte) RecordCursor[int] {
				return FromList([]int{outer * 10, outer*10 + 1})
			},
			nil, 1,
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{10, 11, 20, 21, 30, 31}))
	})

	It("FlatMapPipelined outer empty", func() {
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[int] { return Empty[int]() },
			func(outer int, cont []byte) RecordCursor[int] {
				return FromList([]int{outer * 10})
			},
			nil, 1,
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("FlatMapPipelined inner empty", func() {
		// Some outer values produce empty inner cursors
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[int] { return FromList([]int{1, 2, 3}) },
			func(outer int, cont []byte) RecordCursor[int] {
				if outer == 2 {
					return Empty[int]() // skip middle
				}
				return FromList([]int{outer * 10})
			},
			nil, 1,
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{10, 30}))
	})

	It("FlatMapPipelined all inner empty", func() {
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[int] { return FromList([]int{1, 2, 3}) },
			func(outer int, cont []byte) RecordCursor[int] {
				return Empty[int]()
			},
			nil, 1,
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("FlatMapPipelined exhaustion returns SourceExhausted", func() {
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[int] { return FromList([]int{1}) },
			func(outer int, cont []byte) RecordCursor[int] {
				return FromList([]int{10})
			},
			nil, 1,
		)
		r1, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r1.HasNext()).To(BeTrue())
		Expect(r1.GetValue()).To(Equal(10))

		r2, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r2.HasNext()).To(BeFalse())
		Expect(r2.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("FlatMapPipelined continuation preserves position", func() {
		makeOuter := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation([]int{1, 2, 3}, cont)
		}
		makeInner := func(outer int, cont []byte) RecordCursor[int] {
			return FromListWithContinuation([]int{outer * 10, outer*10 + 1}, cont)
		}

		// Read 3 items: 10, 11, 20
		cursor := LimitRowsCursor(FlatMapPipelined(makeOuter, makeInner, nil, 1), 3)
		var results []int
		var lastCont RecordCursorContinuation
		for {
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !r.HasNext() {
				break
			}
			results = append(results, r.GetValue())
			lastCont = r.GetContinuation()
		}
		Expect(results).To(Equal([]int{10, 11, 20}))
		Expect(lastCont).NotTo(BeNil())
		Expect(cursor.Close()).To(Succeed())

		// Continue from continuation — should get 21, 30, 31
		contBytes, err := lastCont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())

		cursor2 := FlatMapPipelined(makeOuter, makeInner, contBytes, 1)
		results2, err := AsList(ctx, cursor2)
		Expect(err).NotTo(HaveOccurred())
		Expect(results2).To(Equal([]int{21, 30, 31}))
	})

	It("FlatMapPipelined with check value", func() {
		makeOuter := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation([]int{1, 2}, cont)
		}
		makeInner := func(outer int, cont []byte) RecordCursor[int] {
			return FromListWithContinuation([]int{outer * 10, outer*10 + 1}, cont)
		}
		checkFunc := func(outer int) []byte {
			return []byte(fmt.Sprintf("id:%d", outer))
		}

		// Read 1 item, get continuation with check value
		cursor := LimitRowsCursor(
			FlatMapPipelinedWithCheck(makeOuter, makeInner, checkFunc, nil, 1),
			1,
		)
		r, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(10))

		contBytes, err := r.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(cursor.Close()).To(Succeed())

		// Resume — check value should match, inner cursor picks up
		cursor2 := FlatMapPipelinedWithCheck(makeOuter, makeInner, checkFunc, contBytes, 1)
		results, err := AsList(ctx, cursor2)
		Expect(err).NotTo(HaveOccurred())
		// Should continue from where we left off: 11, 20, 21
		Expect(results).To(Equal([]int{11, 20, 21}))
	})

	It("FlatMapPipelined with type transformation", func() {
		// Outer: strings, Inner: ints (different types)
		cursor := FlatMapPipelined(
			func(cont []byte) RecordCursor[string] {
				return FromList([]string{"a", "bb", "ccc"})
			},
			func(outer string, cont []byte) RecordCursor[int] {
				return FromList([]int{len(outer)})
			},
			nil, 1,
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{1, 2, 3}))
	})

	// --- FlatMapPipelined TIME_LIMIT tests (Java RecordCursorTest alignment) ---

	It("FlatMapPipelined testFlatMapReasons (5x5 grid, TIME_LIMIT every 3)", func() {
		// Port of Java's testFlatMapReasons: 5×5 grid where the inner
		// cursor fires TIME_LIMIT_REACHED every 3 items. Verifies 6
		// continuation cycles produce the exact same item sequence.
		list := []int{1, 2, 3, 4, 5}
		outer := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(list, cont)
		}
		timedInner := func(outerVal int, cont []byte) RecordCursor[int] {
			inner := make([]int, len(list))
			for j, v := range list {
				inner[j] = outerVal*10 + v
			}
			return fakeOutOfBandCursor(FromListWithContinuation(inner, cont), 3)
		}

		// Cycle 1: 11, 12, 13 → TIME_LIMIT
		cursor := FlatMapPipelined(outer, timedInner, nil, 10)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{11, 12, 13}))
		Expect(cont).NotTo(BeNil())

		// Cycle 2: 14, 15, 21, 22, 23 → TIME_LIMIT
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = FlatMapPipelined(outer, timedInner, contBytes, 10)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{14, 15, 21, 22, 23}))

		// Cycle 3: 24, 25, 31, 32, 33 → TIME_LIMIT
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = FlatMapPipelined(outer, timedInner, contBytes, 10)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{24, 25, 31, 32, 33}))

		// Cycle 4: 34, 35, 41, 42, 43 → TIME_LIMIT
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = FlatMapPipelined(outer, timedInner, contBytes, 10)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{34, 35, 41, 42, 43}))

		// Cycle 5: 44, 45, 51, 52, 53 → TIME_LIMIT
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = FlatMapPipelined(outer, timedInner, contBytes, 10)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{44, 45, 51, 52, 53}))

		// Cycle 6: 54, 55 → SOURCE_EXHAUSTED
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = FlatMapPipelined(outer, timedInner, contBytes, 10)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{54, 55}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("FlatMapPipelined pipelineWithInnerLimits (out-of-band)", func() {
		// Port of Java's pipelineWithInnerLimits: inner cursor hits
		// TIME_LIMIT every 3 items, filter y < x. Verifies full N*(N-1)/2
		// product across continuation cycles.
		ints := make([]int, 10)
		for i := range ints {
			ints[i] = i
		}
		outerFunc := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(ints, cont)
		}
		innerFunc := func(x int, cont []byte) RecordCursor[[2]int] {
			limited := fakeOutOfBandCursor(FromListWithContinuation(ints, cont), 3)
			mapped := MapCursor(limited, func(y int) [2]int { return [2]int{x, y} })
			return &filterCursor[[2]int]{inner: mapped, predicate: func(pair [2]int) bool { return pair[1] < pair[0] }}
		}

		results := iterateGrid(func(cont []byte) RecordCursor[[2]int] {
			return FlatMapPipelined(outerFunc, innerFunc, cont, 5)
		})
		expectedResults := len(ints) * (len(ints) - 1) / 2
		Expect(results).To(Equal(expectedResults))
	})

	It("FlatMapPipelined pipelineWithOuterLimits (out-of-band)", func() {
		// Port of Java's pipelineWithOuterLimits: outer cursor hits
		// TIME_LIMIT every 3 items, filtered to x in [7,9). Inner also
		// limited to 3 items, filtered y < x.
		ints := make([]int, 10)
		for i := range ints {
			ints[i] = i
		}
		outerFunc := func(cont []byte) RecordCursor[int] {
			limited := fakeOutOfBandCursor(FromListWithContinuation(ints, cont), 3)
			return &filterCursor[int]{inner: limited, predicate: func(x int) bool { return x >= 7 && x < 9 }}
		}
		innerFunc := func(x int, cont []byte) RecordCursor[[2]int] {
			limited := fakeOutOfBandCursor(FromListWithContinuation(ints, cont), 3)
			mapped := MapCursor(limited, func(y int) [2]int { return [2]int{x, y} })
			return &filterCursor[[2]int]{inner: mapped, predicate: func(pair [2]int) bool { return pair[1] < pair[0] }}
		}

		results := iterateGrid(func(cont []byte) RecordCursor[[2]int] {
			return FlatMapPipelined(outerFunc, innerFunc, cont, 5)
		})
		// outer = {7, 8}, inner y < x: 7→{0..6}=7, 8→{0..7}=8 → total 15
		Expect(results).To(Equal(15))
	})

	It("FlatMapPipelined pipelineWithInnerLimits (row-limit)", func() {
		// Port of Java's pipelineWithInnerLimits with outOfBand=false:
		// inner cursor uses LimitRowsCursor (RETURN_LIMIT) every 3 items,
		// filter y < x. Verifies full N*(N-1)/2 product across continuation cycles.
		ints := make([]int, 10)
		for i := range ints {
			ints[i] = i
		}
		outerFunc := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(ints, cont)
		}
		innerFunc := func(x int, cont []byte) RecordCursor[[2]int] {
			limited := LimitRowsCursor(FromListWithContinuation(ints, cont), 3)
			mapped := MapCursor(limited, func(y int) [2]int { return [2]int{x, y} })
			return &filterCursor[[2]int]{inner: mapped, predicate: func(pair [2]int) bool { return pair[1] < pair[0] }}
		}

		results := iterateGrid(func(cont []byte) RecordCursor[[2]int] {
			return FlatMapPipelined(outerFunc, innerFunc, cont, 5)
		})
		expectedResults := len(ints) * (len(ints) - 1) / 2
		Expect(results).To(Equal(expectedResults))
	})

	It("FlatMapPipelined pipelineWithOuterLimits (row-limit)", func() {
		// Port of Java's pipelineWithOuterLimits with outOfBand=false:
		// outer cursor uses LimitRowsCursor (RETURN_LIMIT) every 3 items,
		// filtered to x in [7,9). Inner also limited to 3 items via
		// LimitRowsCursor, filtered y < x.
		ints := make([]int, 10)
		for i := range ints {
			ints[i] = i
		}
		outerFunc := func(cont []byte) RecordCursor[int] {
			limited := LimitRowsCursor(FromListWithContinuation(ints, cont), 3)
			return &filterCursor[int]{inner: limited, predicate: func(x int) bool { return x >= 7 && x < 9 }}
		}
		innerFunc := func(x int, cont []byte) RecordCursor[[2]int] {
			limited := LimitRowsCursor(FromListWithContinuation(ints, cont), 3)
			mapped := MapCursor(limited, func(y int) [2]int { return [2]int{x, y} })
			return &filterCursor[[2]int]{inner: mapped, predicate: func(pair [2]int) bool { return pair[1] < pair[0] }}
		}

		results := iterateGrid(func(cont []byte) RecordCursor[[2]int] {
			return FlatMapPipelined(outerFunc, innerFunc, cont, 5)
		})
		// outer = {7, 8}, inner y < x: 7→{0..6}=7, 8→{0..7}=8 → total 15
		Expect(results).To(Equal(15))
	})

	It("OrElse testOrElseReasons", func() {
		// Port of Java's testOrElseReasons: primary has 5 items, all filtered
		// out, with fakeOutOfBandCursor(limit=3). First cycle scans 3 items,
		// all filtered → TIME_LIMIT. Resume: scans remaining 2, all filtered →
		// SOURCE_EXHAUSTED → switches to alternative → emits [0].
		list := []int{1, 2, 3, 4, 5}

		primaryFactory := func(cont []byte) RecordCursor[int] {
			raw := FromListWithContinuation(list, cont)
			limited := fakeOutOfBandCursor(raw, 3)
			return &filterCursor[int]{inner: limited, predicate: func(i int) bool { return false }}
		}
		alternative := func() RecordCursor[int] {
			return FromList([]int{0})
		}

		// Cycle 1: primary scans [1,2,3], all filtered → TIME_LIMIT, no values
		cursor := OrElseWithContinuation(primaryFactory, alternative, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(BeEmpty())
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume, primary scans [4,5], all filtered → SOURCE_EXHAUSTED
		// → switches to alternative → emits [0] → SOURCE_EXHAUSTED
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())
		cursor = OrElseWithContinuation(primaryFactory, alternative, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{0}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("OrElse orElseWithEventuallyNonEmptyInner", func() {
		// Port of Java's orElseWithEventuallyNonEmptyInner: primary has 5
		// items filtered by i > 4 (only 5 passes), with fakeOutOfBandCursor
		// (limit=3). First cycle: scans [1,2,3], all filtered → TIME_LIMIT.
		// Resume: scans [4,5], 5 passes → USE_INNER, emits [5] → SOURCE_EXHAUSTED.
		list := []int{1, 2, 3, 4, 5}

		primaryFactory := func(cont []byte) RecordCursor[int] {
			raw := FromListWithContinuation(list, cont)
			limited := fakeOutOfBandCursor(raw, 3)
			return &filterCursor[int]{inner: limited, predicate: func(i int) bool { return i > 4 }}
		}
		alternative := func() RecordCursor[int] {
			return FromList([]int{0})
		}

		// Cycle 1: primary scans [1,2,3], all filtered → TIME_LIMIT, no values
		cursor := OrElseWithContinuation(primaryFactory, alternative, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(BeEmpty())
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume, primary scans [4,5], 5 passes filter → USE_INNER
		// emits [5] → SOURCE_EXHAUSTED
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())
		cursor = OrElseWithContinuation(primaryFactory, alternative, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{5}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("OrElse orElseContinueWithInnerBranchAfterDecision", func() {
		// Port of Java's orElseContinueWithInnerBranchAfterDecision: primary
		// has 18 items filtered by i > 10, fakeOutOfBandCursor(limit=3).
		// Cycles 1-3 produce no values (scanning [1-9]). Cycle 4 finds items
		// 11,12 then TIME_LIMIT. Cycles 5-6 drain [13-18].
		list := make([]int, 18)
		for i := range list {
			list[i] = i + 1
		}

		primaryFactory := func(cont []byte) RecordCursor[int] {
			raw := FromListWithContinuation(list, cont)
			limited := fakeOutOfBandCursor(raw, 3)
			return &filterCursor[int]{inner: limited, predicate: func(i int) bool { return i > 10 }}
		}
		alternative := func() RecordCursor[int] {
			return FromList([]int{0})
		}

		var allItems []int
		var contBytes []byte

		// Cycles 1-3: primary scans [1-3], [4-6], [7-9] — all filtered → TIME_LIMIT
		for cycle := range 3 {
			cursor := OrElseWithContinuation(primaryFactory, alternative, contBytes)
			items, cont := collectUntilStop(ctx, cursor)
			Expect(items).To(BeEmpty(), "cycle %d should emit nothing", cycle+1)
			Expect(cont).NotTo(BeNil())
			Expect(cont.IsEnd()).To(BeFalse())
			var err error
			contBytes, err = cont.ToBytes()
			Expect(err).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeEmpty())
		}

		// Cycle 4: scans [10,11,12], 11 and 12 pass → USE_INNER, emits [11,12] → TIME_LIMIT
		cursor := OrElseWithContinuation(primaryFactory, alternative, contBytes)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{11, 12}))
		allItems = append(allItems, items...)
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())

		// Continue resuming until SOURCE_EXHAUSTED
		for {
			cursor = OrElseWithContinuation(primaryFactory, alternative, contBytes)
			items, cont = collectUntilStop(ctx, cursor)
			allItems = append(allItems, items...)
			if cont.IsEnd() {
				break
			}
			contBytes, err = cont.ToBytes()
			Expect(err).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeEmpty())
		}

		Expect(allItems).To(Equal([]int{11, 12, 13, 14, 15, 16, 17, 18}))
	})

	It("OrElse orElseContinueWithElseBranchAfterDecision", func() {
		// Port of Java's orElseContinueWithElseBranchAfterDecision: primary
		// has 5 items, all filtered out, with fakeOutOfBandCursor(limit=3).
		// Alternative is [-1,-2,-3,-4,-5] (no time limit).
		// Cycle 1: primary scans [1,2,3], all filtered → TIME_LIMIT.
		// Cycle 2: primary scans [4,5], all filtered → SOURCE_EXHAUSTED →
		// switches to alternative → emits [-1,-2,-3,-4,-5] → SOURCE_EXHAUSTED.
		list := []int{1, 2, 3, 4, 5}

		primaryFactory := func(cont []byte) RecordCursor[int] {
			raw := FromListWithContinuation(list, cont)
			limited := fakeOutOfBandCursor(raw, 3)
			return &filterCursor[int]{inner: limited, predicate: func(i int) bool { return false }}
		}
		alternative := func() RecordCursor[int] {
			return FromList([]int{-1, -2, -3, -4, -5})
		}

		// Cycle 1: primary scans [1,2,3], all filtered → TIME_LIMIT
		cursor := OrElseWithContinuation(primaryFactory, alternative, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(BeEmpty())
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume, primary scans [4,5], all filtered → SOURCE_EXHAUSTED
		// → switches to alternative → emits all 5 items → SOURCE_EXHAUSTED
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())
		cursor = OrElseWithContinuation(primaryFactory, alternative, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{-1, -2, -3, -4, -5}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("asListWithContinuation iterates in chunks", func() {
		// Port of Java's asListWithContinuationTest: creates [0..49], iterates
		// in chunks of 10, verifying each chunk returns exactly the right items
		// and the continuation correctly resumes. 5 chunks with data + 1 empty
		// = 6 iterations total.
		ints := make([]int, 50)
		for i := range ints {
			ints[i] = i
		}

		iterations := 0
		var continuation []byte
		for {
			iterations++
			items, cont, err := AsListWithContinuation(ctx,
				LimitRowsCursor(FromListWithContinuation(ints, continuation), 10))
			Expect(err).NotTo(HaveOccurred())

			if len(items) > 0 {
				Expect(items).To(HaveLen(10))
				Expect(items[0]).To(Equal((iterations - 1) * 10))
				// Should have a continuation (limit reached)
				Expect(cont).NotTo(BeNil())
			}
			continuation = cont
			if continuation == nil {
				break
			}
		}

		// 5 chunks with 10 items each + 1 final empty iteration = 6
		Expect(iterations).To(Equal(6))
	})

	It("mapPipelinedContinuationWithTimeLimit", func() {
		// Port of Java's mapPipelinedContinuationWithTimeLimit: MapCursor over
		// a fakeOutOfBandCursor(limit=3). The inner cursor delivers 3 items
		// then TIME_LIMIT. MapCursor should pass through the mapped values and
		// the TIME_LIMIT stop reason with the correct continuation.
		inner := fakeOutOfBandCursor(FromList([]int{0, 1, 2, 3}), 3)
		cursor := MapCursor(inner, func(i int) int { return (i + 1) * 1000 })

		// Should get 3 mapped items: 1000, 2000, 3000
		r, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(1000))

		r, err = cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(2000))

		r, err = cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(3000))
		lastCont := r.GetContinuation()

		// Next call should hit TIME_LIMIT with the same continuation as the
		// last returned result.
		r, err = cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeFalse())
		Expect(r.GetNoNextReason()).To(Equal(TimeLimitReached))

		// Continuation from TIME_LIMIT stop should let us resume correctly.
		contBytes, err := lastCont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor2 := FromListWithContinuation([]int{0, 1, 2, 3}, contBytes)
		resumed, err := cursor2.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(resumed.HasNext()).To(BeTrue())
		Expect(resumed.GetValue()).To(Equal(3))
		Expect(cursor.Close()).To(Succeed())
	})

	It("mapPipelinedContinuationWithTimeLimitWithMoreToReturn", func() {
		// Port of Java's variant: inner delivers 4 items then TIME_LIMIT.
		// MapCursor returns all 4 mapped items, then TIME_LIMIT. Resume
		// from last continuation should start at item index 4.
		inner := fakeOutOfBandCursor(FromList([]int{0, 1, 2, 3, 4}), 4)
		cursor := MapCursor(inner, func(i int) int { return (i + 1) * 1000 })

		// Collect the 4 items
		var lastCont RecordCursorContinuation
		for i := range 4 {
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeTrue())
			Expect(r.GetValue()).To(Equal((i + 1) * 1000))
			lastCont = r.GetContinuation()
		}

		// TIME_LIMIT after 4 items
		r, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeFalse())
		Expect(r.GetNoNextReason()).To(Equal(TimeLimitReached))

		// Resume from continuation of last returned item
		contBytes, err := lastCont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor2 := FromListWithContinuation([]int{0, 1, 2, 3, 4}, contBytes)
		resumed, err := cursor2.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(resumed.HasNext()).To(BeTrue())
		Expect(resumed.GetValue()).To(Equal(4))
		Expect(cursor.Close()).To(Succeed())
	})

	It("mapPipelinedContinuationWithTimeLimitBeforeFirstEntry", func() {
		// Port of Java's variant: inner delivers 2 items then TIME_LIMIT.
		// MapCursor returns both mapped items, then TIME_LIMIT. Resume from
		// last continuation should start at item index 2.
		inner := fakeOutOfBandCursor(FromList([]int{0, 1, 2}), 2)
		cursor := MapCursor(inner, func(i int) int { return (i + 1) * 1000 })

		// Get 2 items
		r, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(1000))

		r, err = cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeTrue())
		Expect(r.GetValue()).To(Equal(2000))
		lastCont := r.GetContinuation()

		// TIME_LIMIT
		r, err = cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasNext()).To(BeFalse())
		Expect(r.GetNoNextReason()).To(Equal(TimeLimitReached))

		// Resume
		contBytes, err := lastCont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor2 := FromListWithContinuation([]int{0, 1, 2}, contBytes)
		resumed, err := cursor2.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(resumed.HasNext()).To(BeTrue())
		Expect(resumed.GetValue()).To(Equal(2))
		Expect(cursor.Close()).To(Succeed())
	})

	It("ConcatCursors with TIME_LIMIT on first cursor", func() {
		// First cursor: fakeOutOfBandCursor([1,2,3,4,5], limit=3).
		// Second cursor: [6,7,8].
		// Cycle 1: [1,2,3] + TIME_LIMIT.
		// Cycle 2: resume → [4,5,6,7,8] + SOURCE_EXHAUSTED.
		items1 := []int{1, 2, 3, 4, 5}
		items2 := []int{6, 7, 8}
		makeFirst := func(cont []byte) RecordCursor[int] {
			return fakeOutOfBandCursor(FromListWithContinuation(items1, cont), 3)
		}
		makeSecond := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(items2, cont)
		}

		// Cycle 1: first cursor yields [1,2,3] then TIME_LIMIT
		cursor := ConcatCursors(makeFirst, makeSecond, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{1, 2, 3}))
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume from continuation → first yields [4,5], exhausts,
		// switches to second → [6,7,8] → SOURCE_EXHAUSTED
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())
		cursor = ConcatCursors(makeFirst, makeSecond, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{4, 5, 6, 7, 8}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("ConcatCursors with TIME_LIMIT on second cursor", func() {
		// First cursor: [1,2]. Second cursor: fakeOutOfBandCursor([3,4,5,6], limit=2).
		// Cycle 1: first exhausts [1,2], switches to second which yields [3,4] → TIME_LIMIT.
		// Cycle 2: resume second, yields [5,6] → TIME_LIMIT (oob limit resets each cycle).
		// Cycle 3: resume second, underlying exhausted → SOURCE_EXHAUSTED.
		items1 := []int{1, 2}
		items2 := []int{3, 4, 5, 6}
		makeFirst := func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(items1, cont)
		}
		makeSecond := func(cont []byte) RecordCursor[int] {
			return fakeOutOfBandCursor(FromListWithContinuation(items2, cont), 2)
		}

		// Cycle 1: first [1,2] → SOURCE_EXHAUSTED → switch to second [3,4] → TIME_LIMIT
		cursor := ConcatCursors(makeFirst, makeSecond, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{1, 2, 3, 4}))
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume second → [5,6] → TIME_LIMIT (oob counter resets)
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(contBytes).NotTo(BeEmpty())
		cursor = ConcatCursors(makeFirst, makeSecond, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{5, 6}))
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 3: resume second → underlying exhausted → SOURCE_EXHAUSTED
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = ConcatCursors(makeFirst, makeSecond, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(BeEmpty())
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("ConcatCursors with TIME_LIMIT on both cursors", func() {
		// First: fakeOutOfBandCursor([1,2,3], limit=2).
		// Second: fakeOutOfBandCursor([4,5,6], limit=2).
		// Cycle 1: first yields [1,2] → TIME_LIMIT.
		// Cycle 2: resume first → [3], exhausts, switches to second → [4,5] → TIME_LIMIT.
		// Cycle 3: resume second → [6] → SOURCE_EXHAUSTED.
		items1 := []int{1, 2, 3}
		items2 := []int{4, 5, 6}
		makeFirst := func(cont []byte) RecordCursor[int] {
			return fakeOutOfBandCursor(FromListWithContinuation(items1, cont), 2)
		}
		makeSecond := func(cont []byte) RecordCursor[int] {
			return fakeOutOfBandCursor(FromListWithContinuation(items2, cont), 2)
		}

		// Cycle 1: first yields [1,2] → TIME_LIMIT
		cursor := ConcatCursors(makeFirst, makeSecond, nil)
		items, cont := collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{1, 2}))
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 2: resume first → [3], exhausts → switch to second → [4,5] → TIME_LIMIT
		contBytes, err := cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = ConcatCursors(makeFirst, makeSecond, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{3, 4, 5}))
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())

		// Cycle 3: resume second → [6] → SOURCE_EXHAUSTED
		contBytes, err = cont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor = ConcatCursors(makeFirst, makeSecond, contBytes)
		items, cont = collectUntilStop(ctx, cursor)
		Expect(items).To(Equal([]int{6}))
		Expect(cont.IsEnd()).To(BeTrue())
	})

	It("AutoContinuingCursor scans across transaction boundaries", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		runner := NewFDBDatabaseRunner(sharedDB)

		// Use a scan limit of 3 to force multiple transactions
		autoCursor := NewAutoContinuingCursor(
			runner,
			func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				scan := ForwardScan()
				scan.ExecuteProperties.ScannedRecordsLimit = 3
				return store.ScanRecords(continuation, scan)
			},
			0,
		)

		// Should get all 10 records despite 3-per-transaction limit
		records, err := AsList(ctx, autoCursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(10))

		// Verify order
		for i, rec := range records {
			order := rec.Record.(*gen.Order)
			Expect(order.GetOrderId()).To(Equal(int64(i + 1)))
		}
	})

	It("AutoContinuingCursor with empty store", func() {
		ks := specSubspace()
		// Don't populate — empty store
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		runner := NewFDBDatabaseRunner(sharedDB)
		autoCursor := NewAutoContinuingCursor(
			runner,
			func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				return store.ScanRecords(continuation, ForwardScan())
			},
			0,
		)

		records, err := AsList(ctx, autoCursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("AutoContinuingCursor with row limit per transaction", func() {
		ks := specSubspace()
		populate10Orders(ctx, metaData)

		runner := NewFDBDatabaseRunner(sharedDB)

		// ReturnedRowLimit of 2 per inner cursor
		autoCursor := NewAutoContinuingCursor(
			runner,
			func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				scan := ForwardScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2
				return store.ScanRecords(continuation, scan)
			},
			0,
		)

		records, err := AsList(ctx, autoCursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(10))
	})

	It("FromListWithContinuation resumes at position", func() {
		items := []int{10, 20, 30, 40, 50}

		// Read first 2 items, get continuation
		cursor := LimitRowsCursor(FromList(items), 2)
		var lastCont RecordCursorContinuation
		for {
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !r.HasNext() {
				break
			}
			lastCont = r.GetContinuation()
		}
		Expect(cursor.Close()).To(Succeed())

		// Resume from continuation — should get 30, 40, 50
		contBytes, err := lastCont.ToBytes()
		Expect(err).NotTo(HaveOccurred())
		cursor2 := FromListWithContinuation(items, contBytes)
		result, err := AsList(ctx, cursor2)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{30, 40, 50}))
	})

	It("FromListWithContinuation nil starts from beginning", func() {
		items := []int{1, 2, 3}
		cursor := FromListWithContinuation(items, nil)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{1, 2, 3}))
	})

	It("SkipThenLimit", func() {
		items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		cursor := SkipThenLimit(FromList(items), 3, 4)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{4, 5, 6, 7}))
	})

	It("OrElse uses primary when non-empty", func() {
		cursor := OrElse(
			FromList([]int{1, 2, 3}),
			func() RecordCursor[int] { return FromList([]int{99}) },
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{1, 2, 3}))
	})

	It("OrElse falls back when primary is empty", func() {
		cursor := OrElse(
			Empty[int](),
			func() RecordCursor[int] { return FromList([]int{99, 100}) },
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal([]int{99, 100}))
	})

	It("OrElse both empty", func() {
		cursor := OrElse(
			Empty[int](),
			func() RecordCursor[int] { return Empty[int]() },
		)
		result, err := AsList(ctx, cursor)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})

	It("ScannedRecordsLimit", func() {
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range int64(10) {
				order := &gen.Order{OrderId: proto.Int64(i + 1), Price: proto.Int32(int32((i + 1) * 10))}
				if _, saveErr := store.SaveRecord(order); saveErr != nil {
					return nil, saveErr
				}
			}

			// Scanned records limit of 3 — scan 3, stop with ScanLimitReached
			scan := ForwardScan()
			scan.ExecuteProperties.ScannedRecordsLimit = 3
			cursor := store.ScanRecords(nil, scan)

			var records []*FDBStoredRecord[proto.Message]
			var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
			for {
				result, nextErr := cursor.OnNext(ctx)
				Expect(nextErr).NotTo(HaveOccurred())
				lastResult = result
				if !result.HasNext() {
					break
				}
				records = append(records, result.GetValue())
			}
			Expect(records).To(HaveLen(3))
			Expect(lastResult.GetNoNextReason()).To(Equal(ScanLimitReached))
			Expect(lastResult.HasStoppedBeforeEnd()).To(BeTrue())

			Expect(cursor.Close()).To(Succeed())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
