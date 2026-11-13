package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"math"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Java reads and clears these over Subspace.range(t), which excludes values whose
// packing extends t's: b"\x01" (01 01 00) against b"\x01\x00\x05" (01 01 00 ff 05 00).
var _ = Describe("Tuple-range index probes", func() {
	ctx := context.Background()

	shortB, longB := []byte{0x01}, []byte{0x01, 0x00, 0x05}
	shortS, longS := "a", "a\x00b"

	build := func(configure func(*RecordMetaDataBuilder), indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if configure != nil {
			configure(builder)
		}
		for _, idx := range indexes {
			builder.AddIndex("TypedRecord", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	run := func(md *RecordMetaData, ks subspace.Subspace, fn func(store *FDBRecordStore)) {
		GinkgoHelper()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			fn(store)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	save := func(store *FDBRecordStore, rec *demopb.TypedRecord) {
		GinkgoHelper()
		_, err := store.SaveRecord(rec)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("unique VALUE index", func() {
		bytesIdx := func() *Index { return NewIndex("typed$bytes_unique", Field("val_bytes")).SetUnique() }
		stringIdx := func() *Index { return NewIndex("typed$string_unique", Field("val_string")).SetUnique() }

		It("accepts a value whose packing a stored value extends", func() {
			bIdx, sIdx := bytesIdx(), stringIdx()
			md := build(nil, bIdx, sIdx)
			run(md, specSubspace(), func(store *FDBRecordStore) {
				save(store, &demopb.TypedRecord{Id: proto.Int64(1), ValBytes: longB, ValString: proto.String(longS)})
				save(store, &demopb.TypedRecord{Id: proto.Int64(2), ValBytes: shortB, ValString: proto.String(shortS)})

				// A real duplicate on either index is still refused.
				_, err := store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(3), ValBytes: shortB, ValString: proto.String("z")})
				var violation *RecordIndexUniquenessViolationError
				Expect(errors.As(err, &violation)).To(BeTrue(), "duplicate bytes: %v", err)
				Expect(violation.IndexName).To(Equal(bIdx.Name))
				Expect(violation.ExistingKey).To(Equal(tuple.Tuple{int64(2)}))

				_, err = store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(4), ValBytes: []byte{0x09}, ValString: proto.String(shortS)})
				Expect(errors.As(err, &violation)).To(BeTrue(), "duplicate string: %v", err)
				Expect(violation.IndexName).To(Equal(sIdx.Name))
				Expect(violation.ExistingKey).To(Equal(tuple.Tuple{int64(2)}))
			})
		})

		It("writes no violation entries for an extending value while WRITE_ONLY", func() {
			bIdx := bytesIdx()
			md := build(nil, bIdx)
			ks := specSubspace()
			run(md, ks, func(store *FDBRecordStore) {
				_, err := store.MarkIndexWriteOnly(bIdx.Name)
				Expect(err).NotTo(HaveOccurred())
			})
			run(md, ks, func(store *FDBRecordStore) {
				save(store, &demopb.TypedRecord{Id: proto.Int64(1), ValBytes: longB})
				save(store, &demopb.TypedRecord{Id: proto.Int64(2), ValBytes: shortB})
				violations, err := store.ScanUniquenessViolations(bIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())

				// A real duplicate records both sides.
				save(store, &demopb.TypedRecord{Id: proto.Int64(3), ValBytes: shortB})
				violations, err = store.ScanUniquenessViolations(bIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(2))
				for _, v := range violations {
					Expect(v.IndexKey).To(Equal(tuple.Tuple{shortB}))
				}
			})
		})

		It("resolves one value's violations without deleting an extending value's records", func() {
			sIdx := stringIdx()
			md := build(nil, sIdx)
			ks := specSubspace()
			run(md, ks, func(store *FDBRecordStore) {
				_, err := store.MarkIndexWriteOnly(sIdx.Name)
				Expect(err).NotTo(HaveOccurred())
			})
			run(md, ks, func(store *FDBRecordStore) {
				for id, s := range map[int64]string{1: shortS, 2: shortS, 3: longS, 4: longS} {
					save(store, &demopb.TypedRecord{Id: proto.Int64(id), ValString: proto.String(s)})
				}
				all, err := store.ScanUniquenessViolations(sIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(all).To(HaveLen(4))

				forShort, err := store.ScanUniquenessViolationsForValue(sIdx, tuple.Tuple{shortS})
				Expect(err).NotTo(HaveOccurred())
				var pks []tuple.Tuple
				for _, v := range forShort {
					Expect(v.IndexKey).To(Equal(tuple.Tuple{shortS}))
					pks = append(pks, v.PrimaryKey)
				}
				Expect(pks).To(ConsistOf(tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)}))

				Expect(store.ResolveUniquenessViolationByDeletion(sIdx, tuple.Tuple{shortS}, tuple.Tuple{int64(1)})).To(Succeed())
				for id, want := range map[int64]bool{1: true, 2: false, 3: true, 4: true} {
					rec, err := store.LoadRecord(tuple.Tuple{id})
					Expect(err).NotTo(HaveOccurred())
					Expect(rec != nil).To(Equal(want), "record %d", id)
				}
				// Java's survivor clear is Range.startsWith(pack("a")), which also takes
				// "a\x00b"'s violations (removeUniquenessViolationsAsync); Go matches.
				all, err = store.ScanUniquenessViolations(sIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(all).To(BeEmpty())
			})
		})
	})

	It("RANK removes a score whose packing a remaining score extends", func() {
		sIdx := NewRankIndex("typed$string_rank", GroupBy(Field("val_string")))
		bIdx := NewRankIndex("typed$bytes_rank", GroupBy(Field("val_bytes")))
		md := build(nil, sIdx, bIdx)
		run(md, specSubspace(), func(store *FDBRecordStore) {
			for _, idx := range []*Index{sIdx, bIdx} {
				m, err := store.getIndexMaintainer(idx)
				Expect(err).NotTo(HaveOccurred())
				Expect(m.(*rankIndexMaintainer).rankedSetConfig.CountDuplicates).To(BeFalse(),
					"only the non-counting ranked set probes the B-tree before removing")
			}
			save(store, &demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String(longS), ValBytes: longB})
			save(store, &demopb.TypedRecord{Id: proto.Int64(2), ValString: proto.String(shortS), ValBytes: shortB})
			_, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			for idx, scores := range map[*Index][2]any{sIdx: {shortS, longS}, bIdx: {shortB, longB}} {
				m, err := store.getIndexMaintainer(idx)
				Expect(err).NotTo(HaveOccurred())
				rank := m.(*rankIndexMaintainer)
				gone, err := rank.RankForScore(tuple.Tuple{scores[0]}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(gone).To(BeNil(), "%s: removed score stays in the ranked set", idx.Name)
				kept, err := rank.RankForScore(tuple.Tuple{scores[1]}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*kept).To(Equal(int64(0)), idx.Name)
			}
		})
	})

	It("TIME_WINDOW_LEADERBOARD removes a score whose packing a remaining score extends", func() {
		idx := NewTimeWindowLeaderboardIndex("typed$leaderboard",
			Ungrouped(Concat(Field("price"), Field("val_int64"), Field("val_string"))))
		md := build(nil, idx)
		ks := specSubspace()
		run(md, ks, func(store *FDBRecordStore) {
			m, err := store.GetIndexMaintainer(idx)
			Expect(err).NotTo(HaveOccurred())
			update := &TimeWindowLeaderboardWindowUpdate{UpdateTimestamp: 500, AllTime: true}
			lb := m.(*timeWindowLeaderboardIndexMaintainer)
			Expect(lb.rankedSetConfig.CountDuplicates).To(BeFalse())
			Expect(lb.PerformWindowUpdate(update, store)).To(Succeed())
		})
		run(md, ks, func(store *FDBRecordStore) {
			save(store, &demopb.TypedRecord{Id: proto.Int64(1), Price: proto.Int32(100), ValInt64: proto.Int64(1000), ValString: proto.String(longS)})
			save(store, &demopb.TypedRecord{Id: proto.Int64(2), Price: proto.Int32(100), ValInt64: proto.Int64(1000), ValString: proto.String(shortS)})
			_, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Rank 0 is the remaining record; a stale score at rank 0 maps to an
			// empty B-tree range instead.
			entries, err := AsList(ctx, store.ScanTimeWindowLeaderboard(
				idx, IndexScanByRank, AllTimeLeaderboardType, 0,
				TupleRange{
					Low: tuple.Tuple{int64(0)}, High: tuple.Tuple{int64(1)},
					LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
		})
	})

	It("SUM removes math.MinInt64 modulo 2^64, as Java does", func() {
		idx := NewSumIndex("typed$sum_int64", Ungrouped(Field("val_int64")))
		md := build(nil, idx)
		ks := specSubspace()
		sum := func(store *FDBRecordStore) int64 {
			GinkgoHelper()
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			return entries[0].Value[0].(int64)
		}
		minRecord := func(v int64) *demopb.TypedRecord {
			return &demopb.TypedRecord{Id: proto.Int64(1), ValInt64: proto.Int64(v)}
		}

		run(md, ks, func(store *FDBRecordStore) {
			save(store, minRecord(math.MinInt64))
			save(store, &demopb.TypedRecord{Id: proto.Int64(2), ValInt64: proto.Int64(5)})
			Expect(sum(store)).To(Equal(int64(math.MinInt64 + 5)))
		})
		run(md, ks, func(store *FDBRecordStore) {
			save(store, minRecord(7))
			Expect(sum(store)).To(Equal(int64(12)))
		})
		run(md, ks, func(store *FDBRecordStore) {
			save(store, minRecord(math.MinInt64))
			Expect(sum(store)).To(Equal(int64(math.MinInt64 + 5)))
		})
		run(md, ks, func(store *FDBRecordStore) {
			_, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(sum(store)).To(Equal(int64(5)))
		})
	})

	// Format 5 keeps versions in the legacy RecordVersionKey subspace; 0 is the default format.
	for _, format := range []int32{0, formatVersionSaveUnsplitWithSuffix} {
		It(fmt.Sprintf("DeleteRecordsWhere keeps the records, versions, counts and permuted entries of an extending prefix (format %d)", format), func() {
			permuted := NewPermutedMaxIndex("typed$permuted_max",
				GroupBy(Field("val_int64"), Field("val_string"), Field("price")), 1)
			value := NewIndex("typed$string_price", Concat(Field("val_string"), Field("price")))
			md := build(func(b *RecordMetaDataBuilder) {
				b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(Field("val_string"), Field("id")))
				b.SetRecordCountKey(Concat(Field("val_string"), Field("price")))
				b.SetStoreRecordVersions(true)
			}, permuted, value)
			ks := specSubspace()
			typed := func(s string, id, v int64) *demopb.TypedRecord {
				return &demopb.TypedRecord{Id: proto.Int64(id), ValString: proto.String(s), ValInt64: proto.Int64(v), Price: proto.Int32(10)}
			}
			runFormat := func(fn func(store *FDBRecordStore)) {
				GinkgoHelper()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					builder := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
					if format != 0 {
						builder.SetFormatVersion(format)
					}
					store, err := builder.CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					fn(store)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}

			runFormat(func(store *FDBRecordStore) {
				Expect(store.useOldVersionFormat()).To(Equal(format != 0))
				save(store, typed(shortS, 1, 5))
				save(store, typed(longS, 2, 7))
			})
			// Records 3 and 4 still have pending version mutations and local versions
			// when the clear runs; record 3 reads its local version back in that transaction.
			runFormat(func(store *FDBRecordStore) {
				save(store, typed(shortS, 4, 6))
				saved, err := store.SaveRecord(typed(longS, 3, 8))
				Expect(err).NotTo(HaveOccurred())
				Expect(saved.Version).NotTo(BeNil())
				Expect(saved.Version.IsComplete()).To(BeFalse())
				Expect(store.DeleteRecordsWhere(tuple.Tuple{shortS})).To(Succeed())
				rec, err := store.LoadRecord(tuple.Tuple{longS, int64(3)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.Version).To(Equal(saved.Version))
			})
			runFormat(func(store *FDBRecordStore) {
				for _, pk := range []tuple.Tuple{{shortS, int64(1)}, {shortS, int64(4)}} {
					rec, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).To(BeNil(), "%v", pk)
				}
				for _, pk := range []tuple.Tuple{{longS, int64(2)}, {longS, int64(3)}} {
					rec, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "%v", pk)
					Expect(rec.Version).NotTo(BeNil(), "%v", pk)
				}

				count, err := store.GetSnapshotRecordCount(tuple.Tuple{longS, int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(2)))
				count, err = store.GetSnapshotRecordCount(tuple.Tuple{shortS, int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(BeZero())

				// Java clears the permuted entries over the prefix's tuple range;
				// the extending group's maximum survives.
				byGroup, err := AsList(ctx, store.ScanIndexByType(permuted, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(1))
				Expect(byGroup[0].Key).To(Equal(tuple.Tuple{longS, int64(8), int64(10)}))

				// Java clears a standard index over [key, strinc(key)), so the extending
				// prefix loses its VALUE entries but keeps its records; Go matches.
				entries, err := AsList(ctx, store.ScanIndex(value, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())
			})
		})
	}

	It("DeleteIndexEntriesInRange clears the prefix's tuple range and its own key", func() {
		valueIdx := NewIndex("typed$string", Field("val_string"))
		countIdx := NewCountIndex("typed$count_by_string", GroupAll(Field("val_string")))
		md := build(nil, valueIdx, countIdx)
		run(md, specSubspace(), func(store *FDBRecordStore) {
			save(store, &demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String(shortS)})
			save(store, &demopb.TypedRecord{Id: proto.Int64(2), ValString: proto.String(longS)})
			for _, idx := range []*Index{valueIdx, countIdx} {
				Expect(store.DeleteIndexEntriesInRange(idx, tuple.Tuple{shortS})).To(Succeed())
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), idx.Name)
				Expect(entries[0].Key[0]).To(Equal(longS), idx.Name)
			}
		})
	})
})
