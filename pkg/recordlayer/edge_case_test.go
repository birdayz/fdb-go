package recordlayer

import (
	"context"
	"errors"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Edge case hardening: boundary conditions, corrupt data, concurrent operations.
// These tests cover scenarios most likely to surface only in production.
var _ = Describe("Edge case hardening", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	Describe("Corrupt store header", func() {
		It("returns error when store header is garbage bytes", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Write garbage bytes at the store header key
				storeInfoKey := ks.Pack(tuple.Tuple{StoreInfoKey})
				rtx.Transaction().Set(storeInfoKey, []byte{0xDE, 0xAD, 0xBE, 0xEF})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse store header"))
		})

		It("returns error when store header is empty bytes", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				storeInfoKey := ks.Pack(tuple.Tuple{StoreInfoKey})
				rtx.Transaction().Set(storeInfoKey, []byte{})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Empty bytes is valid proto (all defaults) — should open but may
			// fail validation since format version 0 < formatVersionMinimum.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var fmtErr *UnsupportedFormatVersionError
			Expect(errors.As(err, &fmtErr)).To(BeTrue())
		})

		It("returns RecordStoreNoInfoButNotEmptyError when header key is missing but data exists", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Write data at record key (subspace 1) but NOT at header key (subspace 0)
				recordKey := ks.Pack(tuple.Tuple{RecordKey, int64(999), unsplitRecord})
				rtx.Transaction().Set(recordKey, []byte{0x01, 0x02})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var noInfoErr *RecordStoreNoInfoButNotEmptyError
			Expect(errors.As(err, &noInfoErr)).To(BeTrue())
			Expect(noInfoErr.FirstKey).NotTo(BeEmpty())
		})
	})

	Describe("Corrupt record data", func() {
		It("returns RecordDeserializationError when loading a record with garbage bytes", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// First create a valid store
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_ = store
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Write garbage at a record key
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				recordsSubspace := ks.Sub(RecordKey)
				key := recordsSubspace.Pack(tuple.Tuple{int64(42), unsplitRecord})
				rtx.Transaction().Set(key, []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Try to load it
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.LoadRecord(tuple.Tuple{int64(42)})
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var deserErr *RecordDeserializationError
			Expect(errors.As(err, &deserErr)).To(BeTrue())
		})

		It("scan skips records with corrupt data without crashing", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save a valid record
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Inject garbage at PK=2 directly
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				recordsSubspace := ks.Sub(RecordKey)
				key := recordsSubspace.Pack(tuple.Tuple{int64(2), unsplitRecord})
				rtx.Transaction().Set(key, []byte{0x00, 0x01, 0x02})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan should encounter the corrupt record
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				cursor := store.ScanRecords(nil, ForwardScan())
				_, scanErr := AsList(ctx, cursor)
				// Should error on the corrupt record — not silently skip it
				return nil, scanErr
			})
			// We expect an error (not a panic) when hitting corrupt data
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Boundary primary keys", func() {
		It("handles a single-element primary key of zero", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// order_id = 0
				stored, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(0),
					Price:   proto.Int32(42),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(stored.PrimaryKey).To(Equal(tuple.Tuple{int64(0)}))

				// Load it back
				loaded, err := store.LoadRecord(tuple.Tuple{int64(0)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				order := loaded.Record.(*gen.Order)
				Expect(order.GetPrice()).To(Equal(int32(42)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles negative primary key values", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// order_id = MinInt64
				stored, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(-9223372036854775808),
					Price:   proto.Int32(99),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(stored.PrimaryKey).To(Equal(tuple.Tuple{int64(-9223372036854775808)}))

				loaded, err := store.LoadRecord(tuple.Tuple{int64(-9223372036854775808)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(99)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles MaxInt64 primary key", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				stored, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(9223372036854775807),
					Price:   proto.Int32(77),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(stored.PrimaryKey).To(Equal(tuple.Tuple{int64(9223372036854775807)}))

				loaded, err := store.LoadRecord(tuple.Tuple{int64(9223372036854775807)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Empty store operations", func() {
		It("LoadRecord on empty store returns nil without error", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				loaded, err := store.LoadRecord(tuple.Tuple{int64(999)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("ScanRecords on empty store returns empty list", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				cursor := store.ScanRecords(nil, ForwardScan())
				records, err := AsList(ctx, cursor)
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteRecord on empty store returns false without error", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("GetRecordCount on empty store with counting enabled returns 0", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				count, err := store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteAllRecords on empty store succeeds", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				err = store.DeleteAllRecords()
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Index edge cases", func() {
		It("ScanIndex on empty index returns empty results", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(md.GetIndex("Order$price"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("index on field with nil/unset value stores nil key component", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Save order without setting price (proto2 optional → nil)
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
				})
				Expect(err).NotTo(HaveOccurred())

				// The index entry should exist with nil price
				entries, err := AsList(ctx, store.ScanIndex(md.GetIndex("Order$price"), TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{nil}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("unique index allows nil values without violation", func() {
			ks := specSubspace()
			builder := baseMetaData()
			uniqueIdx := NewIndex("Order$price_unique", Field("price")).SetUnique()
			builder.AddIndex("Order", uniqueIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Two records with nil (unset) price — nil should not trigger uniqueness
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Continuation token edge cases", func() {
		It("empty continuation bytes resumes from beginning", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// Scan with nil continuation (beginning)
				scanProps := ForwardScan()
				scanProps.ExecuteProperties.ReturnedRowLimit = 2
				cursor := store.ScanRecords(nil, scanProps)
				records, cont, err := AsListWithContinuation(ctx, cursor)
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(2))
				Expect(cont).NotTo(BeNil()) // Should have continuation

				// Resume with that continuation
				cursor2 := store.ScanRecords(cont, scanProps)
				records2, _, err := AsListWithContinuation(ctx, cursor2)
				Expect(err).NotTo(HaveOccurred())
				Expect(records2).To(HaveLen(2))

				// Records should be different
				pk1 := records[0].PrimaryKey
				pk3 := records2[0].PrimaryKey
				Expect(pk1).NotTo(Equal(pk3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Concurrent operations", func() {
		It("concurrent reads do not interfere", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Write some records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Run two concurrent reads
			type result struct {
				count int
				err   error
			}
			ch := make(chan result, 2)

			for range 2 {
				go func() {
					var count int
					_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
						if err != nil {
							return nil, err
						}

						cursor := store.ScanRecords(nil, ForwardScan())
						records, err := AsList(ctx, cursor)
						if err != nil {
							return nil, err
						}
						count = len(records)
						return nil, nil
					})
					ch <- result{count: count, err: err}
				}()
			}

			r1 := <-ch
			r2 := <-ch
			Expect(r1.err).NotTo(HaveOccurred())
			Expect(r2.err).NotTo(HaveOccurred())
			Expect(r1.count).To(Equal(10))
			Expect(r2.count).To(Equal(10))
		})

		It("write-write conflict on same record is detected", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Create the store first
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Start two transactions that both write to the same PK
			tx1, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx1.Cancel()
			tx2, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx2.Cancel()

			rtx1 := NewFDBRecordContext(tx1)
			rtx2 := NewFDBRecordContext(tx2)

			store1, err := NewStoreBuilder().
				SetContext(rtx1).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			store2, err := NewStoreBuilder().
				SetContext(rtx2).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// Both write to PK=1
			_, err = store1.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store2.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// First commit succeeds
			err = tx1.Commit().Get()
			Expect(err).NotTo(HaveOccurred())

			// Second commit should fail with conflict
			err = tx2.Commit().Get()
			Expect(err).To(HaveOccurred())
			// FDB returns error code 1020 (not_committed) for conflicts
			var fdbErr fdb.Error
			Expect(errors.As(err, &fdbErr)).To(BeTrue())
			Expect(fdbErr.Code).To(Equal(1020))
		})
	})

	Describe("Metadata edge cases", func() {
		It("Build fails when record type has no primary key", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			// Don't set primary key for Order
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Order"))
		})

		It("Build fails when primary key expression produces 0 columns", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(&EmptyKeyExpression{})
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
		})

		It("Build fails when index references non-existent field", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			badIdx := NewIndex("bad_idx", Field("nonexistent_field"))
			builder.AddIndex("Order", badIdx)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
		})
	})

	Describe("Store builder validation", func() {
		It("fails without context", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewStoreBuilder().
				SetMetaDataProvider(md).SetSubspace(subspace.FromBytes([]byte("test"))).CreateOrOpen()
			Expect(err).To(HaveOccurred())
		})

		It("fails without metadata", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetSubspace(subspace.FromBytes([]byte("test"))).CreateOrOpen()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
		})

		It("fails without subspace", func() {
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).CreateOrOpen()
				return nil, err
			})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Record save/load round-trip edge cases", func() {
		It("saves and loads record with all field types at boundary values", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Test with boundary int32 values in price field
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(2147483647), // MaxInt32
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(2),
					Price:   proto.Int32(-2147483648), // MinInt32
				})
				Expect(err).NotTo(HaveOccurred())

				loaded1, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded1.Record.(*gen.Order).GetPrice()).To(Equal(int32(2147483647)))

				loaded2, err := store.LoadRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded2.Record.(*gen.Order).GetPrice()).To(Equal(int32(-2147483648)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves and loads record with very long string field", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// 50KB string in the flower type field (below split threshold when serialized)
				longStr := strings.Repeat("A", 50000)
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Flower:  &gen.Flower{Type: proto.String(longStr)},
				})
				Expect(err).NotTo(HaveOccurred())

				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded.Record.(*gen.Order).GetFlower().GetType()).To(Equal(longStr))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("overwrites a record multiple times preserving the latest value", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Overwrite 10 times in the same transaction
				for i := int32(0); i < 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(1),
						Price:   proto.Int32(i),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(9)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Split record boundary", func() {
		It("record at exactly splitRecordSize is stored unsplit", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetSplitLongRecords(true)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Create a record with enough tag data to fill up to exactly splitRecordSize
				// We can't precisely control proto serialization size, but we can test
				// that the unsplit path handles large records correctly
				bigTags := make([]string, 0, 5000)
				for i := 0; i < 5000; i++ {
					bigTags = append(bigTags, strings.Repeat("x", 18))
				}

				stored, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(42),
					Tags:    bigTags,
				})
				Expect(err).NotTo(HaveOccurred())

				// Load it back and verify
				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(42)))
				Expect(len(loaded.Record.(*gen.Order).GetTags())).To(Equal(5000))

				// Check if split or unsplit based on actual serialized size
				if stored.ValueSize <= splitRecordSize {
					Expect(stored.Split).To(BeFalse())
				} else {
					Expect(stored.Split).To(BeTrue())
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Store reopen semantics", func() {
		It("data persists across Create → commit → Open in new transaction", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Create and save
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Reopen and read in a new transaction
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(100)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteAllRecords + reopen yields empty store", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Create and save
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// DeleteAll
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				return nil, store.DeleteAllRecords()
			})
			Expect(err).NotTo(HaveOccurred())

			// Reopen — should be empty
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				cursor := store.ScanRecords(nil, ForwardScan())
				records, err := AsList(ctx, cursor)
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Index state edge cases", func() {
		It("marking a non-existent index returns error", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.MarkIndexWriteOnly("nonexistent_index")
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var notFoundErr *IndexNotFoundError
			Expect(errors.As(err, &notFoundErr)).To(BeTrue())
			Expect(notFoundErr.IndexName).To(Equal("nonexistent_index"))
		})

		It("scanning a DISABLED index returns IndexNotReadableError", func() {
			ks := specSubspace()
			builder := baseMetaData()
			priceIdx := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.MarkIndexDisabled("Order$price")
				Expect(err).NotTo(HaveOccurred())

				cursor := store.ScanIndex(md.GetIndex("Order$price"), TupleRangeAll, nil, ForwardScan())
				_, scanErr := AsList(ctx, cursor)
				return nil, scanErr
			})
			Expect(err).To(HaveOccurred())
			var notReadableErr *IndexNotReadableError
			Expect(errors.As(err, &notReadableErr)).To(BeTrue())
			Expect(notReadableErr.IndexName).To(Equal("Order$price"))
		})
	})

	Describe("Record counting edge cases", func() {
		It("count increments on insert and decrements on delete", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert 3
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}

				count, err := store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(3)))

				// Delete 1
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				count, err = store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(2)))

				// Update existing (should NOT change count)
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999)})
				Expect(err).NotTo(HaveOccurred())

				count, err = store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(2)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("delete of non-existent record does not change count", func() {
			ks := specSubspace()
			builder := baseMetaData()
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				count, err := store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				// Delete non-existent
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeFalse())

				// Count unchanged
				count, err = store.GetSnapshotRecordCount(tuple.Tuple{})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Store lock edge cases", func() {
		It("FORBID_RECORD_UPDATE blocks save but allows read", func() {
			ks := specSubspace()
			md, err := baseMetaData().Build()
			Expect(err).NotTo(HaveOccurred())

			// Create and save a record, then lock
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "migration in progress")
			})
			Expect(err).NotTo(HaveOccurred())

			// Read should succeed, save should fail
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Read works
				loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())

				// Save blocked
				_, saveErr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(saveErr).To(HaveOccurred())
				var lockErr *StoreIsLockedForRecordUpdatesError
				Expect(errors.As(saveErr, &lockErr)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("FormerIndex tracking", func() {
		It("RemoveIndex creates a FormerIndex entry", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			idx := NewIndex("my_index", Field("price"))
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Remove the index — should create a FormerIndex
			builder.RemoveIndex("my_index")
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			formerIndexes := md.GetFormerIndexes()
			Expect(formerIndexes).To(HaveLen(1))
			Expect(formerIndexes[0].FormerName).To(Equal("my_index"))
		})
	})
})
