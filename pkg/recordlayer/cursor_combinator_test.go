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
		r4 := NewResultWithValue(42, &BytesContinuation{})
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
