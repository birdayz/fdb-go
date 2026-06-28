package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

	It("DisabledStateSkipsMutations", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

			// Insert 3 records with counting active
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))

			// Transition to DISABLED — clears count data
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_DISABLED)).To(Succeed())

			// Saves should still work, but count should not be maintained
			order4 := &gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(40)}
			_, err = store.SaveRecord(order4)
			Expect(err).NotTo(HaveOccurred())

			// Deletes should work too
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			// GetRecordCount should fail (state is DISABLED, not READABLE)
			_, err = store.GetRecordCount()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not readable"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DisabledStateIsTerminal", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

			// READABLE → DISABLED: allowed
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_DISABLED)).To(Succeed())

			// DISABLED → READABLE: forbidden
			err = store.UpdateRecordCountState(gen.DataStoreInfo_READABLE)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DISABLED"))

			// DISABLED → WRITE_ONLY: also forbidden
			err = store.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DISABLED"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("WriteOnlyStateMaintainsCountButBlocksQuery", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

			// Insert 2 records
			for i := int64(1); i <= 2; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			// Transition to WRITE_ONLY
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)).To(Succeed())

			// Saves should still maintain the count
			order3 := &gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)}
			_, err = store.SaveRecord(order3)
			Expect(err).NotTo(HaveOccurred())

			// But querying should be blocked
			_, err = store.GetRecordCount()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not readable"))

			// Transition back to READABLE
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_READABLE)).To(Succeed())

			// Now count should work and reflect all 3 records
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("StateTransitionValidation", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

			// Same state → no-op
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_READABLE)).To(Succeed())

			// READABLE → WRITE_ONLY: allowed
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)).To(Succeed())

			// WRITE_ONLY → READABLE: allowed
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_READABLE)).To(Succeed())

			// READABLE → DISABLED: allowed
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_DISABLED)).To(Succeed())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CountRecordsByScan", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			// CountRecords scans all records
			count, err := store.CountRecords(ctx, nil, nil, EndpointTypeTreeStart, EndpointTypeTreeEnd, nil, ForwardScan())
			if err != nil {
				return nil, err
			}
			Expect(count).To(Equal(5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CountRecords errors on a scan-limit truncation, not a partial count (RFC-106a)", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 10; i++ {
				if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}); err != nil {
					return nil, err
				}
			}

			// A ScannedRecordsLimit below the row count (paginate mode) stops the
			// leaf scan OUT-OF-BAND. GetCount can't paginate, so CountRecords must
			// error rather than return a silently-truncated partial count (codex).
			scan := ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithScannedRecordsLimit(5)
			_, cErr := store.CountRecords(ctx, nil, nil, EndpointTypeTreeStart, EndpointTypeTreeEnd, nil, scan)
			var sle *ScanLimitReachedError
			Expect(errors.As(cErr, &sle)).To(BeTrue(),
				"CountRecords past the scan limit must error, not return a partial count; got: %v", cErr)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
