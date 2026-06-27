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

var _ = Describe("MaxEverVersionIndex", func() {
	ctx := context.Background()

	buildMeta := func(indexes ...*Index) *RecordMetaData {
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

	// scanRawIndexKVs reads raw FDB key-values from the index subspace (bypasses cursor).
	scanRawIndexKVs := func(store *FDBRecordStore, index *Index) []fdb.KeyValue {
		idxSubspace := store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
		rng, err := fdb.PrefixRange(idxSubspace.FDBKey())
		Expect(err).NotTo(HaveOccurred())
		kvs, err := store.context.Transaction().GetRange(rng, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}

	// =========================================================================
	// 1. Ungrouped: basic save tracks max version
	// =========================================================================
	It("ungrouped: basic save tracks max version", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Save a record (incomplete versionstamp → SET_VERSIONSTAMPED_VALUE)
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

		// Verify the index entry contains a committed versionstamp
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			kvs := scanRawIndexKVs(store, idx)
			Expect(kvs).To(HaveLen(1))

			// Ungrouped key = empty tuple, value = tuple-packed versionstamp
			idxSubspace := store.subspace.Sub(IndexKey, idx.SubspaceTupleKey())
			keyTuple, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(keyTuple).To(HaveLen(0), "ungrouped → empty key")

			valueTuple, err := tuple.Unpack(kvs[0].Value)
			Expect(err).NotTo(HaveOccurred())
			Expect(valueTuple).To(HaveLen(1))

			vs, ok := valueTuple[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "value should contain a Versionstamp, got %T", valueTuple[0])
			// Must be complete (not all 0xFF)
			allFF := true
			for _, b := range vs.TransactionVersion {
				if b != 0xFF {
					allFF = false
					break
				}
			}
			Expect(allFF).To(BeFalse(), "versionstamp should be complete after commit")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 2. Ungrouped: multiple saves in different transactions → max is the later one
	// =========================================================================
	It("ungrouped: multiple saves in different transactions keep max version", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Transaction 1: save first record
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: save second record (later transaction → higher version)
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// vs2 must be strictly greater than vs1 (BYTE_MAX keeps the larger one)
		Expect(vs2).NotTo(Equal(vs1), "second transaction should have different versionstamp")

		// Verify the index value is the later (larger) versionstamp
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0), "ungrouped → empty key")

			valueTuple := entries[0].Value
			Expect(valueTuple).To(HaveLen(1))

			vs, ok := valueTuple[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())

			// The stored version should match the second (later) transaction's versionstamp
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs.TransactionVersion[i]).To(Equal(vs2[i]),
					"TransactionVersion byte %d should match vs2", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 3. Ungrouped: delete is no-op (_EVER semantics)
	// =========================================================================
	It("ungrouped: delete does not revert max version (_EVER semantics)", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Save a record
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete the record — should NOT remove the index entry
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

		// Verify index entry still exists with the original version
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1), "index entry should survive deletion (_EVER)")

			vs, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())

			// Should match original committed version
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs.TransactionVersion[i]).To(Equal(vs1[i]),
					"versionstamp should still match the original after delete")
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 4. Ungrouped: update record keeps max version (later tx always wins)
	// =========================================================================
	It("ungrouped: update record advances max version to later transaction", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Transaction 1: save
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: update same record
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Max version should be from the later transaction
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			vs, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())

			// vs2 > vs1 because it's from a later transaction, and BYTE_MAX keeps max
			Expect(vs.TransactionVersion).NotTo(Equal(vs1), "version should have advanced")
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs.TransactionVersion[i]).To(Equal(vs2[i]),
					"should match the later transaction's version at byte %d", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 5. Grouped: separate max per group
	// =========================================================================
	It("grouped: each grouping key tracks its own max version", func() {
		// Grouped by price, aggregating VersionKey
		idx := NewMaxEverVersionIndex("Order$maxVersionByPrice",
			GroupBy(VersionKey(), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Transaction 1: save records with price=100 and price=200
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
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

		// Transaction 2: save another record with price=100 (advances that group's max)
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify two groups with different max versions
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Group price=100: max version should be from tx2
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			vs100, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs100.TransactionVersion[i]).To(Equal(vs2[i]),
					"price=100 group should have vs2 at byte %d", i)
			}

			// Group price=200: max version should be from tx1
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			vs200, ok := entries[1].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs200.TransactionVersion[i]).To(Equal(vs1[i]),
					"price=200 group should have vs1 at byte %d", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 6. Grouped: update within group advances that group's max
	// =========================================================================
	It("grouped: update within group advances max version", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersionByPrice",
			GroupBy(VersionKey(), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Transaction 1: save a record with price=100
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

		// Transaction 2: update order 1 (still price=100, changes something else)
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			// Re-save with same price but different tags — triggers update
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Tags: []string{"updated"}})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify group price=100 has the later version
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))

			vs, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())
			for i := 0; i < GlobalVersionBytes; i++ {
				Expect(vs.TransactionVersion[i]).To(Equal(vs2[i]),
					"group version should match the update transaction at byte %d", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 7. Incomplete versions: multiple saves in same tx keep max local version
	// =========================================================================
	It("incomplete versions: max local version wins within same transaction", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Save two records in the same transaction — they both get incomplete
		// versionstamps with different local versions (auto-assigned by context).
		// The one with the higher local version should win.
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

		// Verify single ungrouped entry exists with the higher local version
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			vs, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())

			// The second record gets local version 1 (0-indexed), which is > 0.
			// Unsigned byte comparison of tuple-packed versionstamps means the one
			// with the higher UserVersion (local) wins.
			Expect(vs.UserVersion).To(BeNumerically(">", 0),
				"max local version should be > 0 (the second record's local version)")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 8. Multi-column grouped: version with extra column
	// =========================================================================
	It("multi-column grouped: version + field in grouped portion", func() {
		// GroupBy(Concat(Field("quantity"), VersionKey()), Field("price"))
		// grouped = [quantity, version], grouping = [price]
		// Note: Order doesn't have "quantity" in our proto, so we use order_id
		// as a stand-in for the extra grouped column.
		// Actually, let's use: GroupBy(Concat(VersionKey(), Field("order_id")), Field("price"))
		// This won't validate because it requires exactly 1 version column in grouped.
		// So let's just use a simpler case: GroupBy(VersionKey(), Field("price"))
		// which has 1 version in grouped and 1 field in grouping — already tested above.

		// The real multi-column test: an extra non-version field in the grouped portion
		// alongside the required version. This requires Concat in the grouped portion.
		// GroupBy(Concat(Field("order_id"), VersionKey()), Field("price"))
		// → wholeKey = Concat(price, order_id, version)
		// → groupingCount = 1 (price)
		// → groupedCount = 2 (order_id, version)
		// → 1 version in grouped ✓, 0 in grouping ✓
		idx := NewMaxEverVersionIndex("Order$maxVersionMulti",
			GroupBy(Concat(Field("order_id"), VersionKey()), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify the entry has grouping key = (price=100) and value = tuple(order_id, versionstamp)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Key = grouping portion = (price)
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))

			// Value = grouped portion = (order_id, versionstamp)
			Expect(entries[0].Value).To(HaveLen(2))
			Expect(entries[0].Value[0]).To(Equal(int64(42)))

			_, ok := entries[0].Value[1].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "second value element should be Versionstamp")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 9. Scan: verify scannable via ScanIndex
	// =========================================================================
	It("scan: entries are scannable via ScanIndex with correct keys and values", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersionByPrice",
			GroupBy(VersionKey(), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Save records in two price groups across two transactions
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
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Scan all entries
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Verify each entry has correct grouping key and a versionstamp value
			expectedPrices := []int64{100, 200, 300}
			for i, e := range entries {
				Expect(e.Key).To(Equal(tuple.Tuple{expectedPrices[i]}),
					"entry %d key should be price %d", i, expectedPrices[i])
				Expect(e.Value).To(HaveLen(1))

				_, ok := e.Value[0].(tuple.Versionstamp)
				Expect(ok).To(BeTrue(), "entry %d value should be Versionstamp", i)
			}

			// Scan specific range (price=200 only)
			ranged := TupleRangeAllOf(tuple.Tuple{int64(200)})
			filtered, err := AsList(ctx, store.ScanIndex(idx, ranged, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Key).To(Equal(tuple.Tuple{int64(200)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 10. Aggregate function: EvaluateAggregateFunction with max_ever
	// =========================================================================
	It("aggregate: EvaluateAggregateFunction with FunctionNameMaxEver", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersionByPrice",
			GroupBy(VersionKey(), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

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

		// Query the aggregate for price=100 group
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			fn := &IndexAggregateFunction{
				Name:    FunctionNameMaxEver,
				Operand: GroupBy(VersionKey(), Field("price")),
				Index:   "Order$maxVersionByPrice",
			}

			// Get max_ever for price=100
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result).To(HaveLen(1))

			_, ok := result[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "aggregate result should be a Versionstamp, got %T", result[0])

			// Get max_ever across all groups (no filter)
			resultAll, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(resultAll).NotTo(BeNil())
			Expect(resultAll).To(HaveLen(1))

			_, ok = resultAll[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 11. Metadata validation: requires storeRecordVersions
	// =========================================================================
	It("metadata validation: requires SetStoreRecordVersions(true)", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// NOT calling SetStoreRecordVersions(true)
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad", Ungrouped(VersionKey())))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("SetStoreRecordVersions(true)"))
	})

	// =========================================================================
	// 12. Metadata validation: requires GroupingKeyExpression
	// =========================================================================
	It("metadata validation: requires GroupingKeyExpression", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		// Using raw VersionKey() without GroupBy/Ungrouped wrapper
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad", VersionKey()))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("GroupingKeyExpression"))
	})

	// =========================================================================
	// 13. Metadata validation: version must be in grouped portion (not grouping)
	// =========================================================================
	It("metadata validation: version in grouping portion rejected", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		// GroupAll puts everything in grouping, nothing in grouped.
		// VersionKey is 1 column; GroupAll → groupedCount=0 → "at least 1 grouped column" error
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad", GroupAll(VersionKey())))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("at least 1 grouped column"))
	})

	It("metadata validation: version in grouping portion with field in grouped rejected", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		// GroupBy(Field("price"), VersionKey())
		// → wholeKey = Concat(VersionKey, price), groupingCount = 1 (version column size)
		// → grouping has 1 version, grouped has 0 versions
		// → should fail: "no version entries in grouping key" AND "exactly 1 version in grouped"
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad",
			GroupBy(Field("price"), VersionKey())))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(SatisfyAny(
			ContainSubstring("no version entries in grouping key"),
			ContainSubstring("exactly 1 version entry in grouped key"),
		))
	})

	// =========================================================================
	// 14. Metadata validation: exactly 1 version column in grouped
	// =========================================================================
	It("metadata validation: 0 version columns in grouped rejected", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		// GroupBy(Field("price"), Field("order_id"))
		// → grouped = [price], grouping = [order_id], 0 version columns
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad",
			GroupBy(Field("price"), Field("order_id"))))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("exactly 1 version entry in grouped key"))
	})

	It("metadata validation: 2 version columns in grouped rejected", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		// GroupBy(Concat(VersionKey(), VersionKey()), Field("price"))
		// → grouped = [version, version] (count=2), grouping = [price]
		builder.AddIndex("Order", NewMaxEverVersionIndex("bad",
			GroupBy(Concat(VersionKey(), VersionKey()), Field("price"))))
		_, err := builder.Build()
		var mdErr2 *MetaDataError
		Expect(errors.As(err, &mdErr2)).To(BeTrue())
		Expect(mdErr2.Message).To(ContainSubstring("exactly 1 version entry in grouped key"))
	})

	// =========================================================================
	// 15. Rebuild: RebuildIndex works for MAX_EVER_VERSION
	// =========================================================================
	It("rebuild: RebuildIndex produces correct entries", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersionByPrice",
			GroupBy(VersionKey(), Field("price")))
		metaData := buildMeta(idx)
		ks := specSubspace()

		// Save records across two transactions
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

		_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Rebuild the index
		_, _, err = sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			err = store.RebuildIndex(idx)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify entries after rebuild
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2), "should have 2 price groups after rebuild")

			// price=100
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			_, ok := entries[0].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "price=100 value should be a Versionstamp")

			// price=200
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			_, ok = entries[1].Value[0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue(), "price=200 value should be a Versionstamp")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 16. Empty store produces no entries
	// =========================================================================
	It("empty store: no index entries", func() {
		idx := NewMaxEverVersionIndex("Order$maxVersion", Ungrouped(VersionKey()))
		metaData := buildMeta(idx)
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
