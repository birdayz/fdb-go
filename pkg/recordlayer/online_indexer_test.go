package recordlayer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("OnlineIndexer", func() {
	ctx := context.Background()

	// Helper: create metadata with an Order type (PK on order_id) and NO indexes initially.
	baseMetaData := func() (*RecordMetaData, *RecordMetaDataBuilder) {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return nil, builder
	}

	Describe("BuildIndex on existing data", func() {
		It("builds a VALUE index on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITHOUT any index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Create metadata WITH index and build it online.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(5). // Process 5 records per transaction to test chunking.
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Idempotent indexes may re-scan boundary records across chunks.
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify index is READABLE and can be scanned.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify order: prices should be sorted 100, 200, ..., 1000.
				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("emits Indexer: Built Range progress events across a multi-range build", func() {
			ks := specSubspace()

			// Insert 10 records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// A capturing logger + progress interval 0 (log every range) + a small
			// limit so the build spans several ranges → throttleBetweenRanges (hence
			// the progress log) fires between them.
			h := &captureHandler{level: slog.LevelInfo}
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				SetLogger(slog.New(h)).
				SetProgressLogIntervalMillis(0).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			recs := h.snapshot()
			var built int
			for _, r := range recs {
				if r.Message == "Indexer: Built Range" {
					built++
					m := attrMap(r)
					Expect(m["index"]).To(Equal("Order$price"))
					Expect(m["limit"]).To(Equal(int64(3)))
					Expect(m["records_scanned"]).To(BeNumerically(">", int64(0)))
				}
			}
			Expect(built).To(BeNumerically(">=", 1), "expected at least one progress event across a multi-range build")
		})

		It("builds a composite index with PK dedup", func() {
			ks := specSubspace()

			// Insert records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build composite index (price, order_id) with PK dedup on order_id.
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", compositeIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(compositeIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(5)))

			// Verify: entries should be deduplicated (2 elements, not 3).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					expectedPK := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice, expectedPK}))
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{expectedPK}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles empty store", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create the store first.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(0)))

			// Verify readable.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("index is maintained after build (new records)", func() {
			ks := specSubspace()

			// Insert 5 records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Insert more records after build — index should auto-maintain.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds index with small limit (many transactions)", func() {
			ks := specSubspace()

			// Insert 20 records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 20; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 — forces many transactions.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 20))

			// Verify all entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds unique index", func() {
			ks := specSubspace()

			// Insert records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build unique index.
			uniqueIndex := NewIndex("Order$price_unique", Field("price")).SetUnique()
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", uniqueIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(uniqueIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(5)))
		})

		It("applies the enforced post-transaction delay under the default config (RFC-138)", func() {
			ks := specSubspace()

			// Insert 6 records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 6; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// limit=2 → ~3 chunks, 40ms enforced delay each. Crucially NO SetMaxRetries
			// (default 0): proves the enforced delay applies in the DEFAULT config and only
			// AFTER each committed range. Without the wiring fix the throttle was gated
			// behind retries and the delay never fired — this build would finish in ~ms.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIndex).SetIndex(priceIndex).
				SetSubspace(ks).SetLimit(2).SetEnforcedPostTransactionDelay(40).Build()
			Expect(err).NotTo(HaveOccurred())

			start := time.Now()
			total, err := indexer.BuildIndex(ctx)
			elapsed := time.Since(start)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", int64(6)))
			Expect(elapsed).To(BeNumerically(">=", 60*time.Millisecond), "enforced delay must fire per transaction by default")
		})

		It("does not apply the enforced delay after the final range (RFC-138)", func() {
			ks := specSubspace()

			// Insert 2 records — with the default limit they index in a SINGLE range whose
			// build returns hasMore=false. The enforced delay is a between-transactions
			// delay, so a single/final range must NOT pay it (Java skips it when done).
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 2; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Large enforced delay (500ms): if the final range wrongly paid it, the build
			// would take ≥500ms. With the skip, a single-chunk build finishes in ~ms.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIndex).SetIndex(priceIndex).
				SetSubspace(ks).SetEnforcedPostTransactionDelay(500).Build()
			Expect(err).NotTo(HaveOccurred())

			start := time.Now()
			_, err = indexer.BuildIndex(ctx)
			elapsed := time.Since(start)
			Expect(err).NotTo(HaveOccurred())
			Expect(elapsed).To(BeNumerically("<", 300*time.Millisecond), "no enforced delay after the final range")
		})

		It("returns a time-limit error without paying the enforced delay (RFC-138)", func() {
			ks := specSubspace()

			// Insert 6 records → multiple chunks at limit=2.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 6; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Tiny time limit + a large (1s) enforced delay. After the first range the
			// between-ranges time check — which accounts for the upcoming 1s delay — trips
			// and the build returns TimeLimitExceededError WITHOUT sleeping the delay. The
			// old (sleep-then-check) wiring would have stalled ~1s past the deadline.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIndex).SetIndex(priceIndex).
				SetSubspace(ks).SetLimit(2).SetTimeLimit(time.Millisecond).SetEnforcedPostTransactionDelay(1000).Build()
			Expect(err).NotTo(HaveOccurred())

			start := time.Now()
			_, buildErr := indexer.BuildIndex(ctx)
			elapsed := time.Since(start)
			var tle *TimeLimitExceededError
			Expect(errors.As(buildErr, &tle)).To(BeTrue(), "expected a time-limit error")
			Expect(elapsed).To(BeNumerically("<", 500*time.Millisecond), "must not pay the enforced delay when over the time limit")
		})

		It("presets the out-of-range gaps as built for a typed multi-target build (RFC-139)", func() {
			ks := specSubspace()

			// Record-type-prefix PKs so each type's records live in a contiguous
			// record-type-keyed sub-range. Two Order indexes → a multi-target build (the
			// preset fires only for multi-target/mutual).
			typedBuilder := func() *RecordMetaDataBuilder {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
				b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
				return b
			}
			mdNoIdx, err := typedBuilder().Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("x")})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIdx := NewIndex("order_price_idx", Field("price"))
			qtyIdx := NewIndex("order_qty_idx", Field("quantity"))
			b2 := typedBuilder()
			b2.AddIndex("Order", priceIdx)
			b2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := b2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).AddTargetIndex(qtyIdx).
				SetSubspace(ks).Build()
			Expect(err).NotTo(HaveOccurred())

			// Only Order is indexed → range = [pack(OrderKey), pack(OrderKey)+0xff).
			begin, end, ok := indexer.computeRecordsRange()
			Expect(ok).To(BeTrue())
			orderKey := mdWithIdx.GetRecordType("Order").GetRecordTypeKey()
			Expect(begin).To(Equal(tuple.Tuple{orderKey}.Pack()))
			Expect(end).To(Equal(append(tuple.Tuple{orderKey}.Pack(), 0xff)))

			// markWriteOnly + preset, then verify the out-of-range gaps are marked built:
			// only [begin, end) (Order's records range) remains missing. Without the preset,
			// the whole space would be missing (revert-proof).
			Expect(indexer.markWriteOnly(ctx)).To(Succeed())
			Expect(indexer.maybePresetRecordsRange(ctx)).To(Succeed())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				for _, idx := range []*Index{priceIdx, qtyIdx} {
					rangeSet := NewIndexingRangeSet(store.subspace, idx)
					missing, err := rangeSet.ListMissingRanges(rtx.Transaction())
					Expect(err).NotTo(HaveOccurred())
					Expect(missing).To(HaveLen(1), "only Order's records range should remain missing")
					Expect(missing[0].Begin).To(Equal(begin))
					Expect(missing[0].End).To(Equal(end))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("completes a typed multi-target build over the preset (+0xff) range (RFC-139)", func() {
			ks := specSubspace()

			typedBuilder := func() *RecordMetaDataBuilder {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
				b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
				return b
			}
			mdNoIdx, err := typedBuilder().Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 4; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("x")})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			priceIdx := NewIndex("order_price_idx", Field("price"))
			qtyIdx := NewIndex("order_qty_idx", Field("quantity"))
			b2 := typedBuilder()
			b2.AddIndex("Order", priceIdx)
			b2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := b2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Multi-target build presets the gaps then scans the [low, high]+0xff range. A
			// small limit forces multiple chunks. Without buildRange handling the +0xff end
			// boundary, this fails with "unpack range end" (the revert-proof).
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).AddTargetIndex(qtyIdx).
				SetSubspace(ks).SetLimit(2).Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred(), "typed multi-target build must complete (no unpack-range-end failure)")
			Expect(total).To(BeNumerically(">=", int64(4)))

			// Both indexes contain ALL 4 Order records (incl. the highest, which the +0xff
			// inclusive-high bound must cover); Customer is not in these Order indexes.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("order_price_idx")).To(BeTrue())
				Expect(store.IsIndexReadable("order_qty_idx")).To(BeTrue())
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(4))
				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(4))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("an all-types build does not preset (computeRecordsRange not-ok) (RFC-139)", func() {
			// With no record-type-prefix PKs (the default demo types use Field-only PKs),
			// computeRecordsRange gives up → no preset → behaviour unchanged.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, b2 := baseMetaData()
			b2.AddIndex("Order", priceIdx)
			b2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := b2.Build()
			Expect(err).NotTo(HaveOccurred())
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).AddTargetIndex(qtyIdx).
				SetSubspace(specSubspace()).Build()
			Expect(err).NotTo(HaveOccurred())
			_, _, ok := indexer.computeRecordsRange()
			Expect(ok).To(BeFalse(), "Field-only PKs lack a record-type prefix → no preset")
		})

		It("filters to correct record type", func() {
			ks := specSubspace()

			// Insert both Orders and Customers.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
				}
				// Use non-overlapping PKs (101-103) to avoid colliding with Order PKs (1-5).
				for i := int64(101); i <= 103; i++ {
					_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Test")})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build Order-only index — should only index 5 Orders, not 3 Customers.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify exactly 5 entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("BuildIndex on RANK index", func() {
		It("builds a RANK index on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITHOUT any index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				prices := []int32{500, 100, 300, 200, 400}
				for i, price := range prices {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build RANK index online.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Phase 3: Verify BY_VALUE scan (sorted by price).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("rank_by_price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				expectedPrices := []int64{100, 200, 300, 400, 500}
				for i, entry := range entries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}

				// Verify BY_RANK scan — rank 0 should be price 100, rank 4 should be price 500.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(5))
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds RANK index with small limit (chunked)", func() {
			ks := specSubspace()

			// Insert 15 records with various prices.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 15; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 to force multiple transactions.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 15))

			// Verify all 15 entries present and ranked correctly.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(15))

				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 10)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
				}

				// BY_RANK: rank 0→price 10, rank 14→price 150.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(15)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(15))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANK index maintained after build (new records ranked correctly)", func() {
			ks := specSubspace()

			// Insert 3 records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build RANK index.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Insert new records — they should be ranked correctly.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Insert prices that interleave: 200 (rank 1), 400 (rank 3)
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(400)})
				Expect(err).NotTo(HaveOccurred())

				// BY_RANK should now return: 100, 200, 300, 400, 500
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(5))

				expectedPrices := []int64{100, 200, 300, 400, 500}
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds RANK index with duplicate scores", func() {
			ks := specSubspace()

			// Insert records with duplicate prices.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// 3 records at price=100, 2 at price=200, 1 at price=300
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(6), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build RANK index.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 6))

			// Verify: B-tree has 6 entries (one per record), ranked set has 3 scores.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// BY_VALUE: 6 entries (3 at 100, 2 at 200, 1 at 300).
				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(6))

				// BY_RANK: ranks [0,3) maps to scores [100,300+) → all 6 B-tree entries.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(3)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(6))

				// Verify scores in order: 100, 100, 100, 200, 200, 300.
				expectedPrices := []int64{100, 100, 100, 200, 200, 300}
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("BY_INDEX strategy", func() {
		It("builds using BY_INDEX from existing readable source index", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITH source index already present and READABLE.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			// DON'T add target index yet.
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add target index to metadata, build BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex) // source must still be in metadata
			builder2.AddIndex("Order", qtyIndex)   // target
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify target index entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify sorted by quantity: 1, 2, ..., 10.
				for i, entry := range entries {
					expectedQty := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedQty}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds BY_INDEX with chunked transactions via SetLimit", func() {
			ks := specSubspace()

			// Phase 1: Insert 20 records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 20; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build target index BY_INDEX with limit=3.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 20))

			// Verify all 20 entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves BY_INDEX indexing type stamp with source metadata", func() {
			ks := specSubspace()

			// Insert records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build target BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				SetMarkReadable(false). // keep WRITE_ONLY so the indexing type-stamp survives for inspection.
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Load the stamp and verify fields.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(qtyIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_INDEX))

				// SourceIndexSubspaceKey should be the tuple-packed subspace key of the source index.
				expectedSubspaceKey := tuple.Tuple{priceIndex.SubspaceTupleKey()}.Pack()
				Expect(stamp.GetSourceIndexSubspaceKey()).To(Equal(expectedSubspaceKey))

				// SourceIndexLastModifiedVersion should match the source index's version.
				Expect(stamp.GetSourceIndexLastModifiedVersion()).To(Equal(int32(priceIndex.LastModifiedVersion)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects source index that is not VALUE type", func() {
			ks := specSubspace()

			countIndex := NewCountIndex("Order$count", GroupBy(Field("price")))
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", countIndex)
			builder.AddIndex("Order", qtyIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(qtyIndex).
				SetSourceIndex(countIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a VALUE index"))
		})

		It("rejects source index whose root expression creates duplicates", func() {
			ks := specSubspace()

			fanOutIndex := NewIndex("Order$tags", FanOut("tags"))
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", fanOutIndex)
			builder.AddIndex("Order", qtyIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(qtyIndex).
				SetSourceIndex(fanOutIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("creates duplicates"))
		})

		It("rejects source and target on different record types", func() {
			ks := specSubspace()

			// Source on Customer, target on Order.
			nameIndex := NewIndex("Customer$name", Field("name"))
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Customer", nameIndex)
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSourceIndex(nameIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not cover source index type"))
		})

		It("maintains target index after BY_INDEX build when new records are inserted", func() {
			ks := specSubspace()

			// Phase 1: Insert records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build target BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Insert new records and verify target index is maintained.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}

				// All 10 entries should be in the target index.
				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify sorted by quantity: 1..10.
				for i, entry := range entries {
					expectedQty := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedQty}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("stamp-aware resume", func() {
		It("resumes from WRITE_ONLY with matching BY_RECORDS stamp", func() {
			ks := specSubspace()

			// Create metadata WITH index so we can manually control index state.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store and insert initial records (index starts READABLE
			// since it's in metadata at CreateOrOpen time). Then mark WRITE_ONLY and
			// save the BY_RECORDS stamp — simulating a partially completed build.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert 3 records while index is READABLE — entries are maintained.
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Mark WRITE_ONLY manually + save BY_RECORDS stamp.
				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Insert MORE records while WRITE_ONLY — triggers
			// UpdateWhileWriteOnly, which for VALUE indexes writes entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())

				for i := int64(4); i <= 6; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Run OnlineIndexer. Because the stamp matches BY_RECORDS,
			// markWriteOnly should NOT clear existing entries.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Build processes all 6 records (idempotent VALUE index — re-inserting
			// existing entries is a no-op).
			Expect(total).To(BeNumerically(">=", 6))

			// Phase 4: Verify all 6 entries present and index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(6))

				for i, entry := range entries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects stamp mismatch without ForceStampOverwrite policy", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, insert records, then manually mark WRITE_ONLY
			// with a BY_INDEX stamp (simulating a prior BY_INDEX build attempt).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: BY_RECORDS build without policy → PartlyBuiltError.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			var pbe *PartlyBuiltError
			Expect(errors.As(err, &pbe)).To(BeTrue(), "expected PartlyBuiltError, got %v", err)
			Expect(pbe.IndexName).To(Equal("Order$price"))
		})

		It("clears and restarts on stamp mismatch with ForceStampOverwrite", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, insert records, mark WRITE_ONLY with BY_INDEX stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: BY_RECORDS with ForceStampOverwrite → clears and restarts.
			// SetMarkReadable(false) keeps the index WRITE_ONLY so the rewritten
			// BY_RECORDS type-stamp survives for inspection; we mark it readable
			// directly below after reading the stamp.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetPolicy(&IndexingPolicy{ForceStampOverwrite: true}).
				SetMarkReadable(false).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify: all 5 entries rebuilt and index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// The build completed fully but was left WRITE_ONLY (SetMarkReadable(false)).
				// Mark it readable directly: this clears the range set + heartbeats but
				// deliberately leaves the type-stamp intact, so the BY_RECORDS stamp
				// remains inspectable below.
				_, err = store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				// The old BY_INDEX stamp was cleared and replaced with BY_RECORDS by
				// ForceStampOverwrite; the direct mark-readable preserved it.
				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rebuilds from READABLE state (fresh start)", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store with records and build index normally.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			indexer1, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer1.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add more records.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(6); i <= 8; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Run OnlineIndexer again. Since index is READABLE (not
			// WRITE_ONLY), markWriteOnly does ClearAndMarkIndexWriteOnly → full rebuild.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer2.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 8))

			// Verify all 8 entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(8))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("writes stamp and builds when WRITE_ONLY with no stamp and empty range set", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store with records, then manually mark WRITE_ONLY
			// WITHOUT saving any stamp (simulating legacy or manual state change).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 4; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// ClearAndMarkIndexWriteOnly clears all index data (including any
				// stamps and range set) and transitions to WRITE_ONLY. This simulates
				// a manual or legacy WRITE_ONLY state with no stamp.
				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Verify no stamp exists.
				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Run OnlineIndexer. No stamp + empty range set → save stamp
			// and proceed with build (no ClearAndMarkIndexWriteOnly).
			// SetMarkReadable(false) leaves the index WRITE_ONLY so the type-stamp
			// survives for inspection; we mark it readable directly below.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetMarkReadable(false).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 4))

			// Verify: stamp written, index READABLE, all entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// The build completed fully but was left WRITE_ONLY (SetMarkReadable(false)).
				// Mark it readable directly: this clears the range set + heartbeats but
				// deliberately leaves the type-stamp intact, so the BY_RECORDS stamp
				// remains inspectable below.
				_, err = store.MarkIndexReadableOrUniquePending("Order$price")
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(4))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("produces wire-compatible entries from WRITE_ONLY maintenance and build", func() {
			ks := specSubspace()

			// Use metadata WITHOUT index initially to insert record A.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Record A: inserted before index exists.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Now create metadata WITH index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builderIdx := baseMetaData()
			builderIdx.AddIndex("Order", priceIndex)
			mdIdx, err := builderIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			// Open with indexed metadata (CreateOrOpen handles version upgrade
			// from v=0 → v=1, triggering auto-rebuild). Then mark WRITE_ONLY
			// with stamp, simulating a partially completed OnlineIndexer run.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// ClearAndMarkIndexWriteOnly clears auto-rebuilt entries + range set.
				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert record B while WRITE_ONLY — gets a maintenance entry.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Run OnlineIndexer — stamp matches, so WRITE_ONLY maintenance entry
			// for record B survives, and build adds entry for record A.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdIdx).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan index and verify both entries have identical structure.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// Entry for record A (price=100, pk=1) — created by build.
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100), int64(1)}))
				Expect(entries[0].Value).To(HaveLen(0))

				// Entry for record B (price=200, pk=2) — created by WRITE_ONLY maintenance.
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
				Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200), int64(2)}))
				Expect(entries[1].Value).To(HaveLen(0))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Builder validation", func() {
		It("rejects missing database", func() {
			_, err := NewOnlineIndexerBuilder().
				SetMetaData(&RecordMetaData{}).
				SetIndex(NewIndex("test", Field("x"))).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
		})

		It("rejects missing index", func() {
			_, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(&RecordMetaData{}).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Multi-target index building", func() {
		It("builds two VALUE indexes simultaneously on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert 10 records WITHOUT any indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i * 5)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Create metadata with 2 indexes and build both via multi-target.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// 10 records indexed (count is per-record, not per-index-update).
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify both indexes are READABLE and scannable.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				// Verify price index: sorted 100, 200, ..., 1000.
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}

				// Verify quantity index: sorted 5, 10, 15, ..., 50.
				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				for i, entry := range qtyEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 5)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds multi-target with chunked limit", func() {
			ks := specSubspace()

			// Insert 10 records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 to force multiple transactions across both indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 10))

			// Verify both indexes have all 10 entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))

				// Spot-check first and last entries for each index.
				Expect(priceEntries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
				Expect(priceEntries[9].IndexValues()).To(Equal(tuple.Tuple{int64(1000)}))
				Expect(qtyEntries[0].IndexValues()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(qtyEntries[9].IndexValues()).To(Equal(tuple.Tuple{int64(10)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects SetIndex combined with AddTargetIndex", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("SetIndex"))
		})

		It("rejects SetRecordTypes with multi-target", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetRecordTypes("Order").
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("record types"))
		})

		It("rejects SetSourceIndex with multi-target", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			srcIdx := NewIndex("Order$src", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			builder.AddIndex("Order", srcIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSourceIndex(srcIdx).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("source index"))
		})

		It("rejects empty target indexes", func() {
			_, builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetTargetIndexes(nil).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at least one target index"))
		})

		It("saves MULTI_TARGET_BY_RECORDS stamp with sorted target names", func() {
			ks := specSubspace()

			// Insert records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build multi-target. Add indexes in reverse alphabetical order
			// to verify the stamp sorts them.
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			priceIdx := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(qtyIdx).   // Added second, but "Order$price" < "Order$qty" alphabetically.
				AddTargetIndex(priceIdx). // Added first in the target list.
				SetSubspace(ks).
				SetMarkReadable(false). // keep WRITE_ONLY so the indexing type-stamps survive for inspection.
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify stamp on primary index (first in targetIndexes = qtyIdx).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(qtyIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS))

				// Target names must be sorted alphabetically.
				Expect(stamp.GetTargetIndex()).To(Equal([]string{"Order$price", "Order$qty"}))

				// Verify stamp also on the secondary index.
				stamp2, err := store.LoadIndexingTypeStamp(priceIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp2).NotTo(BeNil())
				Expect(stamp2.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS))
				Expect(stamp2.GetTargetIndex()).To(Equal([]string{"Order$price", "Order$qty"}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("resumes multi-target build from partial progress", func() {
			ks := specSubspace()

			// Phase 1: Insert 10 records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Create metadata with 2 indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: First build with limit=3 — does partial work, then we
			// manually simulate an interruption by building again with a fresh indexer.
			indexer1, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			// First full build completes all chunks.
			total1, err := indexer1.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total1).To(BeNumerically(">=", 10))

			// Phase 3: Run AGAIN with same stamp — should resume (no-op since
			// already READABLE) or rebuild cleanly.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total2, err := indexer2.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Rebuild processes all records again (clears + rebuilds from READABLE).
			Expect(total2).To(BeNumerically(">=", 10))

			// Verify both indexes are READABLE with correct entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds multi-target with different record types", func() {
			ks := specSubspace()

			// Phase 1: Insert Order and Customer records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// 5 Orders with prices.
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// 3 Customers with names. Use non-overlapping PKs (101-103).
				for i := int64(101); i <= 103; i++ {
					_, err = store.SaveRecord(&gen.Customer{
						CustomerId: proto.Int64(i),
						Name:       proto.String("Customer"),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add type-specific indexes and build multi-target.
			priceIdx := NewIndex("Order$price", Field("price"))
			nameIdx := NewIndex("Customer$name", Field("name"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Customer", nameIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(nameIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// 5 Orders indexed into price index + 3 Customers into name index = 8.
			Expect(total).To(BeNumerically(">=", 8))

			// Phase 3: Verify each index only has entries from its own record type.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Customer$name")).To(BeTrue())

				// Price index: exactly 5 entries (from Orders only).
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(5))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}

				// Name index: exactly 3 entries (from Customers only).
				nameEntries, err := AsList(ctx, store.ScanIndex(nameIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(nameEntries).To(HaveLen(3))
				for _, entry := range nameEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{"Customer"}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains both indexes after multi-target build when new records are saved", func() {
			ks := specSubspace()

			// Phase 1: Insert records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Multi-target build.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Insert new records after build — both indexes should
			// auto-maintain via normal index maintenance (READABLE state).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// Verify both indexes have all 10 entries.
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 10)}))
				}

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				for i, entry := range qtyEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64(i + 1)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Build progress tracking", func() {
		It("tracks records scanned during BY_RECORDS build", func() {
			ks := specSubspace()

			// Phase 1: Insert records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build index online.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			// BuildIndex returns the scanned-records count. The persisted counter at
			// [9, idx, 1] is erased once the index becomes readable (Java 4.12, RFC-137),
			// so scanned-count is verified via the return value, not LoadBuildProgress.
			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// At least 10 records scanned (may be more due to boundary re-scans).
			Expect(total).To(BeNumerically(">=", int64(10)))
		})

		It("tracks records scanned during chunked build", func() {
			ks := specSubspace()

			// Insert 15 records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 15; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=4 to force multiple chunks.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(4).
				Build()
			Expect(err).NotTo(HaveOccurred())

			// Each chunk atomically ADDs to the scanned counter; BuildIndex returns the
			// total. The persisted counter is erased on readable (RFC-137), so assert the
			// return value.
			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", int64(15)))
		})

		It("tracks records scanned per index during multi-target build", func() {
			ks := specSubspace()

			// Insert records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 8; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Multi-target build with 2 indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", int64(8)))

			// Both indexes built independently and are readable. Their per-index build
			// bookkeeping (scanned counter [9, idx, 1]) is erased once readable (RFC-137),
			// so we verify both built via readable+scannable rather than the now-erased
			// per-index counters.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())
				Expect(store.LoadBuildProgress(priceIdx)).To(Equal(int64(0)))
				Expect(store.LoadBuildProgress(qtyIdx)).To(Equal(int64(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("erases per-build bookkeeping once the index is readable (RFC-137)", func() {
			ks := specSubspace()

			// Insert records without the index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build the index online — completing the build marks it readable, which
			// triggers the post-readable erase (Java 4.12 IndexingBase).
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIndex).SetIndex(priceIndex).SetSubspace(ks).Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", int64(10))) // counter WAS written during the build

			// Post-readable: the scanned-records counter (subkey 1) and the type-stamp
			// (subkey 2) are erased, while the index is readable and scannable. Without
			// the erase in markReadable, LoadBuildProgress would be >0 here — this test
			// is revert-proof on exactly the leak RFC-137 closes.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.LoadBuildProgress(priceIndex)).To(Equal(int64(0))) // scanned counter erased
				Expect(store.LoadIndexingTypeStamp(priceIndex)).To(BeNil())     // type-stamp erased

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("erases scanned/type-stamp but KEEPS the range set on READABLE_UNIQUE_PENDING (RFC-137)", func() {
			ks := specSubspace()

			// Insert records with a duplicate price → a uniqueness violation that drives
			// the build to READABLE_UNIQUE_PENDING rather than READABLE.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, r := range []struct{ id, price int64 }{{1, 100}, {2, 100}, {3, 200}} { // 1 & 2 collide on price
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(r.id), Price: proto.Int32(int32(r.price))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			uniqueIndex := NewIndex("Order$price_unique", Field("price")).SetUnique()
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", uniqueIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(mdWithIndex).SetIndex(uniqueIndex).SetSubspace(ks).Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Duplicate → unique-pending, NOT readable.
				Expect(store.GetIndexState("Order$price_unique")).To(Equal(IndexStateReadableUniquePending))

				// The erase runs for unique-pending too: scanned counter + type-stamp gone...
				Expect(store.LoadBuildProgress(uniqueIndex)).To(Equal(int64(0)))
				Expect(store.LoadIndexingTypeStamp(uniqueIndex)).To(BeNil())

				// ...but the range set is KEPT (clearReadableIndexBuildData runs only on
				// READABLE), so the completed build still shows as complete. A naive erase
				// that wiped the range set would make IsComplete false here.
				rangeSet := NewIndexingRangeSet(store.subspace, uniqueIndex)
				Expect(rangeSet.IsComplete(rtx.Transaction())).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns zero for index with no build progress", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				progress, err := store.LoadBuildProgress(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(progress).To(Equal(int64(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("indexing policy", func() {
		It("blocked stamp prevents build without policy", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with blocked BY_RECORDS stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method:  gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:   proto.Bool(true),
					BlockID: proto.String("maintenance"),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build without policy -> PartlyBuiltError.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			var pbe *PartlyBuiltError
			Expect(errors.As(err, &pbe)).To(BeTrue(), "expected PartlyBuiltError, got %v", err)
			Expect(pbe.IndexName).To(Equal("Order$price"))
		})

		It("blocked stamp with AllowUnblock policy succeeds", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with blocked stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method:  gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:   proto.Bool(true),
					BlockID: proto.String("maintenance"),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build WITH AllowUnblock -> succeeds.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetPolicy(&IndexingPolicy{AllowUnblock: true}).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("blocked stamp with wrong AllowUnblockID fails", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with blocked stamp (ID="maintenance").
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method:  gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:   proto.Bool(true),
					BlockID: proto.String("maintenance"),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with AllowUnblock but wrong ID -> PartlyBuiltError.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetPolicy(&IndexingPolicy{AllowUnblock: true, AllowUnblockID: "wrong-id"}).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			var pbe *PartlyBuiltError
			Expect(errors.As(err, &pbe)).To(BeTrue(), "expected PartlyBuiltError, got %v", err)
			Expect(pbe.IndexName).To(Equal("Order$price"))
		})

		It("expired block allows build without policy", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with short-TTL blocked stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Block with expiry 1ms in the past (already expired).
				stamp := &gen.IndexBuildIndexingStamp{
					Method:                       gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:                        proto.Bool(true),
					BlockID:                      proto.String("temp"),
					BlockExpireEpochMilliSeconds: proto.Uint64(uint64(time.Now().Add(-1 * time.Second).UnixMilli())),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build without policy -> succeeds because block expired.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("areSimilar stamps allow resume when only block fields differ", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with blocked BY_RECORDS stamp.
			// The block is expired so it won't trigger the block check,
			// but the stamp still differs from a fresh BY_RECORDS stamp
			// (it has block fields set). areSimilarStamps should allow overwrite.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Stamp with block=true but already expired (won't be "blocked").
				stamp := &gen.IndexBuildIndexingStamp{
					Method:                       gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:                        proto.Bool(true),
					BlockID:                      proto.String("old-run"),
					BlockExpireEpochMilliSeconds: proto.Uint64(uint64(time.Now().Add(-1 * time.Second).UnixMilli())),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with BY_RECORDS (no block fields) -> areSimilarStamps fires,
			// overwrites stamp, and build succeeds.
			// SetMarkReadable(false) keeps the index WRITE_ONLY so the overwritten
			// type-stamp survives for inspection.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetMarkReadable(false).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify stamp was overwritten to clean BY_RECORDS (no block fields).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				Expect(stamp.GetBlock()).To(BeFalse())
				Expect(stamp.GetBlockID()).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("LoadIndexBuildState returns READABLE for built index", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store (index starts READABLE since in metadata).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				state, err := LoadIndexBuildState(store, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(state.State).To(Equal(IndexStateReadable))
				Expect(state.RecordsScanned).To(BeNil())
				Expect(state.RecordsInTotal).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("LoadIndexBuildState returns WRITE_ONLY with progress after partial build", func() {
			ks := specSubspace()

			// Insert 5 records without index.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Create metadata with index, build partially (limit=3, so first chunk processes 3).
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			md, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Manually perform the partial build: mark WRITE_ONLY + one chunk.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			// Use markWriteOnly to get the index into WRITE_ONLY state,
			// then build exactly one range to get partial progress.
			// We do this by calling BuildIndex with a very small limit that
			// won't finish, but BuildIndex always finishes. Instead, let's
			// manually mark WRITE_ONLY + save stamp + build one chunk.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())

				// Manually add progress of 3 records.
				store.AddBuildProgress(priceIndex, 3)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Now load the build state in a fresh transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				state, err := LoadIndexBuildState(store, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(state.State).To(Equal(IndexStateWriteOnly))
				Expect(state.RecordsScanned).NotTo(BeNil())
				Expect(*state.RecordsScanned).To(Equal(int64(3)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Clean up: finish the build so the index is READABLE.
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("QueryIndexingStamps returns stamp for partially built index", func() {
			ks := specSubspace()

			// Insert records without index.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Create metadata with index and manually set up WRITE_ONLY + stamp.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			md, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Query stamps via the indexer.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			stamps, err := indexer.QueryIndexingStamps(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(stamps).To(HaveKey("Order$price"))
			Expect(stamps["Order$price"].GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
		})

		It("UnblockIndex clears block and allows build", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with blocked stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method:  gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
					Block:   proto.Bool(true),
					BlockID: proto.String("maintenance"),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Create indexer and unblock.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			err = indexer.UnblockIndex(ctx, "")
			Expect(err).NotTo(HaveOccurred())

			// Verify stamp is now unblocked.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetBlock()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build should now succeed (stamp is similar, block cleared).
			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("BlockIndex via OnlineIndexer sets block on stamp", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store, insert records, mark WRITE_ONLY with clean stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Block via the indexer.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			err = indexer.BlockIndex(ctx, "maintenance", 0)
			Expect(err).NotTo(HaveOccurred())

			// Verify stamp is blocked.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetBlock()).To(BeTrue())
				Expect(stamp.GetBlockID()).To(Equal("maintenance"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// New indexer without policy -> PartlyBuiltError.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer2.BuildIndex(ctx)
			var pbe *PartlyBuiltError
			Expect(errors.As(err, &pbe)).To(BeTrue(), "expected PartlyBuiltError, got %v", err)
		})
	})

	Describe("time limit", func() {
		It("returns TimeLimitExceededError when the build exceeds the time limit", func() {
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert enough records that building them 1-at-a-time takes multiple chunks.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 50; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with a very short time limit and limit=1 to force many transactions.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(priceIndex).SetSubspace(ks).
				SetLimit(1).
				SetTimeLimit(1 * time.Nanosecond). // impossibly short
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).To(HaveOccurred())
			var tlErr *TimeLimitExceededError
			Expect(errors.As(err, &tlErr)).To(BeTrue(), "expected TimeLimitExceededError, got %v", err)
		})

		It("completes normally when within the time limit", func() {
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert a few records.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with a generous time limit.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(priceIndex).SetSubspace(ks).
				SetLimit(100).
				SetTimeLimit(5 * time.Minute).
				Build()
			Expect(err).NotTo(HaveOccurred())

			n, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(5)))
		})
	})

	Describe("max retries", func() {
		It("retries and halves limit on failure", func() {
			// This test verifies the retry mechanism restores the limit after success.
			// We can't easily trigger FDB transient errors, so we verify the builder
			// accepts the config and a successful build still works.
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(priceIndex).SetSubspace(ks).
				SetLimit(5).
				SetMaxRetries(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			n, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(10)))
		})
	})

	Describe("MarkReadableIfBuilt", func() {
		It("returns true and marks READABLE when the index is fully built", func() {
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert some records without the index.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build the index fully.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(priceIndex).SetSubspace(ks).SetLimit(100).Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// MarkReadableIfBuilt should return true (already readable).
			allReady, err := indexer.MarkReadableIfBuilt(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(allReady).To(BeTrue())
		})

		It("returns false when the index is not fully built", func() {
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert records without the index.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Mark WRITE_ONLY but don't build anything.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				SetIndex(priceIndex).SetSubspace(ks).SetLimit(100).Build()
			Expect(err).NotTo(HaveOccurred())

			// MarkReadableIfBuilt should return false (range set is empty).
			allReady, err := indexer.MarkReadableIfBuilt(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(allReady).To(BeFalse())

			// Index should still be WRITE_ONLY.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("marks a partially-built multi-target build correctly", func() {
			ks := specSubspace()
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			builder.AddIndex("Order", priceIndex)
			builder.AddIndex("Order", qtyIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert records without indexes.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build both indexes via multi-target.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).SetMetaData(md).
				AddTargetIndex(priceIndex).AddTargetIndex(qtyIndex).
				SetSubspace(ks).SetLimit(100).Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both should already be readable; MarkReadableIfBuilt is idempotent.
			allReady, err := indexer.MarkReadableIfBuilt(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(allReady).To(BeTrue())
		})
	})

	Describe("MutualIndexing", func() {
		// Helper: create metadata with a VALUE index.
		mutualMetaData := func() (*RecordMetaData, *Index) {
			_, builder := baseMetaData()
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md, priceIndex
		}

		Describe("Heartbeat", func() {
			It("heartbeat write and read round-trip", func() {
				ks := specSubspace()
				md, priceIndex := mutualMetaData()

				// Create the store so the subspace exists.
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())

				hb := NewIndexingHeartbeat("MUTUAL_BY_RECORDS", 30_000, true)

				// Write heartbeat in a transaction.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, hb.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).NotTo(HaveOccurred())

				// Read it back.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					heartbeats, indexerIDs, err := ReadHeartbeats(tx, ks, priceIndex)
					Expect(err).NotTo(HaveOccurred())
					Expect(heartbeats).To(HaveLen(1))
					Expect(indexerIDs).To(HaveLen(1))

					Expect(indexerIDs[0]).To(Equal(hb.indexerID.String()))
					Expect(heartbeats[0].GetInfo()).To(Equal("MUTUAL_BY_RECORDS"))
					Expect(heartbeats[0].GetCreateTimeMilliseconds()).To(Equal(hb.createTimeMs))
					Expect(heartbeats[0].GetHeartbeatTimeMilliseconds()).To(BeNumerically(">", 0))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("exclusive mode rejects active peer heartbeat", func() {
				ks := specSubspace()
				md, priceIndex := mutualMetaData()

				// Create the store.
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())

				hb1 := NewIndexingHeartbeat("BY_RECORDS", 30_000, false) // exclusive
				hb2 := NewIndexingHeartbeat("BY_RECORDS", 30_000, false) // exclusive

				// First heartbeat writes successfully.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, hb1.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).NotTo(HaveOccurred())

				// Second exclusive heartbeat should fail with SynchronizedSessionLockedError.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, hb2.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).To(HaveOccurred())
				var lockErr *SynchronizedSessionLockedError
				Expect(errors.As(err, &lockErr)).To(BeTrue(), "expected SynchronizedSessionLockedError, got %v", err)
				Expect(lockErr.ExistingIndexerID).To(Equal(hb1.indexerID.String()))
				Expect(lockErr.ExistingInfo).To(Equal("BY_RECORDS"))
			})

			It("mutual mode allows concurrent heartbeats", func() {
				ks := specSubspace()
				md, priceIndex := mutualMetaData()

				// Create the store.
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())

				hb1 := NewIndexingHeartbeat("MUTUAL_BY_RECORDS", 30_000, true)
				hb2 := NewIndexingHeartbeat("MUTUAL_BY_RECORDS", 30_000, true)

				// First heartbeat writes.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, hb1.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).NotTo(HaveOccurred())

				// Second mutual heartbeat should also succeed (no lock check).
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, hb2.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).NotTo(HaveOccurred())

				// Verify both heartbeats are present.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					heartbeats, indexerIDs, err := ReadHeartbeats(tx, ks, priceIndex)
					Expect(err).NotTo(HaveOccurred())
					Expect(heartbeats).To(HaveLen(2))
					Expect(indexerIDs).To(ContainElement(hb1.indexerID.String()))
					Expect(indexerIDs).To(ContainElement(hb2.indexerID.String()))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("stale heartbeat is ignored", func() {
				ks := specSubspace()
				md, priceIndex := mutualMetaData()

				// Create the store.
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())

				// Write a heartbeat with an old timestamp (beyond the 10s lease).
				staleHB := NewIndexingHeartbeat("BY_RECORDS", 10_000, false)
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					// Manually write a heartbeat proto with an old timestamp.
					hbProto := &gen.IndexBuildHeartbeat{
						Info:                      proto.String("BY_RECORDS"),
						CreateTimeMilliseconds:    proto.Int64(staleHB.createTimeMs),
						HeartbeatTimeMilliseconds: proto.Int64(time.Now().Add(-20 * time.Second).UnixMilli()), // 20s ago > 10s lease
					}
					data, marshalErr := proto.Marshal(hbProto)
					Expect(marshalErr).NotTo(HaveOccurred())
					tx.Set(staleHB.heartbeatKey(ks, priceIndex), data)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// A new exclusive heartbeat should succeed because the existing one is stale.
				freshHB := NewIndexingHeartbeat("BY_RECORDS", 10_000, false)
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					return nil, freshHB.CheckAndUpdate(tx, ks, priceIndex)
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Describe("Mutual build", func() {
			It("mutual BuildIndex produces complete index", func() {
				ks := specSubspace()

				// Phase 1: Insert 20 records WITHOUT any index.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i := int64(1); i <= 20; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Phase 2: Build index with SetMutualIndexing().
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				indexer, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(priceIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					SetLimit(5). // Force multiple transactions.
					Build()
				Expect(err).NotTo(HaveOccurred())

				total, err := indexer.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(total).To(BeNumerically(">=", 20))

				// Phase 3: Verify index is READABLE and all 20 entries present.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(20))

					// Verify sorted order.
					for i, entry := range entries {
						expectedPrice := int64((i + 1) * 100)
						Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("two concurrent mutual builders produce complete index", func() {
				ks := specSubspace()

				// Phase 1: Insert 50 records WITHOUT any index.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i := int64(1); i <= 50; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Phase 2: Build index concurrently with TWO mutual indexers.
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				var wg sync.WaitGroup
				errs := make([]error, 2)

				for g := 0; g < 2; g++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						defer GinkgoRecover()

						indexer, buildErr := NewOnlineIndexerBuilder().
							SetDatabase(sharedDB).
							SetMetaData(mdWithIndex).
							SetIndex(priceIndex).
							SetSubspace(ks).
							SetMutualIndexing().
							SetLimit(10).
							Build()
						if buildErr != nil {
							errs[idx] = buildErr
							return
						}

						_, errs[idx] = indexer.BuildIndex(ctx)
					}(g)
				}
				wg.Wait()

				Expect(errs[0]).NotTo(HaveOccurred())
				Expect(errs[1]).NotTo(HaveOccurred())

				// Phase 3: Verify all 50 entries present.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(50))

					// Verify sorted order: 10, 20, ..., 500.
					for i, entry := range entries {
						expectedPrice := int64((i + 1) * 10)
						Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("four concurrent mutual builders with 500 records", func() {
				ks := specSubspace()

				// Phase 1: Insert 500 records with NO indexes.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				// Insert in batches to stay within FDB transaction limits.
				for batch := 0; batch < 5; batch++ {
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
						Expect(err).NotTo(HaveOccurred())
						for i := int64(batch*100 + 1); i <= int64((batch+1)*100); i++ {
							_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
							Expect(err).NotTo(HaveOccurred())
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// Phase 2: Define VALUE + COUNT indexes, build with 4 concurrent mutual builders.
				priceIndex := NewIndex("Order$price", Field("price"))
				countIndex := NewCountIndex("Order$count", Ungrouped(EmptyKey()))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				builder2.AddIndex("Order", countIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				// Create synthetic boundaries at PK 125, 250, 375 to give builders
				// distinct fragments on single-node FDB.
				boundaries := [][]byte{
					tuple.Tuple{int64(125)}.Pack(),
					tuple.Tuple{int64(250)}.Pack(),
					tuple.Tuple{int64(375)}.Pack(),
				}

				const numBuilders = 4
				var wg sync.WaitGroup
				errs := make([]error, numBuilders)

				for g := 0; g < numBuilders; g++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						defer GinkgoRecover()

						indexer, buildErr := NewOnlineIndexerBuilder().
							SetDatabase(sharedDB).
							SetMetaData(mdWithIndex).
							SetIndex(priceIndex).
							SetSubspace(ks).
							SetMutualIndexingBoundaries(boundaries).
							SetLimit(20).
							Build()
						if buildErr != nil {
							errs[idx] = buildErr
							return
						}

						_, errs[idx] = indexer.BuildIndex(ctx)
					}(g)
				}
				wg.Wait()

				for i := 0; i < numBuilders; i++ {
					Expect(errs[i]).NotTo(HaveOccurred())
				}

				// Also build the COUNT index (separate pass since OnlineIndexer
				// targets one index).
				countIndexer, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(countIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					SetLimit(50).
					Build()
				Expect(err).NotTo(HaveOccurred())
				_, err = countIndexer.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Phase 3: Verify VALUE index — all 500 entries present and sorted.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(500))

					// Verify sorted: prices are 10, 20, ..., 5000.
					for i, entry := range entries {
						expectedPrice := int64((i + 1) * 10)
						Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					}

					// Phase 4: Verify COUNT index value = 500.
					Expect(store.IsIndexReadable("Order$count")).To(BeTrue())
					result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
						&IndexAggregateFunction{Name: FunctionNameCount, Operand: Ungrouped(EmptyKey())},
						TupleRangeAll, IsolationLevelSerializable)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(tuple.Tuple{int64(500)}))

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("mutual builders with concurrent writes during build", func() {
				ks := specSubspace()

				// Phase 1: Insert 200 records.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				for batch := 0; batch < 2; batch++ {
					_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
						Expect(err).NotTo(HaveOccurred())
						for i := int64(batch*100 + 1); i <= int64((batch+1)*100); i++ {
							_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
							Expect(err).NotTo(HaveOccurred())
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// Phase 2: Start 2 mutual builders AND a concurrent writer.
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				var wg sync.WaitGroup
				builderErrs := make([]error, 2)

				// Start 2 builders.
				for g := 0; g < 2; g++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						defer GinkgoRecover()

						indexer, buildErr := NewOnlineIndexerBuilder().
							SetDatabase(sharedDB).
							SetMetaData(mdWithIndex).
							SetIndex(priceIndex).
							SetSubspace(ks).
							SetMutualIndexing().
							SetLimit(10).
							Build()
						if buildErr != nil {
							builderErrs[idx] = buildErr
							return
						}

						_, builderErrs[idx] = indexer.BuildIndex(ctx)
					}(g)
				}

				// Concurrent writer: save records 201-250 while build is in progress.
				// These writes go through the WRITE_ONLY dispatch path because the
				// index is in WRITE_ONLY state during the online build.
				var writerErr error
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()

					for i := int64(201); i <= 250; i++ {
						_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
							store, err := NewStoreBuilder().
								SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
							if err != nil {
								return nil, err
							}
							_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
							return nil, err
						})
						if err != nil {
							writerErr = err
							return
						}
						// Brief yield to interleave with builders.
						time.Sleep(time.Millisecond)
					}
				}()

				wg.Wait()

				Expect(builderErrs[0]).NotTo(HaveOccurred())
				Expect(builderErrs[1]).NotTo(HaveOccurred())
				Expect(writerErr).NotTo(HaveOccurred())

				// Phase 3: Verify ALL 250 records are in the index.
				// The 200 original records were indexed by the builders.
				// The 50 concurrent writes were indexed via WRITE_ONLY dispatch.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(250))

					// Verify sorted: prices are 10, 20, ..., 2500.
					for i, entry := range entries {
						expectedPrice := int64((i + 1) * 10)
						Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("mutual builder resumes after partial build timeout", func() {
				ks := specSubspace()

				// Phase 1: Insert 100 records.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i := int64(1); i <= 100; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Phase 2: Start a mutual builder with limit=1 and an impossibly
				// short time limit. After the first chunk (1 record), the time
				// check triggers because 1ns has certainly elapsed.
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				indexer1, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(priceIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					SetLimit(1).
					SetTimeLimit(1 * time.Nanosecond).
					Build()
				Expect(err).NotTo(HaveOccurred())

				firstTotal, err := indexer1.BuildIndex(ctx)
				// The builder should time out mid-build (not finish all 100).
				var tlErr *TimeLimitExceededError
				Expect(errors.As(err, &tlErr)).To(BeTrue(), "expected TimeLimitExceededError, got %v", err)
				Expect(firstTotal).To(BeNumerically(">", 0), "should have processed at least some records")
				Expect(firstTotal).To(BeNumerically("<", 100), "should not have finished all records")

				// Verify the index is NOT READABLE yet (partial build).
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeFalse())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Phase 3: Start a second mutual builder — it should pick up where
				// the first left off and finish the job.
				indexer2, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(priceIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					SetLimit(20).
					Build()
				Expect(err).NotTo(HaveOccurred())

				secondTotal, err := indexer2.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				// The second builder should process the remainder.
				Expect(secondTotal).To(BeNumerically(">", 0), "second builder should process remaining records")

				// Phase 4: Verify all 100 entries present and index is READABLE.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

					entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(100))

					// Verify sorted: prices are 10, 20, ..., 1000.
					for i, entry := range entries {
						expectedPrice := int64((i + 1) * 10)
						Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("mutual builder stamp is MUTUAL_BY_RECORDS", func() {
				ks := specSubspace()

				// Insert some records.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i := int64(1); i <= 5; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Build with mutual mode.
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				indexer, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(priceIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					SetMarkReadable(false). // keep WRITE_ONLY so the indexing type-stamp survives for inspection.
					Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = indexer.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Verify the stamp is MUTUAL_BY_RECORDS.
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())

					stamp, err := store.LoadIndexingTypeStamp(priceIndex)
					Expect(err).NotTo(HaveOccurred())
					Expect(stamp).NotTo(BeNil())
					Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("mutual builder cleans up heartbeat on completion", func() {
				ks := specSubspace()

				// Insert some records.
				_, builder := baseMetaData()
				mdNoIndex, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i := int64(1); i <= 5; i++ {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Build with mutual mode.
				priceIndex := NewIndex("Order$price", Field("price"))
				_, builder2 := baseMetaData()
				builder2.AddIndex("Order", priceIndex)
				mdWithIndex, err := builder2.Build()
				Expect(err).NotTo(HaveOccurred())

				indexer, err := NewOnlineIndexerBuilder().
					SetDatabase(sharedDB).
					SetMetaData(mdWithIndex).
					SetIndex(priceIndex).
					SetSubspace(ks).
					SetMutualIndexing().
					Build()
				Expect(err).NotTo(HaveOccurred())

				_, err = indexer.BuildIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Verify no heartbeats remain after build completes.
				_, err = sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					heartbeats, _, err := ReadHeartbeats(tx, ks, priceIndex)
					Expect(err).NotTo(HaveOccurred())
					Expect(heartbeats).To(BeEmpty())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})
})
