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
				cursor.Seq(ctx),
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
						cursor.Seq(ctx),
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
			limited := Limit(cursor.Seq(ctx), 0)
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
				cursor.Seq2(ctx),
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
		for range cursor.Seq(ctx) {
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
