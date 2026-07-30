package simfdb_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestSimFDB_ContinuationUnderConcurrentModification is RFC-199 Tier 2's continuation-under-
// fault replay. A scan mints a continuation on page 1; a concurrent transaction then modifies
// UN-scanned data (deletes one record, inserts another) and commits — bumping the version; the
// scan resumes from the continuation bytes in a FRESH transaction (new read version) and must
// yield the rest with no duplicate or lost record, correctly reflecting the modification. This
// is the version/RYW path-dependence a continuation resume exposes: a token minted at version V,
// resumed after the data changed underneath it. Over SimFDB it is fully deterministic — the reproducible
// replay no real-FDB harness can give (real FDB's version/timing would vary run to run).
func TestSimFDB_ContinuationUnderConcurrentModification(t *testing.T) {
	t.Parallel()
	db, sub := newSimDatabase(1)
	md := buildOrderMetadata(t)
	ctx := context.Background()

	// Seed pk 0..19.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		for i := 0; i < 20; i++ {
			if _, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(int64(i)),
				Price:   proto.Int32(int32(i)),
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	scanPage := func(cont []byte) (pks []int64, next []byte) {
		if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx, md, sub)
			if err != nil {
				return nil, err
			}
			cur := store.ScanRecords(cont, recordlayer.ScanProperties{
				ExecuteProperties:   recordlayer.ExecuteProperties{ReturnedRowLimit: 5},
				CursorStreamingMode: recordlayer.StreamingModeWantAll,
			})
			recs, c, err := recordlayer.AsListWithContinuation(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, r := range recs {
				pks = append(pks, r.Record.(*gen.Order).GetOrderId())
			}
			next = c
			return nil, nil
		}); err != nil {
			t.Fatalf("scan page: %v", err)
		}
		return pks, next
	}

	// Page 1: the first 5, capturing the continuation.
	page1, cont := scanPage(nil)
	if len(page1) != 5 || page1[0] != 0 || page1[4] != 4 {
		t.Fatalf("page1 = %v, want [0 1 2 3 4]", page1)
	}
	if len(cont) == 0 {
		t.Fatal("no continuation after page 1")
	}

	// FAULT: a concurrent transaction modifies UN-scanned data — delete pk=10, insert pk=100 —
	// and commits, bumping the version before the scan resumes.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		if _, err := store.DeleteRecord(tuple.Tuple{int64(10)}); err != nil {
			return nil, err
		}
		if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(100), Price: proto.Int32(100)}); err != nil {
			return nil, err
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent modification: %v", err)
	}

	// RESUME from the continuation in fresh transactions until exhausted.
	all := append([]int64(nil), page1...)
	for len(cont) > 0 {
		pks, next := scanPage(cont)
		all = append(all, pks...)
		cont = next
	}

	// Expected: 0..9, 11..19 (pk 10 deleted), then 100 (inserted after the scanned range) — 20
	// records, ascending, no dup/loss of the untouched records; the modification to un-scanned
	// keys is correctly reflected.
	var want []int64
	for i := int64(0); i < 20; i++ {
		if i != 10 {
			want = append(want, i)
		}
	}
	want = append(want, 100)
	if len(all) != len(want) {
		t.Fatalf("resumed scan = %v (%d rows), want %v (%d)", all, len(all), want, len(want))
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("at index %d: got %d, want %d (full: %v)", i, all[i], want[i], all)
		}
	}
}
