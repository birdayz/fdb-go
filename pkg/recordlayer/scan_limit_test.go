package recordlayer

import (
	"context"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// NoNextReason_Helpers tests the NoNextReason helper methods directly.
var _ = Describe("NoNextReason_Helpers", func() {
	It("SourceExhausted", func() {
		r := SourceExhausted
		Expect(r.IsSourceExhausted()).To(BeTrue())
		Expect(r.IsLimitReached()).To(BeFalse())
		Expect(r.IsOutOfBand()).To(BeFalse())
	})

	It("ReturnLimitReached", func() {
		r := ReturnLimitReached
		Expect(r.IsSourceExhausted()).To(BeFalse())
		Expect(r.IsLimitReached()).To(BeTrue())
		// ReturnLimitReached is NOT out-of-band (it's driven by returned records)
		Expect(r.IsOutOfBand()).To(BeFalse())
	})

	It("ByteLimitReached", func() {
		r := ByteLimitReached
		Expect(r.IsSourceExhausted()).To(BeFalse())
		Expect(r.IsLimitReached()).To(BeTrue())
		Expect(r.IsOutOfBand()).To(BeTrue())
	})

	It("TimeLimitReached", func() {
		r := TimeLimitReached
		Expect(r.IsSourceExhausted()).To(BeFalse())
		Expect(r.IsLimitReached()).To(BeTrue())
		Expect(r.IsOutOfBand()).To(BeTrue())
	})

	It("ScanLimitReached", func() {
		r := ScanLimitReached
		Expect(r.IsSourceExhausted()).To(BeFalse())
		Expect(r.IsLimitReached()).To(BeTrue())
		Expect(r.IsOutOfBand()).To(BeTrue())
	})
})

// RecordCursorResult_HasStoppedBeforeEnd tests the HasStoppedBeforeEnd helper.
var _ = Describe("RecordCursorResult_HasStoppedBeforeEnd", func() {
	It("SourceExhaustedNotStopped", func() {
		result := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeFalse())
	})

	It("ReturnLimitIsStopped", func() {
		result := NewResultNoNext[int](ReturnLimitReached, &BytesContinuation{bytes: []byte{0x01}})
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("ByteLimitIsStopped", func() {
		result := NewResultNoNext[int](ByteLimitReached, &BytesContinuation{bytes: []byte{0x01}})
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("TimeLimitIsStopped", func() {
		result := NewResultNoNext[int](TimeLimitReached, &BytesContinuation{bytes: []byte{0x01}})
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("ScanLimitIsStopped", func() {
		result := NewResultNoNext[int](ScanLimitReached, &BytesContinuation{bytes: []byte{0x01}})
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("WithValueNotStopped", func() {
		v := 42
		result := NewResultWithValue(v, &BytesContinuation{bytes: []byte{0x01}})
		Expect(result.HasNext()).To(BeTrue())
		Expect(result.HasStoppedBeforeEnd()).To(BeFalse())
	})
})

// ByteScanLimit tests byte scan limit enforcement in record scanning.
var _ = Describe("ByteScanLimit", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("StopsWhenByteLimitExceeded_OneRecord", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// 1 byte limit — byte check is pre-read with free initial pass
			// (matching Java's CursorLimitManager), so the first record is always returned.
			// The limit fires on the second call.
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:    SerializableIsolation,
					ScannedBytesLimit: 1,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			// First call: free initial pass — returns a record
			result, err := cursor.OnNext(rtx.ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())

			// Second call: byte limit exceeded — stops
			result, err = cursor.OnNext(rtx.ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(ByteLimitReached))
			Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
			contBytes, contErr := result.GetContinuation().ToBytes()
			Expect(contErr).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("StopsWhenByteLimitExceeded_PartialScan", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 10; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
					Flower: &gen.Flower{
						Type:  proto.String("Rose"),
						Color: gen.Color_RED.Enum(),
					},
				}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Large enough limit to get some records but not all 10.
			// Each record is ~100-200 bytes in FDB; 500 bytes should allow a few.
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:    SerializableIsolation,
					ScannedBytesLimit: 500,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			var count int
			var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
			for {
				result, err := cursor.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				lastResult = result
				if !result.HasNext() {
					break
				}
				count++
			}

			// Should have gotten at least 1 record but not all 10
			Expect(count).To(BeNumerically(">=", 1))
			Expect(count).To(BeNumerically("<", 10))
			Expect(lastResult.GetNoNextReason()).To(Equal(ByteLimitReached))
			Expect(lastResult.HasStoppedBeforeEnd()).To(BeTrue())
			contBytes, contErr := lastResult.GetContinuation().ToBytes()
			Expect(contErr).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ResumeAfterByteLimit", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 20 records with larger payloads to ensure byte limit triggers
			for i := int64(1); i <= 20; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
					Flower: &gen.Flower{
						Type:  proto.String("LongFlowerNameForByteSize"),
						Color: gen.Color_RED.Enum(),
					},
				}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Use a limit that allows some records but not all 20
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:    SerializableIsolation,
					ScannedBytesLimit: 500,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			var firstBatch []*FDBStoredRecord[proto.Message]
			var contBytes []byte
			var firstReason NoNextReason
			for {
				result, err := cursor.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					var contErr error
					contBytes, contErr = result.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					firstReason = result.GetNoNextReason()
					break
				}
				firstBatch = append(firstBatch, result.GetValue())
			}
			Expect(contBytes).NotTo(BeNil())
			Expect(firstReason).To(Equal(ByteLimitReached))
			Expect(len(firstBatch)).To(BeNumerically(">=", 1))
			Expect(len(firstBatch)).To(BeNumerically("<", 20))

			// Resume with no byte limit — should get the rest
			scanProps2 := ForwardScan()
			cursor2 := store.ScanRecords(contBytes, scanProps2)

			var secondBatch []*FDBStoredRecord[proto.Message]
			for {
				result, err := cursor2.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
					break
				}
				secondBatch = append(secondBatch, result.GetValue())
			}

			// Both batches together should cover all 20 records
			total := len(firstBatch) + len(secondBatch)
			Expect(total).To(Equal(20))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("NoByteLimitScansAll", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with zero byte limit (no limit)
			scanProps := ForwardScan()
			cursor := store.ScanRecords(nil, scanProps)

			var count int
			for {
				result, err := cursor.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
					break
				}
				count++
			}
			Expect(count).To(Equal(5))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// ScannedRecordsLimit_BoundaryTests tests ScannedRecordsLimit edge cases.
var _ = Describe("ScannedRecordsLimit_BoundaryTests", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitEqualsRecordCount", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scanned records limit = exactly the number of records
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:      SerializableIsolation,
					ScannedRecordsLimit: 3,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			var count int
			var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
			for {
				result, err := cursor.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				lastResult = result
				if !result.HasNext() {
					break
				}
				count++
			}

			// Should get all 3 records, then ScanLimitReached
			// (limit checked before read, so after reading 3, limit hits)
			Expect(count).To(Equal(3))
			Expect(lastResult.GetNoNextReason()).To(Equal(ScanLimitReached))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitOfOneReturnsOneRecord", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:      SerializableIsolation,
					ScannedRecordsLimit: 1,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			result, err := cursor.OnNext(rtx.ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())

			result, err = cursor.OnNext(rtx.ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(ScanLimitReached))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// RowLimit_SourceExhausted tests that row limit on exact boundary returns SourceExhausted.
var _ = Describe("RowLimit_SourceExhausted", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("RowLimitExceedsRecordCountReturnsExhausted", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Row limit > record count → should exhaust source
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:   SerializableIsolation,
					ReturnedRowLimit: 10,
				},
			}
			cursor := store.ScanRecords(nil, scanProps)

			var count int
			var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
			for {
				result, err := cursor.OnNext(rtx.ctx)
				Expect(err).NotTo(HaveOccurred())
				lastResult = result
				if !result.HasNext() {
					break
				}
				count++
			}

			Expect(count).To(Equal(3))
			Expect(lastResult.GetNoNextReason()).To(Equal(SourceExhausted))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
