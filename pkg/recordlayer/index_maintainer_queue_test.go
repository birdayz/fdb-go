package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ = Describe("Index maintainer pending queue", func() {
	It("explicitly refuses unsupported maintainers", func() {
		index := NewIndex("unsupported", Field("price"))
		base := standardIndexMaintainer{index: index}
		for _, maintainer := range []IndexMaintainer{
			&base, &atomicMutationIndexMaintainer{index: index}, &bitmapValueIndexMaintainer{index: index},
			&maxEverVersionIndexMaintainer{index: index}, &textIndexMaintainer{index: index}, &versionIndexMaintainer{index: index},
			&rankIndexMaintainer{standardIndexMaintainer: base}, &multidimensionalIndexMaintainer{standardIndexMaintainer: base},
			&timeWindowLeaderboardIndexMaintainer{standardIndexMaintainer: base},
		} {
			Expect(maintainer.IsPendingWriteQueueAllowed()).To(BeFalse())
			_, err := maintainer.SerializePendingWriteQueue(nil, nil)
			var unsupported *UnsupportedOperationError
			Expect(errors.As(err, &unsupported)).To(BeTrue())
			Expect(unsupported.Message).To(Equal("unsupported does not support the pending write queue"))
			Expect(errors.As(maintainer.UpdateFromQueue(nil), &unsupported)).To(BeTrue())
		}
		window := &slidingWindowIndexMaintainer{delegate: &base}
		Expect(window.IsPendingWriteQueueAllowed()).To(BeFalse())
	})
})

var _ = Describe("Index pending queue application", func() {
	It("applies ordered updates and computed DELETE_WHERE prefixes before clearing committed entries", func() {
		ctx := context.Background()
		root := specSubspace()
		index := newValueWithQueueIndex("pending_apply", Concat(Field("quantity"), Field("price")))
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			Expect(err).NotTo(HaveOccurred())
			maintainer, err := store.GetIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			queue := store.indexingPendingWriteQueue(index, 100)
			Expect(queue.entries.Bytes()).To(Equal(root.Sub(int64(9), index.SubspaceTupleKey(), int64(8)).Bytes()))
			Expect(queue.counter.Bytes()).To(Equal(root.Sub(int64(9), index.SubspaceTupleKey(), int64(9)).Bytes()))
			incarnation, err := store.GetIncarnation()
			Expect(err).NotTo(HaveOccurred())
			for _, partition := range []int32{7, 8} {
				record := &FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(int64(partition)), Quantity: proto.Int32(partition), Price: proto.Int32(42)}, PrimaryKey: tuple.Tuple{int64(partition)}, RecordType: md.GetRecordType("Order")}
				data, err := maintainer.SerializePendingWriteQueue(nil, record)
				Expect(err).NotTo(HaveOccurred())
				Expect(queue.Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum(), Data: data}, incarnation)).To(Succeed())
			}
			deletion, err := anypb.New(&gen.DeleteWhere{Prefix: tuple.Tuple{int64(7)}.Pack()})
			Expect(err).NotTo(HaveOccurred())
			return nil, queue.Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_DELETE_WHERE.Enum(), Data: deletion}, incarnation)
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			queue := store.indexingPendingWriteQueue(index, 100)
			cursor := queue.GetQueueCursor(rc, ForwardScan(), nil)
			defer cursor.Close()
			count := 0
			for {
				row, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !row.HasNext() {
					break
				}
				count++
				Expect(store.replayPendingIndexWrite(index, row.GetValue())).To(Succeed())
				found, err := queuedIndexPrimaryKeys(store, index, tuple.Tuple{int64(7)})
				Expect(err).NotTo(HaveOccurred())
				if count < 3 {
					Expect(found).To(HaveLen(1))
				} else {
					Expect(found).To(BeEmpty())
				}
			}
			Expect(count).To(Equal(3))
			empty, err := queue.IsQueueEmpty(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(empty).To(BeTrue())
			size, err := queue.GetQueueSizeNoConflict(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).NotTo(BeNil())
			Expect(*size).To(BeZero())
			found, err := queuedIndexPrimaryKeys(store, index, tuple.Tuple{int64(8)})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(Equal([]tuple.Tuple{{int64(8)}}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	for _, operation := range []gen.PendingWritesQueueEntry_Operation{gen.PendingWritesQueueEntry_UPDATE, gen.PendingWritesQueueEntry_DELETE_WHERE} {
		It("retains a committed entry when "+operation.String()+" replay fails", func() {
			ctx := context.Background()
			root := specSubspace()
			index := newValueWithQueueIndex("pending_failure", Field("price"))
			builder := baseBuilder()
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				Expect(err).NotTo(HaveOccurred())
				return nil, store.indexingPendingWriteQueue(index, 100).Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: operation.Enum(), Data: &anypb.Any{TypeUrl: "wrong/type"}}, 0)
			})
			Expect(err).NotTo(HaveOccurred())
			for attempt := 0; attempt < 2; attempt++ {
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
					Expect(err).NotTo(HaveOccurred())
					queue := store.indexingPendingWriteQueue(index, 100)
					cursor := queue.GetQueueCursor(rc, ForwardScan(), nil)
					defer cursor.Close()
					row, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(row.HasNext()).To(BeTrue())
					var core *RecordCoreError
					Expect(errors.As(store.replayPendingIndexWrite(index, row.GetValue()), &core)).To(BeTrue())
					Expect(core.Cause).NotTo(BeNil())
					empty, err := queue.IsQueueEmpty(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(empty).To(BeFalse())
					size, err := queue.GetQueueSizeNoConflict(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(size).NotTo(BeNil())
					Expect(*size).To(Equal(int64(1)))
					// Commit deliberately, proving retention is not just transaction rollback.
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	}
})

var _ = Describe("Pending queue checked readability", func() {
	for _, otherHandle := range []bool{false, true} {
		for _, allowPending := range []bool{false, true} {
			name := "same handle"
			if otherHandle {
				name = "other handle"
			}
			if allowPending {
				name += " unique-pending setter"
			}
			It("rejects buffered entries through "+name+" and follows context clears", func() {
				ctx := context.Background()
				root := specSubspace()
				index := newValueWithQueueIndex("pending_readable", Field("price"))
				otherIndex := newValueWithQueueIndex("other_queue", Field("price"))
				builder := baseBuilder()
				builder.AddIndex("Order", index)
				builder.AddIndex("Order", otherIndex)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
					Expect(err).NotTo(HaveOccurred())
					_, err = store.MarkIndexWriteOnly(index.Name)
					Expect(err).NotTo(HaveOccurred())
					reader := store
					if otherHandle {
						reader, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
						Expect(err).NotTo(HaveOccurred())
					}
					data, err := anypb.New(&gen.OldAndNewRecords{})
					Expect(err).NotTo(HaveOccurred())
					payload := &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum(), Data: data}
					queue := store.indexingPendingWriteQueue(index, 100)
					Expect(queue.Enqueue(rc, payload, 0)).To(Succeed())
					Expect(store.indexingPendingWriteQueue(otherIndex, 100).Enqueue(rc, payload, 0)).To(Succeed())
					persistedEmpty, err := queue.IsQueueEmpty(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(persistedEmpty).To(BeTrue())
					mark := reader.MarkIndexReadable
					if allowPending {
						mark = reader.MarkIndexReadableOrUniquePending
					}
					changed, err := mark(index.Name)
					Expect(changed).To(BeFalse())
					var notBuilt *IndexNotBuiltError
					Expect(errors.As(err, &notBuilt)).To(BeTrue())
					Expect(notBuilt.PendingWrites).To(BeTrue())
					Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateWriteOnly))
					keys, err := fdb.PrefixRange(queue.entries.Bytes())
					Expect(err).NotTo(HaveOccurred())
					rc.ClearRange(keys)
					Expect(rc.HasVersionMutations()).To(BeTrue(), "the other index still has pending entries")
					changed, err = mark(index.Name)
					Expect(err).NotTo(HaveOccurred())
					Expect(changed).To(BeTrue())
					Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
	It("rejects persisted entries despite a zero size counter until they are applied", func() {
		ctx := context.Background()
		root := specSubspace()
		index := newValueWithQueueIndex("persisted_readable", Field("price"))
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexWriteOnly(index.Name)
			Expect(err).NotTo(HaveOccurred())
			data, err := anypb.New(&gen.OldAndNewRecords{})
			Expect(err).NotTo(HaveOccurred())
			return nil, store.indexingPendingWriteQueue(index, 100).Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum(), Data: data}, 0)
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			Expect(err).NotTo(HaveOccurred())
			queue := store.indexingPendingWriteQueue(index, 100)
			rc.Transaction().Set(fdb.Key(queue.counter.Bytes()), make([]byte, 8))
			_, err = store.MarkIndexReadable(index.Name)
			var notBuilt *IndexNotBuiltError
			Expect(errors.As(err, &notBuilt)).To(BeTrue())
			Expect(notBuilt.PendingWrites).To(BeTrue())
			cursor := queue.GetQueueCursor(rc, ForwardScan(), nil)
			defer cursor.Close()
			row, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.HasNext()).To(BeTrue())
			Expect(store.replayPendingIndexWrite(index, row.GetValue())).To(Succeed())
			_, err = store.MarkIndexReadable(index.Name)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Pending queue closeout conflicts", func() {
	for _, writerFirst := range []bool{false, true} {
		label := "closeout commits first"
		if writerFirst {
			label = "queued writer commits first"
		}
		It("rejects the losing transaction when "+label, func() {
			ctx := context.Background()
			root := specSubspace()
			index := newValueWithQueueIndex("pending_conflict", Field("price"))
			builder := baseBuilder()
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexWriteOnly(index.Name)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			open := func() (*FDBRecordContext, *FDBRecordStore) {
				tx, err := sharedDB.CreateWritableTransaction()
				Expect(err).NotTo(HaveOccurred())
				rc := NewFDBRecordContext(tx, nil)
				DeferCleanup(rc.Cancel)
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
				Expect(err).NotTo(HaveOccurred())
				return rc, store
			}
			reader, readStore := open()
			writer, writeStore := open()
			state, err := writeStore.readIndexState(index.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(Equal(IndexStateWriteOnly))
			data, err := anypb.New(&gen.OldAndNewRecords{})
			Expect(err).NotTo(HaveOccurred())
			Expect(writeStore.indexingPendingWriteQueue(index, 100).Enqueue(writer, &gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum(), Data: data}, 0)).To(Succeed())
			changed, err := readStore.MarkIndexReadable(index.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			var loser error
			if writerFirst {
				Expect(writer.Commit()).To(Succeed())
				loser = reader.Commit()
			} else {
				Expect(reader.Commit()).To(Succeed())
				loser = writer.Commit()
			}
			var conflict fdb.Error
			Expect(errors.As(loser, &conflict)).To(BeTrue())
			Expect(conflict.Code).To(Equal(1020))
		})
	}
})

var _ = Describe("Nested pending queue Any compatibility", func() {
	for _, boundary := range []string{"update", "delete-where"} {
		for _, prefix := range []string{"", "type.googleapis.com/", "custom.example/v1/", "/"} {
			It(fmt.Sprintf("replays boundary=%s prefix=%q with Java URL semantics", boundary, prefix), func() {
				ctx := context.Background()
				root := specSubspace()
				index := newValueWithQueueIndex("nested_any", Concat(Field("quantity"), Field("price")))
				deleting := boundary == "delete-where"
				builder := baseBuilder()
				builder.AddIndex("Order", index)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
					Expect(err).NotTo(HaveOccurred())
					maintainer, err := store.GetIndexMaintainer(index)
					Expect(err).NotTo(HaveOccurred())
					record := &FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(7), Price: proto.Int32(42), CoordX: proto.Int64(3), CoordY: proto.Int64(4)}, PrimaryKey: tuple.Tuple{int64(1)}, RecordType: md.GetRecordType("Order")}
					var data *anypb.Any
					operation := gen.PendingWritesQueueEntry_UPDATE
					if deleting {
						_, err = store.SaveRecord(record.Record)
						Expect(err).NotTo(HaveOccurred())
						if boundary == "delete-where" {
							operation = gen.PendingWritesQueueEntry_DELETE_WHERE
							data, err = anypb.New(&gen.DeleteWhere{Prefix: tuple.Tuple{int64(7)}.Pack()})
						}
					} else {
						data, err = maintainer.SerializePendingWriteQueue(nil, record)
					}
					Expect(err).NotTo(HaveOccurred())
					data.TypeUrl = prefix + string(data.MessageName())
					return nil, store.indexingPendingWriteQueue(index, 100).Enqueue(rc, &gen.PendingWritesQueueEntry{Operation: operation.Enum(), Data: data}, 0)
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
					Expect(err).NotTo(HaveOccurred())
					cursor := store.indexingPendingWriteQueue(index, 100).GetQueueCursor(rc, ForwardScan(), nil)
					defer cursor.Close()
					row, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred(), "the outer envelope is valid")
					Expect(row.HasNext()).To(BeTrue())
					return nil, store.replayPendingIndexWrite(index, row.GetValue())
				})
				if prefix == "" {
					var core *RecordCoreError
					Expect(errors.As(err, &core)).To(BeTrue(), "slashless nested Any must fail: %v", err)
					Expect(core.Cause).NotTo(BeNil())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
					Expect(err).NotTo(HaveOccurred())
					queue := store.indexingPendingWriteQueue(index, 100)
					empty, err := queue.IsQueueEmpty(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(empty).To(Equal(prefix != ""))
					size, err := queue.GetQueueSizeNoConflict(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(size).NotTo(BeNil())
					wantSize := int64(0)
					if prefix == "" {
						wantSize = 1
						cursor := queue.GetQueueCursor(rc, ForwardScan(), nil)
						defer cursor.Close()
						row, err := cursor.OnNext(ctx)
						Expect(err).NotTo(HaveOccurred())
						Expect(row.HasNext()).To(BeTrue(), "failed entry must remain replayable")
					}
					Expect(*size).To(Equal(wantSize))
					wantRows := 1
					if deleting == (prefix != "") {
						wantRows = 0
					}
					rows, err := queuedIndexPrimaryKeys(store, index, tuple.Tuple{int64(7)})
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(wantRows))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
