package simfdb

// A range read that MEETS its row limit must report "more", and the read-conflict extent must
// therefore narrow to the data actually returned.
//
// The rule comes from the storage server, not from bookkeeping convenience: C++ getExactRange
// sets `output.more = (data.size() == limit)` whenever the limit is reached, unconditionally,
// because a server that stopped at the limit has not looked at the next key and cannot know
// whether one exists (ported at client/readpath.go:745-751). SimFDB materializes the whole
// result set and therefore CAN know — and using that knowledge is the divergence: reporting
// more=false at exactly-the-limit keeps the conflict extent at the full requested range
// instead of clamping it to the returned rows, so SimFDB aborts transactions real FDB commits.
//
// Found by the live differential (SimFDB vs real FDB) once its selector-range arm existed;
// these are the narrow, Docker-free pins for the same behaviour.

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestRangeLimit_ExactlyAtLimitReportsMore is the defect pin. The range holds EXACTLY as many
// keys as the limit, which is the only case that distinguishes `len(kvs) > limit` from
// `len(kvs) >= limit`: with strictly more rows available both spellings report more, and with
// fewer neither does.
func TestRangeLimit_ExactlyAtLimitReportsMore(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Exactly 2 keys inside ["a","g"), plus one OUTSIDE it that the concurrent writer touches.
	seedKeys(t, db, "b", "f", "m")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx := reader.(*simTxn)

	kvs := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("g")},
		fdb.RangeOptions{Limit: 2}).GetSliceOrPanic()
	if len(kvs) != 2 {
		t.Fatalf("read %d rows, want exactly 2 (the range must hold exactly the limit for this "+
			"test to distinguish > from >=)", len(kvs))
	}

	if len(tx.readConflicts) != 1 {
		t.Fatalf("recorded %d read-conflict ranges, want 1 (an exact KeyRange needs no GetKey "+
			"resolution, so the extent is the only contribution)", len(tx.readConflicts))
	}
	got := tx.readConflicts[0]
	wantBegin, wantEnd := "a", "f\x00"
	if string(got.begin) != wantBegin || string(got.end) != wantEnd {
		t.Fatalf("conflict range = [%q,%q), want [%q,%q). Meeting the limit means more=true, "+
			"which clamps the extent to keyAfter(last returned key); the full requested range "+
			"[\"a\",\"g\") means more was reported false at exactly-the-limit",
			got.begin, got.end, wantBegin, wantEnd)
	}
}

// TestRangeLimit_ExactlyAtLimitDoesNotOverConflict is the same defect stated as an outcome: a
// concurrent insert BEYOND the last returned row must not abort a limit-satisfied reader. This
// is the shape the live differential caught (SimFDB aborted, real FDB committed).
func TestRangeLimit_ExactlyAtLimitDoesNotOverConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kvs := reader.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Limit: 2}).GetSliceOrPanic()
	if len(kvs) != 2 {
		t.Fatalf("read %d rows, want 2", len(kvs))
	}

	// "g" sits past the last returned key ("f"), inside the REQUESTED range but outside the
	// data the reader actually saw. A limit-stopped read has no claim on it.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("g"), []byte("new"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

	reader.Set(k("sentinel"), []byte("1"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("reader aborted (%v, code %d) on an insert at \"g\", which is past the last row "+
			"its limit-stopped read returned. Real FDB commits this: meeting the limit reports "+
			"more, which clamps the conflict extent to the returned data", err, errCode(t, err))
	}
}

// TestRangeLimit_UnderLimitKeepsFullRange is the guard that stops the fix from becoming
// "always clamp". A read that did NOT meet its limit is exhausted — it really did observe the
// whole requested range, gaps included — so its extent must stay the full range and a
// concurrent insert into a gap MUST conflict. That is the phantom protection the earlier
// empty-range work established, and `>=` must not erode it.
func TestRangeLimit_UnderLimitKeepsFullRange(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Limit 5 over 2 rows: the scan is exhausted, not limit-stopped.
	kvs := reader.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Limit: 5}).GetSliceOrPanic()
	if len(kvs) != 2 {
		t.Fatalf("read %d rows, want 2 (fewer than the limit, so the read is exhausted)", len(kvs))
	}

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("g"), []byte("phantom"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

	reader.Set(k("sentinel"), []byte("1"))
	err = reader.Commit().Get()
	if err == nil {
		t.Fatal("reader committed after a phantom insert at \"g\" inside a range it read to " +
			"EXHAUSTION — an exhausted read observed the whole range including its gaps, so its " +
			"conflict extent must stay the full requested range")
	}
	if code := errCode(t, err); code != 1020 {
		t.Fatalf("reader commit error = %d, want 1020 not_committed", code)
	}
}

// TestRangeLimit_NoLimitKeepsFullRange pins the unlimited case explicitly: with Limit 0 the
// clamp must never engage, whatever the row count.
func TestRangeLimit_NoLimitKeepsFullRange(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx := reader.(*simTxn)
	tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()

	if len(tx.readConflicts) != 1 {
		t.Fatalf("recorded %d read-conflict ranges, want 1", len(tx.readConflicts))
	}
	got := tx.readConflicts[0]
	if string(got.begin) != "a" || string(got.end) != "z" {
		t.Fatalf("conflict range = [%q,%q), want the full [\"a\",\"z\") — an unlimited read is "+
			"always exhausted, so nothing may clamp it", got.begin, got.end)
	}
}
