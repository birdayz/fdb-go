package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Primary-key boundary snapshot reads", func() {
	for _, serializableControl := range []bool{false, true} {
		name := "maps split-record boundaries at the pinned read version without a read conflict"
		if serializableControl {
			name = "conflicts on the concurrent insert when a serializable lookup is added"
		}
		It(name, func() {
			ctx := context.Background()
			root := specSubspace()
			md, err := baseBuilder().SetSplitLongRecords(true).Build()
			Expect(err).NotTo(HaveOccurred())
			sb := NewStoreBuilder().SetMetaDataProvider(md).SetSubspace(root)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := sb.copyBuilder().SetContext(rtx).Create()
				if err != nil {
					return nil, err
				}
				for _, id := range []int64{10, 50, 90} {
					order := &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))}
					if id == 50 {
						order.VectorData = make([]byte, 250_000)
					}
					if _, err := store.SaveRecord(order); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			backend := &shardMapBackend{BackendDatabase: sharedDB.db}
			db := NewFDBDatabaseWithBackend(backend)
			Expect(db.SetMaxAttempts(1)).To(Succeed())
			var readVersion int64
			_, err = db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := sb.copyBuilder().SetContext(rtx).Open()
				if err != nil {
					return nil, err
				}
				records := store.recordsSubspace
				boundary := records.Pack(tuple.Tuple{int64(5)})
				backend.shards = []fdb.Key{
					boundary,
					records.Pack(tuple.Tuple{int64(50), int64(2)}),
					records.Pack(tuple.Tuple{int64(50), int64(3)}),
				}
				readVersion, err = rtx.GetReadVersion()
				Expect(err).NotTo(HaveOccurred())
				splitChunk, err := rtx.Transaction().Snapshot().Get(backend.shards[1]).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(splitChunk).NotTo(BeEmpty(), "the injected boundary must fall inside a real split record")
				if serializableControl {
					rows, err := rtx.Transaction().GetRange(fdb.KeyRange{
						Begin: boundary, End: records.Pack(tuple.Tuple{int64(50)}),
					}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(1))
					Expect(bytes.Equal(rows[0].Key, records.Pack(tuple.Tuple{int64(10), int64(0)}))).To(BeTrue())
				}
				// A later commit lies inside the lookup's gap, but must not change its pinned view.
				_, err = sharedDB.Run(ctx, func(other *FDBRecordContext) (any, error) {
					store, err := sb.copyBuilder().SetContext(other).Open()
					if err != nil {
						return nil, err
					}
					return store.SaveRecord(&demopb.Order{OrderId: proto.Int64(7), Price: proto.Int32(7)})
				})
				Expect(err).NotTo(HaveOccurred())
				got, err := store.GetPrimaryKeyBoundaries(TupleRangeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal([]tuple.Tuple{{int64(10)}, {int64(50)}}))
				Expect(backend.readVersions).To(Equal([]int64{readVersion}))
				// A write makes read conflicts observable at commit, unlike a read-only caller.
				rtx.Transaction().Set(root.Pack(tuple.Tuple{"boundary-marker"}), []byte{1})
				return nil, nil
			})
			if serializableControl {
				var conflict fdb.Error
				Expect(errors.As(err, &conflict)).To(BeTrue(), "%v", err)
				Expect(conflict.Code).To(Equal(1020))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		})
	}
})

var _ = Describe("Online build read conflicts match Java", func() {
	ctx := context.Background()

	demoMetaData := func(configure func(*RecordMetaDataBuilder)) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if configure != nil {
			configure(builder)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// A build adds read conflicts only for records its targets index (Java's
	// IndexingBase.handleCursorResult), so another type's concurrent write does not conflict it.
	for _, mutual := range []bool{false, true} {
		// It returns how many attempts failed with a conflict (1020), so other retries do not count.
		buildRangeWithConcurrentWrite := func(concurrent proto.Message) (conflicts int) {
			GinkgoHelper()
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			plain := demoMetaData(nil)
			indexed := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", priceIdx) })
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(plain).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, rec := range []proto.Message{
					&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("c")},
					&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				} {
					_, err := store.SaveRecord(rec)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			builder := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(indexed).
				SetIndex(priceIdx).SetSubspace(ks)
			if mutual {
				builder.SetMutualIndexing()
			}
			oi, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(oi.allTargetIndexesIdempotent()).To(BeTrue())
			oi.sessionHeartbeat = oi.newHeartbeat(oi.buildIndexingStamp().GetMethod().String(), mutual, oi.db.Env())
			_, err = oi.markWriteOnly(ctx)
			Expect(err).NotTo(HaveOccurred())
			// One chunk of each build path, inside the test's own transaction.
			build := func(rtx *FDBRecordContext) (int64, error) {
				r, err := oi.buildRangeInTransaction(ctx, rtx)
				return r.recordsProcessed, err
			}
			if mutual {
				m := newMutualIndexBuilder(oi)
				build = func(rtx *FDBRecordContext) (int64, error) {
					store, err := oi.openStore(rtx)
					if err != nil {
						return 0, err
					}
					n, _, err := m.buildFragmentRange(ctx, store, rtx.Env().Now(), &RangeSetRange{Begin: rangeSetFirstKey, End: rangeSetFinalKey})
					return n, err
				}
			}

			db := NewFDBDatabaseWithTransactor(sharedDB.transactor, sharedDB.db)
			db.SetAttemptObserver(func(_ AttemptCall, err error) {
				var fdbErr fdb.Error
				if errors.As(err, &fdbErr) && fdbErr.Code == 1020 {
					conflicts++
				}
			})
			attempts := 0
			_, err = db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				n, err := build(rtx)
				if err != nil {
					return nil, err
				}
				Expect(n).To(Equal(int64(3)), "the scanned count includes the skipped record")
				if attempts == 1 {
					_, err := sharedDB.Run(ctx, func(rtx2 *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().SetContext(rtx2).SetMetaDataProvider(indexed).SetSubspace(ks).Open()
						Expect(err).NotTo(HaveOccurred())
						return store.SaveRecord(concurrent)
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return conflicts
		}

		path := "buildRange"
		if mutual {
			path = "the mutual build"
		}
		It(path+" ignores a write to a record type no target indexes", func() {
			Expect(buildRangeWithConcurrentWrite(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("changed")})).To(Equal(0))
		})

		It(path+" still conflicts on a write to an indexed record", func() {
			Expect(buildRangeWithConcurrentWrite(&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(31)})).To(Equal(1))
		})
	}
})
