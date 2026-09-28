package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
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

	It("writes UUID tuple heartbeat keys compatible with Java", func() {
		ss := specSubspace()
		hb := NewIndexingHeartbeat("WIRE_UUID", 60_000, true, nil)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			if err := hb.CheckAndUpdate(rtx.Transaction(), ss, idx); err != nil {
				return nil, err
			}
			rows, err := rtx.Transaction().GetRange(heartbeatSubspace(ss, idx), fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			Expect(rows).To(HaveLen(1))
			key, err := heartbeatSubspace(ss, idx).Unpack(rows[0].Key)
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(tuple.Tuple{tuple.UUID(hb.indexerID)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("recognizes a Java UUID heartbeat and the exact future skew boundary", func() {
		for _, age := range []int64{-86_400_001, -86_400_000, -86_399_999, 0, 59_999, 60_000} {
			ss := specSubspace().Sub(age)
			env := &dst.Env{Clock: dst.NewSimClock(dst.Epoch), Random: dst.NewSeededRandomness(19)}
			peer := NewIndexingHeartbeat("JAVA_PEER", 60_000, false, env)
			self := NewIndexingHeartbeat("GO_SELF", 60_000, false, env)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				data, err := proto.Marshal(&gen.IndexBuildHeartbeat{
					Info: proto.String("JAVA_PEER"), HeartbeatTimeMilliseconds: proto.Int64(dst.Epoch.UnixMilli() - age),
				})
				if err != nil {
					return nil, err
				}
				rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(tuple.Tuple{tuple.UUID(peer.indexerID)}), data)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, self.CheckAndUpdate(rtx.Transaction(), ss, idx)
			})
			if age > -86_400_000 && age < 60_000 {
				var locked *SynchronizedSessionLockedError
				Expect(errors.As(err, &locked)).To(BeTrue(), "age=%d", age)
				Expect(locked.ExistingIndexerID).To(Equal(peer.indexerID.String()))
			} else {
				Expect(err).NotTo(HaveOccurred(), "age=%d", age)
			}
		}
	})

	It("refuses legacy and malformed heartbeat keys even in mutual mode", func() {
		for _, mutual := range []bool{false, true} {
			for _, key := range []tuple.Tuple{{"legacy-id"}, {int64(7)}} {
				ss := specSubspace().Sub(mutual, key.Pack())
				hb := NewIndexingHeartbeat("NEW", 60_000, mutual, nil)
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(key), []byte{})
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					return nil, hb.CheckAndUpdate(rtx.Transaction(), ss, idx)
				})
				var keyErr *IndexingHeartbeatKeyError
				Expect(errors.As(err, &keyErr)).To(BeTrue(), "mutual=%v key=%v", mutual, key)
				Expect(keyErr.IndexName).To(Equal(idx.Name))
				Expect(keyErr.Key).To(Equal([]byte(heartbeatSubspace(ss, idx).Pack(key))))
			}
			// Java reads only element 0 of a heartbeat key (getUUID(0),
			// IndexingHeartbeat.java:201-203), so a (UUID, x) key is that UUID's
			// heartbeat, not a malformed one: with an empty value it parses as a
			// heartbeat at time 0, which is stale and blocks no session.
			key := tuple.Tuple{tuple.UUID{}, int64(1)}
			ss := specSubspace().Sub(mutual, key.Pack())
			hb := NewIndexingHeartbeat("NEW", 60_000, mutual, nil)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(key), []byte{})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, hb.CheckAndUpdate(rtx.Transaction(), ss, idx)
			})
			Expect(err).NotTo(HaveOccurred(), "mutual=%v: a (UUID, x) key is that UUID's stale heartbeat", mutual)
		}
	})

	It("reports malformed UUID heartbeat values in diagnostics without blocking builders", func() {
		ss := specSubspace()
		peer := NewIndexingHeartbeat("BROKEN", 60_000, true, nil)
		self := NewIndexingHeartbeat("HEALTHY", 60_000, false, nil)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(tuple.Tuple{tuple.UUID(peer.indexerID)}), []byte{0xff})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			values, ids, err := ReadHeartbeats(rtx.Transaction(), ss, idx)
			if err != nil {
				return nil, err
			}
			Expect(ids).To(Equal([]string{peer.indexerID.String()}))
			Expect(values).To(HaveLen(1))
			Expect(values[0].GetInfo()).To(Equal("<< Invalid Heartbeat >>"))
			Expect(values[0].GetCreateTimeMilliseconds()).To(BeZero())
			Expect(values[0].GetHeartbeatTimeMilliseconds()).To(BeZero())
			if err := self.CheckAndUpdate(rtx.Transaction(), ss, idx); err != nil {
				return nil, err
			}
			self.Cleanup(rtx.Transaction(), ss, idx)
			_, ids, err = ReadHeartbeats(rtx.Transaction(), ss, idx)
			Expect(ids).To(Equal([]string{peer.indexerID.String()}))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("CheckAndUpdate in mutual mode", func() {
		It("always succeeds even with existing heartbeats", func() {
			ss := specSubspace()

			// Write a heartbeat from indexer A.
			hbA := NewIndexingHeartbeat("MUTUAL_A", 60_000, true, nil)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Indexer B in mutual mode should succeed despite A's heartbeat.
			hbB := NewIndexingHeartbeat("MUTUAL_B", 60_000, true, nil)
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

			hb := NewIndexingHeartbeat("EXCLUSIVE", 60_000, false, nil)
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
			hbA := NewIndexingHeartbeat("EXCLUSIVE_A", 60_000, false, nil)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, hbA.CheckAndUpdate(rtx.Transaction(), store.subspace, idx)
			})
			Expect(err).NotTo(HaveOccurred())

			// Indexer B in non-mutual mode should fail.
			hbB := NewIndexingHeartbeat("EXCLUSIVE_B", 60_000, false, nil)
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
			// Java's INDEXER_ID (the refused indexer) and EXISTING_INDEXER_ID.
			Expect(lockErr.IndexerID).To(Equal(hbB.indexerID))
			Expect(lockErr.ExistingIndexerID).To(Equal(hbA.indexerID.String()))
			Expect(lockErr.HeartbeatAgeMs).To(BeNumerically("<", 5000)) // should be very recent
			Expect(lockErr.LeaseLengthMs).To(Equal(int64(60_000)))
		})

		It("allows when only own heartbeat exists", func() {
			ss := specSubspace()

			hb := NewIndexingHeartbeat("EXCLUSIVE", 60_000, false, nil)

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

			hb := NewIndexingHeartbeat("CLEANUP_TEST", 60_000, false, nil)

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
			hb2 := NewIndexingHeartbeat("AFTER_CLEANUP", 60_000, false, nil)
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
			hbA := NewIndexingHeartbeat("ALL_A", 60_000, true, nil)
			hbB := NewIndexingHeartbeat("ALL_B", 60_000, true, nil)
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
			hbC := NewIndexingHeartbeat("AFTER_CLEAR", 60_000, false, nil)
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

			hbA := NewIndexingHeartbeat("READ_A", 60_000, true, nil)
			hbB := NewIndexingHeartbeat("READ_B", 60_000, true, nil)
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
