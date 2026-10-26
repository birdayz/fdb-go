package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RecordCounting", func() {
	ctx := context.Background()

	It("groups maintained counts by the complete version on insert and delete", func() {
		for _, format := range []int32{0, 5} {
			builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true).SetRecordCountKey(VersionKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			version, err := CompleteVersionFromBytes([]byte{0, 0, 0, 0, 0, 0, 0, 2, 0, 9, 0, 7})
			Expect(err).NotTo(HaveOccurred())
			stamp, err := version.ToVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			ks := specSubspace()
			run := func(fn func(*FDBRecordStore)) {
				GinkgoHelper()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					b := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
					if format != 0 {
						b.SetFormatVersion(format)
					}
					store, err := b.CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					fn(store)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			assertCount := func(store *FDBRecordStore, want int64) {
				GinkgoHelper()
				count, err := store.GetSnapshotRecordCount(tuple.Tuple{stamp})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(want))
				count, err = store.GetSnapshotRecordCount(tuple.Tuple{nil})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(BeZero(), "a complete version must not use the null group")
			}
			run(func(store *FDBRecordStore) {
				_, err := store.SaveRecordWithVersion(&demopb.Order{OrderId: proto.Int64(1)},
					RecordExistenceCheckNone, version, VersionstampSaveBehaviorIfPresent)
				Expect(err).NotTo(HaveOccurred())
				assertCount(store, 1)
			})
			run(func(store *FDBRecordStore) {
				assertCount(store, 1)
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				assertCount(store, 0)
			})
			run(func(store *FDBRecordStore) { assertCount(store, 0) })
		}
	})

	for _, api := range []string{"ordinary", "batch", "pipelined", "supplied incomplete"} {
		It("returns an incomplete-version error and aborts the "+api+" save with a version count key", func() {
			builder := baseBuilder().SetStoreRecordVersions(true).SetRecordCountKey(VersionKey())
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				record := &demopb.Order{OrderId: proto.Int64(1)}
				switch api {
				case "batch":
					return store.SaveRecordBatch([]proto.Message{record})
				case "pipelined":
					return store.SaveRecordsPipelined([]proto.Message{record}, RecordExistenceCheckNone)
				case "supplied incomplete":
					version, err := IncompleteVersion(rtx.ClaimLocalVersion())
					if err != nil {
						return nil, err
					}
					return store.SaveRecordWithVersion(record, RecordExistenceCheckNone, version, VersionstampSaveBehaviorIfPresent)
				default:
					return store.SaveRecord(record)
				}
			})
			var incomplete *IncompleteVersionstampError
			Expect(errors.As(err, &incomplete)).To(BeTrue(), "%v", err)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				for _, space := range []int64{RecordKey, RecordCountKey, RecordVersionKey} {
					rows, err := rtx.Transaction().GetRange(ks.Sub(space), fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					Expect(rows).To(BeEmpty(), "the failed transaction must not persist subspace %d", space)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("BasicCounting", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
			order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Update the same record (overwrite)
			order2 := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
			order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
			order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}
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

	// Java's getSnapshotRecordCountForRecordType (FDBRecordStore.java:
	// 2431-2453) answers from a COUNT index on the type, else from a universal
	// COUNT index grouped by record type, and never from the record count key.
	It("per-type counts come from a COUNT index", func() {
		build := func(configure func(*RecordMetaDataBuilder)) *RecordMetaData {
			b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			configure(b)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}
		// Each mode's store is its own: specSubspace is one per spec.
		stores := 0
		count := func(md *RecordMetaData) (int64, error) {
			stores++
			sub := specSubspace().Sub(int64(stores))
			var n int64
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(i)}); err != nil {
						return nil, err
					}
				}
				if _, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(9)}); err != nil {
					return nil, err
				}
				n, err = store.GetSnapshotRecordCountForRecordType("Order")
				return nil, err
			})
			return n, err
		}

		// A record count key grouped by record type is not read.
		_, err := count(build(func(b *RecordMetaDataBuilder) { b.SetRecordCountKey(RecordTypeKey()) }))
		var rc *RecordCoreError
		Expect(errors.As(err, &rc)).To(BeTrue(), "%v", err)
		Expect(rc.Message).To(Equal("Require a COUNT index on Order"))

		n, err := count(build(func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewCountIndex("order_count", GroupAll(EmptyKey())))
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(3)))

		n, err = count(build(func(b *RecordMetaDataBuilder) {
			b.AddUniversalIndex(NewCountIndex("count_by_type", GroupAll(RecordTypeKey())))
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(3)))
	})

	It("PerTypeCountingWithRecordTypeKey", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			for i := int64(101); i <= 102; i++ {
				customer := &demopb.Customer{CustomerId: proto.Int64(i), Name: proto.String("Test")}
				if _, err := store.SaveRecord(customer); err != nil {
					return nil, err
				}
			}

			// Check per-type counts in the record count key's groups, read at
			// each type's integer key (Java's getSnapshotRecordCount(key, value)).
			orderCount, err := countKeyGroupForType(store, "Order")
			if err != nil {
				return nil, err
			}
			Expect(orderCount).To(Equal(int64(3)))

			customerCount, err := countKeyGroupForType(store, "Customer")
			if err != nil {
				return nil, err
			}
			Expect(customerCount).To(Equal(int64(2)))

			// Delete one order
			if _, err := store.DeleteRecord(tuple.Tuple{int64(2)}); err != nil {
				return nil, err
			}

			orderCount, err = countKeyGroupForType(store, "Order")
			if err != nil {
				return nil, err
			}
			Expect(orderCount).To(Equal(int64(2)))

			// Customer count should be unchanged
			customerCount, err = countKeyGroupForType(store, "Customer")
			if err != nil {
				return nil, err
			}
			Expect(customerCount).To(Equal(int64(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertDeleteInsertCount", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
			order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Delete
			if _, err := store.DeleteRecord(tuple.Tuple{int64(1)}); err != nil {
				return nil, err
			}

			// Re-insert (same key)
			order2 := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
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
			order4 := &demopb.Order{OrderId: proto.Int64(4), Price: proto.Int32(40)}
			_, err = store.SaveRecord(order4)
			Expect(err).NotTo(HaveOccurred())

			// Deletes should work too
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			// A DISABLED count state does not fail the read: Java falls through to
			// the index path (FDBRecordStore.java:2320-2322). This store has no
			// universal COUNT index, so the failure that surfaces is index
			// selection's, not the count state's.
			_, err = store.GetRecordCount()
			Expect(err).To(HaveOccurred())
			Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
				"a non-READABLE count state must fall through to the index path, not fail on the state: %v", err)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DisabledStateIsTerminal", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}

			// Transition to WRITE_ONLY
			Expect(store.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)).To(Succeed())

			// Saves should still maintain the count
			order3 := &demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)}
			_, err = store.SaveRecord(order3)
			Expect(err).NotTo(HaveOccurred())

			// WRITE_ONLY counters are not read, but Java does not fail on that —
			// it falls through to the index path (FDBRecordStore.java:2320-2322),
			// and this store has no universal COUNT index to answer from.
			_, err = store.GetRecordCount()
			Expect(err).To(HaveOccurred())
			Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
				"a WRITE_ONLY count state must fall through to the index path, not fail on the state: %v", err)

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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
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
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// Java's countRecords charges scan limits only through a split store's unsplitter.
		builder.SetSplitLongRecords(true)
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
				if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}); err != nil {
					return nil, err
				}
			}

			// A ScannedRecordsLimit below the row count (paginate mode) stops the
			// leaf scan OUT-OF-BAND. GetCount can't paginate, so CountRecords must
			// error rather than return a silently-truncated partial count.
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

// countKeyGroupForType reads a record type's group of a record count key
// grouped by record type, at the type's integer key: Java's
// getSnapshotRecordCount(recordType(), Key.Evaluated.scalar(typeKey)). Java's
// getSnapshotRecordCountForRecordType does not read the count key; it needs a
// COUNT index (pinned by "per-type counts come from a COUNT index").
func countKeyGroupForType(store *FDBRecordStore, recordTypeName string) (int64, error) {
	rt := store.metaData.GetRecordType(recordTypeName)
	if rt == nil {
		return 0, fmt.Errorf("unknown record type %s", recordTypeName)
	}
	return store.GetSnapshotRecordCount(tuple.Tuple{rt.GetRecordTypeKey()})
}
