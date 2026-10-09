package vectorindex

import (
	"context"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Vector index store timer counters", func() {
	ctx := context.Background()
	// Java 4.14's FDBStoreTimer.Counts.VECTOR_VECTOR_READS (OnRead.onVectorRead,
	// on every reference GuardiANN decodes) and VECTOR_TASK_ENQUEUED /
	// VECTOR_TASK_EXECUTED (OnWrite): a store whose context carries a timer
	// counts them.
	It("counts vector references read and deferred tasks", func() {
		ks := specSubspace()
		vecIdx := recordlayer.NewVectorIndex("vec_counted", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		vecIdx.Options[recordlayer.IndexOptionVectorEngine] = "GUARDIANN"
		vecIdx.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMin] = "1"
		vecIdx.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMax] = "2"
		vecIdx.Options[recordlayer.IndexOptionGuardiannCollapseMinDuplicates] = "1"
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		timer := recordlayer.NewStoreTimer()
		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.SetTimer(timer)
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			// In-transaction maintenance, as embedded relational runs it.
			store.GetIndexDeferredMaintenanceControl().SetAutoMergeDuringCommit(true)
			for id := int64(1); id <= 6; id++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(10 * id)), Quantity: proto.Int32(1)})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(timer.GetCount(CountVectorTaskEnqueued)).To(BeNumerically(">", 0), "a cluster past its maximum enqueues a split")
			Expect(timer.GetCount(CountVectorTaskExecuted)).To(BeNumerically(">", 0), "in-transaction maintenance executes it")
			before := timer.GetCount(CountVectorVectorReads)
			cursor := store.ScanVectorIndexWithOptions(vecIdx, tuple.Tuple{int64(1)}, []float64{15}, 3, recordlayer.VectorIndexScanOptions{}, nil, recordlayer.ForwardScan())
			for {
				r, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					break
				}
			}
			Expect(timer.GetCount(CountVectorVectorReads)).To(BeNumerically(">", before), "a search reads references")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// HnswVectorIndexEngine's OnRead / OnWrite: node reads and writes by layer
	// and the generic index key/value counters, on writes and on searches.
	It("counts HNSW node reads and writes", func() {
		ks := specSubspace()
		vecIdx := recordlayer.NewVectorIndex("vec_hnsw_counted", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		timer := recordlayer.NewStoreTimer()
		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.SetTimer(timer)
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for id := int64(1); id <= 4; id++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(10 * id)), Quantity: proto.Int32(1)})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(timer.GetCount(CountVectorNode0Writes)).To(BeNumerically(">=", 4), "each insert writes its layer-0 node")
			Expect(timer.GetCount(CountVectorNode0WriteBytes)).To(BeNumerically(">", 0))
			Expect(timer.GetCount(recordlayer.CountSaveIndexKey)).To(BeNumerically(">=", timer.GetCount(CountVectorNode0Writes)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		readTimer := recordlayer.NewStoreTimer()
		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.SetTimer(readTimer)
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			cursor := store.ScanVectorIndexWithOptions(vecIdx, tuple.Tuple{int64(1)}, []float64{15}, 2, recordlayer.VectorIndexScanOptions{}, nil, recordlayer.ForwardScan())
			for {
				r, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					break
				}
			}
			Expect(readTimer.GetCount(CountVectorNode0Reads)).To(BeNumerically(">", 0), "a search reads layer-0 nodes")
			Expect(readTimer.GetCount(recordlayer.CountLoadIndexKey)).To(BeNumerically(">=", readTimer.GetCount(CountVectorNode0Reads)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
