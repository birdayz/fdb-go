package vectorindex

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ = Describe("Vector index pending queue", func() {
	// A save maintained in its own transaction that leaves a record's vector
	// entry unchanged makes no graph call, as Java's VectorIndexMaintainer
	// (which inherits StandardIndexMaintainer.update's removal of entries
	// common to the old and the new record) makes none: the index's bytes are
	// unchanged (this index has no RaBitQ, so it has no samples; the graph-level
	// spec in vector_index_test.go pins those). Go deleted and re-inserted the
	// node. The pending-write queue does not skip (next spec).
	It("leaves the graph as it is when a save keeps the vector", func() {
		index := recordlayer.NewVectorIndex("unchanged_vector", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 12; i++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Quantity: proto.Int32(1), Price: proto.Int32(int32(i)), CoordX: proto.Int64(0)})
				Expect(err).NotTo(HaveOccurred())
			}
			snapshot := func() []fdb.KeyValue {
				begin, end := store.IndexSubspace(index).FDBRangeKeys()
				kvs, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				return kvs
			}
			before := snapshot()
			Expect(before).NotTo(BeEmpty())
			// Record 5 again, its (quantity, price) entry unchanged, another
			// field changed.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Quantity: proto.Int32(1), Price: proto.Int32(5), CoordX: proto.Int64(99)})
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshot()).To(Equal(before), "the graph's bytes are unchanged")
			// A save that changes the vector does touch the graph: the control.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Quantity: proto.Int32(1), Price: proto.Int32(500), CoordX: proto.Int64(99)})
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshot()).NotTo(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A save queued for a WRITE_ONLY_WITH_QUEUE index is serialized with both
	// its entries, the old and the new, even when they are equal, as Java's
	// VectorIndexMaintainer.serializePendingWriteQueue writes them
	// (VectorIndexMaintainer.java:432-449): the skip of common entries is
	// update's, and the queue's replay deletes then inserts. Both engines;
	// this pins that no skip reaches the maintainer's serialization (it calls
	// the maintainer directly, so the store's queue path and the replay are
	// not covered here).
	It("serializes both entries of a queued save that keeps the vector", func() {
		index := recordlayer.NewVectorIndex("queued_unchanged", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2)
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			maintainer, err := store.GetIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			record := func(other int64) *recordlayer.FDBStoredRecord[proto.Message] {
				return &recordlayer.FDBStoredRecord[proto.Message]{
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(20), CoordX: proto.Int64(other)},
					PrimaryKey: tuple.Tuple{int64(1)}, RecordType: md.GetRecordType("Order"),
				}
			}
			data, err := maintainer.SerializePendingWriteQueue(record(1), record(2))
			Expect(err).NotTo(HaveOccurred())
			var decoded gen.OldAndNewIndexEntries
			Expect(data.UnmarshalTo(&decoded)).To(Succeed())
			Expect(decoded.GetOldEntries()).To(HaveLen(1))
			Expect(decoded.GetNewEntries()).To(HaveLen(1))
			Expect(proto.Equal(decoded.GetOldEntries()[0], decoded.GetNewEntries()[0])).To(BeTrue(), "the two entries are the equal ones")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("replays captured vector entries without reevaluating records or predicates", func() {
		index := recordlayer.NewVectorIndex("queued_vector", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2)
		allow := true
		index.Predicate = func(proto.Message) bool { return allow }
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			maintainer, err := store.GetIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(maintainer.IsPendingWriteQueueAllowed()).To(BeTrue())
			makeRecord := func(x, y int32) *recordlayer.FDBStoredRecord[proto.Message] {
				return &recordlayer.FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(x), Quantity: proto.Int32(y)}, PrimaryKey: tuple.Tuple{int64(1)}, RecordType: md.GetRecordType("Order")}
			}
			original := makeRecord(10, 20)
			data, err := maintainer.SerializePendingWriteQueue(nil, original)
			Expect(err).NotTo(HaveOccurred())
			var decoded gen.OldAndNewIndexEntries
			Expect(data.UnmarshalTo(&decoded)).To(Succeed())
			Expect(decoded.GetNewEntries()).To(HaveLen(1))
			Expect(decoded.GetNewEntries()[0].GetPrimaryKey()).To(Equal(tuple.Tuple{int64(1)}.Pack()))
			original.Record.(*gen.Order).Price = proto.Int32(999)
			allow = false
			Expect(maintainer.UpdateFromQueue(data)).To(Succeed())
			results, err := store.SearchVectorIndex(index, []float64{10, 20}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Distance).To(BeZero())
			allow = true
			moved := makeRecord(30, 40)
			data, err = maintainer.SerializePendingWriteQueue(makeRecord(10, 20), moved)
			Expect(err).NotTo(HaveOccurred())
			allow = false
			Expect(maintainer.UpdateFromQueue(data)).To(Succeed())
			results, err = store.SearchVectorIndex(index, []float64{30, 40}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Distance).To(BeZero())
			filtered, err := maintainer.SerializePendingWriteQueue(nil, moved)
			Expect(err).NotTo(HaveOccurred())
			Expect(filtered.UnmarshalTo(&decoded)).To(Succeed())
			Expect(decoded.GetNewEntries()).To(BeEmpty())
			allow = true
			data, err = maintainer.SerializePendingWriteQueue(moved, nil)
			Expect(err).NotTo(HaveOccurred())
			allow = false
			Expect(maintainer.UpdateFromQueue(data)).To(Succeed())
			results, err = store.SearchVectorIndex(index, []float64{30, 40}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
			for _, bad := range []*anypb.Any{nil, {TypeUrl: "wrong/type"}, {TypeUrl: data.TypeUrl, Value: []byte{0xff}}} {
				var core *recordlayer.RecordCoreError
				Expect(errors.As(maintainer.UpdateFromQueue(bad), &core)).To(BeTrue())
				Expect(core.Message).To(Equal("failed to parse vector index pending write queue entry data"))
				Expect(core.Cause).NotTo(BeNil())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, partitioned := range []bool{false, true} {
		name := "empty key presence"
		if partitioned {
			name = "full overlapping primary key"
		}
		It("preserves "+name+" in captured vector entries", func() {
			builder := baseBuilder()
			expression := recordlayer.KeyWithValue(recordlayer.Field("price"), 0)
			primaryKey := tuple.Tuple{int64(11)}
			prefix := tuple.Tuple{}
			if partitioned {
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("order_id")))
				expression = recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1)
				primaryKey = tuple.Tuple{int64(7), int64(11)}
				prefix = tuple.Tuple{int64(7)}
			}
			index := recordlayer.NewVectorIndex("queued_pk", expression, 1)
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
				Expect(err).NotTo(HaveOccurred())
				maintainer, err := store.GetIndexMaintainer(index)
				Expect(err).NotTo(HaveOccurred())
				record := &recordlayer.FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(11), Quantity: proto.Int32(7), Price: proto.Int32(42)}, PrimaryKey: primaryKey, RecordType: md.GetRecordType("Order")}
				data, err := maintainer.SerializePendingWriteQueue(nil, record)
				Expect(err).NotTo(HaveOccurred())
				var entries gen.OldAndNewIndexEntries
				Expect(data.UnmarshalTo(&entries)).To(Succeed())
				Expect(entries.NewEntries).To(HaveLen(1))
				entry := entries.NewEntries[0]
				Expect(entry.Key).NotTo(BeNil(), "Java explicitly sets the proto2 bytes field even for an empty tuple")
				Expect(entry.Key).To(Equal(prefix.Pack()))
				Expect(entry.PrimaryKey).To(Equal(primaryKey.Pack()))
				Expect(maintainer.UpdateFromQueue(data)).To(Succeed())
				results, err := store.SearchVectorIndexWithPrefix(index, prefix, []float64{42}, 10, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1))
				Expect(results[0].PrimaryKey).To(Equal(primaryKey))
				Expect(results[0].Distance).To(BeZero())
				data, err = maintainer.SerializePendingWriteQueue(record, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(maintainer.UpdateFromQueue(data)).To(Succeed())
				results, err = store.SearchVectorIndexWithPrefix(index, prefix, []float64{42}, 10, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("replays sliding keys and captured delegates without the source record", func() {
		index := newWindowedVectorIndex("queued_window", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			Expect(err).NotTo(HaveOccurred())
			maintainer, err := store.GetIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			Expect(maintainer.IsPendingWriteQueueAllowed()).To(BeTrue())
			record := &recordlayer.FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), CoordX: proto.Int64(3), CoordY: proto.Int64(4)}, PrimaryKey: tuple.Tuple{int64(1)}, RecordType: md.GetRecordType("Order")}
			inserted, err := maintainer.SerializePendingWriteQueue(nil, record)
			Expect(err).NotTo(HaveOccurred())
			removed, err := maintainer.SerializePendingWriteQueue(record, nil)
			Expect(err).NotTo(HaveOccurred())
			index.Predicate = func(proto.Message) bool { return false }
			Expect(maintainer.UpdateFromQueue(inserted)).To(Succeed())
			Expect(maintainer.UpdateFromQueue(inserted)).To(Succeed())
			Expect(searchPKs(store, index, nil)).To(Equal([]int64{1}))
			count, _ := readSlidingWindowMeta(rc.Transaction(), slidingWindowSubspaceFor(store.Subspace(), index), nil)
			Expect(count).To(Equal(int64(1)))
			Expect(maintainer.UpdateFromQueue(removed)).To(Succeed())
			Expect(searchPKs(store, index, nil)).To(BeEmpty())
			for _, payload := range []*gen.SlidingWindowQueueEntry{
				{OldEntryKey: tuple.Tuple{int64(10), int64(1)}.Pack()},
				{NewEntryKey: tuple.Tuple{int64(10), int64(1)}.Pack()},
			} {
				data, err := anypb.New(payload)
				Expect(err).NotTo(HaveOccurred())
				var core *recordlayer.RecordCoreError
				Expect(errors.As(maintainer.UpdateFromQueue(data), &core)).To(BeTrue())
				Expect(core.IndexName).To(Equal(index.Name))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// The vector maintainer's and the sliding window's nested payloads, read with
// Java's Any URL semantics. The queue envelope and its replay are recordlayer's
// (its "Nested pending queue Any compatibility" spec); this applies the payload
// through the maintainer, as replay does.
var _ = Describe("Nested vector pending queue Any compatibility", func() {
	for _, boundary := range []string{"vector", "sliding", "delegate-insert", "delegate-delete"} {
		for _, prefix := range []string{"", "type.googleapis.com/", "custom.example/v1/", "/"} {
			It(fmt.Sprintf("applies boundary=%s prefix=%q with Java URL semantics", boundary, prefix), func() {
				ctx := context.Background()
				root := specSubspace()
				index := recordlayer.NewVectorIndex("nested_any", recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")), 1), 1)
				sliding := boundary == "sliding" || boundary == "delegate-insert" || boundary == "delegate-delete"
				if sliding {
					index = newWindowedVectorIndex("nested_any", 2, gen.RowNumberWindowPredicate_ASC)
				}
				deleting := boundary == "delegate-delete"
				builder := baseBuilder()
				builder.AddIndex("Order", index)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				record := &recordlayer.FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(7), Price: proto.Int32(42), CoordX: proto.Int64(3), CoordY: proto.Int64(4)}, PrimaryKey: tuple.Tuple{int64(1)}, RecordType: md.GetRecordType("Order")}
				// The delete's record is committed first, so a refused payload
				// rolls back only its own application.
				_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
					Expect(err).NotTo(HaveOccurred())
					if deleting {
						_, err = store.SaveRecord(record.Record)
					}
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
					Expect(err).NotTo(HaveOccurred())
					maintainer, err := store.GetIndexMaintainer(index)
					Expect(err).NotTo(HaveOccurred())
					var data *anypb.Any
					if deleting {
						data, err = maintainer.SerializePendingWriteQueue(record, nil)
					} else {
						data, err = maintainer.SerializePendingWriteQueue(nil, record)
					}
					Expect(err).NotTo(HaveOccurred())
					if boundary == "delegate-insert" || boundary == "delegate-delete" {
						var entry gen.SlidingWindowQueueEntry
						Expect(data.UnmarshalTo(&entry)).To(Succeed())
						delegated := entry.DelegatedInsert
						if deleting {
							delegated = entry.DelegatedDelete
						}
						delegated.TypeUrl = prefix + string(delegated.MessageName())
						data, err = anypb.New(&entry)
						Expect(err).NotTo(HaveOccurred())
					} else {
						data.TypeUrl = prefix + string(data.MessageName())
					}
					return nil, maintainer.UpdateFromQueue(data)
				})
				if prefix == "" {
					var core *recordlayer.RecordCoreError
					Expect(errors.As(err, &core)).To(BeTrue(), "slashless nested Any must fail: %v", err)
					Expect(core.Cause).NotTo(BeNil())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
					Expect(err).NotTo(HaveOccurred())
					wantRows := 1
					if deleting == (prefix != "") {
						wantRows = 0
					}
					if sliding {
						Expect(searchPKs(store, index, nil)).To(HaveLen(wantRows))
					} else {
						rows, err := store.SearchVectorIndexWithPrefix(index, tuple.Tuple{int64(7)}, []float64{42}, 10, 100)
						Expect(err).NotTo(HaveOccurred())
						Expect(rows).To(HaveLen(wantRows))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
