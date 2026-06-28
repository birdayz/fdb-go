package recordlayer

import (
	"context"
	"math"
	"math/big"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
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

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
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
			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
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
			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
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

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
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
	// 22. EvaluateRecordFunction with "rank" returns correct ranks
	// =========================================================================
	It("EvaluateRecordFunction returns correct ranks in all-time window", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_recfn_rank",
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

			// Save 5 records with different scores.
			prices := []int32{500, 100, 300, 200, 400}
			records := make([]*FDBStoredRecord[proto.Message], 5)
			for i, p := range prices {
				rec, err := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
				records[i] = rec
			}

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// Ranks should be: 100→0, 200→1, 300→2, 400→3, 500→4
			// records[0]=500, records[1]=100, records[2]=300, records[3]=200, records[4]=400
			expectedRanks := []int64{4, 0, 2, 1, 3}
			for i, rec := range records {
				rank, err := store.EvaluateRecordFunction(fn, rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(rank).NotTo(BeNil())
				Expect(*rank).To(Equal(expectedRanks[i]))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 23. EvaluateRecordFunction returns nil when record not in any window
	// =========================================================================
	It("EvaluateRecordFunction returns nil for record not in any window", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_recfn_nil",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Only typed window [1000, 2000), NO all-time.
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

			// Record at ts=500 — outside all windows.
			rec, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// "rank" dispatches to all-time leaderboard. No all-time → nil dir? No,
			// there IS a directory, but no all-time leaderboard → nil from
			// oldestLeaderboardMatching → nil result.
			rank, err := store.EvaluateRecordFunction(fn, rec)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 24. Store-level EvaluateRecordFunction auto-selects TIME_WINDOW_LEADERBOARD index
	// =========================================================================
	It("store-level EvaluateRecordFunction auto-selects leaderboard index", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_recfn_auto",
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

			rec1, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			rec2, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Auto-select: no explicit index name.
			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: idx.RootExpression,
			}

			rank1, err := store.EvaluateRecordFunction(fn, rec1)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank1).NotTo(BeNil())
			Expect(*rank1).To(Equal(int64(0))) // 100 is rank 0

			rank2, err := store.EvaluateRecordFunction(fn, rec2)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank2).NotTo(BeNil())
			Expect(*rank2).To(Equal(int64(1))) // 200 is rank 1

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 25. TIME_WINDOW_COUNT aggregate: count entries in a time window
	// =========================================================================
	It("TIME_WINDOW_COUNT returns correct count", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_agg_count",
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

			// 3 records in all-time, 2 in window [1000, 2000)
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(1200),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(1800),
			})
			Expect(err).NotTo(HaveOccurred())

			fn := &IndexAggregateFunction{
				Name:    FunctionNameTimeWindowCount,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// Count in all-time (type=0, timestamp=0)
			result, err := store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			// Count in window type=1 at timestamp=1500
			result, err = store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(1), int64(1500)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 26. SCORE_FOR_TIME_WINDOW_RANK aggregate
	// =========================================================================
	It("SCORE_FOR_TIME_WINDOW_RANK returns score at given rank", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_agg_sfr",
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

			for i, p := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			fn := &IndexAggregateFunction{
				Name:    FunctionNameScoreForTimeWindowRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// Rank 0 → score 100 (lowest)
			result, err := store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result[0]).To(Equal(int64(100)))

			// Rank 1 → score 200
			result, err = store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(1)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result[0]).To(Equal(int64(200)))

			// Rank 2 → score 300
			result, err = store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(2)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 27. TIME_WINDOW_RANK_FOR_SCORE aggregate
	// =========================================================================
	It("TIME_WINDOW_RANK_FOR_SCORE returns rank for given score", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_agg_rfs",
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

			for i, p := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			fn := &IndexAggregateFunction{
				Name:    FunctionNameTimeWindowRankForScore,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// Score 100 → rank 0. Pass score as (score, timestamp) in values.
			result, err := store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(100), int64(1000)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(0)}))

			// Score 300 → rank 2
			result, err = store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(300), int64(1000)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 28. SCORE_FOR_TIME_WINDOW_RANK_ELSE_SKIP returns nil for out-of-range rank
	// =========================================================================
	It("SCORE_FOR_TIME_WINDOW_RANK_ELSE_SKIP returns nil for out-of-range rank", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_agg_skip",
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

			fn := &IndexAggregateFunction{
				Name:    FunctionNameScoreForTimeWindowRankElseSkip,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}

			// Rank 0 → valid (returns score)
			result, err := store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result[0]).To(Equal(int64(100)))

			// Rank 99 → out of range → nil (skip sentinel)
			result, err = store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0), int64(99)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 29. SaveSubDirectory persists per-group highScoreFirst override
	// =========================================================================
	It("SaveSubDirectory persists per-group highScoreFirst override", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_subdir",
			GroupBy(Concat(Field("price"), Field("quantity")), Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			HighScoreFirst:  false, // directory default: low score first
		}, idx)

		// Save a sub-directory override for group=1 (order_id=1): highScoreFirst=true
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.SaveSubDirectory(tuple.Tuple{int64(1)}, true)).To(Succeed())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify the sub-directory persisted across transactions.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
			dir, err := maintainer.LoadDirectory()
			Expect(err).NotTo(HaveOccurred())
			Expect(dir).NotTo(BeNil())
			Expect(dir.HighScoreFirst).To(BeFalse()) // directory default

			// Load sub-directory for group=1
			sub, err := loadLeaderboardSubDirectory(rtx.Transaction(), maintainer.secondarySubspace, dir, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(sub.HighScoreFirst).To(BeTrue()) // override

			// Load sub-directory for group=2 (no override) → defaults to dir
			sub2, err := loadLeaderboardSubDirectory(rtx.Transaction(), maintainer.secondarySubspace, dir, tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(sub2.HighScoreFirst).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 30. Per-group highScoreFirst override affects scan ordering
	// =========================================================================
	It("per-group highScoreFirst override affects score ordering in scan", func() {
		ks := specSubspace()

		// Grouped by order_id, score=price, timestamp=quantity.
		idx := NewTimeWindowLeaderboardIndex("lb_subdir_scan",
			GroupBy(Concat(Field("price"), Field("quantity")), Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			HighScoreFirst:  false, // directory default
		}, idx)

		// Set per-group override for group=1: highScoreFirst=true
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.SaveSubDirectory(tuple.Tuple{int64(1)}, true)).To(Succeed())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Save records into group 1 (with highScoreFirst=true due to sub-directory).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1, two scores: 100 and 300
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Need a second record in the same group — but order_id is the PK
			// and also the group key. Each PK produces one entry per group.
			// With GroupBy(..., order_id), each order_id is its own group with
			// exactly 1 entry, so the highScoreFirst changes the negated storage
			// but each group has exactly 1 entry. Let's verify the score gets
			// un-negated properly on read.

			// Scan group=1 all-time → entry should show original (un-negated) price
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAllOf(tuple.Tuple{int64(1)}),
				nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			// Key: [group, score, ts]. Score should be un-negated.
			Expect(entries[0].Key[0]).To(Equal(int64(1)))   // group
			Expect(entries[0].Key[1]).To(Equal(int64(100))) // un-negated score

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 31. RebuildIndex after adding records rebuilds leaderboard entries
	// =========================================================================
	It("RebuildIndex rebuilds leaderboard entries from existing records", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_rebuild",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		// Save records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Rebuild index in a new transaction.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.RebuildIndex(idx)).To(Succeed())

			// Verify all entries are present after rebuild.
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 32. PerformWindowUpdate with Rebuild.ALWAYS clears and rebuilds
	// =========================================================================
	It("PerformWindowUpdate with Rebuild.ALWAYS clears and rebuilds", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_rebuild_always",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		// Save records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Trigger rebuild with ALWAYS.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
			Expect(maintainer.PerformWindowUpdate(&TimeWindowLeaderboardWindowUpdate{
				UpdateTimestamp: 600,
				AllTime:         true,
				Rebuild:         TimeWindowRebuildAlways,
			}, store)).To(Succeed())

			// After rebuild, all entries should be present.
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 33. OnlineIndexer builds TIME_WINDOW_LEADERBOARD from existing records
	// =========================================================================
	It("OnlineIndexer builds TIME_WINDOW_LEADERBOARD index from existing records", func() {
		ks := specSubspace()

		// Phase 1: Save records WITHOUT the leaderboard index.
		builder1 := baseMetaData()
		mdNoIndex, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(i),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Create metadata WITH leaderboard index.
		idx := NewTimeWindowLeaderboardIndex("lb_online",
			Concat(Field("price"), Field("quantity")))
		builder2 := baseMetaData()
		builder2.AddIndex("Order", idx)
		mdWithIndex, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		// Set up windows before building (the indexer needs them to index).
		setupWindows(ks, mdWithIndex, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		// Build online.
		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(mdWithIndex).
			SetIndex(idx).
			SetSubspace(ks).
			Build()
		Expect(err).NotTo(HaveOccurred())

		total, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(BeNumerically(">=", 5))

		// Phase 3: Verify index is READABLE and entries are correct.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable(idx.Name)).To(BeTrue())

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			for i, entry := range entries {
				Expect(entry.Key[0]).To(Equal(int64((i + 1) * 100)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 34. OnlineIndexer with chunked build (small limit)
	// =========================================================================
	It("OnlineIndexer builds TIME_WINDOW_LEADERBOARD with small chunk limit", func() {
		ks := specSubspace()

		// Phase 1: Save records without index.
		builder1 := baseMetaData()
		mdNoIndex, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(i),
					Price:    proto.Int32(int32(i * 100)),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Create metadata with leaderboard index.
		idx := NewTimeWindowLeaderboardIndex("lb_online_chunked",
			Concat(Field("price"), Field("quantity")))
		builder2 := baseMetaData()
		builder2.AddIndex("Order", idx)
		mdWithIndex, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		setupWindows(ks, mdWithIndex, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
		}, idx)

		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(mdWithIndex).
			SetIndex(idx).
			SetSubspace(ks).
			SetLimit(3). // Small chunk: 3 records per transaction
			Build()
		Expect(err).NotTo(HaveOccurred())

		total, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		// With limit=3, processes 4 chunks (3+3+3+1). Boundary rescan means >= 10.
		Expect(total).To(BeNumerically(">=", 10))

		// Verify all 10 entries present and correctly ordered.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable(idx.Name)).To(BeTrue())

			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(10))
			for i, entry := range entries {
				Expect(entry.Key[0]).To(Equal(int64((i + 1) * 100)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 35. Rebuild.NEVER with highScoreFirst change returns error
	// =========================================================================
	It("Rebuild.NEVER with highScoreFirst change returns error", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_rebuild_never",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Initial setup with highScoreFirst=false.
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500,
			AllTime:         true,
			HighScoreFirst:  false,
		}, idx)

		// Try to change highScoreFirst with Rebuild=NEVER → should fail.
		// Return the error from the callback so the outer Run sees it too.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.GetIndexMaintainer(idx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)
			err = maintainer.PerformWindowUpdate(&TimeWindowLeaderboardWindowUpdate{
				UpdateTimestamp: 600,
				HighScoreFirst:  true,
				AllTime:         true,
				Rebuild:         TimeWindowRebuildNever,
			}, store)
			return nil, err
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot change highScoreFirst without a rebuild"))
	})

	// =========================================================================
	// 36. negateScore with math.MinInt64 produces big.Int (not overflow)
	// =========================================================================
	It("negateScore with math.MinInt64 produces big.Int", func() {
		// math.MinInt64 cannot be negated as int64 (overflow).
		// negateScore should produce a *big.Int with value 2^63.
		input := tuple.Tuple{int64(math.MinInt64)}
		result, err := negateScore(input, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(HaveLen(1))

		bigVal, ok := result[0].(*big.Int)
		Expect(ok).To(BeTrue())
		expected := new(big.Int).SetUint64(1 << 63) // 2^63
		Expect(bigVal.Cmp(expected)).To(Equal(0))

		// Round-trip: negating the big.Int should give back MinInt64.
		roundTrip, err := negateScore(result, 0)
		Expect(err).NotTo(HaveOccurred())
		// math.MinInt64 stores as Go `int` (untyped constant default) in the tuple.
		Expect(roundTrip[0]).To(BeNumerically("==", math.MinInt64))
	})

	// =========================================================================
	// 37. CountDuplicates=true non-idempotent mode basic test
	// =========================================================================
	It("CountDuplicates=true allows duplicate scores to increment rank count", func() {
		ks := specSubspace()

		idx := NewTimeWindowLeaderboardIndex("lb_countdup",
			Concat(Field("price"), Field("quantity")))
		idx.Options[IndexOptionRankCountDuplicates] = "true"
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

			// Two records with the SAME score (price=100, ts=1000).
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// One record with score=200
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan all entries — should have 3 index entries.
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// With CountDuplicates, the ranked set counts score=100 twice.
			// TIME_WINDOW_COUNT should reflect this.
			fn := &IndexAggregateFunction{
				Name:    FunctionNameTimeWindowCount,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}
			result, err := store.EvaluateAggregateFunction(ctx, nil, fn,
				TupleRangeAllOf(tuple.Tuple{int64(0), int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			// With CountDuplicates: size=3 (100, 100, 200). Without: size=2.
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 38. Delete all records clears leaderboard
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

	// =========================================================================
	// 39. Continuation tokens: paginated scan across leaderboard entries
	// =========================================================================
	It("continuation tokens: paginated scan resumes correctly", func() {
		ks := specSubspace()
		idx := NewTimeWindowLeaderboardIndex("leaderboard_score",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 1000,
			AllTime:         true,
			Rebuild:         TimeWindowRebuildIfOverlappingChanged,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 5 records with ascending prices (scores).
			for i, p := range []int32{100, 200, 300, 400, 500} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i + 1)),
					Price:    proto.Int32(p),
					Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Page 1: limit 2
			scanProps := ForwardScan()
			scanProps.ExecuteProperties.ReturnedRowLimit = 2
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, nil, scanProps))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(2))
			Expect(cont1).NotTo(BeNil(), "continuation should be non-nil after partial page")
			Expect(toInt64(page1[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(page1[1].Key[0])).To(Equal(int64(200)))

			// Page 2: resume with continuation, limit 2
			page2, cont2, err := AsListWithContinuation(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, cont1, scanProps))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(2))
			Expect(cont2).NotTo(BeNil())
			Expect(toInt64(page2[0].Key[0])).To(Equal(int64(300)))
			Expect(toInt64(page2[1].Key[0])).To(Equal(int64(400)))

			// Page 3: resume, should get 1 remaining
			page3, cont3, err := AsListWithContinuation(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByTimeWindow, AllTimeLeaderboardType, 0,
				TupleRangeAll, cont2, scanProps))
			Expect(err).NotTo(HaveOccurred())
			Expect(page3).To(HaveLen(1))
			Expect(toInt64(page3[0].Key[0])).To(Equal(int64(500)))
			// cont3 should be nil (source exhausted)
			Expect(cont3).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 40. TIME_WINDOW_RANK record function with specific time window
	// (tests EvaluateRecordFunction TIME_WINDOW_RANK, error when TimeWindow nil,
	// and nil result for record not in window — retained from earlier)
	// =========================================================================
	It("TIME_WINDOW_RANK with specific time window returns correct rank", func() {
		ks := specSubspace()
		idx := NewTimeWindowLeaderboardIndex("leaderboard_score",
			Concat(Field("price"), Field("quantity")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Set up a bounded window [1000, 2000) plus all-time.
		setupWindows(ks, md, &TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 1000,
			AllTime:         true,
			Specs: []TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, StartIncrement: 1000, Duration: 1000, Count: 1},
			},
			Rebuild: TimeWindowRebuildIfOverlappingChanged,
		}, idx)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Record 1: price=300, timestamp=1500 (in window [1000,2000))
			rec1, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(300), Quantity: proto.Int32(1500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record 2: price=100, timestamp=1200 (in window [1000,2000))
			rec2, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(1200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Record 3: price=200, timestamp=500 (NOT in window [1000,2000), only in all-time)
			rec3, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// All-time rank: all 3 records. Sorted by (price, quantity):
			// (100,1200)→rank 0, (200,500)→rank 1, (300,1500)→rank 2
			allTimeFn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}
			rank1, err := store.EvaluateRecordFunction(allTimeFn, rec1)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank1).NotTo(BeNil())
			Expect(*rank1).To(Equal(int64(2))) // price=300 → rank 2

			rank2, err := store.EvaluateRecordFunction(allTimeFn, rec2)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank2).To(Equal(int64(0))) // price=100 → rank 0

			rank3, err := store.EvaluateRecordFunction(allTimeFn, rec3)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank3).To(Equal(int64(1))) // price=200 → rank 1

			// TIME_WINDOW_RANK for window type=1, timestamp=1500:
			// Only records 1 and 2 are in this window (timestamps 1500 and 1200).
			// In the window: (100,1200)→rank 0, (300,1500)→rank 1
			twFn := &IndexRecordFunction{
				Name:    FunctionNameTimeWindowRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
				TimeWindow: &TimeWindowForFunction{
					LeaderboardType:      1,
					LeaderboardTimestamp: 1500,
				},
			}

			twRank1, twErr := store.EvaluateRecordFunction(twFn, rec1)
			Expect(twErr).NotTo(HaveOccurred())
			Expect(twRank1).NotTo(BeNil())
			Expect(*twRank1).To(Equal(int64(1))) // price=300 → rank 1 in bounded window

			twRank2, twErr := store.EvaluateRecordFunction(twFn, rec2)
			Expect(twErr).NotTo(HaveOccurred())
			Expect(twRank2).NotTo(BeNil())
			Expect(*twRank2).To(Equal(int64(0))) // price=100 → rank 0 in bounded window

			// Record 3 (timestamp=500) is NOT in window [1000,2000) → should return nil
			twRank3, twErr := store.EvaluateRecordFunction(twFn, rec3)
			Expect(twErr).NotTo(HaveOccurred())
			Expect(twRank3).To(BeNil())

			// Without TimeWindow, TIME_WINDOW_RANK should error.
			badFn := &IndexRecordFunction{
				Name:    FunctionNameTimeWindowRank,
				Operand: idx.RootExpression,
				Index:   idx.Name,
			}
			_, badErr := store.EvaluateRecordFunction(badFn, rec1)
			Expect(badErr).To(HaveOccurred())
			Expect(badErr.Error()).To(ContainSubstring("requires TimeWindow"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 41. EvaluateTimeWindowRankAndEntry returns rank + score tuple
	// =========================================================================
	Describe("EvaluateTimeWindowRankAndEntry", func() {
		It("returns Tuple{rank, scoreComponents...} for record in window", func() {
			ks := specSubspace()

			idx := NewTimeWindowLeaderboardIndex("lb_rankentry",
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

				// Save 3 records with different scores.
				rec1, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1), Price: proto.Int32(300), Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
				rec2, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(1000),
				})
				Expect(err).NotTo(HaveOccurred())

				maintainerIface, mErr := store.GetIndexMaintainer(idx)
				Expect(mErr).NotTo(HaveOccurred())
				twlm := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)

				// rec1 price=300 → rank 2 in all-time (scores: 100, 200, 300)
				result1, err := twlm.EvaluateTimeWindowRankAndEntry(rec1, AllTimeLeaderboardType, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(result1).NotTo(BeNil())
				// Result is Tuple{rank, score, timestamp}
				Expect(result1[0]).To(Equal(int64(2)))    // rank
				Expect(result1[1]).To(Equal(int64(300)))  // score (price)
				Expect(result1[2]).To(Equal(int64(1000))) // timestamp (quantity)

				// rec2 price=100 → rank 0
				result2, err := twlm.EvaluateTimeWindowRankAndEntry(rec2, AllTimeLeaderboardType, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(result2).NotTo(BeNil())
				Expect(result2[0]).To(Equal(int64(0)))    // rank
				Expect(result2[1]).To(Equal(int64(100)))  // score
				Expect(result2[2]).To(Equal(int64(1000))) // timestamp

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil when record has no rank in the window", func() {
			ks := specSubspace()

			idx := NewTimeWindowLeaderboardIndex("lb_rankentry_nil",
				Concat(Field("price"), Field("quantity")))
			builder := baseMetaData()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Only typed window [1000, 2000), NO all-time.
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

				// Record at ts=500 — outside the window.
				rec, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())

				maintainerIface, mErr := store.GetIndexMaintainer(idx)
				Expect(mErr).NotTo(HaveOccurred())
				twlm := maintainerIface.(*timeWindowLeaderboardIndexMaintainer)

				// All-time leaderboard doesn't exist → nil result.
				result, err := twlm.EvaluateTimeWindowRankAndEntry(rec, AllTimeLeaderboardType, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNil())

				// Also nil for the typed window (record timestamp 500 is outside [1000, 2000)).
				result2, err := twlm.EvaluateTimeWindowRankAndEntry(rec, 1, 1500)
				Expect(err).NotTo(HaveOccurred())
				Expect(result2).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// 42. ScanIndexRanked prefix validation: grouped index requires group prefix
	// =========================================================================
	It("BY_RANK scan on grouped index errors when range too short", func() {
		ks := specSubspace()

		// Grouped leaderboard: group by order_id.
		idx := NewTimeWindowLeaderboardIndex("lb_rank_prefix_err",
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

			// Save a record so the leaderboard has data.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000),
			})
			Expect(err).NotTo(HaveOccurred())

			// BY_RANK scan with TupleRange that's too short (missing group prefix).
			// The grouped index has groupingCount=1, so Low/High must each have
			// at least 1 element for the group prefix. Passing empty tuples (or nil)
			// triggers the validation error.
			_, err = AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByRank, AllTimeLeaderboardType, 0,
				TupleRange{
					Low:          tuple.Tuple{}, // empty — missing group prefix
					High:         tuple.Tuple{}, // empty — missing group prefix
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rank scan range must include group"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
