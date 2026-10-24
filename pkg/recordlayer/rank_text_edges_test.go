package recordlayer

import (
	"context"
	"errors"
	"math"
	"math/big"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Rank and TEXT edges match Java", func() {
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

	It("GetNth returns nil below level 0 unless the set counts duplicates", func() {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := specSubspace().Sub("rs")
			counting := rankedSetConfig{HashFunction: pureHash(jdkArrayHash), NLevels: rankedSetDefaultLevels, CountDuplicates: true}
			tx := rtx.Transaction()
			writer := newRankedSet(sub, counting)
			Expect(writer.Init(tx)).To(Succeed())
			for range 2 {
				_, err := writer.Add(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())
			}
			nth, err := writer.GetNth(tx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(Equal([]byte("a")))

			// Java's NthLookup drops the key when it runs past level 0 without
			// duplicate counting (RankedSet.java:576-581).
			plain := counting
			plain.CountDuplicates = false
			nth, err = newRankedSet(sub, plain).GetNth(tx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nth).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a BY_RANK bound is a Number: longValue() for floats, ClassCastError otherwise", func() {
		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		md := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", rankIdx) })
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i, p := range []int32{100, 200, 300} {
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(p)})
				Expect(err).NotTo(HaveOccurred())
			}
			byRank := func(low any) ([]*IndexEntry, error) {
				return AsList(ctx, store.ScanIndexByType(rankIdx, IndexScanByRank, TupleRange{
					Low: tuple.Tuple{low}, High: tuple.Tuple{int64(3)},
					LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeExclusive,
				}, nil, ForwardScan()))
			}
			entries, err := byRank(float64(1.5))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2), "rank 1.5 is (long) 1.5 = 1")
			Expect(entries[0].Key[0]).To(Equal(int64(200)))

			_, err = byRank("1")
			var cast *ClassCastError
			Expect(errors.As(err, &cast)).To(BeTrue(), "%v", err)
			Expect(cast.Message).To(ContainSubstring("java.lang.String"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SCORE_FOR_RANK takes the rank as a Number, as Java does", func() {
		rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
		md := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", rankIdx) })
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i, p := range []int32{100, 200, 300} {
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(p)})
				Expect(err).NotTo(HaveOccurred())
			}
			scoreForRank := func(rank any) (tuple.Tuple, error) {
				return store.EvaluateAggregateFunction(ctx, []string{"Order"},
					&IndexAggregateFunction{Name: FunctionNameScoreForRank, Operand: GroupBy(Field("price"))},
					TupleRangeAllOf(tuple.Tuple{rank}), IsolationLevelSerializable)
			}
			for rank, want := range map[any]int64{int32(1): 200, float64(2.5): 300, uint64(0): 100} {
				got, err := scoreForRank(rank)
				Expect(err).NotTo(HaveOccurred(), "%v", rank)
				Expect(got).To(Equal(tuple.Tuple{want}), "%v", rank)
			}
			_, err = scoreForRank("1")
			var cast *ClassCastError
			Expect(errors.As(err, &cast)).To(BeTrue(), "%v", err)
			got, err := scoreForRank(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeNil(), "a null rank answers null")

			m, err := store.getIndexMaintainer(rankIdx)
			Expect(err).NotTo(HaveOccurred())
			score, err := m.(*rankIndexMaintainer).ScoreForRank(tuple.Tuple{int32(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(Equal(tuple.Tuple{int64(300)}))
			score, err = m.(*rankIndexMaintainer).ScoreForRank(tuple.Tuple{nil})
			Expect(err).NotTo(HaveOccurred())
			Expect(score).To(BeNil(), "a null rank answers null")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a rank bound converts as Java's Number.longValue()", func() {
		huge, ok := new(big.Int).SetString("18446744073709551621", 10) // 2^64 + 5
		Expect(ok).To(BeTrue())
		negHuge := new(big.Int).Neg(huge) // -(2^64 + 5)
		for _, c := range []struct {
			bound any
			want  int64
		}{
			{int64(-3), -3},
			{uint(2), 2},
			{int8(-4), -4},
			{*big.NewInt(6), 6},
			{uint64(math.MaxUint64), -1},
			{huge, 5},
			{negHuge, -5},
			{float32(2.9), 2},
			{-2.9, -2},
			{math.NaN(), 0},
			{math.Inf(1), math.MaxInt64},
			{math.Inf(-1), math.MinInt64},
		} {
			got, err := extractRankValue(1, tuple.Tuple{"group", c.bound})
			Expect(err).NotTo(HaveOccurred(), "%v", c.bound)
			Expect(*got).To(Equal(c.want), "%v", c.bound)
		}
		got, err := extractRankValue(1, tuple.Tuple{"group", nil})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "a null bound is unbounded")
		_, err = extractRankValue(1, tuple.Tuple{"group", true})
		var cast *ClassCastError
		Expect(errors.As(err, &cast)).To(BeTrue(), "%v", err)
	})

	It("a TEXT index skips a null or empty text column, as Java does", func() {
		textIdx := NewTextIndex("customer$name_text", Field("name"))
		md := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Customer", textIdx) })
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("")})
			Expect(err).NotTo(HaveOccurred())
			entries, err := AsList(ctx, store.ScanIndexByType(textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			// A text position past the key is Java's IndexOutOfBoundsException.
			m, err := store.getIndexMaintainer(textIdx)
			Expect(err).NotTo(HaveOccurred())
			rec := &FDBStoredRecord[proto.Message]{PrimaryKey: tuple.Tuple{int64(3)}, Record: &demopb.Customer{CustomerId: proto.Int64(3)}}
			err = m.(*textIndexMaintainer).updateOneKey(rec, false, []any{"word"}, 5, 0)
			var oob *IndexOutOfBoundsError
			Expect(errors.As(err, &oob)).To(BeTrue(), "%v", err)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a TEXT index whose text column is not a string fails the save", func() {
		// Validation checks the first field it reads, name, a string; the text
		// column at position 0 is the literal. Java's Tuple.getString throws there.
		textIdx := NewTextIndex("customer$literal_text", Concat(Literal(int64(5)), Field("name")))
		md := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Customer", textIdx) })
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("hello world")})
			var cast *ClassCastError
			Expect(errors.As(err, &cast)).To(BeTrue(), "%v", err)
			Expect(cast.Message).To(ContainSubstring("java.lang.Long"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
