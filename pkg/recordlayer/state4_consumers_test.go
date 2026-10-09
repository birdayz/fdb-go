package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// State 4 (WRITE_ONLY_WITH_QUEUE) as every state consumer reads it. Java's
// IndexState.isWriteOnly() covers both write-only states, so a queued index is
// write-only to GetWriteOnlyIndexes, not scannable, and absent from the
// readable set. Java writes state 4 for any index (markIndexNotReadable does
// not check capability), so a store a Java process queued may hold it on a
// unique VALUE or RANK index: a save then fails where Java's does, in the
// maintainer's serializePendingWriteQueue ("does not support the pending
// write queue", FDBRecordStore.updateSecondaryIndexes), and a rank scan is
// refused as not readable. A queue-capable index enqueues the save.
var _ = Describe("WRITE_ONLY_WITH_QUEUE consumers", func() {
	ctx := context.Background()
	metadata := func() *RecordMetaData {
		builder := baseBuilder()
		builder.AddIndex("Order", NewIndex("uniq", Field("quantity")).SetUnique())
		builder.AddIndex("Order", NewRankIndex("rank", Ungrouped(Field("price"))))
		builder.AddIndex("Order", newValueWithQueueIndex("vector", Concat(Field("quantity"), Field("price"))))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	order := func(id int64) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(int32(id)), Price: proto.Int32(int32(10 * id))}
	}
	// queue forces state 4 as a Java process writes it, without Go's
	// eligibility check.
	queue := func(md *RecordMetaData, names ...string) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).
				SetFormatVersion(15).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				store.setIndexState(name, IndexStateWriteOnlyWithQueue)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	inStore := func(md *RecordMetaData, f func(*FDBRecordStore) error) error {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Open()
			if err != nil {
				return nil, err
			}
			return nil, f(store)
		})
		return err
	}

	It("is write-only, not scannable and not readable", func() {
		md := metadata()
		queue(md, "uniq", "rank", "vector")
		Expect(inStore(md, func(store *FDBRecordStore) error {
			var writeOnly []string
			for _, index := range store.GetWriteOnlyIndexes() {
				writeOnly = append(writeOnly, index.Name)
			}
			Expect(writeOnly).To(ConsistOf("uniq", "rank", "vector"))
			for _, name := range []string{"uniq", "rank", "vector"} {
				Expect(store.IsIndexWriteOnly(name)).To(BeTrue(), name)
				Expect(store.IsIndexWriteOnlyWithQueue(name)).To(BeTrue(), name)
				Expect(store.IsIndexWriteOnlyNoQueue(name)).To(BeFalse(), name)
				Expect(store.IsIndexScannable(name)).To(BeFalse(), name)
			}
			for _, index := range store.GetReadableIndexes() {
				Expect(index.Name).NotTo(BeElementOf("uniq", "rank", "vector"))
			}
			_, err := AsList(ctx, store.ScanRankIndex(md.GetIndex("rank"),
				RankScanBounds{ScanType: IndexScanByRank, RankRange: TupleRangeAll}, nil, ForwardScan()))
			var notReadable *IndexNotReadableError
			Expect(errors.As(err, &notReadable)).To(BeTrue(), "rank scan of a queued index: %v", err)
			return nil
		})).To(Succeed())
	})

	for _, name := range []string{"uniq", "rank"} {
		It("refuses a save into a queued "+name+" index, which cannot serialize a queue entry", func() {
			md := metadata()
			queue(md, name)
			err := inStore(md, func(store *FDBRecordStore) error {
				_, err := store.SaveRecord(order(1))
				return err
			})
			var unsupported *UnsupportedOperationError
			Expect(errors.As(err, &unsupported)).To(BeTrue(), "save: %v", err)
			Expect(unsupported.Message).To(Equal(name + " does not support the pending write queue"))
		})
	}

	It("enqueues a save into a queued index and leaves the others maintained", func() {
		md := metadata()
		queue(md, "vector")
		Expect(inStore(md, func(store *FDBRecordStore) error {
			_, err := store.SaveRecord(order(1))
			return err
		})).To(Succeed())
		// The queue key is versionstamped: read it after the commit.
		Expect(inStore(md, func(store *FDBRecordStore) error {
			entries, err := AsList(ctx, store.indexingPendingWriteQueue(md.GetIndex("vector"), 100).
				GetQueueCursor(store.context, ForwardScan(), nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			ranks, err := AsList(ctx, store.ScanRankIndex(md.GetIndex("rank"),
				RankScanBounds{ScanType: IndexScanByRank, RankRange: TupleRangeAll}, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(ranks).To(HaveLen(1))
			return nil
		})).To(Succeed())
	})
})
