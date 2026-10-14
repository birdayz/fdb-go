package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

func TestRemoteFetchAPIVersionCheck(t *testing.T) {
	t.Parallel()
	for version, supported := range map[int]bool{0: false, 700: false, 709: false, 710: true, 730: true} {
		err := remoteFetchAPIVersionCheck(version)
		var unsupported *UnsupportedMethodError
		if supported != (err == nil) || (!supported && !errors.As(err, &unsupported)) {
			t.Fatalf("version %d: err = %v, supported = %t", version, err, supported)
		}
	}
}

func TestIndexEntryPrimaryKeyPositions(t *testing.T) {
	t.Parallel()
	plain := NewIndex("plain", Concat(Field("a"), Field("b")))
	if got := plain.EntryPrimaryKeyPositions(2); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("no overlap: %v", got)
	}
	overlap := NewIndex("overlap", Concat(Field("a"), Field("b"), Field("c")))
	overlap.primaryKeyComponentPositions = []int{-1, 1, -1}
	// The overlap mapping's length wins over the requested length.
	if got := overlap.EntryPrimaryKeyPositions(1); fmt.Sprint(got) != "[3 1 4]" {
		t.Fatalf("overlap: %v", got)
	}
}

// remoteFetchNoMappedReads is a client without mapped reads, like the libfdb_c
// backend: every read still goes to the real transaction.
type remoteFetchNoMappedReads struct{ fdb.WritableTransaction }

type remoteFetchRow struct {
	IndexKey, PrimaryKey, Data, Version, Continuation []byte
	Present                                           bool
	KeyCount, KeySize, ValueSize                      int
	Split                                             bool
}

type remoteFetchPage struct {
	Rows          []remoteFetchRow
	Reason        NoNextReason
	Continuation  []byte
	Err           error
	Scanned       int
	Bytes         int64
	RemoteEntries int64
	RemoteFetches int64
	BadEntries    int64
}

var _ = Describe("Index remote fetch", func() {
	ctx := context.Background()
	methods := []IndexFetchMethod{IndexFetchScanAndFetch, IndexFetchUseRemoteFetch, IndexFetchUseRemoteFetchWithFallback}

	type layout struct {
		split, compound bool
		format          int32 // 0: the current format
	}
	current := layout{split: true}

	metadata := func(l layout) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
			SetSplitLongRecords(l.split).SetStoreRecordVersions(true)
		pk := Field("order_id")
		if l.compound {
			pk = Concat(Field("quantity"), Field("order_id"))
		}
		builder.GetRecordType("Order").SetPrimaryKey(pk)
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		bitmap := NewBitmapValueIndex("bitmap", GroupBy(Field("order_id"), Concat(Field("price"), Field("quantity"))))
		bitmap.SetOption(IndexOptionBitmapValueEntrySize, "16")
		for _, index := range []*Index{
			NewIndex("price", Field("price")),
			NewIndex("pk_first", Concat(Field("price"), Field("quantity"))),
			NewIndex("pk_second", Concat(Field("price"), Field("order_id"))),
			NewVersionIndex("version", Concat(Field("price"), VersionKey())),
			NewRankIndex("rank", GroupBy(Field("price"))),
			bitmap,
		} {
			builder.AddIndex("Order", index)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	pkLength := func(l layout) int {
		if l.compound {
			return 2
		}
		return 1
	}
	open := func(ks subspace.Subspace, l layout) *FDBRecordStore {
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		rc := sharedDB.NewRecordContext(tx)
		DeferCleanup(rc.Cancel)
		builder := NewStoreBuilder().SetContext(rc).SetSubspace(ks).SetMetaDataProvider(metadata(l))
		if l.format != 0 {
			builder = builder.SetFormatVersion(l.format)
		}
		store, err := builder.CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store
	}
	order := func(id int) *demopb.Order {
		return &demopb.Order{
			OrderId: proto.Int64(int64(id)), Price: proto.Int32(int32(1000 - id)),
			Quantity: proto.Int32(int32(id % 3)), Tags: []string{"seed"},
		}
	}
	seed := func(ks subspace.Subspace, l layout, count int, build func(int) *demopb.Order) {
		for first := 0; first < count; first += 100 {
			store := open(ks, l)
			for id := first; id < min(count, first+100); id++ {
				_, err := store.SaveRecord(build(id))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(store.Context().Commit()).To(Succeed())
		}
	}
	scan := func(store *FDBRecordStore, method IndexFetchMethod, index string, commonPKLength int, props ScanProperties, continuation []byte, orphan IndexOrphanBehavior) remoteFetchPage {
		timer := NewStoreTimer()
		store.Context().SetTimer(timer)
		state := NewScanLimiterState()
		props.ExecuteProperties.ScanState = state
		cursor := store.ScanIndexRecordsWithFetchMethod(store.GetRecordMetaData().GetIndex(index), method, IndexScanByValue,
			TupleRangeAll, commonPKLength, continuation, orphan, props)
		defer func() { Expect(cursor.Close()).To(Succeed()) }()
		var page remoteFetchPage
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				page.Err = err
				break
			}
			token, err := result.GetContinuation().ToBytes()
			Expect(err).NotTo(HaveOccurred())
			if !result.HasNext() {
				page.Reason, page.Continuation = result.GetNoNextReason(), token
				break
			}
			indexed := result.GetValue()
			row := remoteFetchRow{IndexKey: indexed.IndexEntry.Key.Pack(), PrimaryKey: indexed.IndexEntry.PrimaryKey().Pack(), Continuation: token}
			if record := indexed.Record; record != nil {
				row.Present = true
				row.Data, err = proto.MarshalOptions{Deterministic: true}.Marshal(record.Record)
				Expect(err).NotTo(HaveOccurred())
				if record.Version != nil {
					row.Version = record.Version.ToBytes()
				}
				row.KeyCount, row.KeySize, row.ValueSize, row.Split = record.KeyCount, record.KeySize, record.ValueSize, record.Split
			}
			page.Rows = append(page.Rows, row)
		}
		state.WaitForIssuedRecordLoadCharges()
		page.Scanned, page.Bytes = state.RecordsScanned(), state.BytesScanned()
		page.RemoteEntries, page.BadEntries = timer.GetCount(EventScanRemoteFetchEntry), timer.GetCount(CountBadIndexEntry)
		page.RemoteFetches = timer.GetCount(CountRemoteFetch)
		return page
	}
	ids := func(page remoteFetchPage) []int64 {
		out := make([]int64, 0, len(page.Rows))
		for _, row := range page.Rows {
			pk, err := tuple.Unpack(row.PrimaryKey)
			Expect(err).NotTo(HaveOccurred())
			out = append(out, pk[len(pk)-1].(int64))
		}
		return out
	}
	expectFDBError := func(err error, code int) {
		var native fdb.Error
		ExpectWithOffset(1, errors.As(err, &native)).To(BeTrue(), "error %v", err)
		ExpectWithOffset(1, native.Code).To(Equal(code))
	}

	// Ports RemoteFetchIndexScanTest's simple, reverse and complex index cases,
	// RemoteFetchSplitRecordsTest, RemoteFetchWithoutSplitRecordsTest,
	// RemoteFetchMultiColumnKeyTest and RemoteFetchOldVersionTest's layouts.
	DescribeTable("returns SCAN_AND_FETCH's records, versions and sizes for every layout",
		func(l layout, indexes ...string) {
			ks := specSubspace()
			seed(ks, l, 12, func(id int) *demopb.Order {
				o := order(id)
				if id == 3 && l.split {
					o.VectorData = bytes.Repeat([]byte{0x80}, 150_017)
				}
				return o
			})
			for _, index := range indexes {
				for _, reverse := range []bool{false, true} {
					var baseline remoteFetchPage
					for _, method := range methods {
						page := scan(open(ks, l), method, index, pkLength(l), ForwardScan().WithReverse(reverse), nil, IndexOrphanError)
						Expect(page.Err).NotTo(HaveOccurred(), "%s %s reverse=%t", index, method, reverse)
						Expect(page.Rows).To(HaveLen(12))
						Expect(page.Reason).To(Equal(SourceExhausted))
						for _, row := range page.Rows {
							Expect(row.Present).To(BeTrue())
							Expect(row.Version).To(HaveLen(VersionBytes))
						}
						if method == IndexFetchScanAndFetch {
							baseline = page
							Expect(page.RemoteEntries).To(BeZero())
							Expect(page.RemoteFetches).To(BeZero())
							continue
						}
						Expect(page.Rows).To(Equal(baseline.Rows), "%s %s reverse=%t", index, method, reverse)
						Expect(page.Bytes).To(Equal(baseline.Bytes))
						Expect(page.RemoteEntries).To(Equal(int64(13)), "one per result, including the exhausted one")
						Expect(page.RemoteFetches).To(Equal(int64(1)), "one mapped read for the whole scan")
					}
					split := 0
					for _, row := range baseline.Rows {
						if row.Split {
							split++
							Expect(row.KeyCount).To(Equal(3), "version plus two chunks")
						}
					}
					if l.split {
						Expect(split).To(Equal(1))
					} else {
						Expect(split).To(BeZero())
					}
				}
			}
		},
		Entry("split records", current, "price", "version"),
		Entry("unsplit records", layout{}, "price", "version"),
		Entry("multi-column primary key", layout{split: true, compound: true}, "price", "pk_first", "pk_second", "version"),
		Entry("old version format without suffix", layout{format: 4}, "price"),
		Entry("old version format with suffix", layout{format: 5}, "price"),
	)

	It("resumes every continuation with every fetch method", func() {
		ks := specSubspace()
		seed(ks, current, 12, order)
		for _, reverse := range []bool{false, true} {
			all := scan(open(ks, current), IndexFetchScanAndFetch, "price", 1, ForwardScan().WithReverse(reverse), nil, IndexOrphanError)
			Expect(all.Rows).To(HaveLen(12))
			for position, row := range all.Rows {
				for _, method := range methods {
					rest := scan(open(ks, current), method, "price", 1, ForwardScan().WithReverse(reverse), row.Continuation, IndexOrphanError)
					Expect(rest.Err).NotTo(HaveOccurred())
					Expect(rest.Reason).To(Equal(SourceExhausted))
					Expect(rest.Rows).To(HaveLen(len(all.Rows)-position-1), "%s from %d reverse=%t", method, position, reverse)
					for i, row := range rest.Rows {
						Expect(row).To(Equal(all.Rows[position+1+i]), "%s from %d reverse=%t", method, position, reverse)
					}
				}
			}
			// indexPrefetchWithMixedContinuationTest: pages of four alternate methods.
			var got []remoteFetchRow
			var token []byte
			for page := 0; ; page++ {
				props := NewScanProperties(DefaultExecuteProperties().WithReturnedRowLimit(4)).WithReverse(reverse)
				next := scan(open(ks, current), methods[page%len(methods)], "price", 1, props, token, IndexOrphanError)
				Expect(next.Err).NotTo(HaveOccurred())
				got = append(got, next.Rows...)
				if next.Reason != ReturnLimitReached {
					Expect(next.Reason).To(Equal(SourceExhausted))
					break
				}
				Expect(next.Rows).To(HaveLen(4))
				token = next.Continuation
			}
			Expect(got).To(Equal(all.Rows))
		}
	})

	It("stops at row, scanned-record, byte and time limits where SCAN_AND_FETCH stops", func() {
		ks := specSubspace()
		seed(ks, current, 12, order)
		limits := map[string]ExecuteProperties{
			"rows":    DefaultExecuteProperties().WithReturnedRowLimit(5),
			"exact":   DefaultExecuteProperties().WithReturnedRowLimit(12),
			"scanned": DefaultExecuteProperties().WithScannedRecordsLimit(3),
			"bytes":   DefaultExecuteProperties().WithScannedBytesLimit(1),
			"time":    DefaultExecuteProperties().WithTimeLimit(time.Millisecond),
		}
		for name, execute := range limits {
			var baseline remoteFetchPage
			for _, method := range methods {
				store := open(ks, current)
				if name == "time" {
					time.Sleep(5 * time.Millisecond)
				}
				page := scan(store, method, "price", 1, NewScanProperties(execute), nil, IndexOrphanError)
				Expect(page.Err).NotTo(HaveOccurred())
				if method == IndexFetchScanAndFetch {
					baseline = page
					continue
				}
				Expect(page.Rows).To(Equal(baseline.Rows), "%s %s", name, method)
				Expect(page.Reason).To(Equal(baseline.Reason), "%s %s", name, method)
				Expect(page.Continuation).To(Equal(baseline.Continuation), "%s %s", name, method)
			}
			wantRows := map[string]int{"rows": 5, "exact": 12, "scanned": 3, "bytes": 1, "time": 1}[name]
			Expect(baseline.Rows).To(HaveLen(wantRows), name)
		}
	})

	It("fails remote fetch on read-your-writes and falls back from the last result", func() {
		ks := specSubspace()
		seed(ks, current, 500, func(id int) *demopb.Order {
			return &demopb.Order{OrderId: proto.Int64(int64(id)), Price: proto.Int32(int32(id))}
		})
		for _, reverse := range []bool{false, true} {
			// failAfterRecordsReturnedTest: change a non-indexed field of the
			// record the scan reaches last, so the first batches succeed.
			modified := 499
			if reverse {
				modified = 0
			}
			for _, method := range methods {
				store := open(ks, current)
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(int64(modified)), Price: proto.Int32(int32(modified)), Tags: []string{"local"}})
				Expect(err).NotTo(HaveOccurred())
				page := scan(store, method, "price", 1, ForwardScan().WithReverse(reverse), nil, IndexOrphanError)
				if method == IndexFetchUseRemoteFetch {
					expectFDBError(page.Err, 2039)
					Expect(page.Rows).NotTo(BeEmpty(), "must fail after returning rows")
					continue
				}
				Expect(page.Err).NotTo(HaveOccurred())
				Expect(page.Rows).To(HaveLen(500))
				for position, id := range ids(page) {
					want := int64(position)
					if reverse {
						want = int64(499 - position)
					}
					Expect(id).To(Equal(want))
				}
				local := page.Rows[499]
				record := &demopb.Order{}
				Expect(proto.Unmarshal(local.Data, record)).To(Succeed())
				Expect(record.GetTags()).To(Equal([]string{"local"}))
			}
		}
		// testReadYourWriteInRange: a local write of an index entry.
		for _, method := range methods {
			store := open(ks, current)
			_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1000), Price: proto.Int32(250)})
			Expect(err).NotTo(HaveOccurred())
			page := scan(store, method, "price", 1, ForwardScan(), nil, IndexOrphanError)
			if method == IndexFetchUseRemoteFetch {
				expectFDBError(page.Err, 2039)
				continue
			}
			Expect(page.Err).NotTo(HaveOccurred())
			Expect(page.Rows).To(HaveLen(501))
		}
	})

	It("rejects or falls back from scans remote fetch cannot run", func() {
		ks := specSubspace()
		seed(ks, current, 12, order)
		type arm struct {
			index    string
			pkLength int
			props    ScanProperties
			noMapped bool
			remote   func(error) bool
			fetches  int64 // mapped reads requested before the failure
		}
		as := func(target any) func(error) bool { return func(err error) bool { return errors.As(err, target) } }
		var argument *RecordCoreArgumentError
		var unsupportedIndex *UnsupportedRemoteFetchIndexError
		var unsupportedOperation *UnsupportedOperationError
		var recordCore *RecordCoreError
		arms := map[string]arm{
			// testScanFailsImmediately
			"snapshot": {index: "price", pkLength: 1, props: NewScanProperties(DefaultExecuteProperties().WithIsolationLevel(IsolationLevelSnapshot)), remote: as(&unsupportedOperation), fetches: 1},
			// testInvalidIntegerPkLength
			"zero primary key length":     {index: "price", pkLength: 0, props: ForwardScan(), remote: as(&argument)},
			"negative primary key length": {index: "price", pkLength: -1, props: ForwardScan(), remote: as(&argument)},
			// testTooLargeIntegerPkLength: the server rejects the mapper.
			"oversized primary key length": {index: "price", pkLength: 86, props: ForwardScan(), fetches: 1, remote: func(err error) bool {
				var native fdb.Error
				return errors.As(err, &native)
			}},
			// a client without mapped reads, like the libfdb_c backend
			"no mapped reads": {index: "price", pkLength: 1, props: ForwardScan(), noMapped: true, remote: as(&unsupportedIndex), fetches: 1},
			// RANK's maintainer embeds the standard one but keeps Java's unsupported default.
			"rank index": {index: "rank", pkLength: 1, props: ForwardScan(), remote: as(&unsupportedIndex)},
		}
		for name, a := range arms {
			for _, method := range methods {
				store := open(ks, current)
				if a.noMapped {
					store.context.tx = remoteFetchNoMappedReads{store.context.tx}
				}
				page := scan(store, method, a.index, a.pkLength, a.props, nil, IndexOrphanError)
				if method != IndexFetchScanAndFetch {
					Expect(page.RemoteFetches).To(Equal(a.fetches), "%s %s", name, method)
				}
				if method == IndexFetchUseRemoteFetch {
					Expect(a.remote(page.Err)).To(BeTrue(), "%s: %v", name, page.Err)
					continue
				}
				Expect(page.Err).NotTo(HaveOccurred(), name)
				Expect(page.Rows).To(HaveLen(12), name)
				if method == IndexFetchUseRemoteFetchWithFallback && a.pkLength == 86 {
					Expect(page.RemoteEntries).To(Equal(int64(1)), "the server rejects the first remote result")
				} else {
					Expect(page.RemoteEntries).To(BeZero(), "%s %s falls back before the remote cursor exists", name, method)
				}
			}
		}
		// testScanUnsupportedIndex: only the ordinary scan reaches the bitmap
		// maintainer's scan-type check.
		for _, method := range methods {
			page := scan(open(ks, current), method, "bitmap", 1, ForwardScan(), nil, IndexOrphanError)
			if method == IndexFetchUseRemoteFetch {
				Expect(errors.As(page.Err, &unsupportedIndex)).To(BeTrue(), "%v", page.Err)
			} else {
				Expect(errors.As(page.Err, &recordCore)).To(BeTrue(), "%v", page.Err)
				Expect(recordCore.Message).To(Equal("Can only scan bitmap index by group."))
			}
		}
	})

	It("routes skipped scans to SCAN_AND_FETCH after remote fetch's checks", func() {
		ks := specSubspace()
		seed(ks, current, 12, order)
		props := NewScanProperties(DefaultExecuteProperties().WithSkip(2).WithReturnedRowLimit(4))
		for _, reverse := range []bool{false, true} {
			want := scan(open(ks, current), IndexFetchScanAndFetch, "price", 1, props.WithReverse(reverse), nil, IndexOrphanError)
			Expect(want.Err).NotTo(HaveOccurred())
			for _, method := range methods[1:] {
				page := scan(open(ks, current), method, "price", 1, props.WithReverse(reverse), nil, IndexOrphanError)
				Expect(page.Err).NotTo(HaveOccurred())
				Expect(page.Rows).To(Equal(want.Rows))
				Expect(page.Continuation).To(Equal(want.Continuation))
				Expect(page.RemoteEntries).To(BeZero(), "a skipped scan is not timed as remote fetch")
				Expect(page.RemoteFetches).To(BeZero(), "a skipped scan requests no mapped read")
			}
		}
		var unsupportedOperation *UnsupportedOperationError
		snapshot := props
		snapshot.ExecuteProperties = snapshot.ExecuteProperties.WithIsolationLevel(IsolationLevelSnapshot)
		page := scan(open(ks, current), IndexFetchUseRemoteFetch, "price", 1, snapshot, nil, IndexOrphanError)
		Expect(errors.As(page.Err, &unsupportedOperation)).To(BeTrue(), "%v", page.Err)
		var unsupportedIndex *UnsupportedRemoteFetchIndexError
		for _, index := range []string{"bitmap", "rank"} {
			page = scan(open(ks, current), IndexFetchUseRemoteFetch, index, 1, props, nil, IndexOrphanError)
			Expect(errors.As(page.Err, &unsupportedIndex)).To(BeTrue(), "%s: %v", index, page.Err)
		}
		store := open(ks, current)
		store.context.tx = remoteFetchNoMappedReads{store.context.tx}
		page = scan(store, IndexFetchUseRemoteFetch, "price", 1, props, nil, IndexOrphanError)
		Expect(errors.As(page.Err, &unsupportedIndex)).To(BeTrue(), "%v", page.Err)
	})

	It("applies ERROR, SKIP and RETURN to orphan entries", func() {
		ks := specSubspace()
		seed(ks, current, 12, order)
		store := open(ks, current)
		store.context.Transaction().ClearRange(store.recordsSubspace.Sub(int64(2)))
		Expect(store.Context().Commit()).To(Succeed())
		for _, reverse := range []bool{false, true} {
			for _, method := range methods {
				for _, orphan := range []IndexOrphanBehavior{IndexOrphanError, IndexOrphanSkip, IndexOrphanReturn} {
					page := scan(open(ks, current), method, "price", 1, ForwardScan().WithReverse(reverse), nil, orphan)
					switch orphan {
					case IndexOrphanError:
						var storage *RecordCoreStorageError
						Expect(errors.As(page.Err, &storage)).To(BeTrue(), "%v", page.Err)
						Expect(storage.PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
						switch method {
						case IndexFetchUseRemoteFetch:
							Expect(storage.Message).To(Equal("record not found for prefetched index entry"))
							Expect(page.BadEntries).To(Equal(int64(1)))
						case IndexFetchScanAndFetch:
							Expect(storage.Message).To(Equal("record not found from index entry"))
							Expect(page.BadEntries).To(Equal(int64(1)))
						default:
							// Java's FallbackCursor falls back on any failure, so the
							// ordinary scan meets the orphan a second time.
							Expect(storage.Message).To(Equal("record not found from index entry"))
							Expect(page.BadEntries).To(Equal(int64(2)))
						}
					case IndexOrphanSkip:
						Expect(page.Err).NotTo(HaveOccurred())
						Expect(ids(page)).NotTo(ContainElement(int64(2)))
						Expect(page.Rows).To(HaveLen(11))
					case IndexOrphanReturn:
						Expect(page.Err).NotTo(HaveOccurred())
						Expect(page.Rows).To(HaveLen(12))
						for _, row := range page.Rows {
							Expect(row.Present).To(Equal(!bytes.Equal(row.PrimaryKey, tuple.Tuple{int64(2)}.Pack())))
						}
					}
				}
			}
		}
	})

	It("builds Java's mapper", func() {
		ks := specSubspace()
		store := open(ks, layout{split: true, compound: true})
		records, err := tuple.Unpack(store.RecordsSubspace().Bytes())
		Expect(err).NotTo(HaveOccurred())
		for index, positions := range map[string][]int{"price": {1, 2}, "pk_first": {1, 2}, "pk_second": {2, 1}} {
			idx := store.GetRecordMetaData().GetIndex(index)
			prefix, err := tuple.Unpack(store.IndexSubspace(idx).Bytes())
			Expect(err).NotTo(HaveOccurred())
			want := append(tuple.Tuple{}, records...)
			for _, position := range positions {
				want = append(want, fmt.Sprintf("{K[%d]}", len(prefix)+position))
			}
			mapper, err := store.CreateRemoteFetchMapper(idx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(mapper.Pack()).To(Equal(append(want, "{...}").Pack()), index)
		}
		Expect(store.GetCommonPrimaryKeyLength(store.GetRecordMetaData().GetIndex("price"))).To(Equal(2))
	})

	It("rebuilds records from secondary rows like Java's unsplitter", func() {
		ks := specSubspace()
		seed(ks, current, 1, func(int) *demopb.Order {
			return &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(1), VectorData: bytes.Repeat([]byte{1}, 150_017)}
		})
		store := open(ks, current)
		stored, err := store.LoadRecord(tuple.Tuple{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		rows, err := store.context.Transaction().GetRange(store.recordsSubspace, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(3), "version and two chunks")
		raw := func(kvs ...fdb.KeyValue) *FDBIndexedRawRecord {
			out := &FDBIndexedRawRecord{
				IndexEntry: &IndexEntry{Index: store.GetRecordMetaData().GetIndex("price"), Key: tuple.Tuple{int64(1), int64(1)}},
				RawRecord:  fdb.MappedKeyValue{Kind: fdb.MappedResultGetRange},
			}
			for _, kv := range kvs {
				out.RawRecord.GetRange.Rows = append(out.RawRecord.GetRange.Rows, client.KeyValue{Key: kv.Key, Value: kv.Value})
			}
			return out
		}
		other := fdb.KeyValue{Key: store.recordsSubspace.Pack(tuple.Tuple{int64(2), int64(0)}), Value: []byte("next record")}
		built, err := store.BuildSingleRecord(raw(rows[0], rows[1], rows[2], other))
		Expect(err).NotTo(HaveOccurred())
		Expect(proto.Equal(built.Record.Record, stored.Record)).To(BeTrue(), "stops at the next primary key")
		Expect(built.Record.Version).To(Equal(stored.Version))
		Expect([]int{built.Record.KeyCount, built.Record.KeySize, built.Record.ValueSize}).
			To(Equal([]int{stored.KeyCount, stored.KeySize, stored.ValueSize}))
		Expect(built.Record.Split).To(BeTrue())

		empty, err := store.BuildSingleRecord(raw())
		Expect(err).NotTo(HaveOccurred())
		Expect(empty).To(BeNil(), "no rows leave the orphan decision to the scan")

		var withoutStart *FoundSplitWithoutStartError
		_, err = store.BuildSingleRecord(raw(rows[0]))
		Expect(errors.As(err, &withoutStart)).To(BeTrue(), "version without data: %v", err)
		Expect(withoutStart.NextIndex).To(Equal(recordVersionSuffix))
		_, err = store.BuildSingleRecord(raw(rows[2]))
		Expect(errors.As(err, &withoutStart)).To(BeTrue(), "second chunk first: %v", err)
		_, err = store.BuildSingleRecord(raw(rows[0], rows[2]))
		Expect(errors.As(err, &withoutStart)).To(BeTrue(), "version then second chunk: %v", err)
		var deserialization *RecordDeserializationError
		_, err = store.BuildSingleRecord(raw(rows[0], fdb.KeyValue{Key: store.recordsSubspace.Pack(tuple.Tuple{int64(1), int64(0)})}))
		Expect(errors.As(err, &deserialization)).To(BeTrue(), "version then an empty data row: %v", err)
		var recordCore *RecordCoreError
		_, err = store.BuildSingleRecord(raw(rows[1], fdb.KeyValue{Key: store.recordsSubspace.Pack(tuple.Tuple{int64(1), int64(3)})}))
		Expect(errors.As(err, &recordCore)).To(BeTrue(), "%v", err)
		Expect(recordCore.Message).To(Equal("Split record segments out of order"))

		// A version this transaction saved is reported incomplete, as Java's
		// unsplitter does through getLocalVersion.
		_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(1)})
		Expect(err).NotTo(HaveOccurred())
		local, err := store.context.Transaction().GetRange(store.recordsSubspace, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		built, err = store.BuildSingleRecord(raw(local...))
		Expect(err).NotTo(HaveOccurred())
		Expect(built.Record.Version.IsComplete()).To(BeFalse())
		reloaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(built.Record.Version).To(Equal(reloaded.Version))
		Expect([]int{built.Record.KeyCount, built.Record.KeySize, built.Record.ValueSize}).
			To(Equal([]int{reloaded.KeyCount, reloaded.KeySize, reloaded.ValueSize}))

		// The old version format keeps no version rows with the record.
		old := open(specSubspace().Sub("old"), layout{format: 5})
		versionRow := fdb.KeyValue{Key: old.recordsSubspace.Pack(tuple.Tuple{int64(1), recordVersionSuffix}), Value: rows[0].Value}
		_, err = old.BuildSingleRecord(raw(versionRow))
		Expect(err).To(MatchError(ContainSubstring("Found record version when old format specified")))
	})
})
