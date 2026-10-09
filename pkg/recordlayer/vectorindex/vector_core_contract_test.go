package vectorindex

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// The vector indexes' halves of core contracts whose recordlayer specs run
// without a vector index (that package's tests cannot link this one).
var _ = Describe("vector indexes under core contracts", func() {
	ctx := context.Background()

	spfreshIndex := func(name string) *recordlayer.Index {
		idx := recordlayer.NewIndex(name, recordlayer.Field("price"))
		idx.Type = recordlayer.IndexTypeVectorSPFresh
		idx.Options = map[string]string{recordlayer.IndexOptionSPFreshNumDimensions: "1"}
		return idx
	}

	// recordlayer's "explicitly refuses unsupported maintainers".
	It("SPFresh explicitly refuses the pending write queue", func() {
		idx := spfreshIndex("unsupported")
		m, err := newSPFreshIndexMaintainer(idx, subspace.FromBytes([]byte{1}), nil, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.IsPendingWriteQueueAllowed()).To(BeFalse())
		_, err = m.SerializePendingWriteQueue(nil, nil)
		var unsupported *recordlayer.UnsupportedOperationError
		Expect(errors.As(err, &unsupported)).To(BeTrue())
		Expect(unsupported.Message).To(Equal("unsupported does not support the pending write queue"))
		Expect(errors.As(m.UpdateFromQueue(nil), &unsupported)).To(BeTrue())
	})

	// recordlayer's "rejects queued eligibility".
	It("rejects queued eligibility for SPFresh", func() {
		index := spfreshIndex("ineligible")
		builder := baseBuilder()
		builder.SetStoreRecordVersions(true)
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).SetFormatVersion(15).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
			var core *recordlayer.RecordCoreError
			Expect(errors.As(err, &core)).To(BeTrue(), "%v", err)
			Expect(core.IndexName).To(Equal(index.Name))
			Expect(store.GetIndexState(index.Name)).To(Equal(recordlayer.IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// HNSW keeps no deferred work: an explicit merge, plain or through the
	// sliding-window decorator, succeeds and requests no further merge.
	It("explicitly merges HNSW and sliding HNSW without inventing deferred work", func() {
		root := specSubspace()
		plain := recordlayer.NewVectorIndex("vector", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		windowed := newWindowedVectorIndex("windowed", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseBuilder()
		builder.AddIndex("Order", plain)
		builder.AddIndex("Order", windowed)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			_, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		oi, err := recordlayer.NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).
			SetTargetIndexes([]*recordlayer.Index{plain, windowed}).Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(oi.MergeIndexes(ctx)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			for _, index := range []*recordlayer.Index{plain, windowed} {
				maintainer, err := store.GetIndexMaintainer(index)
				Expect(err).NotTo(HaveOccurred())
				Expect(maintainer.MergeIndex()).To(Succeed())
			}
			Expect(store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes()).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A failing GuardiANN merge surfaces from MergeIndexes while the other
	// target is still merged (recordlayer's "attempts every target").
	It("surfaces a failing GuardiANN merge and still merges the other target", func() {
		root := specSubspace()
		failing := recordlayer.NewVectorIndex("Order$merge_guardiann", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		failing.Options[recordlayer.IndexOptionVectorEngine] = "GUARDIANN"
		failing.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMin] = "3"
		failing.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMax] = "12"
		failing.Options[recordlayer.IndexOptionGuardiannPrimaryClusterHardMax] = "40"
		failing.Options[recordlayer.IndexOptionGuardiannCollapseMinDuplicates] = "6"
		merged := recordlayer.NewIndex("Order$merge_ok", recordlayer.Field("quantity"))
		builder := baseBuilder()
		builder.AddIndex("Order", failing)
		builder.AddIndex("Order", merged)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		// Thirteen vectors over a maximum of twelve queue a split of the one cluster.
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 13; i++ {
				if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i)), Quantity: proto.Int32(1)}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		// Make that cluster's metadata unreadable, so the split fails.
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			g := &guardiann{ss: store.IndexSubspace(md.GetIndex(failing.Name)).Sub(int64(1)), codec: &guardiannVectorCodec{config: guardiannConfig{numDimensions: 1}}}
			tasks, err := g.fetchSomeTasks(rc.Transaction(), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).NotTo(BeEmpty(), "no split was queued")
			for _, t := range tasks {
				rc.Transaction().Set(g.clusterMetadataKey(t.target()), tuple.Tuple{int64(0), int64(0), tuple.Tuple{int64(0), 0.0, 0.0, 0.0}, int64(0)}.Pack())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		oi, err := recordlayer.NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).
			SetTargetIndexes([]*recordlayer.Index{md.GetIndex(failing.Name), md.GetIndex(merged.Name)}).SetSubspace(root).Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(oi.MergeIndexes(ctx)).To(MatchError(ContainSubstring("has 4 elements, want 5")))
	})

	// recordlayer's "scan checks transaction-visible state before dispatch"
	// and "scan propagates index-state read failures", for the vector entry
	// points.
	Describe("the store's vector entry points", func() {
		buildMetaData := func() *recordlayer.RecordMetaData {
			builder := baseBuilder()
			builder.AddIndex("Order", recordlayer.NewVectorIndex("original", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}
		for _, method := range []string{"vector-scan", "vector-search"} {
			scan := func(store *recordlayer.FDBRecordStore, index *recordlayer.Index) error {
				if method == "vector-scan" {
					_, err := recordlayer.AsList(ctx, store.ScanVectorIndex(index, []float64{1, 0}, 1, 10, nil, recordlayer.ForwardScan()))
					return err
				}
				_, err := store.SearchVectorIndex(index, []float64{1, 0}, 1, 10)
				return err
			}
			It(method+" checks transaction-visible state before dispatch", func() {
				md := buildMetaData()
				_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					ss := specSubspace()
					first, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					second, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
					_, err = first.MarkIndexDisabled("original")
					Expect(err).NotTo(HaveOccurred())
					err = scan(second, md.GetIndex("original"))
					var unreadable *recordlayer.IndexNotReadableError
					Expect(errors.As(err, &unreadable)).To(BeTrue(), "%v", err)
					Expect(unreadable.CurrentState).To(Equal(recordlayer.IndexStateDisabled))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
			It(method+" propagates index-state read failures", func() {
				md := buildMetaData()
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := recordlayer.NewFDBRecordContext(tx, nil)
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				tx.Cancel()
				var canceled fdb.Error
				Expect(errors.As(scan(store, md.GetIndex("original")), &canceled)).To(BeTrue())
				Expect(canceled.Code).To(Equal(1025))
			})
		}
	})
})
