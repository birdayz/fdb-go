package recordlayer

import (
	"context"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// makeLargeOrder creates an Order with a large enough Flower.Type string to
// produce a serialized size exceeding the target bytes. Returns the order and
// its approximate serialized size.
func makeLargeOrder(orderID int64, targetSize int) *gen.Order {
	// Flower.Type is a string field — fill it to push the serialized size past the target.
	// Protobuf overhead is small, so the string length is close to the total size.
	padding := strings.Repeat("X", targetSize)
	return &gen.Order{
		OrderId: proto.Int64(orderID),
		Price:   proto.Int32(42),
		Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
	}
}

func splitMetadata() *RecordMetaData {
	builder := NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto).
		SetSplitLongRecords(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		panic(fmt.Sprintf("splitMetadata: %v", err))
	}
	return md
}

var _ = Describe("SplitRecords", func() {
	It("saves and loads a record that exceeds 100KB", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create a ~250KB record (will be split into 3 chunks)
			largeOrder := makeLargeOrder(1, 250_000)
			stored, err := store.SaveRecord(largeOrder)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.Split).To(BeTrue())
			Expect(stored.KeyCount).To(Equal(3)) // 250KB / 100KB = 3 chunks

			// Load it back
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Split).To(BeTrue())
			Expect(loaded.KeyCount).To(Equal(3))

			order, ok := loaded.Record.(*gen.Order)
			Expect(ok).To(BeTrue())
			Expect(*order.OrderId).To(Equal(int64(1)))
			Expect(*order.Price).To(Equal(int32(42)))
			Expect(len(*order.Flower.Type)).To(Equal(250_000))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("saves and loads an unsplit record with splitLongRecords enabled", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Small record — should NOT be split even when splitLongRecords is enabled
			smallOrder := &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(10),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			}
			stored, err := store.SaveRecord(smallOrder)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.Split).To(BeFalse())
			Expect(stored.KeyCount).To(Equal(1))

			loaded, err := store.LoadRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Split).To(BeFalse())

			order, ok := loaded.Record.(*gen.Order)
			Expect(ok).To(BeTrue())
			Expect(*order.OrderId).To(Equal(int64(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects large records when splitLongRecords is disabled", func() {
		ctx := context.Background()

		// Metadata WITHOUT splitLongRecords
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			largeOrder := makeLargeOrder(1, 150_000)
			_, err = store.SaveRecord(largeOrder)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exceeds limit"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deletes a split record completely", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			largeOrder := makeLargeOrder(3, 200_000)
			_, err = store.SaveRecord(largeOrder)
			Expect(err).NotTo(HaveOccurred())

			// Verify it exists
			exists, err := store.RecordExists(tuple.Tuple{int64(3)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			// Delete it
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify it's gone
			loaded, err := store.LoadRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).To(BeNil())

			exists, err = store.RecordExists(tuple.Tuple{int64(3)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates a split record (overwrite with different size)", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save a 300KB record (4 chunks)
			large := makeLargeOrder(4, 300_000)
			stored, err := store.SaveRecord(large)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.Split).To(BeTrue())

			// Overwrite with a 120KB record (2 chunks) — old chunks must be cleared
			smaller := makeLargeOrder(4, 120_000)
			stored2, err := store.SaveRecord(smaller)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored2.Split).To(BeTrue())
			Expect(stored2.KeyCount).To(Equal(2))

			// Load and verify the smaller version
			loaded, err := store.LoadRecord(tuple.Tuple{int64(4)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Split).To(BeTrue())
			Expect(loaded.KeyCount).To(Equal(2))

			order, ok := loaded.Record.(*gen.Order)
			Expect(ok).To(BeTrue())
			Expect(len(*order.Flower.Type)).To(Equal(120_000))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates from split to unsplit", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save a split record
			large := makeLargeOrder(5, 200_000)
			stored, err := store.SaveRecord(large)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.Split).To(BeTrue())

			// Overwrite with a small record — should clear all split chunks
			small := &gen.Order{
				OrderId: proto.Int64(5),
				Price:   proto.Int32(99),
				Flower:  &gen.Flower{Type: proto.String("Tiny"), Color: gen.Color_BLUE.Enum()},
			}
			stored2, err := store.SaveRecord(small)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored2.Split).To(BeFalse())
			Expect(stored2.KeyCount).To(Equal(1))

			loaded, err := store.LoadRecord(tuple.Tuple{int64(5)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Split).To(BeFalse())

			order, ok := loaded.Record.(*gen.Order)
			Expect(ok).To(BeTrue())
			Expect(*order.Flower.Type).To(Equal("Tiny"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scans a mix of split and unsplit records", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save a mix: small, large, small, large
			small1 := &gen.Order{
				OrderId: proto.Int64(10),
				Price:   proto.Int32(1),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			}
			_, err = store.SaveRecord(small1)
			Expect(err).NotTo(HaveOccurred())

			large1 := makeLargeOrder(20, 150_000)
			_, err = store.SaveRecord(large1)
			Expect(err).NotTo(HaveOccurred())

			small2 := &gen.Order{
				OrderId: proto.Int64(30),
				Price:   proto.Int32(3),
				Flower:  &gen.Flower{Type: proto.String("Lily"), Color: gen.Color_BLUE.Enum()},
			}
			_, err = store.SaveRecord(small2)
			Expect(err).NotTo(HaveOccurred())

			large2 := makeLargeOrder(40, 250_000)
			_, err = store.SaveRecord(large2)
			Expect(err).NotTo(HaveOccurred())

			// Scan all records
			cursor := store.ScanRecords(nil, ForwardScan())
			defer func() { _ = cursor.Close() }()

			var foundIDs []int64
			scanCtx := context.Background()
			for {
				result, scanErr := cursor.OnNext(scanCtx)
				Expect(scanErr).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				rec := result.GetValue()
				order, ok := rec.Record.(*gen.Order)
				Expect(ok).To(BeTrue())
				foundIDs = append(foundIDs, *order.OrderId)
			}

			Expect(foundIDs).To(Equal([]int64{10, 20, 30, 40}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scans split records with row limit and continuation", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 5 large records
			for i := int64(1); i <= 5; i++ {
				large := makeLargeOrder(i, 150_000)
				_, err = store.SaveRecord(large)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with limit of 2
			props := ForwardScan()
			props.ExecuteProperties.ReturnedRowLimit = 2

			cursor := store.ScanRecords(nil, props)
			var batch1 []int64
			scanCtx := context.Background()
			var continuation []byte

			for {
				result, scanErr := cursor.OnNext(scanCtx)
				Expect(scanErr).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(ReturnLimitReached))
					var contErr error
					continuation, contErr = result.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					break
				}
				order, ok := result.GetValue().Record.(*gen.Order)
				Expect(ok).To(BeTrue())
				batch1 = append(batch1, *order.OrderId)
			}
			_ = cursor.Close()
			Expect(batch1).To(Equal([]int64{1, 2}))
			Expect(continuation).NotTo(BeNil())

			// Continue with the next batch
			props2 := ForwardScan()
			props2.ExecuteProperties.ReturnedRowLimit = 2
			cursor2 := store.ScanRecords(continuation, props2)
			var batch2 []int64
			for {
				result, scanErr := cursor2.OnNext(scanCtx)
				Expect(scanErr).NotTo(HaveOccurred())
				if !result.HasNext() {
					var contErr error
					continuation, contErr = result.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					break
				}
				order, ok := result.GetValue().Record.(*gen.Order)
				Expect(ok).To(BeTrue())
				batch2 = append(batch2, *order.OrderId)
			}
			_ = cursor2.Close()
			Expect(batch2).To(Equal([]int64{3, 4}))

			// Final batch — should get 1 record + SourceExhausted
			props3 := ForwardScan()
			props3.ExecuteProperties.ReturnedRowLimit = 2
			cursor3 := store.ScanRecords(continuation, props3)
			var batch3 []int64
			for {
				result, scanErr := cursor3.OnNext(scanCtx)
				Expect(scanErr).NotTo(HaveOccurred())
				if !result.HasNext() {
					Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
					break
				}
				order, ok := result.GetValue().Record.(*gen.Order)
				Expect(ok).To(BeTrue())
				batch3 = append(batch3, *order.OrderId)
			}
			_ = cursor3.Close()
			Expect(batch3).To(Equal([]int64{5}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RecordExists detects both split and unsplit records", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save one small, one large
			small := &gen.Order{
				OrderId: proto.Int64(100),
				Price:   proto.Int32(1),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			}
			_, err = store.SaveRecord(small)
			Expect(err).NotTo(HaveOccurred())

			large := makeLargeOrder(200, 150_000)
			_, err = store.SaveRecord(large)
			Expect(err).NotTo(HaveOccurred())

			// Check existence
			exists, err := store.RecordExists(tuple.Tuple{int64(100)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			exists, err = store.RecordExists(tuple.Tuple{int64(200)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			exists, err = store.RecordExists(tuple.Tuple{int64(999)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles exact boundary size (exactly 100KB)", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create a record that serializes to just under 100KB — should NOT split
			// The padding string needs to be small enough that after protobuf wrapping
			// in the UnionDescriptor it still fits.
			order := makeLargeOrder(500, 99_900)
			stored, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Check if it's split based on actual serialized size
			if stored.ValueSize <= splitRecordSize {
				Expect(stored.Split).To(BeFalse())
			} else {
				Expect(stored.Split).To(BeTrue())
			}

			// Regardless, load should work
			loaded, err := store.LoadRecord(tuple.Tuple{int64(500)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reverse scans split records correctly", func() {
		ctx := context.Background()
		metaData := splitMetadata()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 large records
			for i := int64(1); i <= 3; i++ {
				large := makeLargeOrder(i*10, 150_000)
				_, err = store.SaveRecord(large)
				Expect(err).NotTo(HaveOccurred())
			}

			// Reverse scan
			cursor := store.ScanRecords(nil, ReverseScan())
			defer func() { _ = cursor.Close() }()

			var foundIDs []int64
			scanCtx := context.Background()
			for {
				result, scanErr := cursor.OnNext(scanCtx)
				Expect(scanErr).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				order, ok := result.GetValue().Record.(*gen.Order)
				Expect(ok).To(BeTrue())
				foundIDs = append(foundIDs, *order.OrderId)
			}

			Expect(foundIDs).To(Equal([]int64{30, 20, 10}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
