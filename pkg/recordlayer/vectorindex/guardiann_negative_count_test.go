package vectorindex

import (
	"context"
	"encoding/binary"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("GuardiANN negative task count", func() {
	// VectorIndexMaintainer.disableIndexOnNegativeTaskCount: a merge that finds
	// a negative deferred-task count disables the index, counts
	// VECTOR_INDEX_DISABLED_ON_NEGATIVE_TASK_COUNT, and reports success, so the
	// disable commits.
	It("disables the index, counts it, and commits", func() {
		ctx := context.Background()
		root := specSubspace()
		builder := baseBuilder()
		index := recordlayer.NewVectorIndex("Order$negative_count", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		index.Options[recordlayer.IndexOptionVectorEngine] = "GUARDIANN"
		index.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMin] = "3"
		index.Options[recordlayer.IndexOptionGuardiannPrimaryClusterMax] = "12"
		index.Options[recordlayer.IndexOptionGuardiannPrimaryClusterHardMax] = "40"
		index.Options[recordlayer.IndexOptionGuardiannCollapseMinDuplicates] = "6"
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		// Thirteen vectors over a maximum of twelve queue a split (deferred mode).
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
		// Drive the partition's count below zero.
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			m, err := store.GetIndexMaintainer(md.GetIndex(index.Name))
			if err != nil {
				return nil, err
			}
			vm := m.(*vectorIndexMaintainer)
			queued, err := vm.taskCounts.outstanding(rc.Transaction(), 16)
			Expect(err).NotTo(HaveOccurred())
			Expect(queued).To(HaveLen(1), "the split's partition has a count")
			delta := make([]byte, 8)
			binary.LittleEndian.PutUint64(delta, uint64(-(queued[0].count + 1)))
			vm.taskCounts.adjust(rc.Transaction(), queued[0].prefix, delta)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		timer := recordlayer.NewStoreTimer()
		db := recordlayer.NewFDBDatabase(sharedRawDB)
		db.SetTimer(timer)
		oi, err := recordlayer.NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).
			SetIndex(md.GetIndex(index.Name)).SetSubspace(root).Build()
		Expect(err).NotTo(HaveOccurred())
		// The merge reports success with nothing done (0/0), so the disable,
		// a commit check that runs after the merger's heartbeat refresh,
		// commits and the merge ends.
		Expect(oi.MergeIndexes(ctx)).To(Succeed())
		Expect(timer.GetCount(CountVectorIndexDisabledOnNegativeTaskCount)).To(Equal(int64(1)))
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.GetIndexState(index.Name)).To(Equal(recordlayer.IndexStateDisabled), "the disable committed")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
