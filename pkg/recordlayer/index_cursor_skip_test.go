package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// skipRangeObserver records the index range reads a cursor issues, unchanged.
type skipRangeObserver struct {
	fdb.WritableTransaction
	prefix []byte
	reads  []skipRangeRead
}

type skipRangeRead struct {
	begin, end fdb.KeySelector
	options    fdb.RangeOptions
	snapshot   bool
}

func (o *skipRangeObserver) observe(r fdb.Range, options fdb.RangeOptions, snapshot bool) {
	b, e := r.FDBRangeKeySelectors()
	begin, end := b.FDBKeySelector(), e.FDBKeySelector()
	if len(o.prefix) > 0 && (bytes.HasPrefix(begin.Key.FDBKey(), o.prefix) || bytes.HasPrefix(end.Key.FDBKey(), o.prefix)) {
		o.reads = append(o.reads, skipRangeRead{begin: begin, end: end, options: options, snapshot: snapshot})
	}
}

func (o *skipRangeObserver) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	o.observe(r, options, false)
	return o.WritableTransaction.GetRange(r, options)
}

func (o *skipRangeObserver) Snapshot() fdb.ReadTransaction {
	return skipRangeSnapshot{ReadTransaction: o.WritableTransaction.Snapshot(), owner: o}
}

type skipRangeSnapshot struct {
	fdb.ReadTransaction
	owner *skipRangeObserver
}

func (s skipRangeSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	s.owner.observe(r, options, true)
	return s.ReadTransaction.GetRange(r, options)
}

type skipScanRequest struct {
	index        string
	reverse      bool
	records      bool
	snapshot     bool
	skip, limit  int
	scanLimit    int
	byteLimit    int64
	mode         CursorStreamingMode
	continuation []byte
	write        func(*testing.T, *FDBRecordStore)
}

type skipRow struct {
	key, continuation []byte
	bytes             int64
}

type skipPage struct {
	rows         []skipRow
	reason       NoNextReason
	end          bool
	continuation []byte
	scanned      int
	scannedBytes int64
	reads        []skipRangeRead
}

func skipWindow(rows []skipRow, from, n int) []skipRow {
	from = min(from, len(rows))
	if n <= 0 || n > len(rows)-from {
		n = len(rows) - from
	}
	return rows[from : from+n]
}

func skipBytes(rows []skipRow) int64 {
	var total int64
	for _, row := range rows {
		total += row.bytes
	}
	return total
}

func expectSkipRows(t *testing.T, got skipPage, want []skipRow) {
	t.Helper()
	if len(got.rows) != len(want) {
		t.Fatalf("rows=%d want %d (reason=%v)", len(got.rows), len(want), got.reason)
	}
	for i := range want {
		if !bytes.Equal(got.rows[i].key, want[i].key) || !bytes.Equal(got.rows[i].continuation, want[i].continuation) {
			t.Fatalf("row %d key/continuation = %x/%x want %x/%x", i, got.rows[i].key, got.rows[i].continuation, want[i].key, want[i].continuation)
		}
	}
}

func expectSkipTerminal(t *testing.T, got skipPage, rows int, reason NoNextReason) {
	t.Helper()
	if got.reason != reason || got.end != (reason == SourceExhausted) {
		t.Fatalf("terminal reason/end = %v/%v want %v", got.reason, got.end, reason)
	}
	// CursorLimitManager charges each returned row plus the terminal attempt;
	// rows the selector skipped never reach the cursor.
	if got.scanned != rows+1 {
		t.Fatalf("scanned records=%d want %d", got.scanned, rows+1)
	}
}

func expectSkipSelector(t *testing.T, got, want fdb.KeySelector) {
	t.Helper()
	if !bytes.Equal(got.Key.FDBKey(), want.Key.FDBKey()) || got.OrEqual != want.OrEqual || got.Offset != want.Offset {
		t.Fatalf("selector = %x/%v/%d want %x/%v/%d", got.Key.FDBKey(), got.OrEqual, got.Offset,
			want.Key.FDBKey(), want.OrEqual, want.Offset)
	}
}

func onlySkipRead(t *testing.T, page skipPage) skipRangeRead {
	t.Helper()
	if len(page.reads) != 1 {
		t.Fatalf("index range reads=%d want 1", len(page.reads))
	}
	return page.reads[0]
}

// Java KeyValueCursorBase.Builder.prepare (KeyValueCursorBase.java:327-356)
// applies a positive Skip as selector offsets after the continuation: begin
// FGE(low)+skip forward, end FGE(high)-skip reverse, with the unadjusted
// returned-row limit and Java's streaming mode.
func TestFDB_IndexCursorSkipSelectorOffsets(t *testing.T) {
	t.Parallel()
	const count = 6
	db := newLoadParityDatabase(t)
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetStoreRecordVersions(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price", Field("price")))
	builder.AddIndex("Order", NewVersionIndex("version", VersionKey()))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	open := func(t *testing.T, observer *skipRangeObserver) *FDBRecordStore {
		t.Helper()
		tx, err := db.CreateWritableTransaction()
		if err != nil {
			t.Fatal(err)
		}
		observer.WritableTransaction = tx
		rc := NewFDBRecordContext(observer, nil)
		t.Cleanup(rc.Cancel)
		store, err := NewStoreBuilder().SetContext(rc).SetSubspace(root).SetMetaDataProvider(metadata).CreateOrOpen()
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	seed := open(t, &skipRangeObserver{})
	for id := range count {
		if _, err := seed.SaveRecord(&demopb.Order{OrderId: proto.Int64(int64(id)), Price: proto.Int32(int32(100 - id))}); err != nil {
			t.Fatal(err)
		}
	}
	assertLoadParityCommit(t, seed.context, false)

	scan := func(t *testing.T, req skipScanRequest) skipPage {
		t.Helper()
		observer := &skipRangeObserver{}
		store := open(t, observer)
		if req.write != nil {
			req.write(t, store)
		}
		index := store.GetRecordMetaData().GetIndex(req.index)
		observer.prefix = store.IndexSubspace(index).Bytes()
		state := NewScanLimiterState()
		execute := DefaultExecuteProperties().WithSkip(req.skip).WithReturnedRowLimit(req.limit).
			WithScannedRecordsLimit(req.scanLimit).WithScannedBytesLimit(req.byteLimit)
		if req.snapshot {
			execute = execute.WithIsolationLevel(SnapshotIsolation)
		}
		execute.ScanState = state
		props := NewScanProperties(execute).WithReverse(req.reverse).WithStreamingMode(req.mode)
		var cursor RecordCursor[*IndexEntry]
		if req.records {
			cursor = MapCursor(store.ScanIndexRecords(req.index, TupleRangeAll, req.continuation, props),
				func(record *FDBIndexedRecord) *IndexEntry { return record.IndexEntry })
		} else {
			cursor = store.ScanIndex(index, TupleRangeAll, req.continuation, props)
		}
		defer func() { _ = cursor.Close() }()
		var page skipPage
		for {
			before := state.BytesScanned()
			result, err := cursor.OnNext(context.Background())
			if err != nil {
				t.Fatalf("scan %+v: %v", req, err)
			}
			continuation, err := result.GetContinuation().ToBytes()
			if err != nil {
				t.Fatal(err)
			}
			if !result.HasNext() {
				page.reason, page.end, page.continuation = result.GetNoNextReason(), result.GetContinuation().IsEnd(), continuation
				break
			}
			page.rows = append(page.rows, skipRow{key: result.GetValue().Key.Pack(), continuation: continuation, bytes: state.BytesScanned() - before})
		}
		page.scanned, page.scannedBytes, page.reads = state.RecordsScanned(), state.BytesScanned(), observer.reads
		return page
	}

	for _, indexName := range []string{"price", "version"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", indexName, reverse), func(t *testing.T) {
				t.Parallel()
				request := func(skip, limit int) skipScanRequest {
					return skipScanRequest{index: indexName, reverse: reverse, skip: skip, limit: limit, mode: StreamingModeIterator}
				}
				base := scan(t, request(0, 0))
				if len(base.rows) != count || base.reason != SourceExhausted {
					t.Fatalf("baseline rows=%d reason=%v", len(base.rows), base.reason)
				}
				baseRead := onlySkipRead(t, base)
				// shifted is the selector Java's KeySelector.add produces for skip.
				shifted := func(skip int) (fdb.KeySelector, fdb.KeySelector) {
					begin, end := baseRead.begin, baseRead.end
					if reverse {
						end.Offset -= skip
					} else {
						begin.Offset += skip
					}
					return begin, end
				}

				for _, skip := range []int{1, 2, count - 1, count, count + 1, count + 5} {
					t.Run(fmt.Sprintf("exhausted/skip=%d", skip), func(t *testing.T) {
						page := scan(t, request(skip, 0))
						want := skipWindow(base.rows, skip, 0)
						expectSkipRows(t, page, want)
						expectSkipTerminal(t, page, len(want), SourceExhausted)
						if page.scannedBytes != skipBytes(want) {
							t.Fatalf("scanned bytes=%d want %d", page.scannedBytes, skipBytes(want))
						}
						read := onlySkipRead(t, page)
						begin, end := shifted(skip)
						expectSkipSelector(t, read.begin, begin)
						expectSkipSelector(t, read.end, end)
					})
				}

				for _, skip := range []int{1, 3} {
					for _, limit := range []int{1, 2, count - skip, count - skip + 1} {
						t.Run(fmt.Sprintf("row_limit/skip=%d/limit=%d", skip, limit), func(t *testing.T) {
							page := scan(t, request(skip, limit))
							want := skipWindow(base.rows, skip, limit)
							expectSkipRows(t, page, want)
							if skip+limit <= count {
								expectSkipTerminal(t, page, len(want), ReturnLimitReached)
								if !bytes.Equal(page.continuation, want[len(want)-1].continuation) {
									t.Fatalf("limit continuation=%x want last row %x", page.continuation, want[len(want)-1].continuation)
								}
							} else {
								expectSkipTerminal(t, page, len(want), SourceExhausted)
							}
							// The skip is not added to the native row limit.
							if read := onlySkipRead(t, page); read.options.Limit != limit {
								t.Fatalf("native limit=%d want %d", read.options.Limit, limit)
							}
						})
					}
				}

				t.Run("continuation", func(t *testing.T) {
					for position, row := range base.rows {
						req := request(2, 0)
						req.continuation = row.continuation
						page := scan(t, req)
						want := skipWindow(base.rows, position+1+2, 0)
						expectSkipRows(t, page, want)
						expectSkipTerminal(t, page, len(want), SourceExhausted)
						read := onlySkipRead(t, page)
						if !reverse && read.begin.Offset != 3 || reverse && read.end.Offset != -1 {
							t.Fatalf("resumed selector offsets begin=%d end=%d", read.begin.Offset, read.end.Offset)
						}
					}
					// A caller that keeps Skip re-applies it to every resumed page.
					req := request(1, 2)
					var pages [][]skipRow
					for range 3 {
						page := scan(t, req)
						pages = append(pages, page.rows)
						req.continuation = page.continuation
					}
					for i, want := range [][]skipRow{skipWindow(base.rows, 1, 2), skipWindow(base.rows, 4, 2), nil} {
						expectSkipRows(t, skipPage{rows: pages[i]}, want)
					}
				})

				for _, scanLimit := range []int{1, 2} {
					t.Run(fmt.Sprintf("scanned_limit=%d", scanLimit), func(t *testing.T) {
						req := request(2, 0)
						req.scanLimit = scanLimit
						page := scan(t, req)
						want := skipWindow(base.rows, 2, scanLimit)
						expectSkipRows(t, page, want)
						expectSkipTerminal(t, page, len(want), ScanLimitReached)
					})
				}

				t.Run("byte_limit", func(t *testing.T) {
					req := request(2, 0)
					req.byteLimit = 1
					page := scan(t, req)
					want := skipWindow(base.rows, 2, 1)
					expectSkipRows(t, page, want)
					expectSkipTerminal(t, page, 1, ByteLimitReached)
					if page.scannedBytes != want[0].bytes {
						t.Fatalf("scanned bytes=%d want %d", page.scannedBytes, want[0].bytes)
					}
				})

				for _, mode := range []CursorStreamingMode{StreamingModeIterator, StreamingModeWantAll, StreamingModeSmall, StreamingModeMedium, StreamingModeLarge} {
					for _, limit := range []int{0, 2} {
						t.Run(fmt.Sprintf("mode=%d/limit=%d", mode, limit), func(t *testing.T) {
							req := request(2, limit)
							req.mode = mode
							page := scan(t, req)
							expectSkipRows(t, page, skipWindow(base.rows, 2, limit))
							want := mode.ToFDB()
							if mode == StreamingModeWantAll && limit > 0 {
								want = fdb.StreamingModeExact // Java calcStreamingMode.
							}
							if read := onlySkipRead(t, page); read.options.Mode != want || read.options.Limit != limit {
								t.Fatalf("native mode/limit=%v/%d want %v/%d", read.options.Mode, read.options.Limit, want, limit)
							}
						})
					}
				}

				t.Run("snapshot", func(t *testing.T) {
					req := request(2, 0)
					req.snapshot = true
					page := scan(t, req)
					expectSkipRows(t, page, skipWindow(base.rows, 2, 0))
					if !onlySkipRead(t, page).snapshot {
						t.Fatal("snapshot scan read through the serializable transaction")
					}
				})

				t.Run("records", func(t *testing.T) {
					req := request(2, 2)
					req.records = true
					page := scan(t, req)
					expectSkipRows(t, page, skipWindow(base.rows, 2, 2))
				})

				if indexName == "price" {
					t.Run("local_write", func(t *testing.T) {
						// Selector resolution sees the transaction's own uncommitted entry.
						req := request(1, 0)
						price := int32(1)
						if reverse {
							price = 1000
						}
						req.write = func(t *testing.T, store *FDBRecordStore) {
							if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(100), Price: proto.Int32(price)}); err != nil {
								t.Fatal(err)
							}
						}
						page := scan(t, req)
						expectSkipRows(t, page, base.rows)
					})
				}

				for _, skip := range []int{math.MaxInt32 - 1, math.MaxInt32, math.MaxInt32 + 5, math.MaxInt} {
					t.Run(fmt.Sprintf("bound/skip=%d", skip), func(t *testing.T) {
						page := scan(t, request(skip, 0))
						expectSkipRows(t, page, nil)
						expectSkipTerminal(t, page, 0, SourceExhausted)
						// No int32 wrap: the read never moves back before low (forward)
						// or forward past high (reverse).
						read := onlySkipRead(t, page)
						for _, selector := range []fdb.KeySelector{read.begin, read.end} {
							if selector.Offset < math.MinInt32 || selector.Offset > math.MaxInt32 {
								t.Fatalf("selector offset %d is outside the native int32 range", selector.Offset)
							}
						}
						if !reverse && (read.begin.Offset < 0 || bytes.Compare(read.begin.Key.FDBKey(), baseRead.begin.Key.FDBKey()) < 0) {
							t.Fatalf("forward begin %x/%d anchors before low %x", read.begin.Key.FDBKey(), read.begin.Offset, baseRead.begin.Key.FDBKey())
						}
						if reverse && (read.end.Offset > 1 || bytes.Compare(read.end.Key.FDBKey(), baseRead.end.Key.FDBKey()) > 0) {
							t.Fatalf("reverse end %x/%d anchors after high %x", read.end.Key.FDBKey(), read.end.Offset, baseRead.end.Key.FDBKey())
						}
					})
				}
			})
		}
	}
}
