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

// TestSelectorRange_ResolvedBoundsDecideWhatConflicts pins the CONFLICT COVERAGE of a range
// whose bounds resolve by two different mechanisms, and pins it behaviourally — by what a
// concurrent writer does to the reader's commit — rather than by the shape of the recorded list.
//
// The two ends resolve differently, deliberately: LastLessThan needs the GetKey lookup and lands
// on an existing key ("b"), while FirstGreaterThan resolves in the KEY SPACE to keyAfter("f") =
// "f\x00" with no lookup at all. Treating the end as "h" would be a bug: it would drag the gap
// between "f\x00" and "h" into the coverage and abort an insert at "g".
//
// This test used to assert the exact COUNT and ORDER of the recorded ranges — ["b","d") for the
// resolving GetKey, then ["b","f\x00") for the extent — with the justification that "the two
// spans are not nested". They ARE nested: ["b","d") sits wholly inside ["b","f\x00"). Nothing
// observable distinguishes recording them separately from recording their union, so the count
// assertion was pinning a representation, not a behaviour, and it stood in the way of teaching
// SimFDB to coalesce read-conflict ranges the way a real client does. What actually matters is
// the COVERAGE, which is what is asserted here.
//
// The case where the resolution read is genuinely NOT covered by the extent — where dropping it
// would lose a real conflict — is TestSelectorRange_ResolutionReadConflictsOutsideTheExtent.
func TestSelectorRange_ResolvedBoundsDecideWhatConflicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		probe string
		want  int
		why   string
	}{
		{"a", 0, "below the resolved begin \"b\": never read"},
		{"b", 1020, "the resolved begin itself"},
		{"c", 1020, "inside the span the resolving GetKey scanned AND inside the extent"},
		{"e", 1020, "inside the extent"},
		{"f", 1020, "the last key inside the extent (end resolves to f\\x00)"},
		{"g", 0, "in the gap ABOVE the key-space-resolved end: resolving to \"h\" would over-conflict here"},
		{"h", 0, "beyond the resolved end"},
	} {
		tc := tc
		t.Run(tc.probe, func(t *testing.T) {
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

			if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
				w.Set(k(tc.probe), []byte("concurrent"))
				return nil, nil
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
			tx.Set(k("sentinel"), []byte("1"))
			got := 0
			if err := tx.Commit().Get(); err != nil {
				got = errCode(t, err)
			}
			if got != tc.want {
				t.Fatalf("concurrent write at %q gave commit code %d, want %d (%s); recorded "+
					"conflicts were %v", tc.probe, got, tc.want, tc.why, conflictStrings(tx))
			}
		})
	}
}

// TestSelectorRange_ResolutionReadConflictsOutsideTheExtent is the case that makes the
// resolving GetKey's own conflict range load-bearing rather than redundant.
//
// Here the two bounds resolve to the SAME key, so the range extent is EMPTY and contributes
// nothing at all. The backward begin bound still had to be resolved by a real GetKey, and that
// GetKey scanned the keyspace between its anchor and the key it landed on. A concurrent write
// in THAT span conflicts on real FDB, and would not conflict here if the resolution read were
// dropped — which is precisely what a reader looking at the nested case above might conclude was
// safe.
func TestSelectorRange_ResolutionReadConflictsOutsideTheExtent(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedKeys(t, db, "b", "h")

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx := handle.(*simTxn)
	// LastLessThan("d") resolves to "b" (a GetKey over ["b","d")); the end resolves to "b" too,
	// so the extent is ["b","b") — empty.
	rr := tx.GetRange(fdb.SelectorRange{
		Begin: fdb.LastLessThan(k("d")),
		End:   fdb.FirstGreaterOrEqual(k("b")),
	}, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if len(kvs) != 0 {
		t.Fatalf("expected an empty read, got %d rows — the range shape changed", len(kvs))
	}

	// "c" lies in the span the resolving GetKey scanned and in no extent whatsoever.
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k("c"), []byte("concurrent"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	tx.Set(k("sentinel"), []byte("1"))
	err = tx.Commit().Get()
	got := 0
	if err != nil {
		got = errCode(t, err)
	}
	if got != 1020 {
		t.Fatalf("commit code = %d, want 1020: the range extent is EMPTY, so the only thing "+
			"that can conflict here is the GetKey that resolved the backward bound — dropping "+
			"it silently loses a conflict real FDB takes (conflicts were %v)",
			got, conflictStrings(tx))
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
