package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Helper: save N orders with consecutive IDs [1..n] into a store.
func saveOrders(store *FDBRecordStore, n int) {
	for i := int64(1); i <= int64(n); i++ {
		order := &gen.Order{
			OrderId: proto.Int64(i),
			Price:   proto.Int32(int32(i * 10)),
			Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
		}
		_, err := store.SaveRecord(order)
		Expect(err).NotTo(HaveOccurred())
	}
}

// Helper: collect all order IDs from a cursor until exhausted or stopped.
func drainOrderIDs(cursor RecordCursor[*FDBStoredRecord[proto.Message]]) ([]int64, RecordCursorResult[*FDBStoredRecord[proto.Message]]) {
	ctx := context.Background()
	var ids []int64
	var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
	for {
		result, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		lastResult = result
		if !result.HasNext() {
			break
		}
		order := result.GetValue().Record.(*gen.Order)
		ids = append(ids, order.GetOrderId())
	}
	return ids, lastResult
}

var _ = Describe("KeyValueCursor", func() {
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

	// =========================================================================
	// Endpoint type combinations
	// =========================================================================
	Describe("Endpoint type combinations", func() {
		It("RANGE_EXCLUSIVE low + RANGE_INCLUSIVE high", func() {
			// Data: records 1..5. Range (1, 4] should return 2, 3, 4.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(1)}, tuple.Tuple{int64(4)},
					EndpointTypeRangeExclusive, EndpointTypeRangeInclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 3, 4}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_INCLUSIVE low + RANGE_EXCLUSIVE high", func() {
			// Data: records 1..5. Range [2, 5) should return 2, 3, 4.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(5)},
					EndpointTypeRangeInclusive, EndpointTypeRangeExclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 3, 4}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_EXCLUSIVE low + RANGE_EXCLUSIVE high", func() {
			// Data: records 1..5. Range (1, 5) should return 2, 3, 4.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(1)}, tuple.Tuple{int64(5)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 3, 4}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_INCLUSIVE low + RANGE_INCLUSIVE high", func() {
			// Data: records 1..5. Range [2, 4] should return 2, 3, 4.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(4)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 3, 4}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("TREE_START low + RANGE_EXCLUSIVE high", func() {
			// Data: records 1..5. Range [tree_start, 3) should return 1, 2.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					nil, tuple.Tuple{int64(3)},
					EndpointTypeTreeStart, EndpointTypeRangeExclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{1, 2}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_INCLUSIVE low + TREE_END high", func() {
			// Data: records 1..5. Range [3, tree_end) should return 3, 4, 5.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(3)}, nil,
					EndpointTypeRangeInclusive, EndpointTypeTreeEnd,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{3, 4, 5}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_EXCLUSIVE low + RANGE_INCLUSIVE high in reverse", func() {
			// Data: records 1..5. Range (1, 4] reverse should return 4, 3, 2.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(1)}, tuple.Tuple{int64(4)},
					EndpointTypeRangeExclusive, EndpointTypeRangeInclusive,
					nil, ReverseScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{4, 3, 2}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_INCLUSIVE low + RANGE_EXCLUSIVE high in reverse", func() {
			// Data: records 1..5. Range [2, 5) reverse should return 4, 3, 2.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(5)},
					EndpointTypeRangeInclusive, EndpointTypeRangeExclusive,
					nil, ReverseScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{4, 3, 2}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANGE_EXCLUSIVE low + RANGE_EXCLUSIVE high in reverse", func() {
			// Data: records 1..5. Range (2, 5) reverse should return 4, 3.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(5)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, ReverseScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{4, 3}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("single record range with inclusive endpoints", func() {
			// Data: records 1..5. Range [3, 3] should return just 3.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(3)}, tuple.Tuple{int64(3)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{3}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("single record range with exclusive endpoints yields empty", func() {
			// Data: records 1..5. Range (3, 3) is empty.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(3)}, tuple.Tuple{int64(3)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Empty / edge range scenarios
	// =========================================================================
	Describe("Empty range scenarios", func() {
		It("no keys in range (non-overlapping range)", func() {
			// Data: records 1..5. Range [100, 200] has nothing.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(100)}, tuple.Tuple{int64(200)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				cont := last.GetContinuation()
				Expect(cont).NotTo(BeNil())
				Expect(cont.IsEnd()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("boundary-only range excluded by exclusive endpoints", func() {
			// Data: records 1..5. Range (2, 3) excludes both 2 and 3 — no records.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(3)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("empty store full scan returns SourceExhausted", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())

				cursor := store.ScanRecords(nil, ForwardScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("empty store reverse scan returns SourceExhausted", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())

				cursor := store.ScanRecords(nil, ReverseScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("range below all records returns empty", func() {
			// Data: records 1..5. Range [-100, -1] should be empty.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(-100)}, tuple.Tuple{int64(-1)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, ForwardScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(BeEmpty())
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Limit interactions
	// =========================================================================
	Describe("Limit interactions", func() {
		It("row limit of 0 (unlimited) returns all records", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 0, // unlimited
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(10))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("row limit of 1 returns exactly one record", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 1,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{1}))
				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("row limit equals record count reports SourceExhausted", func() {
			// When limit == count, the cursor reads all records, then checks
			// iterator.Advance() which returns false -> SourceExhausted.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 3)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 3,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{1, 2, 3}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("row limit + scanned records limit active simultaneously", func() {
			// Row limit = 5, scan limit = 3. Scan limit is tighter, so it fires first.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:      SerializableIsolation,
						ReturnedRowLimit:    5,
						ScannedRecordsLimit: 3,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(3))
				Expect(last.GetNoNextReason()).To(Equal(ScanLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("row limit + byte limit active simultaneously", func() {
			// Row limit = 100 (won't fire), byte limit = 1 (fires after 1 record).
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:    SerializableIsolation,
						ReturnedRowLimit:  100,
						ScannedBytesLimit: 1, // fires after first record (free initial pass)
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(1)) // free initial pass
				Expect(last.GetNoNextReason()).To(Equal(ByteLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scan limit of 1 returns exactly one record", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:      SerializableIsolation,
						ScannedRecordsLimit: 1,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(1))
				Expect(ids[0]).To(Equal(int64(1)))
				Expect(last.GetNoNextReason()).To(Equal(ScanLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("row limit tighter than scan limit uses ReturnLimitReached", func() {
			// Row limit = 2, scan limit = 10. Row limit fires first.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:      SerializableIsolation,
						ReturnedRowLimit:    2,
						ScannedRecordsLimit: 10,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(2))
				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Continuation resume correctness
	// =========================================================================
	Describe("Continuation resume", func() {
		It("resume mid-scan forward direction", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				// Read 3 records, get continuation
				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 3,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids1, last1 := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids1).To(Equal([]int64{1, 2, 3}))
				Expect(last1.GetNoNextReason()).To(Equal(ReturnLimitReached))

				contBytes, contErr := last1.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				Expect(contBytes).NotTo(BeNil())

				// Resume from continuation, read 3 more
				cursor2 := store.ScanRecords(contBytes, props)
				ids2, last2 := drainOrderIDs(cursor2)
				_ = cursor2.Close()

				Expect(ids2).To(Equal([]int64{4, 5, 6}))
				Expect(last2.GetNoNextReason()).To(Equal(ReturnLimitReached))

				// Resume again, read remaining
				contBytes2, contErr := last2.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				unlimitedProps := ForwardScan()
				cursor3 := store.ScanRecords(contBytes2, unlimitedProps)
				ids3, last3 := drainOrderIDs(cursor3)
				_ = cursor3.Close()

				Expect(ids3).To(Equal([]int64{7, 8, 9, 10}))
				Expect(last3.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("resume mid-scan reverse direction", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				// Read 3 records in reverse, get continuation
				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(3),
					Reverse:             true,
					CursorStreamingMode: StreamingModeIterator,
				}
				cursor := store.ScanRecords(nil, props)
				ids1, last1 := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids1).To(Equal([]int64{10, 9, 8}))
				Expect(last1.GetNoNextReason()).To(Equal(ReturnLimitReached))

				contBytes, contErr := last1.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())

				// Resume reverse scan
				cursor2 := store.ScanRecords(contBytes, props)
				ids2, last2 := drainOrderIDs(cursor2)
				_ = cursor2.Close()

				Expect(ids2).To(Equal([]int64{7, 6, 5}))
				Expect(last2.GetNoNextReason()).To(Equal(ReturnLimitReached))

				// Resume again to get the rest
				contBytes2, contErr := last2.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				unlimitedRev := ReverseScan()
				cursor3 := store.ScanRecords(contBytes2, unlimitedRev)
				ids3, last3 := drainOrderIDs(cursor3)
				_ = cursor3.Close()

				Expect(ids3).To(Equal([]int64{4, 3, 2, 1}))
				Expect(last3.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("continuation for deleted key skips to next", func() {
			ks := specSubspace()

			// Save 5 records in first transaction
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Read 2 records, get continuation pointing at record 2
			var contBytes []byte
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 2,
					},
				}
				cursor := store.ScanRecords(nil, props)
				_, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				var contErr error
				contBytes, contErr = last.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete record 3 (the one the continuation would land on next)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())
				deleted, delErr := store.DeleteRecord(tuple.Tuple{int64(3)})
				Expect(delErr).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Resume with continuation — should skip deleted record 3 and get 4, 5
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())

				cursor := store.ScanRecords(contBytes, ForwardScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{4, 5}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("continuation for deleted key in reverse scan", func() {
			ks := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Read 2 records in reverse: [5, 4]. Get continuation pointing at 4.
			var contBytes []byte
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())

				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(2),
					Reverse:             true,
					CursorStreamingMode: StreamingModeIterator,
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()
				Expect(ids).To(Equal([]int64{5, 4}))

				var contErr error
				contBytes, contErr = last.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete record 3 (next in reverse direction)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())
				deleted, delErr := store.DeleteRecord(tuple.Tuple{int64(3)})
				Expect(delErr).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Resume reverse scan — should skip deleted 3 and return 2, 1
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				Expect(stErr).NotTo(HaveOccurred())

				cursor := store.ScanRecords(contBytes, ReverseScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 1}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Reverse scan edge cases
	// =========================================================================
	Describe("Reverse scan edge cases", func() {
		It("single record reverse scan", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 1)

				cursor := store.ScanRecords(nil, ReverseScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{1}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("two records reverse scan", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 2)

				cursor := store.ScanRecords(nil, ReverseScan())
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{2, 1}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan with row limit of 1", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(1),
					Reverse:             true,
					CursorStreamingMode: StreamingModeIterator,
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{5}))
				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan with exclusive range endpoints", func() {
			// Data: 1..10. Reverse (3, 7) = {6, 5, 4}
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(3)}, tuple.Tuple{int64(7)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, ReverseScan(),
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{6, 5, 4}))
				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan with row limit + continuation for full coverage", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 6)

				// Scan all in reverse, in batches of 2
				var allIDs []int64
				var continuation []byte
				for batch := 0; batch < 10; batch++ { // safety limit
					props := ScanProperties{
						ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(2),
						Reverse:             true,
						CursorStreamingMode: StreamingModeIterator,
					}
					cursor := store.ScanRecords(continuation, props)
					batchIDs, last := drainOrderIDs(cursor)
					_ = cursor.Close()
					allIDs = append(allIDs, batchIDs...)

					cont := last.GetContinuation()
					if cont.IsEnd() {
						break
					}
					var contErr error
					continuation, contErr = cont.ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
				}

				Expect(allIDs).To(Equal([]int64{6, 5, 4, 3, 2, 1}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// NoNextReason correctness
	// =========================================================================
	Describe("NoNextReason correctness", func() {
		It("SOURCE_EXHAUSTED when range is fully scanned", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 3)

				cursor := store.ScanRecords(nil, ForwardScan())
				_, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(last.GetNoNextReason()).To(Equal(SourceExhausted))
				Expect(last.GetNoNextReason().IsSourceExhausted()).To(BeTrue())
				Expect(last.GetNoNextReason().IsLimitReached()).To(BeFalse())
				Expect(last.GetNoNextReason().IsOutOfBand()).To(BeFalse())
				Expect(last.HasStoppedBeforeEnd()).To(BeFalse())

				cont := last.GetContinuation()
				Expect(cont).NotTo(BeNil())
				Expect(cont.IsEnd()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("ROW_LIMIT_REACHED when row limit hit with continuation", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 2,
					},
				}
				cursor := store.ScanRecords(nil, props)
				_, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				Expect(last.GetNoNextReason().IsSourceExhausted()).To(BeFalse())
				Expect(last.GetNoNextReason().IsLimitReached()).To(BeTrue())
				Expect(last.GetNoNextReason().IsOutOfBand()).To(BeFalse())
				Expect(last.HasStoppedBeforeEnd()).To(BeTrue())

				cont := last.GetContinuation()
				Expect(cont).NotTo(BeNil())
				Expect(cont.IsEnd()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("BYTE_LIMIT_REACHED when byte limit hit", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:    SerializableIsolation,
						ScannedBytesLimit: 1, // fires after first record
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(1)) // free initial pass
				Expect(last.GetNoNextReason()).To(Equal(ByteLimitReached))
				Expect(last.GetNoNextReason().IsOutOfBand()).To(BeTrue())
				Expect(last.HasStoppedBeforeEnd()).To(BeTrue())

				cont := last.GetContinuation()
				Expect(cont).NotTo(BeNil())
				Expect(cont.IsEnd()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("SCAN_LIMIT_REACHED when scanned records limit hit", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:      SerializableIsolation,
						ScannedRecordsLimit: 4,
					},
				}
				cursor := store.ScanRecords(nil, props)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(HaveLen(4))
				Expect(last.GetNoNextReason()).To(Equal(ScanLimitReached))
				Expect(last.GetNoNextReason().IsOutOfBand()).To(BeTrue())
				Expect(last.HasStoppedBeforeEnd()).To(BeTrue())

				cont := last.GetContinuation()
				Expect(cont).NotTo(BeNil())
				Expect(cont.IsEnd()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("SCAN_LIMIT_REACHED provides resumable continuation", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 6)

				// Scan 2 records at a time via scan limit
				var allIDs []int64
				var continuation []byte
				for batch := 0; batch < 10; batch++ { // safety
					props := ScanProperties{
						ExecuteProperties: ExecuteProperties{
							IsolationLevel:      SerializableIsolation,
							ScannedRecordsLimit: 2,
						},
					}
					cursor := store.ScanRecords(continuation, props)
					batchIDs, last := drainOrderIDs(cursor)
					_ = cursor.Close()
					allIDs = append(allIDs, batchIDs...)

					if last.GetNoNextReason() == SourceExhausted {
						break
					}
					var contErr error
					continuation, contErr = last.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
				}

				Expect(allIDs).To(Equal([]int64{1, 2, 3, 4, 5, 6}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("BYTE_LIMIT_REACHED provides resumable continuation", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 5)

				// Use a tiny byte limit to get one record at a time
				var allIDs []int64
				var continuation []byte
				for batch := 0; batch < 20; batch++ { // safety
					props := ScanProperties{
						ExecuteProperties: ExecuteProperties{
							IsolationLevel:    SerializableIsolation,
							ScannedBytesLimit: 1, // fires after free initial pass
						},
					}
					cursor := store.ScanRecords(continuation, props)
					batchIDs, last := drainOrderIDs(cursor)
					_ = cursor.Close()
					allIDs = append(allIDs, batchIDs...)

					if last.GetNoNextReason() == SourceExhausted {
						break
					}
					var contErr error
					continuation, contErr = last.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
				}

				Expect(allIDs).To(Equal([]int64{1, 2, 3, 4, 5}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Closed cursor behavior
	// =========================================================================
	Describe("Closed cursor", func() {
		It("returns error after Close", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 3)

				cursor := store.ScanRecords(nil, ForwardScan())
				closeErr := cursor.Close()
				Expect(closeErr).NotTo(HaveOccurred())

				_, onNextErr := cursor.OnNext(ctx)
				Expect(onNextErr).To(HaveOccurred())
				Expect(onNextErr.Error()).To(ContainSubstring("closed"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Range endpoint with row limit combinations
	// =========================================================================
	Describe("Range with limits", func() {
		It("inclusive range + row limit returns correct subset", func() {
			// Data: 1..10. Range [3, 8] with limit 2 should return 3, 4.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:   SerializableIsolation,
						ReturnedRowLimit: 2,
					},
				}
				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(3)}, tuple.Tuple{int64(8)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, props,
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{3, 4}))
				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("exclusive range + reverse + row limit", func() {
			// Data: 1..10. Range (2, 9) reverse with limit 3 -> 8, 7, 6
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				props := ScanProperties{
					ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(3),
					Reverse:             true,
					CursorStreamingMode: StreamingModeIterator,
				}
				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(9)},
					EndpointTypeRangeExclusive, EndpointTypeRangeExclusive,
					nil, props,
				)
				ids, last := drainOrderIDs(cursor)
				_ = cursor.Close()

				Expect(ids).To(Equal([]int64{8, 7, 6}))
				Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("range scan with scan limit provides correct continuation", func() {
			// Data: 1..10. Range [2, 8] with scan limit 3, resume to completion.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, stErr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				Expect(stErr).NotTo(HaveOccurred())
				saveOrders(store, 10)

				// First batch: scan limit 3 from [2, 8] -> get 2, 3, 4
				props1 := ScanProperties{
					ExecuteProperties: ExecuteProperties{
						IsolationLevel:      SerializableIsolation,
						ScannedRecordsLimit: 3,
					},
				}
				cursor1 := store.ScanRecordsInRange(
					tuple.Tuple{int64(2)}, tuple.Tuple{int64(8)},
					EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
					nil, props1,
				)
				ids1, last1 := drainOrderIDs(cursor1)
				_ = cursor1.Close()

				Expect(ids1).To(Equal([]int64{2, 3, 4}))
				Expect(last1.GetNoNextReason()).To(Equal(ScanLimitReached))

				contBytes, contErr := last1.GetContinuation().ToBytes()
				Expect(contErr).NotTo(HaveOccurred())

				// Resume: use continuation with TREE_START/TREE_END won't work
				// because ScanRecordsInRange doesn't wire continuation into
				// endpoints. Use ScanRecords which handles continuation properly.
				unlimitedProps := ForwardScan()
				cursor2 := store.ScanRecords(contBytes, unlimitedProps)
				ids2, last2 := drainOrderIDs(cursor2)
				_ = cursor2.Close()

				// Continuation resumes at record after 4, gets 5..10
				Expect(ids2).To(Equal([]int64{5, 6, 7, 8, 9, 10}))
				Expect(last2.GetNoNextReason()).To(Equal(SourceExhausted))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// Continuation token format edge cases
	// =========================================================================
	Describe("Continuation token edge cases", func() {
		It("unwrapContinuation handles raw bytes (TO_OLD format)", func() {
			raw := []byte{0x01, 0x02, 0x03}
			result := unwrapContinuation(raw)
			Expect(result).To(Equal(raw))
		})

		It("unwrapContinuation returns nil for nil input", func() {
			result := unwrapContinuation(nil)
			Expect(result).To(BeNil())
		})

		It("unwrapContinuation handles proto-wrapped format (TO_NEW)", func() {
			magic := continuationMagicNumber
			inner := []byte{0xAA, 0xBB, 0xCC}
			msg := &gen.KeyValueCursorContinuation{
				MagicNumber:       &magic,
				InnerContinuation: inner,
			}
			data, marshalErr := proto.Marshal(msg)
			Expect(marshalErr).NotTo(HaveOccurred())

			result := unwrapContinuation(data)
			Expect(result).To(Equal(inner))
		})

		It("unwrapContinuation treats proto without magic as raw bytes", func() {
			// Valid proto but wrong/missing magic number -> treat as raw
			wrongMagic := int64(12345)
			msg := &gen.KeyValueCursorContinuation{
				MagicNumber:       &wrongMagic,
				InnerContinuation: []byte{0xDD},
			}
			data, marshalErr := proto.Marshal(msg)
			Expect(marshalErr).NotTo(HaveOccurred())

			result := unwrapContinuation(data)
			// Treated as raw bytes since magic doesn't match
			Expect(result).To(Equal(data))
		})

		It("wrapContinuation emits the proto-wrapped TO_NEW format (Java 4.11.1.0 default)", func() {
			inner := []byte{0x01, 0x02}
			result, wrapErr := wrapContinuation(inner)
			Expect(wrapErr).NotTo(HaveOccurred())
			// Not raw: the token is a KeyValueCursorContinuation{inner, magic}.
			Expect(result).NotTo(Equal(inner))
			msg := &gen.KeyValueCursorContinuation{}
			Expect(proto.Unmarshal(result, msg)).To(Succeed())
			Expect(msg.GetMagicNumber()).To(Equal(continuationMagicNumber))
			Expect(msg.GetInnerContinuation()).To(Equal(inner))
			// Round-trips back to the raw suffix via the dual-reader.
			Expect(unwrapContinuation(result)).To(Equal(inner))
		})

		It("wrapContinuation keeps an empty-but-present suffix wire-distinguishable from end", func() {
			// prefixLength == len(lastKey): Java emits a proto carrying the magic, NOT an
			// empty raw token (which would be indistinguishable from start/end).
			result, wrapErr := wrapContinuation([]byte{})
			Expect(wrapErr).NotTo(HaveOccurred())
			Expect(result).NotTo(BeEmpty()) // carries the magic number
			Expect((&BytesContinuation{bytes: result}).IsEnd()).To(BeFalse())
			msg := &gen.KeyValueCursorContinuation{}
			Expect(proto.Unmarshal(result, msg)).To(Succeed())
			Expect(msg.GetMagicNumber()).To(Equal(continuationMagicNumber))
		})
	})
})
