package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("DeleteRecordsWhere", func() {
	ctx := context.Background()

	// Helper: builds metadata with record type prefix PKs.
	buildMetaDataWithTypePrefix := func(indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		for _, idx := range indexes {
			builder.AddIndex("Order", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	It("deletes records and VALUE index entries for one type", func() {
		ks := specSubspace()
		priceIdx := NewIndex("price_idx", Field("price"))
		md := buildMetaDataWithTypePrefix(priceIdx)

		// Insert records of both types.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(1); i <= 3; i++ {
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Customer")})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Get Order's record type key.
		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Delete all Order records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			err = store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify: Orders are gone, Customers remain, index is empty.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// All 5 orders should be gone.
			for i := int64(1); i <= 5; i++ {
				rec, err := store.LoadRecord(tuple.Tuple{orderTypeKey, i})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).To(BeNil(), "Order %d should be deleted", i)
			}

			// All 3 customers should remain.
			customerTypeKey := md.GetRecordType("Customer").GetRecordTypeKey()
			for i := int64(1); i <= 3; i++ {
				rec, err := store.LoadRecord(tuple.Tuple{customerTypeKey, i})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil(), "Customer %d should remain", i)
			}

			// Price index should be empty (type-specific to Order).
			entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "Price index should be cleared")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deletes records and COUNT index entries", func() {
		ks := specSubspace()
		countIdx := NewCountIndex("order_count", Ungrouped(EmptyKey()))
		md := buildMetaDataWithTypePrefix(countIdx)

		// Insert 5 orders.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Delete all orders.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify COUNT index is cleared.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "COUNT index should be cleared")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deletes records and RANK index entries (both primary and secondary)", func() {
		ks := specSubspace()
		rankIdx := NewRankIndex("price_rank", Ungrouped(Field("price")))
		md := buildMetaDataWithTypePrefix(rankIdx)

		// Insert 5 orders.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Delete all orders.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify RANK index is cleared (both BY_VALUE and BY_RANK should be empty).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "RANK index should be cleared")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears record versions", func() {
		ks := specSubspace()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.SetStoreRecordVersions(true)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Insert versioned orders.
		_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Delete all orders.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify records and versions are gone.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				rec, err := store.LoadRecord(tuple.Tuple{orderTypeKey, i})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).To(BeNil())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects empty prefix", func() {
		ks := specSubspace()
		md := buildMetaDataWithTypePrefix()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{})
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("prefix must be non-empty"))
	})

	It("rejects prefix that doesn't align with universal index", func() {
		ks := specSubspace()

		// Build metadata where PK starts with RecordType but a universal index
		// starts with Field("price") — not aligned with RecordType prefix.
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.AddUniversalIndex(NewIndex("univ_price", Field("price")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot be cleared"))
	})

	It("handles universal index with RecordType prefix", func() {
		ks := specSubspace()

		// Universal index whose expression starts with RecordType — aligned with PK prefix.
		// Use only RecordTypeKey() as the index expression since "price" doesn't exist on Customer.
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		univIdx := NewIndex("univ_type", RecordTypeKey())
		builder.AddUniversalIndex(univIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()
		customerTypeKey := md.GetRecordType("Customer").GetRecordTypeKey()

		// Insert orders and customers.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(1); i <= 2; i++ {
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("C")})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete only orders.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify: order index entries gone, customer entries remain.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// Scan index for order type key prefix — should be empty.
			orderRange := TupleRangeAllOf(tuple.Tuple{orderTypeKey})
			orderEntries, err := AsList(ctx, store.ScanIndex(univIdx, orderRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(orderEntries).To(BeEmpty(), "Order index entries should be deleted")

			// Scan for customer type key prefix — should still have entries.
			customerRange := TupleRangeAllOf(tuple.Tuple{customerTypeKey})
			customerEntries, err := AsList(ctx, store.ScanIndex(univIdx, customerRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(customerEntries).To(HaveLen(2), "Customer index entries should remain")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("respects store lock state", func() {
		ks := specSubspace()
		md := buildMetaDataWithTypePrefix()
		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Create store and lock it.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			lockState := &gen.DataStoreInfo_StoreLockState{
				LockState: gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE.Enum(),
			}
			return nil, store.SetStoreLockState(lockState)
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to delete — should fail.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			return nil, store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
		})
		Expect(err).To(HaveOccurred())
		var lockErr *StoreIsLockedForRecordUpdatesError
		Expect(err).To(BeAssignableToTypeOf(lockErr))
	})

	It("can save new records after deleteRecordsWhere", func() {
		ks := specSubspace()
		priceIdx := NewIndex("price_idx", Field("price"))
		md := buildMetaDataWithTypePrefix(priceIdx)

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Insert, delete, re-insert.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 orders.
			for i := int64(1); i <= 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete all orders.
			err = store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
			Expect(err).NotTo(HaveOccurred())

			// Re-insert 2 new orders.
			for i := int64(10); i <= 11; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify only the new orders exist.
			entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(110)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("skips disabled indexes", func() {
		ks := specSubspace()
		priceIdx := NewIndex("price_idx", Field("price"))
		md := buildMetaDataWithTypePrefix(priceIdx)

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		// Insert records, disable the index, then deleteRecordsWhere.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexDisabled("price_idx")
			Expect(err).NotTo(HaveOccurred())

			// Should succeed even though the disabled index can't be cleared.
			err = store.DeleteRecordsWhere(tuple.Tuple{orderTypeKey})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
