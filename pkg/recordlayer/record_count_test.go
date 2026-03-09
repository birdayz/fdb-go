package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RecordCounting", func() {
	ctx := context.Background()

	It("BasicCounting", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey()) // Total count
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert 5 records
			for i := range int64(5) {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(5)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CountNotIncrementedOnUpdate", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey())
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert a record
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Update the same record (overwrite)
			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			if _, err := store.SaveRecord(order2); err != nil {
				return nil, err
			}

			// Count should still be 1, not 2
			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CountDecrementedOnDelete", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey())
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert 3 records
			for i := range int64(3) {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			// Delete one
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			if err != nil {
				return nil, err
			}
			Expect(deleted).To(BeTrue())

			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CountDisabledByDefault", func() {
		// No SetRecordCountKey — counting should be disabled
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save a record — should work even without counting
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// GetRecordCount should return error when counting disabled
			_, err = store.GetRecordCount()
			Expect(err).To(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteNonExistentDoesNotAffectCount", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey())
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert 1 record
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Try to delete non-existent — should not change count
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
			if err != nil {
				return nil, err
			}
			Expect(deleted).To(BeFalse())

			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteAllRecordsResetsCount", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey())
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert 5 records
			for i := range int64(5) {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(5)))

			// Delete all
			Expect(store.DeleteAllRecords()).To(Succeed())

			// Count should be 0
			count, err = store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(0)))

			// Scan should return nothing
			cursor := store.ScanRecords(nil, ForwardScan())
			result, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			Expect(result.HasNext()).To(BeFalse())
			_ = cursor.Close()

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("PerTypeCountingWithRecordTypeKey", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(RecordTypeKey()) // Per-type counting
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert 3 orders and 2 customers
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			for i := int64(101); i <= 102; i++ {
				customer := &gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Test")}
				if _, err := store.SaveRecord(customer); err != nil {
					return nil, err
				}
			}

			// Check per-type counts using GetSnapshotRecordCountForRecordType
			// (which correctly maps record type name → integer type key)
			orderCount, err := store.GetSnapshotRecordCountForRecordType("Order")
			if err != nil {
				return nil, err
			}
			Expect(orderCount).To(Equal(int64(3)))

			customerCount, err := store.GetSnapshotRecordCountForRecordType("Customer")
			if err != nil {
				return nil, err
			}
			Expect(customerCount).To(Equal(int64(2)))

			// Delete one order
			if _, err := store.DeleteRecord(tuple.Tuple{int64(2)}); err != nil {
				return nil, err
			}

			orderCount, err = store.GetSnapshotRecordCountForRecordType("Order")
			if err != nil {
				return nil, err
			}
			Expect(orderCount).To(Equal(int64(2)))

			// Customer count should be unchanged
			customerCount, err = store.GetSnapshotRecordCountForRecordType("Customer")
			if err != nil {
				return nil, err
			}
			Expect(customerCount).To(Equal(int64(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertDeleteInsertCount", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.SetRecordCountKey(EmptyKey())
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Insert
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Delete
			if _, err := store.DeleteRecord(tuple.Tuple{int64(1)}); err != nil {
				return nil, err
			}

			// Re-insert (same key)
			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			if _, err := store.SaveRecord(order2); err != nil {
				return nil, err
			}

			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
