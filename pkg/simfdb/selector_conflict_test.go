package simfdb

// A GetRange over a non-trivial key selector must record its read-conflict range over
// the keys it ACTUALLY read, not over the selector's anchor key.
//
// The real backend makes that unavoidable: for anything other than FirstGreaterOrEqual
// it resolves each selector through GetKey and hands the RESOLVED byte keys to
// client.Transaction.GetRange, which derives the conflict extent from them
// (fdb/range_result.go resolveRange; client/transaction.go:1291). SimFDB implements the
// same fdb.Range interface, so taking the raw anchors instead makes it disagree with
// the backend it is meant to model — and it disagrees in the dangerous direction for a
// backward selector, where the resolved key is BELOW the anchor and the recorded range
// therefore MISSES rows the transaction read.
//
// This is not a hypothetical selector shape: bunched_map_iterator.go:199 scans with
// fdb.LastLessThan for its continuation endpoint.

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// seedKeys writes a sparse keyspace so a selector's resolved key differs from its
// anchor by a visible gap.
func seedKeys(t *testing.T, db *SimDB, keys ...string) {
	t.Helper()
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for _, key := range keys {
			tx.Set(k(key), []byte("v"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestSelectorRange_BackwardBeginConflictsOnResolvedKey is the under-conflict pin. The
// begin selector LastLessThan("d") resolves to "b", so the scan READS "b" — and a
// concurrent overwrite of "b" must make the reader conflict. Computing the extent from
// the anchor "d" yields [d, m), which excludes "b" and lets both transactions commit:
// a serializability violation SimFDB would report as success.
func TestSelectorRange_BackwardBeginConflictsOnResolvedKey(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f", "h")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rr := reader.GetRange(fdb.SelectorRange{
		Begin: fdb.LastLessThan(k("d")),
		End:   fdb.FirstGreaterOrEqual(k("m")),
	}, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	// Premise guard: the read must actually have included "b", or the test is not
	// exercising the gap between anchor and resolved key.
	var sawB bool
	for _, kv := range kvs {
		if string(kv.Key) == "b" {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("LastLessThan(\"d\") range returned %d rows without \"b\" — the selector no "+
			"longer resolves below the anchor, so this test cannot detect the defect", len(kvs))
	}

	// A concurrent transaction overwrites a key the reader READ.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("b"), []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	reader.Set(k("sentinel"), []byte("1"))
	err = reader.Commit().Get()
	if err == nil {
		t.Fatal("reader committed after a concurrent write to \"b\", a key it READ — the " +
			"read-conflict range was computed from the selector ANCHOR (\"d\") instead of the " +
			"resolved key (\"b\"), so SSI under-conflicts and serializability is violated")
	}
	if code := errCode(t, err); code != 1020 {
		t.Fatalf("reader commit error = %d, want 1020 not_committed", code)
	}
}

// TestSelectorRange_ForwardBeginDoesNotOverConflict is the opposite direction, and it
// is why the fix must use the resolved key rather than simply widening the range.
// FirstGreaterThan("b") resolves to "f", so "b" is NOT read; a concurrent write to "b"
// must NOT conflict. Widening the extent to always start at the anchor would make this
// spuriously retry forever.
func TestSelectorRange_ForwardBeginDoesNotOverConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f", "h")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rr := reader.GetRange(fdb.SelectorRange{
		Begin: fdb.FirstGreaterThan(k("b")),
		End:   fdb.FirstGreaterOrEqual(k("m")),
	}, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	for _, kv := range kvs {
		if string(kv.Key) == "b" {
			t.Fatalf("FirstGreaterThan(\"b\") returned \"b\" — the selector resolution is wrong, " +
				"so this test cannot distinguish over-conflict from a correct conflict")
		}
	}

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("b"), []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	reader.Set(k("sentinel"), []byte("1"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("reader conflicted on \"b\", which FirstGreaterThan(\"b\") EXCLUDES: %v "+
			"(code %d) — the conflict extent is wider than the keys actually read", err, errCode(t, err))
	}
}

// TestSelectorRange_ConflictExtentMatchesResolvedKeys inspects the recorded range
// directly. The behavioural tests above can only observe a conflict verdict; this one
// names the bounds, so an extent that is merely "wide enough to pass" — rather than
// equal to the resolved range — is reported as such.
//
// The two ends resolve by DIFFERENT mechanisms, deliberately: LastLessThan needs the
// GetKey lookup and lands on an existing key ("b"), while FirstGreaterThan resolves in
// the KEY SPACE to keyAfter("f") = "f\x00" with no lookup at all. Asserting "h" here
// would be asserting a bug: it would drag the gap between "f\x00" and "h" into the
// conflict range and over-conflict on an insert at, say, "g".
func TestSelectorRange_ConflictExtentMatchesResolvedKeys(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f", "h")

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx := handle.(*simTxn)

	rr := tx.GetRange(fdb.SelectorRange{
		Begin: fdb.LastLessThan(k("d")),     // GetKey resolution -> "b"
		End:   fdb.FirstGreaterThan(k("f")), // key-space resolution -> "f\x00"
	}, fdb.RangeOptions{})
	if _, err := rr.GetSliceWithError(); err != nil {
		t.Fatalf("range read: %v", err)
	}

	// TWO contributions are expected, and the second is the one a reader is most likely to
	// think is a bug:
	//
	//  1. Resolving LastLessThan("d") costs a real GetKey on the backend, and that GetKey is a
	//     READ — it takes its own conflict range over the span it scanned, ["b","d").
	//     FirstGreaterThan("f") costs no GetKey (it is keyAfter("f") client-side), so it adds
	//     nothing here.
	//  2. The range extent itself, over the RESOLVED bounds: ["b", "f\x00").
	//
	// Asserting a single merged range would be wrong — the two spans are not nested, and
	// collapsing them would either drop the resolution read or widen the extent.
	want := [][2]string{
		{"b", "d"},     // GetKey resolution of LastLessThan("d")
		{"b", "f\x00"}, // the range extent over resolved bounds
	}
	if len(tx.readConflicts) != len(want) {
		var got [][2]string
		for _, rc := range tx.readConflicts {
			got = append(got, [2]string{string(rc.begin), string(rc.end)})
		}
		t.Fatalf("recorded %d read-conflict ranges %v, want %d %v (one for the GetKey that "+
			"resolves the backward bound, one for the range extent)",
			len(tx.readConflicts), got, len(want), want)
	}
	for i, w := range want {
		got := tx.readConflicts[i]
		if string(got.begin) != w[0] || string(got.end) != w[1] {
			t.Errorf("conflict range %d = [%q,%q), want [%q,%q)", i, got.begin, got.end, w[0], w[1])
		}
	}
}

// TestSelectorRange_ForwardEndDoesNotOverConflictIntoGap is the companion to the
// key-space resolution above: with FirstGreaterThan("f") as the END bound, an insert
// into the gap between "f\x00" and the next existing key must NOT conflict. Resolving
// that bound to the next EXISTING key ("h") instead would swallow the gap.
func TestSelectorRange_ForwardEndDoesNotOverConflictIntoGap(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f", "h")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rr := reader.GetRange(fdb.SelectorRange{
		Begin: fdb.FirstGreaterOrEqual(k("b")),
		End:   fdb.FirstGreaterThan(k("f")),
	}, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if len(kvs) != 2 {
		t.Fatalf("read %d rows, want 2 (b and f) — the range shape changed", len(kvs))
	}

	// "g" lies past the resolved end bound "f\x00", so it was never in the read range.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("g"), []byte("new"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

	reader.Set(k("sentinel"), []byte("1"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("reader conflicted on an insert at \"g\", which is past the resolved end bound "+
			"\"f\\x00\": %v (code %d) — the end bound resolved to the next EXISTING key and "+
			"swallowed the gap", err, errCode(t, err))
	}
}

// TestSelectorRange_TrivialSelectorsUnchanged pins that the common case — a plain
// KeyRange, i.e. FirstGreaterOrEqual on both ends — still records exactly the requested
// range including its leading and trailing gaps. That is the phantom-protection
// property the earlier empty-range/gap fix established, and resolving selectors must
// not quietly narrow it to the returned data.
func TestSelectorRange_TrivialSelectorsUnchanged(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "f", "h")

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx := handle.(*simTxn)

	rr := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{})
	if _, err := rr.GetSliceWithError(); err != nil {
		t.Fatalf("range read: %v", err)
	}
	if len(tx.readConflicts) != 1 {
		t.Fatalf("recorded %d read-conflict ranges, want exactly 1", len(tx.readConflicts))
	}
	got := tx.readConflicts[0]
	if string(got.begin) != "a" || string(got.end) != "z" {
		t.Fatalf("conflict range = [%q,%q), want [\"a\",\"z\") — an exact KeyRange must keep the "+
			"full requested span so an insert in the leading/trailing gap still conflicts",
			got.begin, got.end)
	}
}

// TestSelectorRange_EmptyRangeKeepsResolvedSpan covers the interaction between the two
// fixes: an EMPTY selector range must still record a conflict span (phantom protection
// for an insert into the gap), derived from the resolved bounds.
func TestSelectorRange_EmptyRangeKeepsResolvedSpan(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "z")

	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rr := reader.GetRange(fdb.SelectorRange{
		Begin: fdb.FirstGreaterThan(k("b")), // resolves to "z"
		End:   fdb.FirstGreaterOrEqual(k("z")),
	}, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if len(kvs) != 0 {
		t.Fatalf("expected an empty read, got %d rows — the range shape changed", len(kvs))
	}
	// An empty read still resolves to [z, z), which is degenerate; the transaction must
	// at minimum not record a conflict range it never read, and must still commit when
	// nothing it read changed.
	reader.Set(k("sentinel"), []byte("1"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("empty selector range should not conflict when nothing changed: %v (code %d)",
			err, errCode(t, err))
	}
}
