package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("StoreBugVerify", func() {
	// Bug 1: ErrorIfTypeChanged must propagate deserialization errors, not swallow them.
	// Before fix: `if deserErr == nil` silently swallowed errors, so corrupted records
	// would pass the type check instead of erroring.
	Describe("ErrorIfTypeChanged deserialization propagation", func() {
		It("detects type change via deserialized old record", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save an Order first.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Try to save a Customer with same PK using ErrorIfTypeChanged.
			// This must deserialize the old record and detect the type mismatch.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordWithOptions(
					&gen.Customer{CustomerId: intPtr(1), Name: strPtr("Alice")},
					RecordExistenceCheckErrorIfTypeChanged,
				)
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var typeChanged *RecordTypeChangedError
			Expect(errors.As(err, &typeChanged)).To(BeTrue())
			Expect(typeChanged.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
			Expect(typeChanged.ActualType).To(Equal("Order"))
			Expect(typeChanged.ExpectedType).To(Equal("Customer"))
		})
	})

	// Bug 2: DeleteRecord must deserialize BEFORE deleteSplit.
	// Before fix: delete happened before deserialization — if deser of deleted data
	// failed, the record was already gone (data loss). Now: deser first, error out
	// if corrupt, only then delete.
	Describe("DeleteRecord deserialize-before-delete ordering", func() {
		It("successfully deletes and decrements count", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetRecordCountKey(EmptyKey())
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save a record.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete — should succeed and decrement record count.
			var deleted bool
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				deleted, err = store.DeleteRecord(tuple.Tuple{int64(1)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify count is 0 after delete.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				count, cErr := store.GetRecordCount()
				if cErr != nil {
					return nil, cErr
				}
				Expect(count).To(Equal(int64(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("delete with index also updates indexes correctly", func() {
			ctx := context.Background()
			ss := specSubspace()

			priceIdx := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIdx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save record.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(99)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete record — index should be cleaned up.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan index — should be empty.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan())
				entries, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(entries).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 3: FDB row limit must be doubled when store record versions enabled.
	// Each record = 2 KV pairs (data + version). Without doubling, scan returns
	// half the expected records.
	Describe("FDB row limit with record versions", func() {
		It("returns correct number of records when versioning enabled", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save 5 orders.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					id := i // avoid closure capture
					_, err := store.SaveRecord(&gen.Order{OrderId: &id, Price: int32Ptr(int32(i * 10))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan with limit=3 — must return exactly 3 records despite version KVs.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				scanProps := ForwardScan()
				scanProps.ExecuteProperties = scanProps.ExecuteProperties.WithReturnedRowLimit(3)
				cursor := store.ScanRecords(nil, scanProps)
				records, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(records).To(HaveLen(3), "should return exactly 3 records with versioning enabled")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 4: Exclusive low endpoint must use Strinc, not append(0x00).
	// With append(0x00), the boundary record's suffixed keys (e.g. pk\x00\x00 for data)
	// are still >= pk\x00, so the boundary record is wrongly included.
	Describe("Exclusive low endpoint uses Strinc", func() {
		It("excludes the boundary record on forward scan", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 orders with IDs 1,2,3.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				for _, id := range []int64{1, 2, 3} {
					oid := id // avoid closure capture
					_, err := store.SaveRecord(&gen.Order{OrderId: &oid, Price: int32Ptr(int32(id * 10))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan with exclusive low=1 — should return only records 2 and 3.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanRecordsInRange(
					tuple.Tuple{int64(1)}, nil,
					EndpointTypeRangeExclusive, EndpointTypeTreeEnd,
					nil, ForwardScan(),
				)
				records, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(records).To(HaveLen(2), "exclusive low should exclude the boundary record")
				// Verify the returned records are 2 and 3, not 1.
				for _, rec := range records {
					order := rec.Record.(*gen.Order)
					Expect(order.GetOrderId()).To(BeNumerically(">=", 2))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func strPtr(s string) *string { return &s }
