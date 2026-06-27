package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("ContinuationStability", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// scanBatch scans records from the store with the given continuation and limit,
	// returns the order IDs found and the continuation bytes for the next page.
	type scanResult struct {
		ids          []int64
		continuation []byte
	}

	scanBatch := func(ctx context.Context, store *FDBRecordStore, continuation []byte, limit int) scanResult {
		props := ScanProperties{
			ExecuteProperties:   DefaultExecuteProperties().WithReturnedRowLimit(limit),
			CursorStreamingMode: StreamingModeIterator,
		}
		cursor := store.ScanRecords(continuation, props)
		defer func() { _ = cursor.Close() }()

		var ids []int64
		var contBytes []byte
		for {
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !result.HasNext() {
				cont := result.GetContinuation()
				if cont != nil && !cont.IsEnd() {
					var contErr error
					contBytes, contErr = cont.ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
				}
				break
			}
			order := result.GetValue().Record.(*gen.Order)
			ids = append(ids, order.GetOrderId())
		}
		return scanResult{ids: ids, continuation: contBytes}
	}

	// scanIndexBatch scans index entries with the given continuation and limit,
	// returns the index values (prices) and continuation bytes.
	type indexScanResult struct {
		prices       []int64
		primaryKeys  []int64
		continuation []byte
	}

	scanIndexBatch := func(ctx context.Context, store *FDBRecordStore, index *Index, continuation []byte, limit int) indexScanResult {
		props := ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = limit
		cursor := store.ScanIndex(index, TupleRangeAll, continuation, props)
		defer func() { _ = cursor.Close() }()

		var prices []int64
		var pks []int64
		var contBytes []byte
		for {
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !result.HasNext() {
				cont := result.GetContinuation()
				if cont != nil && !cont.IsEnd() {
					var contErr error
					contBytes, contErr = cont.ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
				}
				break
			}
			entry := result.GetValue()
			prices = append(prices, entry.IndexValues()[0].(int64))
			pks = append(pks, entry.PrimaryKey()[0].(int64))
		}
		return indexScanResult{prices: prices, primaryKeys: pks, continuation: contBytes}
	}

	It("resumes scan after record deletion mid-scan", func() {
		ks := specSubspace()
		builder := baseMetaData()
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Transaction 1: insert 10 records (IDs 1-10).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: scan first 3 records, capture continuation.
		var continuation []byte
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, nil, 3)
			Expect(batch.ids).To(Equal([]int64{1, 2, 3}))
			Expect(batch.continuation).NotTo(BeNil())
			continuation = batch.continuation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 3: delete records 4, 5, 6.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(4); i <= 6; i++ {
				_, err = store.DeleteRecord(tuple.Tuple{i})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 4: resume with continuation -- should skip deleted records,
		// return remaining records 7-10.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, continuation, 0)
			Expect(batch.ids).To(Equal([]int64{7, 8, 9, 10}))
			Expect(batch.continuation).To(BeNil(), "source should be exhausted")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("resumes scan after record insertion mid-scan", func() {
		ks := specSubspace()
		builder := baseMetaData()
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Transaction 1: insert 5 records with IDs 10, 20, 30, 40, 50 (gaps for interleaving).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []int64{10, 20, 30, 40, 50} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: scan first 2 records, capture continuation.
		var continuation []byte
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, nil, 2)
			Expect(batch.ids).To(Equal([]int64{10, 20}))
			Expect(batch.continuation).NotTo(BeNil())
			continuation = batch.continuation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 3: insert 3 records between existing keys.
		// IDs 15 (between 10 and 20 -- before continuation, should NOT appear),
		// 25 and 35 (after continuation point, should appear).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []int64{15, 25, 35} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 4: resume with continuation.
		// Should see: 25, 30, 35, 40, 50 (records after the continuation point).
		// 15 is before the continuation point so it must not appear.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, continuation, 0)
			Expect(batch.ids).To(Equal([]int64{25, 30, 35, 40, 50}))
			Expect(batch.continuation).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("resumes index scan after record deletion", func() {
		ks := specSubspace()
		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Transaction 1: insert 5 records. Prices 100, 200, 300, 400, 500.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: scan index with limit=2 (should get prices 100, 200).
		var continuation []byte
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanIndexBatch(ctx, store, priceIndex, nil, 2)
			Expect(batch.prices).To(Equal([]int64{100, 200}))
			Expect(batch.continuation).NotTo(BeNil())
			continuation = batch.continuation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 3: delete record with ID=3 (price=300, past the continuation).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 4: resume index scan -- should skip deleted entry (300),
		// return 400 and 500.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanIndexBatch(ctx, store, priceIndex, continuation, 0)
			Expect(batch.prices).To(Equal([]int64{400, 500}))
			Expect(batch.continuation).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("resumes scan after DeleteAllRecords and re-insert", func() {
		ks := specSubspace()
		builder := baseMetaData()
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Transaction 1: insert 10 records (IDs 1-10).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: scan first 3 records, capture continuation.
		var continuation []byte
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, nil, 3)
			Expect(batch.ids).To(Equal([]int64{1, 2, 3}))
			Expect(batch.continuation).NotTo(BeNil())
			continuation = batch.continuation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 3: delete all records, then re-insert 5 new records
		// with IDs 1, 5, 10, 15, 20.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			err = store.DeleteAllRecords()
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []int64{1, 5, 10, 15, 20} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id * 10))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 4: resume with old continuation.
		// The continuation was pointing past record 3 (exclusive after ID=3).
		// From the new data, records with ID > 3 are: 5, 10, 15, 20.
		// Should not crash and should return records past the continuation point.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanBatch(ctx, store, continuation, 0)
			Expect(batch.ids).To(Equal([]int64{5, 10, 15, 20}))
			Expect(batch.continuation).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("resumes index scan after index rebuild", func() {
		ks := specSubspace()
		priceIndex := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Transaction 1: insert 5 records. Prices 100, 200, 300, 400, 500.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 2: scan index with limit=2 (prices 100, 200).
		var continuation []byte
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanIndexBatch(ctx, store, priceIndex, nil, 2)
			Expect(batch.prices).To(Equal([]int64{100, 200}))
			Expect(batch.continuation).NotTo(BeNil())
			continuation = batch.continuation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 3: rebuild the index (clears and re-creates all entries).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			err = store.RebuildIndex(priceIndex)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Transaction 4: resume with continuation -- should still return 300, 400, 500.
		// The rebuild recreates identical entries at the same keys, so the
		// continuation token (which encodes the FDB key position) remains valid.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			batch := scanIndexBatch(ctx, store, priceIndex, continuation, 0)
			Expect(batch.prices).To(Equal([]int64{300, 400, 500}))
			Expect(batch.continuation).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
