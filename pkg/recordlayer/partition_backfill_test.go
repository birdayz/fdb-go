package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ = Describe("Partition backfill participation", func() {
	ctx := context.Background()
	for _, mode := range []string{"inline", "by-records", "by-index", "multi-target", "mutual", "resumed"} {
		It("validates routes without changing directory bytes during "+mode, func() {
			b := partitionTestBuilder()
			p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
			source := NewIndex("backfill_source", Concat(p(), Field("price")))
			first := NewIndex("backfill_first", Concat(p(), Field("quantity")))
			second := NewIndex("backfill_second", Concat(p(), Field("coord_y")))
			for _, idx := range []*Index{source, first, second} {
				b.AddIndex("Order", idx)
			}
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			var protocol []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				for i, x := range []int64{-12, -11, 0, 10, 100} {
					record := partitionOrder(int64(i+1), x, int32(i))
					record.Quantity = proto.Int32(int32(i + 1))
					record.CoordY = proto.Int64(int64(i + 1))
					_, err := store.SaveRecord(record)
					Expect(err).NotTo(HaveOccurred())
				}
				protocol = partitionProtocolRows(rc, root)
				Expect(protocol).To(HaveLen(10))
				for _, idx := range []*Index{first, second} {
					_, err := store.ClearAndMarkIndexWriteOnly(idx.Name)
					Expect(err).NotTo(HaveOccurred())
					rows, err := rc.Transaction().GetRange(store.indexSubspace(idx), fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(BeEmpty(), "real empty target before backfill")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			makeIndexer := func() *OnlineIndexer {
				b := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetIndex(first).SetLimit(2)
				if mode == "by-index" {
					b.SetSourceIndex(source).SetPolicy(&IndexingPolicy{ForbidRecordScan: true})
				}
				if mode == "mutual" {
					b.SetMutualIndexing()
				}
				if mode == "multi-target" {
					b.SetTargetIndexes([]*Index{first, second})
				}
				oi, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				return oi
			}
			if mode == "inline" {
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(rc, md, root)
					return nil, store.RebuildIndex(first)
				})
				Expect(err).NotTo(HaveOccurred())
			} else {
				if mode == "resumed" {
					oi := makeIndexer()
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := oi.openStore(rc)
						if err != nil {
							return nil, err
						}
						return nil, store.SaveIndexingTypeStamp(first, oi.buildIndexingStamp())
					})
					Expect(err).NotTo(HaveOccurred())
					n, more, err := oi.buildRange(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(n).To(Equal(int64(2)))
					Expect(more).To(BeTrue())
				}
				oi := makeIndexer()
				n, err := oi.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(n).To(BeNumerically(">", 0), "must really enter maintenance loops")
				Expect(oi.LastBuildOutcome()).To(Equal(IndexBuildOutcomeBuilt))
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				Expect(partitionProtocolRows(rc, root)).To(Equal(protocol))
				targets := []*Index{first}
				if mode == "multi-target" {
					targets = append(targets, second)
				}
				for _, idx := range targets {
					Expect(store.IsIndexReadable(idx.Name)).To(BeTrue())
					rows, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(5))
					for _, row := range rows {
						Expect(row.Record.PrimaryKey).To(Equal(row.IndexEntry.PrimaryKey()))
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, mode := range []string{"inline", "by-records", "by-index", "multi-target", "mutual"} {
		It("refuses orphaned routes rather than activating them during "+mode, func() {
			b := partitionTestBuilder()
			p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
			source := NewIndex("orphan_source", Concat(p(), Field("price")))
			first := NewIndex("orphan_first", Concat(p(), Field("quantity")))
			second := NewIndex("orphan_second", Concat(p(), Field("coord_y")))
			for _, idx := range []*Index{source, first, second} {
				b.AddIndex("Order", idx)
			}
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			var protocol []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(partitionOrder(2, 10, 8))
				Expect(err).NotTo(HaveOccurred())
				for _, idx := range []*Index{first, second} {
					_, err := store.ClearAndMarkIndexWriteOnly(idx.Name)
					Expect(err).NotTo(HaveOccurred())
				}
				rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"active", int64(-2)}))
				protocol = partitionProtocolRows(rc, root)
				Expect(protocol).To(HaveLen(5))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			if mode == "inline" {
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(rc, md, root)
					before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					err = store.RebuildIndex(first)
					var state *PartitionStateError
					Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
					after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(after).To(Equal(before), "preflight-only refusal is committable")
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			} else {
				b := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetIndex(first).SetLimit(1)
				if mode == "by-index" {
					b.SetSourceIndex(source).SetPolicy(&IndexingPolicy{ForbidRecordScan: true})
				}
				if mode == "mutual" {
					b.SetMutualIndexing()
				}
				if mode == "multi-target" {
					b.SetTargetIndexes([]*Index{first, second})
				}
				oi, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				_, err = oi.BuildIndex(ctx)
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				Expect(partitionProtocolRows(rc, root)).To(Equal(protocol))
				Expect(store.IsIndexReadable(first.Name)).To(BeFalse())
				records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(2))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, kind := range []string{IndexTypeValue, IndexTypeText} {
		It("refuses queued admission and valid unexpected replay without effects for partitioned "+kind, func() {
			b := partitionTestBuilder()
			idx := NewIndex("partition_queue_"+kind, Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
			if kind == IndexTypeText {
				idx = NewTextIndex(idx.Name, GroupBy(Nest("flower", Field("type")), PartitionExpr(Field("coord_x"), 10)))
			}
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			var admissionBytes []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(formatVersionPendingWrites).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				record := partitionOrder(1, -11, 7)
				record.Flower = &demopb.Flower{Type: proto.String("connection timeout")}
				_, err = store.SaveRecord(record)
				Expect(err).NotTo(HaveOccurred())
				before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				for _, request := range []func(string) (bool, error){store.MarkIndexWriteOnlyWithQueue, store.ClearAndMarkIndexWriteOnlyWithQueue} {
					_, err := request(idx.Name)
					var state *PartitionStateError
					Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
					Expect(state.IndexName).To(Equal(idx.Name))
				}
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				admissionBytes = after
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				committed, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(committed).To(Equal(admissionBytes))
				stored, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(stored).NotTo(BeNil())
				// Raw queue contents model unexpected persisted state, not admitted
				// queued VALUE/TEXT maintenance. The plain WithQueue fixture is the positive producer/replay control.
				encoded, err := store.serializePendingRecord(stored)
				Expect(err).NotTo(HaveOccurred())
				update, err := anypb.New(&gen.OldAndNewRecords{NewRecord: encoded})
				Expect(err).NotTo(HaveOccurred())
				deletion, err := anypb.New(&gen.DeleteWhere{Prefix: tuple.Tuple{int64(-2)}.Pack()})
				Expect(err).NotTo(HaveOccurred())
				queue := store.indexingPendingWriteQueue(idx, 0)
				Expect(queue.Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum(), Data: update}, 0)).To(Succeed())
				Expect(queue.Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_DELETE_WHERE.Enum(), Data: deletion}, 0)).To(Succeed())
				store.setIndexState(idx.Name, IndexStateWriteOnlyWithQueue)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			for attempt := 0; attempt < 2; attempt++ {
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(rc, md, root)
					entries, err := AsList(ctx, store.indexingPendingWriteQueue(idx, 0).GetQueueCursor(rc, ForwardScan(), nil))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(2))
					var update gen.OldAndNewRecords
					Expect(UnmarshalPendingQueueAny(entries[0].Payload.GetData(), &update)).To(Succeed())
					decoded, err := store.deserializePendingRecord(update.GetNewRecord())
					Expect(err).NotTo(HaveOccurred())
					Expect(decoded.PrimaryKey).To(Equal(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)}))
					var deletion gen.DeleteWhere
					Expect(UnmarshalPendingQueueAny(entries[1].Payload.GetData(), &deletion)).To(Succeed())
					Expect(deletion.GetPrefix()).To(Equal(tuple.Tuple{int64(-2)}.Pack()))
					indexed, err := rc.Transaction().GetRange(store.indexSubspace(idx), fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(indexed).NotTo(BeEmpty())
					before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					var refused *PartitionStateError
					Expect(errors.As(store.enqueuePendingIndexWrite(idx, entries[0].Payload), &refused)).To(BeTrue())
					Expect(errors.As(store.DeleteRecordsWhere(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2)}), &refused)).To(BeTrue())
					for _, entry := range entries {
						var state *PartitionStateError
						Expect(errors.As(store.replayPendingIndexWrite(idx, entry), &state)).To(BeTrue())
						Expect(state.IndexName).To(Equal(idx.Name))
					}
					after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(after).To(Equal(before))
					Expect(rc.HasVersionMutations()).To(BeFalse())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	}

	for _, corruption := range []string{"misrouted", "wrong-type"} {
		It("rejects a "+corruption+" body during backfill and blocks deliberate commit after rebuild effects", func() {
			b := partitionTestBuilder()
			idx := NewIndex("misrouted_backfill", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(partitionOrder(2, 10, 8))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.ClearAndMarkIndexWriteOnly(idx.Name)
				Expect(err).NotTo(HaveOccurred())
				typeKey := md.GetRecordType("Order").GetRecordTypeKey()
				key := root.Sub(RecordKey).Pack(tuple.Tuple{typeKey, int64(-2), int64(-11), int64(1), int64(0)})
				data := rc.Transaction().Get(key).MustGet()
				Expect(data).NotTo(BeEmpty())
				if corruption == "wrong-type" {
					data, err = serializeUnionOver(&demopb.Customer{CustomerId: proto.Int64(1)}, md.GetRecordType("Customer"), nil)
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Set(key, data)
				} else {
					rc.Transaction().Clear(key)
					rc.Transaction().Set(root.Sub(RecordKey).Pack(tuple.Tuple{typeKey, int64(1), int64(-11), int64(1), int64(0)}), data)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			rc, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer rc.Cancel()
			store := partitionProtocolStore(rc, md, root)
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			if corruption == "wrong-type" {
				rec, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec.RecordType.Name).To(Equal("Customer"))
				Expect(store.shouldIndexRecordForIndex(rec, idx)).To(BeFalse(), "real body would otherwise be skipped by the target filter")
				oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetIndex(idx).Build()
				Expect(err).NotTo(HaveOccurred())
				indexed, err := oi.indexScannedRecord(store, rec)
				Expect(indexed).To(BeFalse())
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue())
				Expect(partitionWholeStore(rc, store)).To(Equal(before), "type refusal precedes online maintenance effects")
			}
			err = store.RebuildIndex(idx)
			var state *PartitionStateError
			Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
			if corruption == "misrouted" {
				Expect(state.Reason).To(Equal("record does not match its physical primary key"))
			} else {
				Expect(state.Reason).To(Equal("record type does not match physical partition prefix"))
			}
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(6))
			var incomplete *PartitionMutationError
			commitErr := rc.Commit()
			Expect(errors.As(commitErr, &incomplete)).To(BeTrue())
			Expect(errors.Is(commitErr, err)).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
