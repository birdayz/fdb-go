package benchfetch

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// cell is one access shape over a payload's rowCount Orders.
type cell struct {
	name string
	// limit is the returned-row limit; 0 drains the range.
	limit int
	// rng is the index range; first and count describe its forward order.
	rng          recordlayer.TupleRange
	first, count int
}

var cells = []cell{
	// A broad eligible range whose scan stops after one returned row.
	{name: "limit1", limit: 1, rng: recordlayer.TupleRangeAll, first: 0, count: rowCount},
	// A bounded range selecting exactly ten records, drained.
	{name: "rows10", rng: recordlayer.TupleRangeBetween(tuple.Tuple{int64(100)}, tuple.Tuple{int64(110)}), first: 100, count: 10},
	// The whole index, drained.
	{name: "rows1000", rng: recordlayer.TupleRangeAll, first: 0, count: rowCount},
}

var methods = []recordlayer.IndexFetchMethod{recordlayer.IndexFetchScanAndFetch, recordlayer.IndexFetchUseRemoteFetch}

// want is the cell's exact result order and terminal reason.
func (c cell) want(reverse bool) ([]int64, recordlayer.NoNextReason) {
	ids := make([]int64, c.count)
	for i := range ids {
		ids[i] = int64(c.first + i)
	}
	if reverse {
		slices.Reverse(ids)
	}
	if c.limit > 0 {
		return ids[:c.limit], recordlayer.ReturnLimitReached
	}
	return ids, recordlayer.SourceExhausted
}

// BenchmarkIndexFetch measures one store-level index scan per operation:
// a fresh transaction, store open, ScanIndexRecordsWithFetchMethod over the
// cell's range, draining the cursor and closing it. Every operation's rows are
// checked outside the timer (IDs, order, fields, payload checksum and terminal
// reason); a counter audit before and after the measured loop, also untimed,
// proves which path ran.
//
// Sub-benchmarks: payload={small,split}/cell={limit1,rows10,rows1000}/
// dir={forward,reverse}/method={SCAN_AND_FETCH,USE_REMOTE_FETCH}.
func BenchmarkIndexFetch(b *testing.B) {
	for _, p := range payloads {
		b.Run("payload="+p.name, func(b *testing.B) {
			f := loadFixture(b, p)
			for _, c := range cells {
				for _, reverse := range []bool{false, true} {
					dir := "forward"
					if reverse {
						dir = "reverse"
					}
					for _, method := range methods {
						b.Run(fmt.Sprintf("cell=%s/dir=%s/method=%s", c.name, dir, method), func(b *testing.B) {
							benchCell(b, f, c, reverse, method)
						})
					}
				}
			}
		})
	}
}

func benchCell(b *testing.B, f *fixture, c cell, reverse bool, method recordlayer.IndexFetchMethod) {
	ctx := context.Background()
	wantIDs, wantReason := c.want(reverse)
	execute := recordlayer.DefaultExecuteProperties()
	if c.limit > 0 {
		execute = execute.WithReturnedRowLimit(c.limit)
	}
	props := recordlayer.NewScanProperties(execute).WithReverse(reverse)

	scan := func(timer *recordlayer.StoreTimer, rows []*recordlayer.FDBIndexedRecord) ([]*recordlayer.FDBIndexedRecord, recordlayer.NoNextReason, error) {
		tx, err := f.db.CreateWritableTransaction()
		if err != nil {
			return rows, 0, err
		}
		rc := f.db.NewRecordContext(tx)
		defer rc.Cancel()
		if timer != nil {
			rc.SetTimer(timer)
		}
		store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(f.ss).SetMetaDataProvider(f.md).Open()
		if err != nil {
			return rows, 0, err
		}
		cursor := store.ScanIndexRecordsWithFetchMethod(f.index, method, recordlayer.IndexScanByValue, c.rng,
			f.commonPrimaryKeyLength, nil, recordlayer.IndexOrphanError, props)
		defer cursor.Close()
		for {
			next, err := cursor.OnNext(ctx)
			if err != nil {
				return rows, 0, err
			}
			if !next.HasNext() {
				return rows, next.GetNoNextReason(), nil
			}
			rows = append(rows, next.GetValue())
		}
	}
	verify := func(rows []*recordlayer.FDBIndexedRecord, reason recordlayer.NoNextReason) error {
		if len(rows) != len(wantIDs) || reason != wantReason {
			return fmt.Errorf("%d rows ending %v, want %d ending %v", len(rows), reason, len(wantIDs), wantReason)
		}
		for i, row := range rows {
			if row == nil || row.IndexEntry == nil {
				return fmt.Errorf("row %d: no indexed record", i)
			}
			if err := f.check(wantIDs[i], row.Record); err != nil {
				return fmt.Errorf("row %d: %w", i, err)
			}
		}
		return nil
	}
	audit := func(phase string) {
		timer := recordlayer.NewStoreTimer()
		rows, reason, err := scan(timer, nil)
		if err == nil {
			err = verify(rows, reason)
		}
		if err != nil {
			b.Fatalf("%s audit: %v", phase, err)
		}
		remote := timer.GetCount(recordlayer.CountRemoteFetch)
		entries := timer.GetCount(recordlayer.EventScanRemoteFetchEntry)
		indexScans := timer.GetCount(recordlayer.EventScanIndex)
		loads := timer.GetCount(recordlayer.EventLoadRecord)
		auditLog(b, map[string]any{
			"phase": phase, "method": method.String(), "payload": f.payload.name,
			"payload_bytes": f.payload.padding, "chunks": f.payload.chunks, "reverse": reverse, "skip": 0,
			"limit": c.limit, "rows": len(rows), "remote_openings": remote, "remote_entries": entries,
			"index_scans": indexScans, "record_loads": loads, "checksum": f.rowsChecksum(rows),
			"prefix_bytes": len(f.ss.Bytes()),
		})
		switch method {
		case recordlayer.IndexFetchUseRemoteFetch:
			if remote < 1 || entries < int64(len(rows)) || indexScans != 0 || loads != 0 {
				b.Fatalf("%s audit: USE_REMOTE_FETCH did not run the mapped read alone", phase)
			}
		default:
			if remote != 0 || entries != 0 || indexScans < 1 {
				b.Fatalf("%s audit: SCAN_AND_FETCH ran a remote fetch or no index scan", phase)
			}
		}
	}

	audit("before")
	rows := make([]*recordlayer.FDBIndexedRecord, 0, len(wantIDs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var reason recordlayer.NoNextReason
		var err error
		rows, reason, err = scan(nil, rows[:0])
		b.StopTimer()
		if err == nil {
			err = verify(rows, reason)
		}
		if err != nil {
			b.Fatalf("operation %d: %v", i, err)
		}
		clear(rows)
		b.StartTimer()
	}
	b.StopTimer()
	audit("after")
	b.ReportMetric(float64(len(wantIDs)), "rows/op")
	b.ReportMetric(float64(len(f.ss.Bytes())), "prefix-B")
}
