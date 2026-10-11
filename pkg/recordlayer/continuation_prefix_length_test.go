package recordlayer

import (
	"context"
	"slices"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Java's continuation prefix is the common prefix of the packed range bounds
// (KeyValueCursorBase.calculatePrefixLength, KeyValueCursorBase.java:425-432),
// one byte shorter when both endpoints are PREFIX_STRING (:318-320). These specs
// pin Go's tokens to Java's bytes on narrowed ranges and resume Java-minted tokens.
var _ = Describe("Continuation prefix length on narrowed ranges", func() {
	ctx := context.Background()

	// Orders 1..12: quantity 10/11/12 round-robin, price = order_id; orders 14
	// and 15 add quantity 11 at prices 200/201. Under quantity 11 the remaining
	// columns sort below, inside and above the narrowed prefix, so a misplaced
	// resume shows as rows outside the range, a repeat, or a truncated scan.
	var (
		valueIdx  *Index
		countIdx  *Index
		bitmapIdx *Index
		nameIdx   *Index
		md        *RecordMetaData
		ks        subspace.Subspace
	)

	// scanFn returns the keys read and the continuation of the last row returned
	// (Java's lastKey token), not the terminal result's.
	type scanFn func(store *FDBRecordStore, cont []byte, limit int, reverse bool) ([]tuple.Tuple, []byte, error)

	props := func(limit int, reverse bool) ScanProperties {
		p := NewScanProperties(DefaultExecuteProperties().WithReturnedRowLimit(limit))
		p.Reverse = reverse
		return p
	}

	drain := func(cur RecordCursor[*IndexEntry]) ([]tuple.Tuple, []byte, error) {
		defer cur.Close()
		var keys []tuple.Tuple
		var last []byte
		for {
			res, err := cur.OnNext(ctx)
			if err != nil {
				return nil, nil, err
			}
			if !res.HasNext() {
				return keys, last, nil
			}
			keys = append(keys, res.GetValue().Key)
			if last, err = res.GetContinuation().ToBytes(); err != nil {
				return nil, nil, err
			}
		}
	}

	expectKeys := func(got, want []tuple.Tuple, desc ...any) {
		if len(want) == 0 {
			Expect(got).To(BeEmpty(), desc...)
			return
		}
		Expect(got).To(Equal(want), desc...)
	}

	// expectJavaContinuations checks a narrowed scan whose in-range keys, in
	// forward order, are want; rawKey maps a key to its FDB key and javaPL is
	// the prefix length Java computes for the range.
	expectJavaContinuations := func(md *RecordMetaData, scan scanFn, want []tuple.Tuple, rawKey func(tuple.Tuple) []byte, javaPL func(*FDBRecordStore) int) {
		Expect(len(want)).To(BeNumerically(">=", 3))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			pl := javaPL(store)
			javaToken := func(k tuple.Tuple) []byte { return javaContinuationBytes(rawKey(k), pl) }

			all, _, err := scan(store, nil, 0, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(Equal(want))

			// Resuming a Java token at every position yields exactly the rest of
			// the range: no rows outside it, none skipped or repeated, no early end.
			for i, k := range want {
				got, _, err := scan(store, javaToken(k), 0, false)
				Expect(err).NotTo(HaveOccurred())
				expectKeys(got, want[i+1:], "forward resume after %v", k)
				got, _, err = scan(store, javaToken(k), 0, true)
				Expect(err).NotTo(HaveOccurred())
				wantRev := slices.Clone(want[:i])
				slices.Reverse(wantRev)
				expectKeys(got, wantRev, "reverse resume before %v", k)
			}

			// Paging one row at a time mints Java's bytes for every row.
			for _, reverse := range []bool{false, true} {
				order := slices.Clone(want)
				if reverse {
					slices.Reverse(order)
				}
				var cont []byte
				for _, k := range order {
					got, next, err := scan(store, cont, 1, reverse)
					Expect(err).NotTo(HaveOccurred())
					Expect(got).To(Equal([]tuple.Tuple{k}))
					Expect(next).To(Equal(javaToken(k)), "token after %v (reverse=%v)", k, reverse)
					cont = next
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		ks = specSubspace()
		valueIdx = NewIndex("qp", Concat(Field("quantity"), Field("price")))
		countIdx = NewCountIndex("cnt_qp", GroupAll(Concat(Field("quantity"), Field("price"))))
		bitmapIdx = NewBitmapValueIndex("bm_q", GroupBy(Field("order_id"), Field("quantity")))
		bitmapIdx.Options[IndexOptionBitmapValueEntrySize] = "4"
		nameIdx = NewIndex("cust_name", Field("name"))
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", valueIdx)
		builder.AddIndex("Order", countIdx)
		builder.AddIndex("Order", bitmapIdx)
		builder.AddIndex("Customer", nameIdx)
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 12; i++ {
				_, err = store.SaveRecord(&demopb.Order{
					OrderId:  proto.Int64(i),
					Quantity: proto.Int32(int32(10 + (i-1)%3)),
					Price:    proto.Int32(int32(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(14); i <= 15; i++ {
				_, err = store.SaveRecord(&demopb.Order{
					OrderId:  proto.Int64(i),
					Quantity: proto.Int32(11),
					Price:    proto.Int32(int32(186 + i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			for i, name := range []string{"aa", "ab", "abc", "abd", "abz", "ac", "b"} {
				_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(101 + i)), Name: proto.String(name)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	indexScan := func(idx *Index, scanType IndexScanType, rng TupleRange) scanFn {
		return func(store *FDBRecordStore, cont []byte, limit int, reverse bool) ([]tuple.Tuple, []byte, error) {
			return drain(store.ScanIndexByType(idx, scanType, rng, cont, props(limit, reverse)))
		}
	}
	indexKey := func(idx *Index) func(*FDBRecordStore) func(tuple.Tuple) []byte {
		return func(store *FDBRecordStore) func(tuple.Tuple) []byte {
			return func(k tuple.Tuple) []byte { return store.indexSubspace(idx).Pack(k) }
		}
	}
	// prefixPL is Java's prefix length for allOf(prefix) over idx.
	prefixPL := func(idx *Index, prefix tuple.Tuple) func(*FDBRecordStore) int {
		return func(store *FDBRecordStore) int { return len(store.indexSubspace(idx).Pack(prefix)) }
	}

	// runIndexCase opens a store only to resolve the index subspace for rawKey.
	runIndexCase := func(scan scanFn, want []tuple.Tuple, rawKey func(*FDBRecordStore) func(tuple.Tuple) []byte, pl func(*FDBRecordStore) int) {
		var keyFn func(tuple.Tuple) []byte
		expectJavaContinuations(md, scan, want, func(k tuple.Tuple) []byte { return keyFn(k) }, func(store *FDBRecordStore) int {
			keyFn = rawKey(store)
			return pl(store)
		})
	}

	It("VALUE index, equality on the leading column", func() {
		want := []tuple.Tuple{
			{int64(11), int64(2), int64(2)},
			{int64(11), int64(5), int64(5)},
			{int64(11), int64(8), int64(8)},
			{int64(11), int64(11), int64(11)},
			{int64(11), int64(200), int64(14)},
			{int64(11), int64(201), int64(15)},
		}
		runIndexCase(indexScan(valueIdx, IndexScanByValue, TupleRangeAllOf(tuple.Tuple{int64(11)})), want,
			indexKey(valueIdx), prefixPL(valueIdx, tuple.Tuple{int64(11)}))
	})

	It("VALUE index, betweenInclusive sharing a leading prefix", func() {
		rng := TupleRangeBetweenInclusive(tuple.Tuple{int64(11), int64(2)}, tuple.Tuple{int64(11), int64(8)})
		want := []tuple.Tuple{{int64(11), int64(2), int64(2)}, {int64(11), int64(5), int64(5)}, {int64(11), int64(8), int64(8)}}
		// Common prefix of pack(11,2) and pack(11,8): the 11 plus the int code 0x15.
		runIndexCase(indexScan(valueIdx, IndexScanByValue, rng), want,
			indexKey(valueIdx), func(store *FDBRecordStore) int {
				return len(store.indexSubspace(valueIdx).Pack(tuple.Tuple{int64(11)})) + 1
			})
	})

	It("PREFIX_STRING range drops the string terminator from the prefix", func() {
		want := []tuple.Tuple{{"ab", int64(102)}, {"abc", int64(103)}, {"abd", int64(104)}, {"abz", int64(105)}}
		runIndexCase(indexScan(nameIdx, IndexScanByValue, TupleRangePrefixString("ab")), want,
			indexKey(nameIdx), func(store *FDBRecordStore) int { return len(store.indexSubspace(nameIdx).Pack(tuple.Tuple{"ab"})) - 1 })
	})

	It("COUNT index, grouped lookup allOf(group)", func() {
		want := []tuple.Tuple{
			{int64(11), int64(2)},
			{int64(11), int64(5)},
			{int64(11), int64(8)},
			{int64(11), int64(11)},
			{int64(11), int64(200)},
			{int64(11), int64(201)},
		}
		runIndexCase(indexScan(countIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(11)})), want,
			indexKey(countIdx), prefixPL(countIdx, tuple.Tuple{int64(11)}))
	})

	It("BITMAP_VALUE index, grouped lookup allOf(group)", func() {
		// quantity 11 holds order_ids 2,5,8,11,14,15: entry-size-4 offsets 0,4,8,12.
		want := []tuple.Tuple{{int64(11), int64(0)}, {int64(11), int64(4)}, {int64(11), int64(8)}, {int64(11), int64(12)}}
		runIndexCase(indexScan(bitmapIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(11)})), want,
			indexKey(bitmapIdx), prefixPL(bitmapIdx, tuple.Tuple{int64(11)}))
	})

	It("record scan over a primary-key range", func() {
		scan := func(store *FDBRecordStore, cont []byte, limit int, reverse bool) ([]tuple.Tuple, []byte, error) {
			cur := store.ScanRecordsInRange(tuple.Tuple{int64(5)}, tuple.Tuple{int64(9)},
				EndpointTypeRangeInclusive, EndpointTypeRangeInclusive, cont, props(limit, reverse))
			defer cur.Close()
			var keys []tuple.Tuple
			var last []byte
			for {
				res, err := cur.OnNext(ctx)
				if err != nil {
					return nil, nil, err
				}
				if !res.HasNext() {
					return keys, last, nil
				}
				keys = append(keys, res.GetValue().PrimaryKey)
				if last, err = res.GetContinuation().ToBytes(); err != nil {
					return nil, nil, err
				}
			}
		}
		var want []tuple.Tuple
		for i := int64(5); i <= 9; i++ {
			want = append(want, tuple.Tuple{i})
		}
		var keyFn func(tuple.Tuple) []byte
		expectJavaContinuations(md, scan, want, func(k tuple.Tuple) []byte { return keyFn(k) }, func(store *FDBRecordStore) int {
			// Unsplit record key: pk + split suffix 0 (no versions stored).
			keyFn = func(k tuple.Tuple) []byte { return store.recordsSubspace.Pack(append(slices.Clone(k), int64(0))) }
			// pack(5) and pack(9) share only the int type code.
			return len(store.recordsSubspace.Bytes()) + 1
		})
	})
})
