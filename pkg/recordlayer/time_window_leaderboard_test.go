package recordlayer

import (
	"context"
	"math"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("TimeWindowLeaderboard", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// Helper: set up leaderboard with time windows via PerformWindowUpdate in a
	// separate transaction before the test transaction, since the directory must
	// be visible when records are saved.
	setupWindows := func(ks subspace.Subspace, md *RecordMetaData, update *TimeWindowLeaderboardWindowUpdate, idx *Index) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainer := store.GetIndexMaintainer(idx).(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.PerformWindowUpdate(update, store)).To(Succeed())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// =========================================================================
	// 1. Basic lifecycle: create windows, save records, scan BY_TIME_WINDOW
	// =========================================================================
	It("basic lifecycle: save records and scan by time window", func() {
		ks := specSubspace()

		// Ungrouped: score=price, timestamp=quantity
		idx := NewTimeWindowLeaderboardIndex("lb_price_ts",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Create a time window [1000, 2000) of type 1.
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}, idx)

		// Save records and scan.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Record at timestamp 1500 (inside window)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(300), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record at timestamp 1200 (inside window)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(1200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record at timestamp 1800 (inside window)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(1800),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan BY_TIME_WINDOW type=1 at timestamp=1500 → all three records
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Sorted by score ascending: 100, 200, 300
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 2. Multiple time windows: different windows capture different records
	// =========================================================================
	It("multiple time windows: records fall into different windows", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_multi_win",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Create two windows: type=1 [1000, 2000) and type=1 [2000, 3000)
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, StartIncrement: 1000, Duration: 1000, Count: 2},
			},
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Record ts=1500 → window [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record ts=2500 → window [2000, 3000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan window at ts=1500 (type=1) → only record with ts=1500
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))

			// Scan window at ts=2500 (type=1) → only record with ts=2500
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 2500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 3. All-time leaderboard: AllTime=true creates an all-time window
	// =========================================================================
	It("all-time leaderboard: records always visible", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_alltime",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Records with widely different timestamps
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(50), Quantity: proto.Int32(0),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(300), Quantity: proto.Int32(999999),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(150), Quantity: proto.Int32(500000),
			})
			Expect(err).NotTo(HaveOccurred())

			// All-time leaderboard (type=0) at any timestamp → all records
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Score ascending: 50, 150, 300
			Expect(entries[0].Key[0]).To(Equal(int64(50)))
			Expect(entries[1].Key[0]).To(Equal(int64(150)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			// Also works via plain ScanIndex (falls back to all-time)
			entries, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 4. High score first: negates scores so highest ranks first
	// =========================================================================
	It("high score first: highest score at rank 0", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_highfirst",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			HighScoreFirst:  true,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(500), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Forward scan with highScoreFirst: the double-flip (negate range + reverse)
			// means BY_VALUE forward scan returns ascending original scores.
			// highScoreFirst primarily affects rank→score mapping (rank 0 = highest).
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// getIndexEntry un-negates, so we see original scores in ascending order
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))
			Expect(entries[2].Key[0]).To(Equal(int64(500)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 5. Delete record: removes entries from all matching leaderboards
	// =========================================================================
	It("delete record clears entries from leaderboard", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_delete",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Delete middle record
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 6. Update record: changing score updates leaderboard entries
	// =========================================================================
	It("update record score updates leaderboard", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_update",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify initial order: 100, 200
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))

			// Update order 1 to have a higher score than order 2
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(500), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Now: 200, 500
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			Expect(entries[1].Key[0]).To(Equal(int64(500)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 7. Window expiration: DeleteBefore removes old windows
	// =========================================================================
	It("DeleteBefore removes expired windows", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_expire",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Create two windows: [1000, 2000) and [2000, 3000)
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, StartIncrement: 1000, Duration: 1000, Count: 2},
			},
		}, idx)

		// Save records into both windows.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2500),
			})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Expire the first window by setting DeleteBefore=2000 (>= endTimestamp of [1000,2000))
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainer := store.GetIndexMaintainer(idx).(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.PerformWindowUpdate(&TimeWindowLeaderboardWindowUpdate{
				UpdateTimestamp: 2100,
				DeleteBefore:    2000,
				Specs: []TimeWindowSpec{
					{Type: 1, BaseTimestamp: 1000, StartIncrement: 1000, Duration: 1000, Count: 2},
				},
			}, store)).To(Succeed())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now scan: window [1000, 2000) should be gone.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Scan at ts=1500 → no leaderboard matches
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			// Scan at ts=2500 → still works
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 2500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 8. BY_RANK scan within a time window
	// =========================================================================
	It("BY_RANK scan returns entries in rank order within time window", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_byrank",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 records with different scores
			for i, p := range []int32{500, 100, 300, 200, 400} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_RANK [0, 5) → all 5 in score order
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByRank, AllTimeLeaderboardType, 0,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(5)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))
			Expect(entries[3].Key[0]).To(Equal(int64(400)))
			Expect(entries[4].Key[0]).To(Equal(int64(500)))

			// BY_RANK [1, 3) → ranks 1 and 2 → scores 200, 300
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByRank, AllTimeLeaderboardType, 0,
				TupleRange{
					Low:          tuple.Tuple{int64(1)},
					High:         tuple.Tuple{int64(3)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 9. Multiple records in same window, ranked by score
	// =========================================================================
	It("multiple records in same window ranked by score", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_multi_rec",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// All records in the same window [1000, 2000)
			for i, p := range []int32{500, 100, 300, 200, 400} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))
			Expect(entries[3].Key[0]).To(Equal(int64(400)))
			Expect(entries[4].Key[0]).To(Equal(int64(500)))

			// Reverse scan
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[4].Key[0]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 10. Record outside all windows: not indexed
	// =========================================================================
	It("record outside all windows is not indexed", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_outside",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Window [1000, 2000) only.
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Record at ts=500, before window [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record at ts=2500, after window [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record at ts=1500, inside window
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Only the record inside the window shows up
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 11. DeleteWhere: bulk delete clears leaderboard entries
	// =========================================================================
	It("DeleteWhere clears leaderboard entries", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_delwhere",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := 1; i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			// DeleteWhere with nil prefix clears everything
			maintainer := store.GetIndexMaintainer(idx).(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.DeleteWhere(nil)).To(Succeed())

			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 12. Directory persistence: directory survives across transactions
	// =========================================================================
	It("directory persists across transactions", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_persist",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}, idx)

		// Save a record in transaction 1.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Read back in a separate transaction.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Directory should have been loaded from FDB.
			maintainer := store.GetIndexMaintainer(idx).(*timeWindowLeaderboardIndexMaintainer)
			dir, err := maintainer.LoadDirectory()
			Expect(err).NotTo(HaveOccurred())
			Expect(dir).NotTo(BeNil())
			Expect(dir.HighScoreFirst).To(BeFalse())

			// All-time window present
			atl := dir.findLeaderboard(AllTimeLeaderboardType, math.MinInt64, math.MaxInt64)
			Expect(atl).NotTo(BeNil())

			// Type=1 window present
			tw := dir.findLeaderboard(1, 1000, 2000)
			Expect(tw).NotTo(BeNil())

			// Scan all-time → should find the record
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))

			// Scan type=1 at ts=1500 → should also find it
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 13. No directory → no results (not an error)
	// =========================================================================
	It("no directory configured returns empty results", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_nodir",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Don't set up any windows — save a record and scan.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// ScanTimeWindowLeaderboard should return empty, not an error.
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1000,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			// Plain ScanIndex (all-time fallback) also returns empty.
			entries, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 14. Nonexistent window type returns empty
	// =========================================================================
	It("scanning nonexistent window type returns empty", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_badtype",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Type 99 does not exist → empty
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 99, 1000,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 15. Score range scan filters by score
	// =========================================================================
	It("score range scan filters entries by score", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_scorerange",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{100, 200, 300, 400, 500} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan scores [200, 400] inclusive
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeBetweenInclusive(tuple.Tuple{int64(200)}, tuple.Tuple{int64(400)}),
				nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))
			Expect(entries[2].Key[0]).To(Equal(int64(400)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 16. High score first with BY_RANK scan
	// =========================================================================
	It("high score first with BY_RANK: rank 0 is highest score", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_hsf_rank",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			HighScoreFirst:  true,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(500), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// BY_RANK [0, 3) with highScoreFirst → 500, 300, 100
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByRank, AllTimeLeaderboardType, 0,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(3)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))
			Expect(entries[2].Key[0]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 17. Grouped leaderboard: per-group rankings
	// =========================================================================
	It("grouped leaderboard: per-group rankings", func() {
		ks := specSubspace()

		// Grouped by order_id (used as "game_id" analog), score=price, timestamp=quantity.
		// GroupBy(Concat(price, quantity), order_id) →
		//   wholeKey = Concat(order_id, price, quantity)
		//   groupedCount = 2, groupingCount = 1
		// So groupKey = [order_id], scoreKey = [price, quantity]
		idx := NewTimeWindowLeaderboardIndex("lb_grouped",
			GroupBy(Concat(Field("price"), Field("quantity")), Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1 (order_id=1): price=300, ts=1000
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(300), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Group 2 (order_id=2): price=100, ts=1000
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Group 3 (order_id=3): price=500, ts=1000
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(500), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan all-time: all entries visible, sorted by [order_id, score, ts]
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Entries sorted by group then score: [1,300,1000], [2,100,1000], [3,500,1000]
			Expect(entries[0].Key[0]).To(Equal(int64(1)))
			Expect(entries[0].Key[1]).To(Equal(int64(300)))
			Expect(entries[1].Key[0]).To(Equal(int64(2)))
			Expect(entries[1].Key[1]).To(Equal(int64(100)))
			Expect(entries[2].Key[0]).To(Equal(int64(3)))
			Expect(entries[2].Key[1]).To(Equal(int64(500)))

			// Scan filtered by group (order_id=2)
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAllOf(tuple.Tuple{int64(2)}),
				nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(2)))
			Expect(entries[0].Key[1]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 18. All-time + typed windows coexist
	// =========================================================================
	It("all-time and typed windows coexist", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_coexist",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Inside window [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Outside window [1000, 2000) but still in all-time
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// All-time → both records
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Type=1 at ts=1500 → only the record inside the window
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 19. Empty store scan returns empty
	// =========================================================================
	It("empty store with windows returns empty scan", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_empty",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 20. Multiple window types with different type IDs
	// =========================================================================
	It("multiple window types: daily and weekly", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_types",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Type 1 = "daily" [1000, 2000), type 2 = "weekly" [0, 5000)
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
				{Type: 2, BaseTimestamp: 0, Duration: 5000, Count: 1},
			},
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// ts=500: in "weekly" [0, 5000) but NOT in "daily" [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// ts=1500: in both "daily" and "weekly"
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Daily at ts=1500 → only record 2
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 1, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))

			// Weekly at ts=1500 → both records
			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, 2, 1500,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 21. PerformWindowUpdate idempotent: same specs don't create duplicates
	// =========================================================================
	It("PerformWindowUpdate is idempotent for same specs", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_idempotent",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		update := &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
		}

		// Apply same update twice.
		setupWindows(ks, md, update, idx)
		setupWindows(ks, md, update, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainer := store.GetIndexMaintainer(idx).(*timeWindowLeaderboardIndexMaintainer)
			dir, err := maintainer.LoadDirectory()
			Expect(err).NotTo(HaveOccurred())
			Expect(dir).NotTo(BeNil())

			// Should have exactly 2 leaderboards: all-time + one type=1 window.
			all := dir.allLeaderboards()
			Expect(all).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 22. Delete all records clears leaderboard
	// =========================================================================
	It("delete all records clears leaderboard entries", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_delall",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Delete all records
			Expect(store.DeleteAllRecords()).To(Succeed())

			entries, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
