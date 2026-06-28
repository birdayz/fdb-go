package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RankedSet", func() {
	ctx := context.Background()

	It("add, rank, getNth, size, contains", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			rs := newRankedSet(sub, defaultRankedSetConfig)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			// Add elements: "a", "c", "b"
			added, err := rs.Add(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeTrue())

			added, err = rs.Add(tx, []byte("c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeTrue())

			added, err = rs.Add(tx, []byte("b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeTrue())

			// Duplicate add (no CountDuplicates) → false
			added, err = rs.Add(tx, []byte("b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeFalse())

			// Size = 3
			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(3)))

			// Contains
			has, err := rs.Contains(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeTrue())

			has, err = rs.Contains(tx, []byte("z"))
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeFalse())

			// Rank (sorted order: a=0, b=1, c=2)
			rank, err := rs.Rank(tx, []byte("a"), false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = rs.Rank(tx, []byte("b"), false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			rank, err = rs.Rank(tx, []byte("c"), false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			// Rank of non-existent key with nullIfMissing=true → nil
			rank, err = rs.Rank(tx, []byte("z"), true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).To(BeNil())

			// GetNth
			nth, err := rs.GetNth(tx, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("a")))

			nth, err = rs.GetNth(tx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("b")))

			nth, err = rs.GetNth(tx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("c")))

			// GetNth out of bounds → nil
			nth, err = rs.GetNth(tx, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(BeNil())

			nth, err = rs.GetNth(tx, -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("remove", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			rs := newRankedSet(sub, defaultRankedSetConfig)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			for _, k := range []string{"a", "b", "c", "d"} {
				_, err := rs.Add(tx, []byte(k))
				Expect(err).NotTo(HaveOccurred())
			}

			// Remove "b"
			removed, err := rs.Remove(tx, []byte("b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeTrue())

			// Size = 3
			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(3)))

			// "b" no longer contains
			has, err := rs.Contains(tx, []byte("b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeFalse())

			// Ranks shifted: a=0, c=1, d=2
			rank, err := rs.Rank(tx, []byte("c"), false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// GetNth: rank 1 → "c"
			nth, err := rs.GetNth(tx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("c")))

			// Remove non-existent → false
			removed, err = rs.Remove(tx, []byte("z"))
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("count duplicates", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			config := rankedSetConfig{
				HashFunction:    jdkArrayHash,
				NLevels:         rankedSetDefaultLevels,
				CountDuplicates: true,
			}
			rs := newRankedSet(sub, config)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			// Add "a" twice, "b" once
			_, err := rs.Add(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			added, err := rs.Add(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeTrue()) // CountDuplicates allows re-add

			_, err = rs.Add(tx, []byte("b"))
			Expect(err).NotTo(HaveOccurred())

			// Size = 3 (two "a"s + one "b")
			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(3)))

			// Count
			count, err := rs.Count(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(2)))

			// Rank of "b" is 2 (two "a"s before it)
			rank, err := rs.Rank(tx, []byte("b"), false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			// GetNth with duplicates: rank 0 → "a", rank 1 → "a" (dup), rank 2 → "b"
			nth, err := rs.GetNth(tx, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("a")))

			nth, err = rs.GetNth(tx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("a"))) // duplicate — still "a"

			nth, err = rs.GetNth(tx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("b")))

			// Remove one "a" — count should be 1
			removed, err := rs.Remove(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeTrue())

			size, err = rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(2)))

			count, err = rs.Count(tx, []byte("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clear reinitializes", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			rs := newRankedSet(sub, defaultRankedSetConfig)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			_, err := rs.Add(tx, []byte("x"))
			Expect(err).NotTo(HaveOccurred())

			Expect(rs.Clear(tx)).To(Succeed())

			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(0)))

			// Can still add after clear
			_, err = rs.Add(tx, []byte("y"))
			Expect(err).NotTo(HaveOccurred())

			size, err = rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("many elements rank consistency", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			rs := newRankedSet(sub, defaultRankedSetConfig)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			// Add 50 elements with tuple-packed keys (like real index usage)
			for i := range 50 {
				key := tuple.Tuple{int64(i) * 10}.Pack()
				_, err := rs.Add(tx, key)
				Expect(err).NotTo(HaveOccurred())
			}

			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(50)))

			// Every rank should correspond to the correct element
			for i := range 50 {
				expected := tuple.Tuple{int64(i) * 10}.Pack()

				nth, err := rs.GetNth(tx, int64(i))
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal(expected), "GetNth(%d) mismatch", i)

				rank, err := rs.Rank(tx, expected, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(i)), "Rank of element %d mismatch", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("CRC hash function", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rs")
			config := rankedSetConfig{
				HashFunction: crcHash,
				NLevels:      rankedSetDefaultLevels,
			}
			rs := newRankedSet(sub, config)
			tx := rtx.Transaction()

			Expect(rs.Init(tx)).To(Succeed())

			for i := range 20 {
				key := tuple.Tuple{int64(i)}.Pack()
				_, err := rs.Add(tx, key)
				Expect(err).NotTo(HaveOccurred())
			}

			size, err := rs.Size(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal(int64(20)))

			// Verify rank/getNth consistency
			for i := range 20 {
				expected := tuple.Tuple{int64(i)}.Pack()
				nth, err := rs.GetNth(tx, int64(i))
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal(expected))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("RankIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("maintains rank index on save and delete", func() {
		ks := specSubspace()

		// RANK index on price (ungrouped — just the score, no group prefix)
		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders with prices: 300, 100, 200
			for _, p := range []struct {
				id    int64
				price int32
			}{
				{1, 300},
				{2, 100},
				{3, 200},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(p.id),
					Price:   proto.Int32(p.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan BY_VALUE — should be sorted by price
			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			// Get the rank maintainer to check ranked set
			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)

			// RankForScore: price 100 → rank 0, price 200 → rank 1, price 300 → rank 2
			rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(200)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(300)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			// ScoreForRank
			score, err := maintainer.ScoreForRank(tuple.Tuple{int64(0)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(100)}))

			score, err = maintainer.ScoreForRank(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(300)}))

			// Delete the order with price=100
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)}) // order_id=2
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Now ranks should shift: price 200 → rank 0, price 300 → rank 1
			// Need fresh maintainer after delete
			maintainerIface, mErr = store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer = maintainerIface.(*rankIndexMaintainer)

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(200)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(300)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// Missing score → nil with nullIfMissing
			rank, err = maintainer.RankForScore(tuple.Tuple{int64(100)}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK returns entries in rank order", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 orders with different prices
			prices := []int32{500, 100, 300, 200, 400}
			for i, p := range prices {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(p),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan BY_RANK [0, 5) — should return all in score order
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(5)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))
			Expect(entries[3].Key[0]).To(Equal(int64(400)))
			Expect(entries[4].Key[0]).To(Equal(int64(500)))

			// Scan BY_RANK [1, 3) — ranks 1 and 2 → prices 200, 300
			entries, err = AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(1)},
					High:         tuple.Tuple{int64(3)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK inclusive range", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(p),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan BY_RANK [0, 2] inclusive — all three
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(2)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeInclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK empty range returns empty", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Exclusive range [0, 0) is empty
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(0)},
					LowEndpoint:  EndpointTypeRangeExclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update record price updates rank", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert: id=1 price=100, id=2 price=200
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Update id=1 price from 100 to 300 (now it should rank higher than id=2)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			// Verify ranks: price 200 → rank 0, price 300 → rank 1
			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)

			rank, err := maintainer.RankForScore(tuple.Tuple{int64(200)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(300)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// Old score 100 should be gone
			rank, err = maintainer.RankForScore(tuple.Tuple{int64(100)}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).To(BeNil())

			// B-tree should only have 2 entries
			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple records with same score (duplicates in ranked set)", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Three orders, two with the same price
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Ranked set has 2 unique scores (100 and 200) — !CountDuplicates
			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)
			rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(200)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// B-tree has 3 entries (each record gets its own entry)
			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Delete one of the two records with price=100
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Score 100 should still be in ranked set (other record has it)
			maintainerIface, mErr = store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer = maintainerIface.(*rankIndexMaintainer)
			rank, err = maintainer.RankForScore(tuple.Tuple{int64(100)}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).NotTo(BeNil())

			// Delete the last record with price=100
			existed, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Now score 100 should be gone from ranked set
			maintainerIface, mErr = store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer = maintainerIface.(*rankIndexMaintainer)
			rank, err = maintainer.RankForScore(tuple.Tuple{int64(100)}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuild rank index", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))

		// Build metadata without the index first
		builder1 := baseMetaData()
		md1, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		// Insert records without index
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{300, 100, 200} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(p),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Build new metadata with the rank index
		builder2 := baseMetaData()
		builder2.SetVersion(2)
		builder2.AddIndex("Order", rankIdx)
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		// Open with new metadata → auto-rebuild
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Verify B-tree entries exist
			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))

			// Verify ranked set works
			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)
			rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{int64(300)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("index state cleared on rebuild includes secondary subspace", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Create store, add data
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Rebuild the index
			Expect(store.RebuildIndex(rankIdx)).To(Succeed())

			// Verify it still works after rebuild
			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)
			rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK with no low bound", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(p),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan BY_RANK with high=2 exclusive, no low → all up to rank 2
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					High:         tuple.Tuple{int64(2)},
					HighEndpoint: EndpointTypeRangeExclusive,
					LowEndpoint:  EndpointTypeTreeStart,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK with no high bound returns all from rank", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, p := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(p),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan BY_RANK from rank 1, no high → ranks 1,2
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(1)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeTreeEnd,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("non-rank index rejects BY_RANK scan", func() {
		ks := specSubspace()

		valueIdx := NewIndex("val_idx", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", valueIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			cursor := store.ScanIndexByType(
				valueIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(1)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			)
			_, err = AsList(ctx, cursor)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not support BY_RANK"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("grouped rank index with groupBy", func() {
		ks := specSubspace()

		// Group by flower type, rank by price
		// Need a field that can serve as group key. Use tags[0] via... actually
		// let's keep it simple — use a composite key where flower.type is the group.
		// Hmm, Order has flower (nested message) and price. Let's just use two
		// simple fields from Order for grouping.
		// Actually Order doesn't have a good group field besides flower (nested).
		// Let's test with ungrouped only for now since the grouped path exercises
		// the same ranked set operations.
		// Instead, test with a scenario that verifies group isolation would work
		// if we had a flat group field. We can skip this test or use the nested field.

		// Use price as "score" and create two sets with no group field — simpler.
		// Actually, let's just test with a direct RankIndexMaintainer on a grouped expression
		// using the nested Flower.type field.
		// Wait — we can just use the direct ranked set for group isolation tests above.
		// For index-level tests, ungrouped covers the full path. Skip grouped index test for now.

		// Instead: test that ScanIndexByType with BY_VALUE works as fallback
		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// BY_VALUE scan through ScanIndexByType
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByValue,
				TupleRangeAll,
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[1].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("JDK hash function matches Java Arrays.hashCode", func() {
		// Test vectors verified against Java's Arrays.hashCode(byte[])
		// Java: Arrays.hashCode(new byte[]{}) = 1
		Expect(jdkArrayHash([]byte{})).To(Equal(int32(1)))

		// Java: Arrays.hashCode(new byte[]{0}) = 31
		Expect(jdkArrayHash([]byte{0})).To(Equal(int32(31)))

		// Java: Arrays.hashCode(new byte[]{1}) = 32
		Expect(jdkArrayHash([]byte{1})).To(Equal(int32(32)))

		// Java: Arrays.hashCode(new byte[]{-1}) = 30 (signed byte -1 = 0xFF)
		Expect(jdkArrayHash([]byte{0xFF})).To(Equal(int32(30)))

		// Java: Arrays.hashCode(new byte[]{1, 2}) = 31*32 + 2 = 994
		Expect(jdkArrayHash([]byte{1, 2})).To(Equal(int32(994)))
	})

	It("deleteAllRecords clears rank index", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			Expect(store.DeleteAllRecords()).To(Succeed())

			entries, err := AsList(ctx, store.ScanIndex(rankIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("grouped rank index isolates groups", func() {
		ks := specSubspace()

		// Group by customer name, rank by customer_id (used as a "score")
		// Expression: GroupBy(Field("customer_id"), Field("name"))
		//   → wholeKey = Concat(name, customer_id), groupedCount = 1
		//   → grouping columns = [name], grouped (score) = [customer_id]
		rankIdx := NewRankIndex("rank_by_name_id", GroupBy(Field("customer_id"), Field("name")))
		builder := baseMetaData()
		builder.AddIndex("Customer", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group "alice": ids 10, 30 → ranks 0, 1
			// Group "bob":   ids 20, 40 → ranks 0, 1
			for _, c := range []struct {
				id   int64
				name string
			}{
				{10, "alice"},
				{20, "bob"},
				{30, "alice"},
				{40, "bob"},
			} {
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(c.id),
					Name:       proto.String(c.name),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)

			// In "alice" group: id 10 → rank 0, id 30 → rank 1
			rank, err := maintainer.RankForScore(tuple.Tuple{"alice", int64(10)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{"alice", int64(30)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// In "bob" group: id 20 → rank 0, id 40 → rank 1
			rank, err = maintainer.RankForScore(tuple.Tuple{"bob", int64(20)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			rank, err = maintainer.RankForScore(tuple.Tuple{"bob", int64(40)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// ScoreForRank within alice group
			score, err := maintainer.ScoreForRank(tuple.Tuple{"alice", int64(0)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(10)}))

			score, err = maintainer.ScoreForRank(tuple.Tuple{"alice", int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(30)}))

			// BY_RANK scan within "alice" group: ranks [0, 2)
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{"alice", int64(0)},
					High:         tuple.Tuple{"alice", int64(2)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			// Entries should have group prefix "alice" + score
			Expect(entries[0].Key[0]).To(Equal("alice"))
			Expect(entries[0].Key[1]).To(Equal(int64(10)))
			Expect(entries[1].Key[0]).To(Equal("alice"))
			Expect(entries[1].Key[1]).To(Equal(int64(30)))

			// Delete alice id=10, verify rank shifts
			existed, err := store.DeleteRecord(tuple.Tuple{int64(10)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			maintainerIface, mErr = store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer = maintainerIface.(*rankIndexMaintainer)

			// Alice group: id 30 is now rank 0
			rank, err = maintainer.RankForScore(tuple.Tuple{"alice", int64(30)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			// Bob group unchanged
			rank, err = maintainer.RankForScore(tuple.Tuple{"bob", int64(20)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("count duplicates at index level", func() {
		ks := specSubspace()

		// RANK index with CountDuplicates=true
		rankIdx := NewRankIndex("rank_by_price_dup", GroupBy(Field("price")))
		rankIdx.Options[IndexOptionRankCountDuplicates] = "true"
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 orders: two with price=100, one with price=200
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
			Expect(mErr).NotTo(HaveOccurred())
			maintainer := maintainerIface.(*rankIndexMaintainer)

			// With CountDuplicates, score 100 has count=2 in the ranked set.
			// Rank of 100 = 0 (first entry)
			rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			// Rank of 200 = 2 (two 100s before it)
			rank, err = maintainer.RankForScore(tuple.Tuple{int64(200)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			// ScoreForRank(0) = 100, ScoreForRank(1) = 100 (duplicate), ScoreForRank(2) = 200
			score, err := maintainer.ScoreForRank(tuple.Tuple{int64(0)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(100)}))

			score, err = maintainer.ScoreForRank(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(100)}))

			score, err = maintainer.ScoreForRank(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(200)}))

			// BY_RANK scan [0, 3) — all three ranks
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(3)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			// This scans the B-tree range [score(rank=0), score(rank=3))
			// = [100, end) = all 3 entries
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan BY_RANK rank out of bounds returns empty", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Rank range [5,10) — out of bounds → empty
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(5)},
					High:         tuple.Tuple{int64(10)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ForwardScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("BY_RANK scan with ReverseScan returns entries in descending score order", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			prices := []int32{500, 100, 300, 200, 400}
			for i, p := range prices {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(p)})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_RANK [0, 5) reverse → 500, 400, 300, 200, 100
			entries, err := AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(0)},
					High:         tuple.Tuple{int64(5)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ReverseScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[1].Key[0]).To(Equal(int64(400)))
			Expect(entries[2].Key[0]).To(Equal(int64(300)))
			Expect(entries[3].Key[0]).To(Equal(int64(200)))
			Expect(entries[4].Key[0]).To(Equal(int64(100)))

			// BY_RANK [1, 4) reverse → rank 1,2,3 = 200,300,400 → reversed = 400,300,200
			entries, err = AsList(ctx, store.ScanIndexByType(
				rankIdx,
				IndexScanByRank,
				TupleRange{
					Low:          tuple.Tuple{int64(1)},
					High:         tuple.Tuple{int64(4)},
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: EndpointTypeRangeExclusive,
				},
				nil,
				ReverseScan(),
			))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].Key[0]).To(Equal(int64(400)))
			Expect(entries[1].Key[0]).To(Equal(int64(300)))
			Expect(entries[2].Key[0]).To(Equal(int64(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("BY_RANK scan with continuation tokens paginates correctly", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 records with distinct prices
			for i, p := range []int32{500, 100, 300, 200, 400} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(p)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Page 1: first 2 entries (rank range [0,5), limit 2)
			rankRange := TupleRange{
				Low:          tuple.Tuple{int64(0)},
				High:         tuple.Tuple{int64(5)},
				LowEndpoint:  EndpointTypeRangeInclusive,
				HighEndpoint: EndpointTypeRangeExclusive,
			}
			props := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					ReturnedRowLimit: 2,
					IsolationLevel:   IsolationLevelSerializable,
				},
			}

			// Page 1: first 2 entries
			cursor := store.ScanIndexByType(rankIdx, IndexScanByRank, rankRange, nil, props)
			var page1 []*IndexEntry
			var continuation []byte
			for {
				r, nextErr := cursor.OnNext(ctx)
				Expect(nextErr).NotTo(HaveOccurred())
				if !r.HasNext() {
					var contErr error
					continuation, contErr = r.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					break
				}
				page1 = append(page1, r.GetValue())
			}
			Expect(cursor.Close()).To(Succeed())
			Expect(page1).To(HaveLen(2))
			Expect(page1[0].Key[0]).To(Equal(int64(100)))
			Expect(page1[1].Key[0]).To(Equal(int64(200)))
			Expect(continuation).NotTo(BeEmpty())

			// Page 2: next 2 entries using continuation
			cursor = store.ScanIndexByType(rankIdx, IndexScanByRank, rankRange, continuation, props)
			var page2 []*IndexEntry
			for {
				r, nextErr := cursor.OnNext(ctx)
				Expect(nextErr).NotTo(HaveOccurred())
				if !r.HasNext() {
					var contErr error
					continuation, contErr = r.GetContinuation().ToBytes()
					Expect(contErr).NotTo(HaveOccurred())
					break
				}
				page2 = append(page2, r.GetValue())
			}
			Expect(cursor.Close()).To(Succeed())
			Expect(page2).To(HaveLen(2))
			Expect(page2[0].Key[0]).To(Equal(int64(300)))
			Expect(page2[1].Key[0]).To(Equal(int64(400)))
			Expect(continuation).NotTo(BeEmpty())

			// Page 3: last entry
			page3, err := AsList(ctx, store.ScanIndexByType(rankIdx, IndexScanByRank, rankRange, continuation, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page3).To(HaveLen(1))
			Expect(page3[0].Key[0]).To(Equal(int64(500)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("RANK Aggregate Functions", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("COUNT_DISTINCT returns number of unique scores", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 orders: prices 100, 200, 200, 300, 300
			for i, price := range []int32{100, 200, 200, 300, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: GroupBy(Field("price"))},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)})) // 3 unique prices

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RANK_FOR_SCORE returns rank of a given score", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Rank of score 100 = 0 (lowest)
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameRankForScore, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(0)}))

			// Rank of score 300 = 2 (highest)
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameRankForScore, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(300)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SCORE_FOR_RANK returns score at a given rank", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Score at rank 0 = 100
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(100)}))

			// Score at rank 2 = 300
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(2)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(300)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SCORE_FOR_RANK returns nil for out-of-bounds rank", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Rank 5 is out of bounds (only 1 record)
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(5)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SCORE_FOR_RANK_ELSE_SKIP returns sentinel for out-of-bounds rank", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Rank 5 out of bounds → sentinel
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameScoreForRankElseSkip, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(5)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{"*"}))

			// Rank 0 in bounds → actual score
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameScoreForRankElseSkip, Operand: GroupBy(Field("price"))},
				TupleRangeAllOf(tuple.Tuple{int64(0)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(100)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("auto-selects RANK index for aggregate functions", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Auto-select (no explicit index name)
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: GroupBy(Field("price"))},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("COUNT_DISTINCT after deletes reflects current state", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 records with distinct prices
			for i, price := range []int32{100, 200, 300} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			fn := &IndexAggregateFunction{Name: FunctionNameCountDistinct, Operand: GroupBy(Field("price"))}

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(3)}))

			// Delete order 2 (price=200)
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("RANK Record Functions", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("rank function returns rank of a record's score", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert records: prices 300, 100, 500, 200, 400
			records := make([]*FDBStoredRecord[proto.Message], 5)
			prices := []int32{300, 100, 500, 200, 400}
			for i, price := range prices {
				rec, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
				records[i] = rec
			}

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: GroupBy(Field("price")),
			}

			// price=300 → rank 2
			rank, err := store.EvaluateRecordFunction(fn, records[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).NotTo(BeNil())
			Expect(*rank).To(Equal(int64(2)))

			// price=100 → rank 0
			rank, err = store.EvaluateRecordFunction(fn, records[1])
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			// price=500 → rank 4
			rank, err = store.EvaluateRecordFunction(fn, records[2])
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(4)))

			// price=200 → rank 1
			rank, err = store.EvaluateRecordFunction(fn, records[3])
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// price=400 → rank 3
			rank, err = store.EvaluateRecordFunction(fn, records[4])
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rank function with explicit index name", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			rec, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: GroupBy(Field("price")),
				Index:   "rank_by_price",
			}

			rank, err := store.EvaluateRecordFunction(fn, rec)
			Expect(err).NotTo(HaveOccurred())
			Expect(rank).NotTo(BeNil())
			Expect(*rank).To(Equal(int64(0)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rank function with duplicate scores", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// 3 records at price=100, 2 at price=200
			var rec100, rec200 *FDBStoredRecord[proto.Message]
			for i := int64(1); i <= 3; i++ {
				r, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				if i == 1 {
					rec100 = r
				}
			}
			for i := int64(4); i <= 5; i++ {
				r, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				if i == 4 {
					rec200 = r
				}
			}

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: GroupBy(Field("price")),
			}

			// All records with price=100 → rank 0
			rank, err := store.EvaluateRecordFunction(fn, rec100)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			// All records with price=200 → rank 1
			rank, err = store.EvaluateRecordFunction(fn, rec200)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rank function updates after delete", func() {
		ks := specSubspace()

		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			rec1, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			rec3, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: GroupBy(Field("price")),
			}

			// Before delete: rank of 300 = 2
			rank, err := store.EvaluateRecordFunction(fn, rec3)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(2)))

			// Delete price=200
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// After delete: rank of 300 = 1
			rank, err = store.EvaluateRecordFunction(fn, rec3)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(1)))

			// rank of 100 still = 0
			rank, err = store.EvaluateRecordFunction(fn, rec1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*rank).To(Equal(int64(0)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rank function errors when no matching index exists", func() {
		ks := specSubspace()

		// No RANK index, only VALUE.
		builder := baseMetaData()
		builder.AddIndex("Order", NewIndex("Order$price", Field("price")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			rec, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			fn := &IndexRecordFunction{
				Name:    FunctionNameRank,
				Operand: GroupBy(Field("price")),
			}

			_, err = store.EvaluateRecordFunction(fn, rec)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires appropriate index"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RANK_FOR_SCORE with composite score packs full sub-tuple", func() {
		ks := specSubspace()

		// RANK index on composite score (price, order_id) — no grouping.
		// The ranked set key is Tuple{price, orderId}.Pack(), NOT just Tuple{price}.Pack().
		rankIdx := NewRankIndex("rank_composite", GroupBy(Concat(Field("price"), Field("order_id"))))
		builder := baseMetaData()
		builder.AddIndex("Order", rankIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders: (price=100, id=1), (price=200, id=2), (price=300, id=3)
			for _, p := range []struct {
				id    int64
				price int32
			}{
				{1, 100},
				{2, 200},
				{3, 300},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(p.id),
					Price:   proto.Int32(p.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// RANK_FOR_SCORE with composite score (100, 1) should return rank 0.
			// Before the fix: splitEqualRangeForRank only took the first trailing
			// element (100), packing Tuple{100} instead of Tuple{100, 1}. This
			// caused a lookup miss in the ranked set because the stored key is
			// Tuple{100, 1}.Pack().
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameRankForScore,
					Operand: GroupBy(Concat(Field("price"), Field("order_id"))),
				},
				TupleRangeAllOf(tuple.Tuple{int64(100), int64(1)}),
				IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(0)}))

			// RANK_FOR_SCORE with composite score (300, 3) should return rank 2.
			result, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameRankForScore,
					Operand: GroupBy(Concat(Field("price"), Field("order_id"))),
				},
				TupleRangeAllOf(tuple.Tuple{int64(300), int64(3)}),
				IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
