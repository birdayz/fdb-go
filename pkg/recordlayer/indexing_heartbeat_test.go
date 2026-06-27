package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("IndexingHeartbeat", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
		idx *Index
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddUniversalIndex(NewIndex("test_idx", Field("price")))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
		idx = md.GetIndex("test_idx")
		Expect(idx).NotTo(BeNil())
	})

	Describe("CheckAndUpdate in mutual mode", func() {
		It("always succeeds even with existing heartbeats", func() {
			ss := specSubspace()

			// Write a heartbeat from indexer A.
			hbA := NewIndexingHeartbeat("MUTUAL_A", 60_000, true)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Indexer B in mutual mode should succeed despite A's heartbeat.
			hbB := NewIndexingHeartbeat("MUTUAL_B", 60_000, true)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, hbB.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("CheckAndUpdate in non-mutual mode", func() {
		It("succeeds when no other heartbeats exist", func() {
			ss := specSubspace()

			hb := NewIndexingHeartbeat("EXCLUSIVE", 60_000, false)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hb.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("blocks when active heartbeat from another indexer exists", func() {
			ss := specSubspace()

			// Indexer A writes heartbeat.
			hbA := NewIndexingHeartbeat("EXCLUSIVE_A", 60_000, false)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Indexer B in non-mutual mode should fail.
			hbB := NewIndexingHeartbeat("EXCLUSIVE_B", 60_000, false)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, hbB.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).To(HaveOccurred())
			var lockErr *SynchronizedSessionLockedError
			Expect(errors.As(err, &lockErr)).To(BeTrue())
			Expect(lockErr.ExistingInfo).To(Equal("EXCLUSIVE_A"))
			Expect(lockErr.HeartbeatAgeMs).To(BeNumerically("<", 5000)) // should be very recent
			Expect(lockErr.LeaseLengthMs).To(Equal(int64(60_000)))
		})

		It("allows when only own heartbeat exists", func() {
			ss := specSubspace()

			hb := NewIndexingHeartbeat("EXCLUSIVE", 60_000, false)

			// First CheckAndUpdate.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hb.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Second CheckAndUpdate from same indexer should succeed.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, hb.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Cleanup", func() {
		It("removes this indexer's heartbeat", func() {
			ss := specSubspace()

			hb := NewIndexingHeartbeat("CLEANUP_TEST", 60_000, false)

			// Write heartbeat.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hb.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Cleanup.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				hb.Cleanup(rtx.Transaction(), store.subspace, idx)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Another non-mutual indexer should succeed now.
			hb2 := NewIndexingHeartbeat("AFTER_CLEANUP", 60_000, false)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, hb2.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("CleanupAllHeartbeats", func() {
		It("removes all heartbeats for an index", func() {
			ss := specSubspace()

			// Write heartbeats from two indexers.
			hbA := NewIndexingHeartbeat("ALL_A", 60_000, true)
			hbB := NewIndexingHeartbeat("ALL_B", 60_000, true)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if err := hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx); err != nil {
					return nil, err
				}
				return nil, hbB.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Clear all.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				CleanupAllHeartbeats(rtx.Transaction(), store.subspace, idx)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Non-mutual indexer should succeed.
			hbC := NewIndexingHeartbeat("AFTER_CLEAR", 60_000, false)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				return nil, hbC.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ReadHeartbeats", func() {
		It("reads all heartbeats for an index", func() {
			ss := specSubspace()

			hbA := NewIndexingHeartbeat("READ_A", 60_000, true)
			hbB := NewIndexingHeartbeat("READ_B", 60_000, true)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if err := hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx); err != nil {
					return nil, err
				}
				return nil, hbB.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				heartbeats, ids, err := ReadHeartbeats(rtx.Transaction(), store.subspace, idx)
				if err != nil {
					return nil, err
				}
				Expect(heartbeats).To(HaveLen(2))
				Expect(ids).To(HaveLen(2))
				// Both indexer IDs should be present.
				Expect(ids).To(ContainElement(hbA.indexerID.String()))
				Expect(ids).To(ContainElement(hbB.indexerID.String()))
				// Info fields should match.
				infos := make(map[string]bool)
				for _, hb := range heartbeats {
					infos[hb.GetInfo()] = true
				}
				Expect(infos).To(HaveKey("READ_A"))
				Expect(infos).To(HaveKey("READ_B"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns empty for index with no heartbeats", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				heartbeats, ids, err := ReadHeartbeats(rtx.Transaction(), store.subspace, idx)
				if err != nil {
					return nil, err
				}
				Expect(heartbeats).To(BeEmpty())
				Expect(ids).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SynchronizedSessionLockedError", func() {
		It("implements error interface with descriptive message", func() {
			err := &SynchronizedSessionLockedError{
				ExistingIndexerID: "abc-123",
				ExistingInfo:      "BY_RECORDS",
				HeartbeatAgeMs:    500,
				LeaseLengthMs:     60000,
			}
			Expect(err.Error()).To(ContainSubstring("abc-123"))
			Expect(err.Error()).To(ContainSubstring("BY_RECORDS"))
			Expect(err.Error()).To(ContainSubstring("500"))
		})
	})
})
