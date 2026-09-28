package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ = Describe("Queued store dispatch", func() {
	ctx := context.Background()
	makeMetadata := func() (*RecordMetaData, *Index) {
		index := NewVectorIndex("queued", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseBuilder()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.AddIndex("Order", index)
		builder.AddIndex("Order", NewIndex("ordinary", Field("quantity")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, index
	}
	// These low-level format fixtures retain the advanced Build API to isolate
	// dispatch/state tests. Normal format-15 opening is covered by lifecycle tests.
	open := func(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace) *FDBRecordStore {
		store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(store.ensureStoreStateLoadedErr()).To(Succeed())
		return store
	}
	seed := func(md *RecordMetaData, index *Index, root subspace.Subspace) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			Expect(err).NotTo(HaveOccurred())
			store.storeHeader.FormatVersion = proto.Int32(15)
			Expect(store.writeStoreHeader(store.storeHeader)).To(Succeed())
			_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}
	order := func(id int64, partition int32) proto.Message {
		return &gen.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(partition), Price: proto.Int32(42)}
	}
	It("commits an empty drain heartbeat and cleans only its own session", func() {
		md, index := makeMetadata()
		root := specSubspace()
		oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{index}, queuedIndexes: []*Index{index}}
		heartbeat := NewIndexingHeartbeat("empty-drain", 30000, false, sharedDB.Env())
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			// Java permits persisted queued state at format 14; keep this raw
			// fixture to pin draining existing Java-created state at that format.
			store.setIndexState(index.Name, IndexStateWriteOnlyWithQueue)
			return nil, store.SaveIndexingTypeStamp(index, oi.buildIndexingStamp())
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(oi.drainPendingIndexWrites(ctx, heartbeat)).To(Succeed())
		other := NewIndexingHeartbeat("other-session", 30000, false, sharedDB.Env())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			value, err := rc.Transaction().Get(heartbeat.heartbeatKey(root, index)).Get()
			Expect(value).NotTo(BeEmpty())
			Expect(err).NotTo(HaveOccurred())
			rc.Transaction().Set(other.heartbeatKey(root, index), []byte("preserve-other-session"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		oi.cleanupPendingQueueHeartbeat(heartbeat)
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			value, err := rc.Transaction().Get(heartbeat.heartbeatKey(root, index)).Get()
			Expect(value).To(BeEmpty())
			Expect(err).NotTo(HaveOccurred())
			value, err = rc.Transaction().Get(other.heartbeatKey(root, index)).Get()
			Expect(value).To(Equal([]byte("preserve-other-session")))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, attempts := range []int64{0, 1, 2} {
		It(fmt.Sprintf("re-drains a closeout writer race within the configured attempt budget %d", attempts), func() {
			md, index := makeMetadata()
			root := specSubspace()
			oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{index}, queuedIndexes: []*Index{index}, policy: &IndexingPolicy{PendingWriteQueueIndexesMaxDrainAttempts: &attempts}, sessionHeartbeat: NewIndexingHeartbeat("closeout", 30000, false, sharedDB.Env())}
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				if err != nil {
					return nil, err
				}
				store.setIndexState(index.Name, IndexStateWriteOnlyWithQueue)
				_, err = NewIndexingRangeSet(root, index).InsertRange(rc.Transaction(), nil, nil, true)
				if err != nil {
					return nil, err
				}
				return nil, store.SaveIndexingTypeStamp(index, oi.buildIndexingStamp())
			})
			Expect(err).NotTo(HaveOccurred())
			interleaver := &batchMutationTransactor{Transactor: sharedDB.transactor, mutate: func() error {
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := oi.openStore(rc)
					if err != nil {
						return nil, err
					}
					payload, err := anypb.New(&gen.DeleteWhere{Prefix: tuple.Tuple{}.Pack()})
					if err != nil {
						return nil, err
					}
					return nil, store.indexingPendingWriteQueue(index, 0).Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_DELETE_WHERE.Enum(), Data: payload}, 0)
				})
				return err
			}}
			oi.db = NewFDBDatabaseWithTransactor(interleaver, sharedDB.db)
			err = oi.markReadable(ctx)
			if attempts < 2 {
				var notBuilt *IndexNotBuiltError
				Expect(errors.As(err, &notBuilt)).To(BeTrue())
				Expect(notBuilt.PendingWrites).To(BeTrue())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				if err != nil {
					return nil, err
				}
				state, err := store.readIndexState(index.Name)
				Expect(err).NotTo(HaveOccurred())
				empty, err := store.isIndexPendingQueueEmpty(index)
				if attempts < 2 {
					Expect(state).To(Equal(IndexStateWriteOnlyWithQueue))
					Expect(empty).To(BeFalse())
				} else {
					Expect(state).To(Equal(IndexStateReadable))
					Expect(empty).To(BeTrue())
				}
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			oi.cleanupPendingQueueHeartbeat(oi.sessionHeartbeat)
		})
	}

	for _, mode := range []string{"single", "batch", "delete-where"} {
		for _, twoHandles := range []bool{false, true} {
			It(fmt.Sprintf("excludes checked publication before queue buffering mode=%s twoHandles=%t", mode, twoHandles), func() {
				md, index := makeMetadata()
				root := specSubspace()
				seed(md, index, root)
				tx, err := sharedDB.CreateWritableTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				barrier := &stateWriteBarrier{WritableTransaction: tx, key: root.Sub(int64(9), index.SubspaceTupleKey(), int64(9)).Bytes(), written: make(chan struct{}), resume: make(chan struct{})}
				rc := NewFDBRecordContext(&queueCapacityBarrier{barrier}, nil)
				writer := open(rc, md, root)
				setter := writer
				if twoHandles {
					setter = open(rc, md, root)
				}
				defer func() {
					select {
					case <-barrier.resume:
					default:
						close(barrier.resume)
					}
				}()
				done := make(chan error, 1)
				go func() {
					var err error
					switch mode {
					case "single":
						_, err = writer.SaveRecord(order(1, 7))
					case "batch":
						_, err = writer.SaveRecordBatch([]proto.Message{order(1, 7)})
					case "delete-where":
						err = writer.DeleteRecordsWhere(tuple.Tuple{int64(7)})
					}
					done <- err
				}()
				Eventually(barrier.written, "5s").Should(BeClosed())
				Expect(rc.HasVersionMutations()).To(BeFalse(), "barrier must precede enqueue buffering")
				unprotected := setter.indexStateView.maintenance.TryLock()
				if unprotected {
					setter.indexStateView.maintenance.Unlock()
				}
				transition := make(chan error, 1)
				go func() { _, err := setter.MarkIndexReadable(index.Name); transition <- err }()
				close(barrier.resume)
				Eventually(done, "5s").Should(Receive(BeNil()))
				var transitionErr error
				Eventually(transition, "5s").Should(Receive(&transitionErr))
				var notBuilt *IndexNotBuiltError
				Expect(errors.As(transitionErr, &notBuilt)).To(BeTrue())
				Expect(notBuilt.PendingWrites).To(BeTrue())
				Expect(unprotected).To(BeFalse())
			})
		}
	}
	for _, tc := range []struct {
		name            string
		format          int32
		request, mutual bool
		queued          int
	}{
		{"requested capable", 15, true, false, 1},
		{"unrequested", 15, false, false, 0},
		{"old format", 14, true, false, 0},
		{"mutual", 15, true, true, 0},
	} {
		It("selects fresh queue targets for "+tc.name, func() {
			md, index := makeMetadata()
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
				Expect(err).NotTo(HaveOccurred())
				store.storeHeader.FormatVersion = proto.Int32(tc.format)
				Expect(store.writeStoreHeader(store.storeHeader)).To(Succeed())
				// A fresh session starts from DISABLED, as Java's queue tests do.
				Expect(disableIndexes(store, md.GetIndex("ordinary"), index)).To(Succeed())
				policy := &IndexingPolicy{PendingWriteQueueIndexes: map[string]bool{index.Name: tc.request, "ordinary": tc.request}}
				oi := &OnlineIndexer{targetIndexes: []*Index{md.GetIndex("ordinary"), index}, policy: policy, mutual: tc.mutual}
				queued, _, err := oi.prepareIndexingState(store)
				Expect(err).NotTo(HaveOccurred())
				Expect(queued).To(HaveLen(tc.queued))
				Expect(store.GetIndexState("ordinary")).To(Equal(IndexStateWriteOnly))
				want := IndexStateWriteOnly
				if tc.queued != 0 {
					want = IndexStateWriteOnlyWithQueue
					Expect(queued[0]).To(Equal(index))
				}
				Expect(store.GetIndexState(index.Name)).To(Equal(want))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
	It("preserves resumed queue state despite a changed request policy", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			_, err := store.SaveRecord(order(1, 7))
			Expect(err).NotTo(HaveOccurred())
			oi := &OnlineIndexer{targetIndexes: []*Index{index}}
			queued, _, err := oi.prepareIndexingState(store)
			Expect(err).NotTo(HaveOccurred())
			Expect(queued).To(Equal([]*Index{index}))
			Expect(store.IsIndexWriteOnlyWithQueue(index.Name)).To(BeTrue())
			Expect(rc.HasVersionMutations()).To(BeTrue(), "resume must not clear queued user writes")
			Expect(oi.policy.GetPendingWriteQueueIndexesMaxDrainAttempts()).To(Equal(int64(100)))
			Expect((&IndexingPolicy{PendingWriteQueueIndexesMaxDrainAttempts: proto.Int64(0)}).GetPendingWriteQueueIndexesMaxDrainAttempts()).To(BeZero())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("rejects mixed ordinary queued resumed followers in either primary order", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			_, err := store.MarkIndexWriteOnly("ordinary")
			Expect(err).NotTo(HaveOccurred())
			for _, targets := range [][]*Index{{index, md.GetIndex("ordinary")}, {md.GetIndex("ordinary"), index}} {
				oi := &OnlineIndexer{targetIndexes: targets, policy: &IndexingPolicy{ForceStampOverwrite: true}}
				_, _, err := oi.prepareIndexingState(store)
				var invalid *IndexingValidationError
				Expect(errors.As(err, &invalid)).To(BeTrue())
				Expect(invalid.IndexName).To(Equal(targets[0].Name))
				Expect(invalid.TargetIndexName).To(Equal(targets[1].Name))
				stamp, err := store.LoadIndexingTypeStamp(targets[0])
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil(), "reject before overwriting build stamps")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("rejects mutual queued takeover before fresh force-overwrite shortcuts", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			oi := &OnlineIndexer{targetIndexes: []*Index{index}, mutual: true, policy: &IndexingPolicy{ForceStampOverwrite: true}}
			_, _, err := oi.prepareIndexingState(store)
			var core *RecordCoreError
			Expect(errors.As(err, &core)).To(BeTrue())
			Expect(core.Message).To(Equal("Mutual indexing cannot continue a pending write queue index build"))
			err = oi.setIndexingTypeOrThrowForIndex(store, false, index, oi.buildIndexingStamp())
			Expect(errors.As(err, &core)).To(BeTrue())
			stamp, err := store.LoadIndexingTypeStamp(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(stamp).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("refuses a queued follower of a DISABLED primary as a state mismatch, as Java does", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			_, err := store.MarkIndexDisabled("ordinary")
			Expect(err).NotTo(HaveOccurred())
			oi := &OnlineIndexer{targetIndexes: []*Index{md.GetIndex("ordinary"), index}, mutual: true}
			_, _, err = oi.prepareIndexingState(store)
			var invalid *IndexingValidationError
			Expect(errors.As(err, &invalid)).To(BeTrue(), "error: %v", err)
			Expect(invalid.Message).To(Equal("A target index state doesn't match the primary index state"))
			Expect(invalid.TargetIndexName).To(Equal(index.Name))
			Expect(invalid.TargetIndexState).To(Equal(IndexStateWriteOnlyWithQueue))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("rebuilds a queued index under a mutual REBUILD but will not continue it", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := open(rc, md, root).SaveRecord(order(1, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		mutual := func(policy *IndexingPolicy) (int64, error) {
			oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).
				SetIndex(index).SetFormatVersion(15).SetMutualIndexing().SetPolicy(policy).Build()
			Expect(err).NotTo(HaveOccurred())
			return oi.BuildIndex(ctx)
		}
		_, err = mutual(nil)
		var core *RecordCoreError
		Expect(errors.As(err, &core)).To(BeTrue(), "error: %v", err)
		Expect(core.Message).To(Equal("Mutual indexing cannot continue a pending write queue index build"))

		// Java clears the index to WRITE_ONLY first, declines the queue for a
		// mutual build, and builds it.
		total, err := mutual(&IndexingPolicy{IfWriteOnly: DesiredActionRebuild})
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(Equal(int64(1)))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
			empty, err := store.isIndexPendingQueueEmpty(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(empty).To(BeTrue(), "the rebuild left the cleared queue's entries behind")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("fails a MARK_READABLE session over a non-empty queue at once rather than retrying a drain it never runs", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			if _, err := NewIndexingRangeSet(root, index).InsertRange(rc.Transaction(), nil, nil, true); err != nil {
				return nil, err
			}
			_, err := store.SaveRecord(order(1, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		counter := &batchMutationTransactor{Transactor: sharedDB.transactor}
		oi, err := NewOnlineIndexerBuilder().SetDatabase(NewFDBDatabaseWithTransactor(counter, sharedDB.db)).
			SetMetaData(md).SetSubspace(root).SetIndex(index).SetFormatVersion(15).
			SetPolicy(&IndexingPolicy{IfWriteOnly: DesiredActionMarkReadable}).Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = oi.BuildIndex(ctx)
		var notBuilt *IndexNotBuiltError
		Expect(errors.As(err, &notBuilt)).To(BeTrue(), "error: %v", err)
		Expect(notBuilt.PendingWrites).To(BeTrue())
		// One transaction starts the session and one tries to publish.
		Expect(counter.attempts).To(Equal(2))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateWriteOnlyWithQueue))
			empty, err := store.isIndexPendingQueueEmpty(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(empty).To(BeFalse(), "the queued write was dropped")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("queues single batch and DELETE_WHERE while ordinary indexes remain maintained", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateWriteOnlyWithQueue))
			Expect(store.IsIndexWriteOnly(index.Name)).To(BeTrue())
			Expect(store.IsIndexWriteOnlyNoQueue(index.Name)).To(BeFalse())
			Expect(store.IsIndexWriteOnlyWithQueue(index.Name)).To(BeTrue())
			_, err := store.SaveRecord(order(1, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecordBatch([]proto.Message{order(2, 7), order(3, 8)})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.DeleteRecordsWhere(tuple.Tuple{int64(7)})).To(Succeed())
			_, err = store.MarkIndexReadable(index.Name)
			var notBuilt *IndexNotBuiltError
			Expect(errors.As(err, &notBuilt)).To(BeTrue())
			Expect(notBuilt.PendingWrites).To(BeTrue())
			maintainer, err := store.GetIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			graph, err := maintainer.(*vectorIndexMaintainer).SearchKNN(tuple.Tuple{int64(8)}, []float64{42}, 100, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(graph).To(BeEmpty(), "user dispatch must not update the queued graph")
			begin, end := store.indexSubspace(md.GetIndex("ordinary")).FDBRangeKeys()
			rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1), "ordinary maintenance and DELETE_WHERE remain immediate")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			cursor := store.indexingPendingWriteQueue(index, 100).GetQueueCursor(rc, ForwardScan(), nil)
			defer cursor.Close()
			operations := []gen.PendingWritesQueueEntry_Operation{}
			for {
				row, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !row.HasNext() {
					break
				}
				operations = append(operations, row.GetValue().Payload.GetOperation())
				Expect(store.replayPendingIndexWrite(index, row.GetValue())).To(Succeed())
			}
			Expect(operations).To(Equal([]gen.PendingWritesQueueEntry_Operation{gen.PendingWritesQueueEntry_UPDATE, gen.PendingWritesQueueEntry_UPDATE, gen.PendingWritesQueueEntry_UPDATE, gen.PendingWritesQueueEntry_DELETE_WHERE}))
			_, err := store.MarkIndexReadable(index.Name)
			Expect(err).NotTo(HaveOccurred())
			found, err := store.SearchVectorIndexWithPrefix(index, tuple.Tuple{int64(8)}, []float64{42}, 100, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(HaveLen(1))
			Expect(found[0].PrimaryKey).To(Equal(tuple.Tuple{int64(8), int64(3)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("rejects format and capability requests without changing state or data", func() {
		md, index := makeMetadata()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
			var unsupported *UnsupportedFeatureForFormatVersionError
			Expect(errors.As(err, &unsupported)).To(BeTrue())
			Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
			store.storeHeader.FormatVersion = proto.Int32(15)
			Expect(store.writeStoreHeader(store.storeHeader)).To(Succeed())
			_, err = store.SaveRecord(order(1, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClearAndMarkIndexWriteOnlyWithQueue("ordinary")
			var core *RecordCoreError
			Expect(errors.As(err, &core)).To(BeTrue())
			Expect(store.GetIndexState("ordinary")).To(Equal(IndexStateReadable))
			begin, end := store.indexSubspace(md.GetIndex("ordinary")).FDBRangeKeys()
			rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	for _, kind := range []string{"version-columns", "spfresh"} {
		It("rejects queued eligibility for "+kind, func() {
			index := NewVectorIndex("ineligible", KeyWithValue(Concat(VersionKey(), Field("price")), 1), 1)
			if kind == "spfresh" {
				index = NewIndex("ineligible", Field("price"))
				index.Type = IndexTypeVectorSPFresh
				index.Options = map[string]string{IndexOptionSPFreshNumDimensions: "1"}
			}
			builder := baseBuilder()
			builder.SetStoreRecordVersions(true)
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
				Expect(err).NotTo(HaveOccurred())
				store.storeHeader.FormatVersion = proto.Int32(15)
				Expect(store.writeStoreHeader(store.storeHeader)).To(Succeed())
				_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
				var core *RecordCoreError
				Expect(errors.As(err, &core)).To(BeTrue())
				Expect(core.IndexName).To(Equal(index.Name))
				Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
	for _, disable := range []bool{false, true} {
		name := "fails the write"
		if disable {
			name = "disables at commit"
		}
		It(name+" when the configured queue capacity is exceeded", func() {
			timer := NewStoreTimer()
			md, index := makeMetadata()
			root := specSubspace()
			seed(md, index, root)
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				rc.SetTimer(timer)
				Expect(rc.PendingWriteQueueOptions()).To(Equal(PendingWriteQueueOptions{MaximumSize: 100000, DisableIndexOnOverflow: true}))
				rc.SetPendingWriteQueueOptions(PendingWriteQueueOptions{MaximumSize: 1, DisableIndexOnOverflow: disable})
				store := open(rc, md, root)
				_, err := store.SaveRecord(order(1, 7))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(order(2, 7))
				if !disable {
					var overflow *PendingWritesQueueTooLargeError
					Expect(errors.As(err, &overflow)).To(BeTrue())
					return nil, err
				}
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateWriteOnlyWithQueue), "disable must wait until the writer has released its read gate")
				return nil, nil
			})
			if disable {
				Expect(err).NotTo(HaveOccurred())
				Expect(timer.GetCount(CountPendingWritesQueueOverflowDisabledIndex)).To(Equal(int64(1)))
			} else {
				Expect(timer.GetCount(CountPendingWritesQueueOverflowDisabledIndex)).To(BeZero())
				var overflow *PendingWritesQueueTooLargeError
				Expect(errors.As(err, &overflow)).To(BeTrue())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := open(rc, md, root)
				expected := IndexStateWriteOnlyWithQueue
				if disable {
					expected = IndexStateDisabled
				}
				Expect(store.GetIndexState(index.Name)).To(Equal(expected))
				empty, err := store.indexingPendingWriteQueue(index, 100).IsQueueEmpty(rc)
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeTrue())
				record, err := store.LoadRecord(tuple.Tuple{int64(7), int64(2)})
				Expect(err).NotTo(HaveOccurred())
				if disable {
					Expect(record).NotTo(BeNil())
				} else {
					Expect(record).To(BeNil())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
	It("cancels buffered entries on DeleteAllRecords and overflow callbacks on DeleteStore", func() {
		md, index := makeMetadata()
		root := specSubspace()
		seed(md, index, root)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc, md, root)
			_, err := store.SaveRecord(order(1, 7))
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.HasVersionMutations()).To(BeTrue())
			Expect(store.DeleteAllRecords()).To(Succeed())
			Expect(rc.HasVersionMutations()).To(BeFalse())
			rc.SetPendingWriteQueueOptions(PendingWriteQueueOptions{MaximumSize: 1, DisableIndexOnOverflow: true})
			_, err = store.SaveRecord(order(2, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(order(3, 7))
			Expect(err).NotTo(HaveOccurred())
			name := pendingWriteCommitCheckPrefix(root) + "overflow:" + index.Name
			Expect(rc.getCommitCheck(name)).NotTo(BeNil())
			Expect(DeleteStore(rc, root)).To(Succeed())
			Expect(rc.getCommitCheck(name)).To(BeNil())
			Expect(rc.HasVersionMutations()).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			begin, end := root.FDBRangeKeys()
			rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty(), "commit callback must not resurrect deleted store data")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Delay the real capacity read, before a queued entry reaches the context's
// mutation registry. No read result or storage behavior is substituted.
type queueCapacityBarrier struct{ *stateWriteBarrier }

func (tx *queueCapacityBarrier) Snapshot() fdb.ReadTransaction {
	return &queueCapacitySnapshot{ReadTransaction: tx.WritableTransaction.Snapshot(), barrier: tx.stateWriteBarrier}
}

type queueCapacitySnapshot struct {
	fdb.ReadTransaction
	barrier *stateWriteBarrier
}

func (tx *queueCapacitySnapshot) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	if bytes.Equal(key.FDBKey(), tx.barrier.key) && tx.barrier.paused.CompareAndSwap(false, true) {
		close(tx.barrier.written)
		<-tx.barrier.resume
	}
	return tx.ReadTransaction.Get(key)
}
