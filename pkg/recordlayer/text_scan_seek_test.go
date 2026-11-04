package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// The leapfrog seek of BunchedMapMultiIterator and the text intersections
// built on it must return exactly what streaming returns, with the same
// continuations, while reading fewer key-values.
var _ = Describe("TEXT leapfrog seek", func() {
	ctx := context.Background()

	// mapKeys are the keys of one BunchedMap: every third integer, written in
	// ascending order so that each bunch holds 20 consecutive keys.
	const mapEntries = 100
	mapKeys := make([]int64, mapEntries)
	for i := range mapKeys {
		mapKeys[i] = int64(3 * i)
	}

	type streamed struct {
		key  int64
		cont []byte // the iterator's continuation right after returning key
	}

	writeMap := func(parent subspace.Subspace) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			bm := NewBunchedMap(textIndexBunchSize)
			for _, k := range mapKeys {
				if _, _, err := bm.Put(rtx.Transaction(), parent.Sub("tok"), tuple.Tuple{k}, []int{int(k)}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	newIterator := func(tx fdb.ReadTransaction, parent subspace.Subspace, reverse bool, kvs *int) *BunchedMapMultiIterator {
		begin, end := parent.FDBRangeKeys()
		return NewBunchedMapMultiIteratorWithCallback(tx, parent, NewTextSubspaceSplitter(parent, 1),
			begin.FDBKey(), end.FDBKey(), nil, 0, func(int, int) { *kvs++ }, reverse, TextIndexBunchedSerializerInstance())
	}

	stream := func(tx fdb.ReadTransaction, parent subspace.Subspace, reverse bool) []streamed {
		var kvs int
		it := newIterator(tx, parent, reverse, &kvs)
		var out []streamed
		for it.HasNext() {
			e := it.Next()
			out = append(out, streamed{key: e.Key[0].(int64), cont: it.GetContinuation()})
		}
		Expect(it.Err()).NotTo(HaveOccurred())
		return out
	}

	It("lands on the first entry at or past the target with the streamed predecessor's continuation", func() {
		parent := specSubspace().Sub("idx")
		writeMap(parent)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			bunches, err := tx.GetRange(parent, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(bunches)).To(Equal(mapEntries / textIndexBunchSize))

			for _, reverse := range []bool{false, true} {
				ref := stream(tx, parent, reverse)
				Expect(ref).To(HaveLen(mapEntries))
				// Start after each of several entries and seek to targets that hit a
				// key, fall between keys, hit a bunch's first key, sit inside the
				// current bunch, sit inside a later bunch, and lie past the end.
				for _, start := range []int{0, 5, 19, 20, 41, 98} {
					for _, delta := range []int64{1, 2, 3, 4, 7, 30, 59, 60, 61, 121, 200, 1000} {
						from := ref[start].key
						target := from + delta
						if reverse {
							target = from - delta
						}
						var kvs int
						it := newIterator(tx, parent, reverse, &kvs)
						for i := 0; i <= start; i++ {
							Expect(it.HasNext()).To(BeTrue())
							it.Next()
						}
						kvsBefore := kvs
						it.seekTo(tuple.Tuple{target})

						// The reference: the first streamed entry at or past the
						// target, and the continuation after the one before it.
						want := -1
						for i := start + 1; i < len(ref); i++ {
							if (!reverse && ref[i].key >= target) || (reverse && ref[i].key <= target) {
								want = i
								break
							}
						}
						desc := fmt.Sprintf("reverse=%v start=%d target=%d", reverse, ref[start].key, target)
						if want < 0 {
							Expect(it.HasNext()).To(BeFalse(), desc)
							Expect(it.Err()).NotTo(HaveOccurred())
							Expect(it.GetContinuation()).To(BeNil(), desc)
							continue
						}
						Expect(it.HasNext()).To(BeTrue(), desc)
						Expect(it.GetContinuation()).To(Equal(ref[want-1].cont), desc)
						e := it.Next()
						Expect(e.Key[0]).To(Equal(ref[want].key), desc)
						Expect(it.GetContinuation()).To(Equal(ref[want].cont), desc)
						if want/textIndexBunchSize == start/textIndexBunchSize {
							Expect(kvs-kvsBefore).To(BeZero(), desc+": a seek inside the bunch reads nothing")
						} else {
							// The containing bunch, and the bunch before it when the
							// target is a bunch's first key.
							Expect(kvs-kvsBefore).To(BeNumerically("<=", 2), desc)
						}
						// The rest of the stream is unchanged.
						for i := want + 1; i < len(ref); i++ {
							Expect(it.HasNext()).To(BeTrue(), desc)
							Expect(it.Next().Key[0]).To(Equal(ref[i].key), desc)
							Expect(it.GetContinuation()).To(Equal(ref[i].cont), desc)
						}
						Expect(it.HasNext()).To(BeFalse(), desc)
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps the logical lower predecessor when a reverse seek reopens the range", func() {
		parent := specSubspace().Sub("bounded_idx")
		writeMap(parent)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			begin := parent.Pack(tuple.Tuple{"tok", int64(10)})
			end := parent.Pack(tuple.Tuple{"tok", int64(100)})
			predecessor, err := tx.GetRange(fdb.SelectorRange{Begin: fdb.LastLessOrEqual(fdb.Key(begin)), End: fdb.FirstGreaterOrEqual(fdb.Key(begin))}, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(predecessor).To(HaveLen(1))
			Expect(predecessor[0].Key).To(Equal(parent.Pack(tuple.Tuple{"tok", int64(0)})))
			for _, target := range []int64{12, 15, 57, 60} {
				it := NewBunchedMapMultiIterator(tx, parent, NewTextSubspaceSplitter(parent, 1), begin, end, nil, 0, true, TextIndexBunchedSerializerInstance())
				Expect(it.HasNext()).To(BeTrue())
				Expect(it.Next().Key).To(Equal(tuple.Tuple{int64(99)}))
				it.seekTo(tuple.Tuple{target})
				Expect(it.HasNext()).To(BeTrue(), "target=%d", target)
				Expect(it.Next().Key).To(Equal(tuple.Tuple{target}))
				cont := it.GetContinuation()
				var rest []int64
				for it.HasNext() {
					rest = append(rest, it.Next().Key[0].(int64))
				}
				Expect(it.Err()).NotTo(HaveOccurred())
				var want []int64
				for k := target - 3; k >= 12; k -= 3 {
					want = append(want, k)
				}
				Expect(rest).To(Equal(want))
				resumed := NewBunchedMapMultiIterator(tx, parent, NewTextSubspaceSplitter(parent, 1), begin, end, cont, 0, true, TextIndexBunchedSerializerInstance())
				var replay []int64
				for resumed.HasNext() {
					replay = append(replay, resumed.Next().Key[0].(int64))
				}
				Expect(resumed.Err()).NotTo(HaveOccurred())
				Expect(replay).To(Equal(want))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A continuation whose key is a bunch's first entry resumes in the bunch
	// before it (lastLessThan); that bunch has nothing past the key, and the
	// key's own bunch must still skip it (BunchedMapIterator.java:126-138).
	It("resumes from every continuation without repeating an entry", func() {
		parent := specSubspace().Sub("idx")
		writeMap(parent)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for _, reverse := range []bool{false, true} {
				ref := stream(tx, parent, reverse)
				Expect(ref).To(HaveLen(mapEntries))
				begin, end := parent.FDBRangeKeys()
				for i, r := range ref {
					it := NewBunchedMapMultiIterator(tx, parent, NewTextSubspaceSplitter(parent, 1),
						begin.FDBKey(), end.FDBKey(), r.cont, 0, reverse, TextIndexBunchedSerializerInstance())
					var rest []int64
					for it.HasNext() {
						rest = append(rest, it.Next().Key[0].(int64))
					}
					Expect(it.Err()).NotTo(HaveOccurred())
					var want []int64
					for _, w := range ref[i+1:] {
						want = append(want, w.key)
					}
					Expect(rest).To(Equal(want), "reverse=%v resume after %d", reverse, r.key)
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("seeks into a bunch whose key sorts before the target", func() {
		parent := specSubspace().Sub("idx")
		writeMap(parent)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			var kvs int
			it := newIterator(tx, parent, false, &kvs)
			Expect(it.HasNext()).To(BeTrue())
			it.Next() // key 0, first bunch
			before := kvs
			// 151 is not a key; the bunch keyed 120 holds 120..177, and 153 follows.
			it.seekTo(tuple.Tuple{int64(151)})
			Expect(it.HasNext()).To(BeTrue())
			Expect(it.GetContinuation()).To(Equal(append(tuple.Tuple{"tok"}.Pack(), tuple.Tuple{int64(150)}.Pack()...)))
			Expect(it.Next().Key).To(Equal(tuple.Tuple{int64(153)}))
			Expect(kvs - before).To(Equal(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// textCorpus saves customers whose names hold: "common" (every record),
	// "frequent" (every other one), "rare" (three records), and phrase pairs.
	textCorpus := func(n int) (*RecordMetaData, *Index, subspace.Subspace) {
		ks := specSubspace()
		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		for batch := 0; batch < n; batch += 100 {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := batch; i < batch+100 && i < n; i++ {
					name := "common"
					if i%2 == 0 {
						name += " frequent"
					}
					if i == 17 || i == n/2 || i == n-3 {
						name += " rare"
					}
					if i%7 == 0 {
						name += " alpha beta"
					} else if i%7 == 1 {
						name += " beta alpha"
					}
					if _, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(i)), Name: proto.String(name)}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		return md, idx, ks
	}

	type page struct {
		keys   []string
		cont   []byte
		reason NoNextReason
	}

	// pages runs a scan to the end in pages of limit entries.
	pages := func(open func(cont []byte) RecordCursor[*IndexEntry]) []page {
		var out []page
		var cont []byte
		for n := 0; n < 1000; n++ {
			c := open(cont)
			var p page
			for {
				r, err := c.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					p.reason = r.GetNoNextReason()
					b, err := r.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					p.cont = b
					break
				}
				p.keys = append(p.keys, fmt.Sprint(r.GetValue().Key))
			}
			Expect(c.Close()).To(Succeed())
			out = append(out, p)
			if p.cont == nil {
				return out
			}
			cont = p.cont
		}
		Fail("scan did not finish")
		return nil
	}

	// streaming is TextScan.scan's intersection paths without seekers: the
	// IntersectionCursor Java runs.
	streaming := func(store *FDBRecordStore, s *TextScan, cont []byte, props ScanProperties) RecordCursor[*IndexEntry] {
		tokens, err := s.tokenList(true)
		Expect(err).NotTo(HaveOccurred())
		exec := props.ExecuteProperties
		child := props.WithExecuteProperties(exec.ClearSkipAndLimit())
		children, err := s.intersectionChildren(store, tokens, s.GroupingKey, s.Suffix, child, 1, cont)
		Expect(err).NotTo(HaveOccurred())
		keyFunc := suffixComparisonKey(1)
		var c RecordCursor[*IndexEntry]
		switch s.Comparison.Type() {
		case TextContainsAll:
			c = IntersectionResume(children.cursors, keyFunc, props.IsReverse(), children.resume)
		case TextContainsPhrase:
			withStop, err := s.tokenList(false)
			Expect(err).NotTo(HaveOccurred())
			c = firstOfMatches(FilterErrCursor(IntersectionMultiResume(children.cursors, keyFunc, props.IsReverse(), children.resume),
				func(e []*IndexEntry) (bool, error) { return entriesContainPhrase(e, withStop) }))
		case TextContainsAllWithin:
			d := s.Comparison.MaxDistance()
			c = firstOfMatches(FilterErrCursor(IntersectionMultiResume(children.cursors, keyFunc, props.IsReverse(), children.resume),
				func(e []*IndexEntry) (bool, error) { return entriesContainAllWithin(e, d) }))
		}
		return LimitRowsCursor(SkipCursor(c, exec.Skip), exec.ReturnedRowLimit)
	}

	It("returns the streaming intersection's entries and continuations on every page", func() {
		md, idx, ks := textCorpus(300)
		comparisons := []*TextComparison{
			NewTextComparison(TextContainsAll, "rare common frequent", "", ""),
			NewTextComparison(TextContainsAll, "frequent common", "", ""),
			NewTextComparison(TextContainsAll, "rare alpha", "", ""),
			NewTextComparison(TextContainsAll, "rare absent common", "", ""),
			NewTextComparison(TextContainsPhrase, "alpha beta", "", ""),
			NewTextComparison(TextContainsPhrase, "common frequent", "", ""),
			NewTextComparisonTokens(TextContainsPhrase, []string{"common", "", "alpha"}, "", ""),
			NewTextWithMaxDistanceComparison("common beta", 2, "", ""),
			NewTextWithMaxDistanceComparison("frequent alpha rare", 3, "", ""),
		}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			for _, cmp := range comparisons {
				scan := NewTextScan(idx, nil, cmp, nil)
				for _, reverse := range []bool{false, true} {
					for _, limit := range []int{0, 1, 3, 17} {
						props := func() ScanProperties {
							return ForwardScan().WithReverse(reverse).WithExecuteProperties(DefaultExecuteProperties().WithReturnedRowLimit(limit))
						}
						desc := fmt.Sprintf("%s reverse=%v limit=%d", cmp, reverse, limit)
						got := pages(func(cont []byte) RecordCursor[*IndexEntry] {
							c, err := scan.Scan(store, cont, props())
							Expect(err).NotTo(HaveOccurred())
							return c
						})
						want := pages(func(cont []byte) RecordCursor[*IndexEntry] { return streaming(store, scan, cont, props()) })
						Expect(got).To(Equal(want), desc)
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads about the rare token's postings when intersecting it with common tokens", func() {
		md, idx, ks := textCorpus(1200)
		scan := NewTextScan(idx, nil, NewTextComparison(TextContainsAll, "rare common frequent", "", ""), nil)
		count := func(open func(*FDBRecordStore, ScanProperties) RecordCursor[*IndexEntry]) (int, int64, int64) {
			timer := NewStoreTimer()
			runner := NewFDBDatabaseRunner(sharedDB).SetContextConfig(&RecordContextConfig{Timer: timer})
			var hits int
			var bytes int64
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				timer.Reset()
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				props := ForwardScan()
				entries, err := AsList(ctx, open(store, props))
				hits, bytes = len(entries), props.ExecuteProperties.ScanState.BytesScanned()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(CountTextScanBytes)).To(Equal(bytes))
			return hits, timer.GetCount(CountTextScanKeyValues), bytes
		}
		seekHits, seekKVs, seekBytes := count(func(store *FDBRecordStore, props ScanProperties) RecordCursor[*IndexEntry] {
			c, err := scan.Scan(store, nil, props)
			Expect(err).NotTo(HaveOccurred())
			return c
		})
		streamHits, streamKVs, streamBytes := count(func(store *FDBRecordStore, props ScanProperties) RecordCursor[*IndexEntry] {
			return streaming(store, scan, nil, props)
		})
		// rare is in records 17, 600 and 1197; only 600 is even (frequent).
		Expect(seekHits).To(Equal(1))
		Expect(streamHits).To(Equal(1))
		// Bunches hold 20 entries: common's bunch b is records 20b..20b+19,
		// frequent's is 40b..40b+38, rare's one bunch is {17, 600, 1197}.
		//
		// Streaming (Java's IntersectionCursorBase) pulls one entry of every
		// non-maximal child per round. After the match at 600 the maximum is
		// rare's 1197; each round moves common by 1 and frequent by 2, so
		// frequent passes 1197 (reaching 1198, the new maximum) after 299
		// rounds, with common at 899. That round discards rare and common;
		// rare is then exhausted and common has pulled 900. Reads: rare's
		// bunch (1) + common's bunches 0..45 (46) + frequent's bunches 0..29
		// (30) = 77.
		Expect(streamKVs).To(Equal(int64(1 + 46 + 30)))
		// Seeking reads each token's first bunch (3). Common and frequent seek
		// in memory to 17 and 18; then both reopen at 600 (2 bunches each:
		// 600 is a bunch's first entry, so the bunch before it is read too, 4);
		// at 1197 common reopens into bunch 59 and frequent into bunch 29 (1
		// each, 2); rare's seek to 1198 rereads its bunch and ends (1).
		// 3 + 4 + 2 + 1 = 10, against 77.
		Expect(seekKVs).To(Equal(int64(10)))
		Expect(seekBytes).To(BeNumerically("<", streamBytes/4))
	})
})
