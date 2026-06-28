package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("VersionIndex", func() {
	// =========================================================================
	// 1. VersionKeyExpression.Evaluate tests (unit, no FDB needed)
	// =========================================================================
	Describe("VersionKeyExpression.Evaluate", func() {
		It("nil record returns [[nil]]", func() {
			expr := VersionKey()
			result, err := expr.Evaluate(nil, &gen.Order{OrderId: proto.Int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("nil version on record returns [[nil]]", func() {
			expr := VersionKey()
			record := &FDBStoredRecord[proto.Message]{
				PrimaryKey: tuple.Tuple{int64(1)},
				Record:     &gen.Order{OrderId: proto.Int64(1)},
				Version:    nil,
			}
			result, err := expr.Evaluate(record, record.Record)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("complete version returns versionstamp with correct bytes", func() {
			expr := VersionKey()

			globalBytes := make([]byte, GlobalVersionBytes)
			globalBytes[0] = 0xAB
			globalBytes[9] = 0xCD
			ver, err := NewCompleteVersion(globalBytes, 7)
			Expect(err).NotTo(HaveOccurred())

			record := &FDBStoredRecord[proto.Message]{
				PrimaryKey: tuple.Tuple{int64(1)},
				Record:     &gen.Order{OrderId: proto.Int64(1)},
				Version:    ver,
			}
			result, err := expr.Evaluate(record, record.Record)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(HaveLen(1))

			vs, ok := result[0][0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "expected tuple.Versionstamp")
			Expect(vs.UserVersion).To(Equal(uint16(7)))
			Expect(vs.TransactionVersion[0]).To(Equal(byte(0xAB)))
			Expect(vs.TransactionVersion[9]).To(Equal(byte(0xCD)))
		})

		It("incomplete version returns versionstamp with 0xFF transaction bytes", func() {
			expr := VersionKey()

			ver, err := IncompleteVersion(42)
			Expect(err).NotTo(HaveOccurred())

			record := &FDBStoredRecord[proto.Message]{
				PrimaryKey: tuple.Tuple{int64(1)},
				Record:     &gen.Order{OrderId: proto.Int64(1)},
				Version:    ver,
			}
			result, err := expr.Evaluate(record, record.Record)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(HaveLen(1))

			vs, ok := result[0][0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "expected tuple.Versionstamp")
			Expect(vs.UserVersion).To(Equal(uint16(42)))
			for i := 0; i < 10; i++ {
				Expect(vs.TransactionVersion[i]).To(Equal(byte(0xFF)),
					"byte %d should be 0xFF for incomplete version", i)
			}
		})
	})

	// =========================================================================
	// 2. VersionKeyExpression serialization tests
	// =========================================================================
	Describe("VersionKeyExpression serialization", func() {
		It("ToKeyExpression produces Version proto", func() {
			expr := VersionKey()
			proto := expr.ToKeyExpression()
			Expect(proto).NotTo(BeNil())
			Expect(proto.Version).NotTo(BeNil())
			// All other fields should be nil
			Expect(proto.Field).To(BeNil())
			Expect(proto.Then).To(BeNil())
			Expect(proto.Nesting).To(BeNil())
		})

		It("KeyExpressionFromProto round-trip", func() {
			original := VersionKey()
			serialized := original.ToKeyExpression()
			restored, err := KeyExpressionFromProto(serialized)
			Expect(err).NotTo(HaveOccurred())
			_, ok := restored.(*VersionKeyExpression)
			Expect(ok).To(BeTrue(), "round-tripped expression should be VersionKeyExpression")
		})

		It("ColumnSize == 1", func() {
			Expect(VersionKey().ColumnSize()).To(Equal(1))
		})

		It("ColumnSize of Concat(VersionKey, Field) == 2", func() {
			expr := Concat(VersionKey(), Field("order_id"))
			Expect(expr.ColumnSize()).To(Equal(2))
		})

		It("createsDuplicates == false", func() {
			Expect(createsDuplicates(VersionKey())).To(BeFalse())
		})

		It("keyExpressionEquals matches two VersionKeyExpressions", func() {
			Expect(keyExpressionEquals(VersionKey(), VersionKey())).To(BeTrue())
		})

		It("keyExpressionEquals does not match VersionKey vs Field", func() {
			Expect(keyExpressionEquals(VersionKey(), Field("x"))).To(BeFalse())
		})
	})

	// =========================================================================
	// 3. tupleHasIncompleteVersionstamp tests
	// =========================================================================
	Describe("tupleHasIncompleteVersionstamp", func() {
		It("tuple with no versionstamp returns false", func() {
			t := tuple.Tuple{int64(1), "hello", int64(42)}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeFalse())
		})

		It("empty tuple returns false", func() {
			Expect(tupleHasIncompleteVersionstamp(tuple.Tuple{})).To(BeFalse())
		})

		It("tuple with complete versionstamp returns false", func() {
			vs := tuple.Versionstamp{UserVersion: 5}
			vs.TransactionVersion[0] = 0x01 // Not all 0xFF
			t := tuple.Tuple{vs}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeFalse())
		})

		It("tuple with incomplete versionstamp returns true", func() {
			var vs tuple.Versionstamp
			for i := range vs.TransactionVersion {
				vs.TransactionVersion[i] = 0xFF
			}
			vs.UserVersion = 3
			t := tuple.Tuple{vs}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeTrue())
		})

		It("mixed tuple returns true if any element is incomplete versionstamp", func() {
			var incomplete tuple.Versionstamp
			for i := range incomplete.TransactionVersion {
				incomplete.TransactionVersion[i] = 0xFF
			}
			t := tuple.Tuple{int64(1), incomplete, "data"}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeTrue())
		})

		It("mixed tuple with only complete versionstamp returns false", func() {
			var complete tuple.Versionstamp
			complete.TransactionVersion[0] = 0x01
			t := tuple.Tuple{int64(1), complete, "data"}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeFalse())
		})

		It("all-zero TransactionVersion is complete (not incomplete)", func() {
			var vs tuple.Versionstamp // all zero
			t := tuple.Tuple{vs}
			Expect(tupleHasIncompleteVersionstamp(t)).To(BeFalse())
		})
	})

	// =========================================================================
	// 4. versionIndexMaintainer integration tests (need FDB testcontainer)
	// =========================================================================
	Describe("versionIndexMaintainer integration", func() {
		ctx := context.Background()

		buildMetaWithVersionIndex := func(indexes ...*Index) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			for _, idx := range indexes {
				builder.AddIndex("Order", idx)
			}
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}

		scanIndexEntries := func(store *FDBRecordStore, index *Index) []fdb.KeyValue {
			idxSubspace := store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
			begin, end := idxSubspace.FDBRangeKeys()
			kvs, err := store.context.Transaction().GetRange(
				fdb.KeyRange{Begin: begin, End: end},
				fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			return kvs
		}

		It("save creates VERSION index entry with versionstamp in key", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Transaction 1: save a record (uses versionstamped key mutation)
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 2: verify the index entry exists and has a versionstamp
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				kvs := scanIndexEntries(store, versionIndex)
				Expect(kvs).To(HaveLen(1))

				// Unpack the entry key
				idxSubspace := store.subspace.Sub(IndexKey, versionIndex.SubspaceTupleKey())
				entryTuple, err := idxSubspace.Unpack(kvs[0].Key)
				Expect(err).NotTo(HaveOccurred())
				// Should be (versionstamp, primaryKey)
				Expect(entryTuple).To(HaveLen(2))

				// First element should be a Versionstamp
				vs, ok := entryTuple[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue(), "first element should be Versionstamp, got %T", entryTuple[0])
				// It should be complete (not all 0xFF)
				allFF := true
				for _, b := range vs.TransactionVersion {
					if b != 0xFF {
						allFF = false
						break
					}
				}
				Expect(allFF).To(BeFalse(), "versionstamp should be complete after commit")

				// Second element should be the primary key
				Expect(entryTuple[1]).To(Equal(int64(1)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("version index entry versionstamp matches record version", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the versionstamp in the index matches the committed versionstamp
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}

				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				indexVS, ok := entries[0].IndexValues()[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue())

				// The global version from committed versionstamp should match
				for i := 0; i < GlobalVersionBytes; i++ {
					Expect(indexVS.TransactionVersion[i]).To(Equal(vs[i]),
						"TransactionVersion byte %d mismatch", i)
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("delete removes VERSION index entry", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Transaction 1: save a record
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry exists
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				kvs := scanIndexEntries(store, versionIndex)
				Expect(kvs).To(HaveLen(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 2: delete the record
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				if err != nil {
					return nil, err
				}
				Expect(deleted).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index entry is gone
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				kvs := scanIndexEntries(store, versionIndex)
				Expect(kvs).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("update removes old version entry and adds new one", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Transaction 1: save a record
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Capture the original version from the index
			var origVS tuple.Versionstamp
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1))
				vs, ok := entries[0].IndexValues()[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue())
				origVS = vs
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 2: update the record (same PK, different price)
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify: still exactly 1 index entry, with a new versionstamp
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1), "should have exactly 1 index entry after update")

				newVS, ok := entries[0].IndexValues()[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue())

				// The new versionstamp must differ from the original
				Expect(newVS).NotTo(Equal(origVS), "versionstamp should change after update")

				// PK should still be the same record
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("unique VERSION index rejected at Build time", func() {
			uniqueVersionIndex := NewVersionIndex("Order$version_unique", VersionKey()).SetUnique()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			builder.AddIndex("Order", uniqueVersionIndex)
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("does not support unique"))
		})

		It("multiple records get separate version index entries", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify 3 entries exist
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scan returns entries in version order (forward)", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Save record 1 in tx1
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Save record 2 in tx2 (later version)
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Forward scan should return record 1 first (earlier version)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// PK of first entry should be record 1 (earlier version)
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				// PK of second entry should be record 2 (later version)
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))

				// Versionstamps should be ordered
				vs1, ok1 := entries[0].IndexValues()[0].(tuple.Versionstamp)
				vs2, ok2 := entries[1].IndexValues()[0].(tuple.Versionstamp)
				Expect(ok1).To(BeTrue())
				Expect(ok2).To(BeTrue())
				// vs1 should be less than vs2
				Expect(vsLess(vs1, vs2)).To(BeTrue(), "first entry version should be less than second")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan returns entries in reverse version order", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Save record 1 in tx1
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Save record 2 in tx2
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Reverse scan should return record 2 first (later version)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ReverseScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// Reverse: record 2 (later version) first
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("composite VERSION index with Concat(VersionKey, Field)", func() {
			versionIndex := NewVersionIndex("Order$ver_price", Concat(VersionKey(), Field("price")))
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(999)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				// IndexValues: (versionstamp, price)
				indexVals := entries[0].IndexValues()
				Expect(indexVals).To(HaveLen(2))

				_, ok := indexVals[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue(), "first index value should be Versionstamp")
				Expect(indexVals[1]).To(Equal(int64(999)))

				// PK
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(42)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("DeleteAllRecords clears VERSION index", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Save multiple records
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// DeleteAllRecords
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.DeleteAllRecords()).To(Succeed())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index is empty
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				kvs := scanIndexEntries(store, versionIndex)
				Expect(kvs).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("save and delete in same transaction cleans up index", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Transaction 1: save two records
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 2: save a new record and delete record 1 in the same transaction
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				// Save a new record (3)
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
				if err != nil {
					return nil, err
				}
				// Delete record 1
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				if err != nil {
					return nil, err
				}
				Expect(deleted).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify: only records 2 and 3 remain in the index
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(2), "should have exactly 2 index entries (records 2 and 3)")

				// Collect primary keys
				pks := make(map[int64]bool)
				for _, entry := range entries {
					pk := entry.PrimaryKey()
					Expect(pk).To(HaveLen(1))
					pks[pk[0].(int64)] = true
				}
				Expect(pks).To(HaveKey(int64(2)))
				Expect(pks).To(HaveKey(int64(3)))
				Expect(pks).NotTo(HaveKey(int64(1)), "deleted record should not be in the index")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("ScanIndex with row limit on VERSION index", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			// Save 3 records in separate transactions for distinct versions
			for i := int64(1); i <= 3; i++ {
				ii := i
				if ii == 1 {
					_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
						if err != nil {
							return nil, err
						}
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(ii), Price: proto.Int32(int32(ii * 100))})
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
				} else {
					_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
						if err != nil {
							return nil, err
						}
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(ii), Price: proto.Int32(int32(ii * 100))})
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
				}
			}

			// Scan with limit=2
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				scan := ForwardScan()
				scan.ExecuteProperties.ReturnedRowLimit = 2
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, scan))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("records saved in same transaction have ascending local versions", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex(versionIndex)

			ks := specSubspace()

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify: forward scan returns them in local version order (same global version)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				// All entries have same global version but ascending local versions
				// -> ascending order by PK (1, 2, 3) since local versions are 0, 1, 2
				for i, entry := range entries {
					vs, ok := entry.IndexValues()[0].(tuple.Versionstamp)
					Expect(ok).To(BeTrue())
					Expect(vs.UserVersion).To(Equal(uint16(i)),
						"entry %d should have local version %d", i, i)
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{int64(i + 1)}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// 5. Scan populates version on scanned records (Fix 1)
	// =========================================================================
	Describe("scan populates version", func() {
		ctx := context.Background()

		buildMetaWithVersionIndex2 := func(indexes ...*Index) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			for _, idx := range indexes {
				builder.AddIndex("Order", idx)
			}
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}

		It("forward scan populates version on scanned records", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex2(versionIndex)
			ks := specSubspace()

			// Tx1: save 3 records with versioning
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: forward ScanRecords, verify each record has a non-nil complete Version
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(records).To(HaveLen(3))
				for i, rec := range records {
					Expect(rec.Version).NotTo(BeNil(), "record %d should have non-nil Version", i)
					Expect(rec.Version.IsComplete()).To(BeTrue(), "record %d version should be complete", i)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan populates version on scanned records", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex2(versionIndex)
			ks := specSubspace()

			// Tx1: save 3 records
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: reverse ScanRecords, verify all records have non-nil complete Version
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				records, err := AsList(ctx, store.ScanRecords(nil, ReverseScan()))
				if err != nil {
					return nil, err
				}
				Expect(records).To(HaveLen(3))
				for i, rec := range records {
					Expect(rec.Version).NotTo(BeNil(), "record %d should have non-nil Version", i)
					Expect(rec.Version.IsComplete()).To(BeTrue(), "record %d version should be complete", i)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("same-transaction scan returns incomplete version", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())
			metaData := buildMetaWithVersionIndex2(versionIndex)
			ks := specSubspace()

			// Single RunWithVersionstamp: save a record then ScanRecords in same tx
			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}

				// Scan in the same transaction
				records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(records).To(HaveLen(1))
				Expect(records[0].Version).NotTo(BeNil(), "in-flight record should have non-nil Version")
				Expect(records[0].Version.IsComplete()).To(BeFalse(), "in-flight record version should be incomplete")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// 6. RebuildIndex with VERSION index (exercises Fix 1)
	// =========================================================================
	Describe("RebuildIndex with VERSION index", func() {
		ctx := context.Background()

		It("RebuildIndex correctly builds VERSION index entries", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			builder.AddIndex("Order", versionIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()

			// Tx1: save 3 records
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: rebuild the VERSION index
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.RebuildIndex(versionIndex)
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx3: verify ScanIndex returns 3 entries with valid versionstamps
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(3))

				for i, entry := range entries {
					vs, ok := entry.IndexValues()[0].(tuple.Versionstamp)
					Expect(ok).To(BeTrue(), "entry %d should have Versionstamp", i)
					// Versionstamp should be complete (not all 0xFF)
					allFF := true
					for _, b := range vs.TransactionVersion {
						if b != 0xFF {
							allFF = false
							break
						}
					}
					Expect(allFF).To(BeFalse(), "entry %d versionstamp should be complete after rebuild", i)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// 7. Metadata validation for VERSION indexes (Fix 3)
	// =========================================================================
	Describe("VERSION index metadata validation", func() {
		It("VERSION index without StoreRecordVersions fails at Build", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			// Deliberately NOT calling SetStoreRecordVersions(true)
			builder.AddIndex("Order", NewVersionIndex("Order$version", VersionKey()))
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("requires SetStoreRecordVersions"))
		})

		It("VERSION index with grouping expression fails at Build", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			// Use a GroupingKeyExpression as the root
			groupedExpr := Ungrouped(VersionKey())
			builder.AddIndex("Order", NewVersionIndex("Order$version_grouped", groupedExpr))
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("does not support grouping"))
		})

		It("VERSION index with no version column fails at Build", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			// Use Field("order_id") as root — no VersionKeyExpression
			builder.AddIndex("Order", NewVersionIndex("Order$no_version", Field("order_id")))
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("exactly 1 version entry"))
		})

		It("VERSION index with composite expression including version passes validation", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			// Concat(VersionKey(), Field("price")) — has exactly 1 version column, should pass
			builder.AddIndex("Order", NewVersionIndex("Order$ver_price", Concat(VersionKey(), Field("price"))))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md).NotTo(BeNil())
		})
	})

	// =========================================================================
	// 8. DeleteAllRecords with VERSION index (exercises Fix 2)
	// =========================================================================
	Describe("DeleteAllRecords with VERSION index cleanup", func() {
		ctx := context.Background()

		It("DeleteAllRecords cleans up VERSION index entries", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			builder.AddIndex("Order", versionIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()

			// Tx1: save 3 records with VERSION index
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: delete all records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.DeleteAllRecords()
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx3: verify ScanIndex returns 0 entries
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(0), "VERSION index should be empty after DeleteAllRecords")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("save then DeleteAllRecords in same transaction leaves no orphaned version mutations", func() {
			versionIndex := NewVersionIndex("Order$version", VersionKey())

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetStoreRecordVersions(true)
			builder.AddIndex("Order", versionIndex)
			metaData, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()

			// Single RunWithVersionstamp tx: save 2 records then DeleteAllRecords
			_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Save 2 records (queues SET_VERSIONSTAMPED_KEY mutations)
				for i := int64(1); i <= 2; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					if err != nil {
						return nil, err
					}
				}
				// Now delete everything
				return nil, store.DeleteAllRecords()
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: verify ScanIndex returns 0 entries and the index subspace is empty
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(versionIndex, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(0), "no index entries should remain after save+DeleteAllRecords")

				// Also verify the raw index subspace is empty
				idxSubspace := store.subspace.Sub(IndexKey, versionIndex.SubspaceTupleKey())
				begin, end := idxSubspace.FDBRangeKeys()
				kvs, err := store.context.Transaction().GetRange(
					fdb.KeyRange{Begin: begin, End: end},
					fdb.RangeOptions{},
				).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				Expect(kvs).To(BeEmpty(), "raw index subspace should be empty")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// vsLess returns true if a < b in versionstamp ordering.
func vsLess(a, b tuple.Versionstamp) bool {
	for i := 0; i < len(a.TransactionVersion); i++ {
		if a.TransactionVersion[i] < b.TransactionVersion[i] {
			return true
		}
		if a.TransactionVersion[i] > b.TransactionVersion[i] {
			return false
		}
	}
	return a.UserVersion < b.UserVersion
}
