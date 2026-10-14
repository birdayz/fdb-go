package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

type recordScanPage struct {
	rows  []*FDBStoredRecord[proto.Message]
	conts []RecordCursorContinuation
	last  RecordCursorResult[*FDBStoredRecord[proto.Message]]
	err   error
}

func (p recordScanPage) pks() []int64 {
	out := make([]int64, 0, len(p.rows))
	for _, row := range p.rows {
		out = append(out, row.PrimaryKey[0].(int64))
	}
	return out
}

func scanRecordPage(store *FDBRecordStore, execute ExecuteProperties, reverse bool, continuation []byte) recordScanPage {
	var page recordScanPage
	cursor := store.ScanRecords(continuation, NewScanProperties(execute).WithReverse(reverse))
	defer func() { _ = cursor.Close() }()
	for {
		page.last, page.err = cursor.OnNext(context.Background())
		if page.err != nil || !page.last.HasNext() {
			return page
		}
		page.rows = append(page.rows, page.last.GetValue())
		page.conts = append(page.conts, page.last.GetContinuation())
	}
}

func recordScanToken(t *testing.T, cont RecordCursorContinuation) []byte {
	t.Helper()
	b, err := cont.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func equalPKs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Java's scanTypedRecords runs SplitHelper.KeyValueUnsplitter: every key-value,
// the inline version included, is charged as a scanned record and counted in
// the record's sizes, and a limit stops only between records.
func TestFDB_RecordScanJavaSizesAndLimits(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	splitMeta := loadParityMetaData(t, true, true)
	root := subspace.FromBytes(tuple.Tuple{t.Name(), "split"}.Pack())
	open := func(t *testing.T, root subspace.Subspace, md *RecordMetaData, format int32) *FDBRecordStore {
		t.Helper()
		store, err := NewStoreBuilder().SetContext(loadParityContext(t, db)).SetSubspace(root).
			SetMetaDataProvider(md).SetFormatVersion(format).CreateOrOpen()
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	order := func(id int64, payload int) *demopb.Order {
		return &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), VectorData: bytes.Repeat([]byte{byte(id)}, payload)}
	}
	seed := open(t, root, splitMeta, 14)
	for _, o := range []*demopb.Order{order(1, 10), order(2, 150_000), order(3, 10)} {
		if _, err := seed.SaveRecord(o); err != nil {
			t.Fatal(err)
		}
	}
	assertLoadParityCommit(t, seed.context, false)

	raw := func(t *testing.T, store *FDBRecordStore) (total int64, version3 int64, count int) {
		t.Helper()
		kvs, err := store.context.Transaction().GetRange(store.recordsSubspace, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			t.Fatal(err)
		}
		v3 := store.recordsSubspace.Pack(tuple.Tuple{int64(3), recordVersionSuffix})
		for _, kv := range kvs {
			total += int64(len(kv.Key) + len(kv.Value))
			if bytes.Equal(kv.Key, v3) {
				version3 = int64(len(kv.Key) + len(kv.Value))
			}
		}
		return total, version3, len(kvs)
	}
	limits := func(f func(*ExecuteProperties)) ExecuteProperties {
		execute := DefaultExecuteProperties()
		f(&execute)
		return execute
	}

	t.Run("sizes match LoadRecord and charge every key-value", func(t *testing.T) {
		t.Parallel()
		store := open(t, root, splitMeta, 14)
		total, version3, kvs := raw(t, store)
		for _, reverse := range []bool{false, true} {
			execute := DefaultExecuteProperties()
			page := scanRecordPage(store, execute, reverse, nil)
			if page.err != nil || len(page.rows) != 3 || page.last.GetNoNextReason() != SourceExhausted {
				t.Fatalf("reverse=%v: rows=%v reason=%v err=%v", reverse, page.pks(), page.last.GetNoNextReason(), page.err)
			}
			var sized int64
			for _, row := range page.rows {
				loaded, err := store.LoadRecord(row.PrimaryKey)
				if err != nil {
					t.Fatal(err)
				}
				if row.KeyCount != loaded.KeyCount || row.KeySize != loaded.KeySize || row.ValueSize != loaded.ValueSize ||
					row.Split != loaded.Split || row.VersionedInline != loaded.VersionedInline || !row.VersionedInline {
					t.Fatalf("reverse=%v pk=%v scan sizes %d/%d/%d split=%v inline=%v, load %d/%d/%d split=%v inline=%v", reverse, row.PrimaryKey,
						row.KeyCount, row.KeySize, row.ValueSize, row.Split, row.VersionedInline,
						loaded.KeyCount, loaded.KeySize, loaded.ValueSize, loaded.Split, loaded.VersionedInline)
				}
				sized += int64(row.KeySize + row.ValueSize)
			}
			if page.rows[1].KeyCount != 3 || !page.rows[1].Split {
				t.Fatalf("split record sizes: count=%d split=%v", page.rows[1].KeyCount, page.rows[1].Split)
			}
			// A forward scan reads record 3's version as the end of record 2 and charges it again.
			wantBytes := total
			if !reverse {
				wantBytes += version3
			}
			if sized != total || execute.ScanState.RecordsScanned() != kvs || execute.ScanState.BytesScanned() != wantBytes {
				t.Fatalf("reverse=%v: sized=%d raw=%d scanned=%d/%d bytes=%d want %d", reverse, sized, total,
					execute.ScanState.RecordsScanned(), kvs, execute.ScanState.BytesScanned(), wantBytes)
			}
		}
	})

	t.Run("scanned-records limit completes the record that crosses it", func(t *testing.T) {
		t.Parallel()
		store := open(t, root, splitMeta, 14)
		execute := limits(func(e *ExecuteProperties) { e.ScannedRecordsLimit = 3 })
		page := scanRecordPage(store, execute, false, nil)
		// v1 r1 | v2 c1 (crosses) c2, then v3 as lookahead.
		if page.err != nil || !equalPKs(page.pks(), 1, 2) || page.last.GetNoNextReason() != ScanLimitReached ||
			execute.ScanState.RecordsScanned() != 6 {
			t.Fatalf("rows=%v reason=%v scanned=%d err=%v", page.pks(), page.last.GetNoNextReason(), execute.ScanState.RecordsScanned(), page.err)
		}
		token := recordScanToken(t, page.last.GetContinuation())
		if !bytes.Equal(token, recordScanToken(t, page.conts[1])) {
			t.Fatal("limit token is not the last record's")
		}
		resumed := limits(func(e *ExecuteProperties) { e.ScannedRecordsLimit = 3 })
		page = scanRecordPage(store, resumed, false, token)
		if page.err != nil || !equalPKs(page.pks(), 3) || page.last.GetNoNextReason() != SourceExhausted || resumed.ScanState.RecordsScanned() != 2 {
			t.Fatalf("resume rows=%v reason=%v scanned=%d err=%v", page.pks(), page.last.GetNoNextReason(), resumed.ScanState.RecordsScanned(), page.err)
		}
		failing := limits(func(e *ExecuteProperties) { e.ScannedRecordsLimit = 1; e.FailOnScanLimitReached = true })
		page = scanRecordPage(store, failing, false, nil)
		var limitErr *ScanLimitReachedError
		if len(page.rows) != 0 || !errors.As(page.err, &limitErr) {
			t.Fatalf("fail mode throws at the crossing key-value: rows=%v err=%v", page.pks(), page.err)
		}
	})

	t.Run("byte and time limits complete the record that crosses them", func(t *testing.T) {
		t.Parallel()
		store := open(t, root, splitMeta, 14)
		first := scanRecordPage(store, DefaultExecuteProperties(), false, nil)
		if first.err != nil || len(first.rows) != 3 {
			t.Fatalf("baseline: %v %v", first.pks(), first.err)
		}
		execute := limits(func(e *ExecuteProperties) {
			e.ScannedBytesLimit = int64(first.rows[0].KeySize + first.rows[0].ValueSize)
		})
		page := scanRecordPage(store, execute, false, nil)
		if page.err != nil || !equalPKs(page.pks(), 1, 2) || page.last.GetNoNextReason() != ByteLimitReached {
			t.Fatalf("byte limit rows=%v reason=%v err=%v", page.pks(), page.last.GetNoNextReason(), page.err)
		}
		timed := open(t, root, splitMeta, 14)
		// The limit runs from the transaction's creation, not the properties'.
		time.Sleep(75 * time.Millisecond)
		execute = limits(func(e *ExecuteProperties) { e.TimeLimit = 50 * time.Millisecond })
		page = scanRecordPage(timed, execute, false, nil)
		if page.err != nil || !equalPKs(page.pks(), 1) || page.last.GetNoNextReason() != TimeLimitReached {
			t.Fatalf("time limit rows=%v reason=%v err=%v", page.pks(), page.last.GetNoNextReason(), page.err)
		}
	})

	t.Run("row limit stops without reading ahead", func(t *testing.T) {
		t.Parallel()
		store := open(t, root, splitMeta, 14)
		execute := DefaultExecuteProperties().WithReturnedRowLimit(3)
		page := scanRecordPage(store, execute, false, nil)
		if page.err != nil || len(page.rows) != 3 || page.last.GetNoNextReason() != ReturnLimitReached || page.last.GetContinuation().IsEnd() {
			t.Fatalf("rows=%v reason=%v end=%v err=%v", page.pks(), page.last.GetNoNextReason(), page.last.GetContinuation().IsEnd(), page.err)
		}
		page = scanRecordPage(store, DefaultExecuteProperties().WithReturnedRowLimit(3), false, recordScanToken(t, page.last.GetContinuation()))
		if page.err != nil || len(page.rows) != 0 || page.last.GetNoNextReason() != SourceExhausted {
			t.Fatalf("resume rows=%v reason=%v err=%v", page.pks(), page.last.GetNoNextReason(), page.err)
		}
	})

	t.Run("same-transaction version is counted", func(t *testing.T) {
		t.Parallel()
		store := open(t, root, splitMeta, 14)
		saved, err := store.SaveRecord(order(4, 10))
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := store.LoadRecord(tuple.Tuple{int64(4)})
		if err != nil {
			t.Fatal(err)
		}
		page := scanRecordPage(store, DefaultExecuteProperties(), true, nil)
		if page.err != nil || len(page.rows) != 4 {
			t.Fatalf("rows=%v err=%v", page.pks(), page.err)
		}
		row := page.rows[0]
		for _, want := range []*FDBStoredRecord[proto.Message]{saved, loaded} {
			if row.KeyCount != 2 || row.KeyCount != want.KeyCount || row.KeySize != want.KeySize || row.ValueSize != want.ValueSize ||
				!row.VersionedInline || !want.VersionedInline || row.Version == nil || row.Version.IsComplete() {
				t.Fatalf("scan %d/%d/%d inline=%v, want %d/%d/%d inline=%v", row.KeyCount, row.KeySize, row.ValueSize, row.VersionedInline,
					want.KeyCount, want.KeySize, want.ValueSize, want.VersionedInline)
			}
		}
	})

	t.Run("inline versions survive disabling them in metadata", func(t *testing.T) {
		t.Parallel()
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
			SetSplitLongRecords(true).SetStoreRecordVersions(false).SetVersion(100)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		unversioned, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		store := open(t, root, unversioned, 14)
		page := scanRecordPage(store, DefaultExecuteProperties(), false, nil)
		if page.err != nil || len(page.rows) != 3 || page.rows[0].Version == nil || page.rows[0].KeyCount != 2 {
			t.Fatalf("rows=%v err=%v", page.pks(), page.err)
		}
	})

	// Java's countRecords counts an unsplit store's key-values uncharged and a split
	// store's unsplitter records; skip moves the selector, row and time limits are ignored.
	t.Run("countRecords counts what Java counts", func(t *testing.T) {
		t.Parallel()
		count := func(store *FDBRecordStore, execute ExecuteProperties) (int, error) {
			return store.CountRecords(context.Background(), nil, nil, EndpointTypeTreeStart, EndpointTypeTreeEnd, nil, NewScanProperties(execute))
		}
		unsplitMeta := loadParityMetaData(t, false, true)
		unsplitRoot := subspace.FromBytes(tuple.Tuple{t.Name(), "unsplit"}.Pack())
		unsplit := open(t, unsplitRoot, unsplitMeta, 14)
		for id := int64(1); id <= 3; id++ {
			if _, err := unsplit.SaveRecord(order(id, 10)); err != nil {
				t.Fatal(err)
			}
		}
		assertLoadParityCommit(t, unsplit.context, false)
		unsplit = open(t, unsplitRoot, unsplitMeta, 14)
		for _, tc := range []struct {
			execute ExecuteProperties
			want    int
		}{
			{DefaultExecuteProperties(), 6},
			{limits(func(e *ExecuteProperties) { e.Skip = 1 }), 5},
			{limits(func(e *ExecuteProperties) { e.Skip = math.MaxInt32 }), 0},
			{limits(func(e *ExecuteProperties) { e.Skip = 1 << 32 }), 0},
			{limits(func(e *ExecuteProperties) {
				e.ReturnedRowLimit, e.ScannedRecordsLimit, e.TimeLimit = 1, 1, time.Nanosecond
			}), 6},
		} {
			got, err := count(unsplit, tc.execute)
			if err != nil || got != tc.want || tc.execute.ScanState.RecordsScanned() != 0 {
				t.Fatalf("unsplit count=%d err=%v charged=%d, want %d key-values uncharged", got, err, tc.execute.ScanState.RecordsScanned(), tc.want)
			}
		}
		split := open(t, root, splitMeta, 14)
		total, version3, kvs := raw(t, split)
		execute := limits(func(e *ExecuteProperties) { e.ReturnedRowLimit, e.TimeLimit = 1, time.Nanosecond })
		got, err := count(split, execute)
		if err != nil || got != 3 || execute.ScanState.RecordsScanned() != kvs || execute.ScanState.BytesScanned() != total+version3 {
			t.Fatalf("split count=%d err=%v charged=%d/%d", got, err, execute.ScanState.RecordsScanned(), execute.ScanState.BytesScanned())
		}
		// Java returns the partial count; Go also reports the truncation (RFC-106a).
		execute = limits(func(e *ExecuteProperties) { e.ScannedRecordsLimit = 3 })
		got, err = count(split, execute)
		var limitErr *ScanLimitReachedError
		if got != 2 || !errors.As(err, &limitErr) || execute.ScanState.RecordsScanned() != 6 {
			t.Fatalf("split limited count=%d err=%v charged=%d", got, err, execute.ScanState.RecordsScanned())
		}
	})

	t.Run("omitted suffix skips by selector and charges the end attempt", func(t *testing.T) {
		t.Parallel()
		bareMeta := loadParityMetaData(t, false, false)
		bareRoot := subspace.FromBytes(tuple.Tuple{t.Name(), "bare"}.Pack())
		bare := open(t, bareRoot, bareMeta, 4)
		for id := int64(1); id <= 3; id++ {
			if _, err := bare.SaveRecord(order(id, 10)); err != nil {
				t.Fatal(err)
			}
		}
		assertLoadParityCommit(t, bare.context, false)
		store := open(t, bareRoot, bareMeta, 4)
		skipped := limits(func(e *ExecuteProperties) { e.Skip = 1 })
		page := scanRecordPage(store, skipped, false, nil)
		if page.err != nil || !equalPKs(page.pks(), 2, 3) || page.last.GetNoNextReason() != SourceExhausted {
			t.Fatalf("skip rows=%v reason=%v err=%v", page.pks(), page.last.GetNoNextReason(), page.err)
		}
		bytesRead := int64(0)
		for _, row := range page.rows {
			bytesRead += int64(row.KeySize + row.ValueSize)
		}
		if skipped.ScanState.RecordsScanned() != 3 || skipped.ScanState.BytesScanned() != bytesRead {
			t.Fatalf("skip charged %d records %d bytes, want 3 attempts and %d bytes", skipped.ScanState.RecordsScanned(), skipped.ScanState.BytesScanned(), bytesRead)
		}
		limited := DefaultExecuteProperties().WithReturnedRowLimit(3)
		page = scanRecordPage(store, limited, false, nil)
		if page.err != nil || len(page.rows) != 3 || page.last.GetNoNextReason() != ReturnLimitReached || limited.ScanState.RecordsScanned() != 4 {
			t.Fatalf("row limit rows=%v reason=%v attempts=%d err=%v", page.pks(), page.last.GetNoNextReason(), limited.ScanState.RecordsScanned(), page.err)
		}
		for _, hugeSkip := range []int{math.MaxInt32, 1 << 32} {
			for _, reverse := range []bool{false, true} {
				huge := limits(func(e *ExecuteProperties) { e.Skip = hugeSkip })
				page = scanRecordPage(store, huge, reverse, nil)
				if page.err != nil || len(page.rows) != 0 || page.last.GetNoNextReason() != SourceExhausted {
					t.Fatalf("huge skip=%d reverse=%v rows=%v reason=%v err=%v", hugeSkip, reverse, page.pks(), page.last.GetNoNextReason(), page.err)
				}
			}
		}
	})
}
