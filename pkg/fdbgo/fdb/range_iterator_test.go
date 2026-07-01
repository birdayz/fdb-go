package fdb_test

// Adversarial tests for the RangeIterator — the lazy-batch range scan
// mechanism in the fdb package. Tests paging, reverse iteration, streaming
// modes, and edge cases around empty ranges and single-element results.

import (
	"errors"
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TestRangeIterator_RowLimitUnlimitedAndInvalid pins the Iterator row-limit fix. Limit=-1
// (ROW_LIMIT_UNLIMITED) must return ALL rows — like GetSliceWithError and libfdb_c — not an empty
// result; Limit<=-2 must surface range_limits_invalid (2012), matching the GetSlice path. Pre-fix
// the Iterator bailed on a non-positive row budget → 0 rows + nil for both, a silent wrong answer
// that also contradicted GetSliceWithError on the same input.
func TestRangeIterator_RowLimitUnlimitedAndInvalid(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	pfx := "iter_rowlimit_"
	const n = 10
	if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < n; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%02d", pfx, i)), []byte("v"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	kr := gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")}

	if _, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		// Limit=-1 → unlimited: Iterator and GetSliceWithError must AGREE (all n rows).
		slice, serr := rtr.GetRange(kr, gofdb.RangeOptions{Limit: -1}).GetSliceWithError()
		if serr != nil {
			return nil, serr
		}
		if len(slice) != n {
			t.Fatalf("GetSlice(Limit:-1): got %d rows, want %d", len(slice), n)
		}
		iter := rtr.GetRange(kr, gofdb.RangeOptions{Limit: -1}).Iterator()
		cnt := 0
		for iter.Advance() {
			iter.MustGet()
			cnt++
		}
		if _, e := iter.Get(); e != nil {
			return nil, e
		}
		if cnt != n {
			t.Fatalf("Iterator(Limit:-1) must return all %d rows (unlimited), got %d — diverges from GetSlice", n, cnt)
		}

		// Limit=-2 → range_limits_invalid (2012), matching GetSlice / libfdb_c.
		it2 := rtr.GetRange(kr, gofdb.RangeOptions{Limit: -2}).Iterator()
		it2.Advance()
		_, e2 := it2.Get()
		var fe gofdb.Error
		if !errors.As(e2, &fe) || fe.Code != 2012 {
			t.Fatalf("Iterator(Limit:-2) must surface range_limits_invalid (2012), got %v", e2)
		}

		// StreamingModeExact with NO limit → exact_mode_without_limits (2210), matching libfdb_c.
		it3 := rtr.GetRange(kr, gofdb.RangeOptions{Mode: gofdb.StreamingModeExact}).Iterator()
		it3.Advance()
		_, e3 := it3.Get()
		var fe3 gofdb.Error
		if !errors.As(e3, &fe3) || fe3.Code != 2210 {
			t.Fatalf("Iterator(StreamingModeExact, no limit) must surface exact_mode_without_limits (2210), got %v", e3)
		}
		// EXACT *with* a limit is fine (positive control).
		it4 := rtr.GetRange(kr, gofdb.RangeOptions{Mode: gofdb.StreamingModeExact, Limit: n}).Iterator()
		c4 := 0
		for it4.Advance() {
			it4.MustGet()
			c4++
		}
		if _, e := it4.Get(); e != nil {
			return nil, e
		}
		if c4 != n {
			t.Fatalf("Iterator(Exact, Limit:%d) must return %d rows, got %d", n, n, c4)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestRangeIterator_ReversePaging verifies that iterating in reverse with a
// small batch size correctly pages through all results. This exercises the
// iterator's boundary advancement for reverse scans.
func TestRangeIterator_ReversePaging(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := "iter_rev_page_"

	// Write 20 keys.
	_, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 20; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%02d", pfx, i)), []byte(fmt.Sprintf("val%02d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Iterate in reverse with ITERATOR streaming mode (small batches).
	result, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		rr := rtr.GetRange(
			gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")},
			gofdb.RangeOptions{Reverse: true, Mode: gofdb.StreamingModeIterator},
		)
		iter := rr.Iterator()
		var keys []string
		for iter.Advance() {
			kv := iter.MustGet()
			keys = append(keys, string(kv.Key))
		}
		if _, err := iter.Get(); err != nil {
			return nil, err
		}
		return keys, nil
	})
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}

	keys := result.([]string)
	if len(keys) != 20 {
		t.Fatalf("expected 20 keys, got %d", len(keys))
	}

	// Verify descending order.
	for i := 0; i < 20; i++ {
		expected := fmt.Sprintf("%s%02d", pfx, 19-i)
		if keys[i] != expected {
			t.Errorf("keys[%d]: got %q, want %q", i, keys[i], expected)
		}
	}

	// Clean up.
	db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")})
		return nil, nil
	})
}

// TestRangeIterator_ForwardPaging verifies forward iteration with SMALL mode.
func TestRangeIterator_ForwardPaging(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := "iter_fwd_page_"

	_, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 50; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%02d", pfx, i)), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		rr := rtr.GetRange(
			gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")},
			gofdb.RangeOptions{Mode: gofdb.StreamingModeSmall}, // batch size 10
		)
		iter := rr.Iterator()
		count := 0
		for iter.Advance() {
			iter.MustGet()
			count++
		}
		if _, err := iter.Get(); err != nil {
			return nil, err
		}
		return count, nil
	})
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if result.(int) != 50 {
		t.Fatalf("expected 50, got %d", result)
	}

	db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")})
		return nil, nil
	})
}

// TestRangeIterator_WithLimit verifies that Limit is respected across batches.
func TestRangeIterator_WithLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := "iter_limit_"

	_, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 30; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%02d", pfx, i)), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Request limit=7 with SMALL mode (batch=10). Should stop at 7.
	result, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		rr := rtr.GetRange(
			gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")},
			gofdb.RangeOptions{Limit: 7, Mode: gofdb.StreamingModeSmall},
		)
		iter := rr.Iterator()
		count := 0
		for iter.Advance() {
			iter.MustGet()
			count++
		}
		return count, nil
	})
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if result.(int) != 7 {
		t.Fatalf("expected 7, got %d", result)
	}

	db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "00"), End: gofdb.Key(pfx + "99")})
		return nil, nil
	})
}

// TestRangeIterator_SnapshotRead verifies that snapshot iteration doesn't
// add read conflicts (important for performance-sensitive scans).
func TestRangeIterator_SnapshotRead(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := "iter_snap_"

	_, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 5; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%d", pfx, i)), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Use Snapshot().GetRange with WANT_ALL mode.
	result, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		snap := rtr.Snapshot()
		rr := snap.GetRange(
			gofdb.KeyRange{Begin: gofdb.Key(pfx + "0"), End: gofdb.Key(pfx + "9")},
			gofdb.RangeOptions{Mode: gofdb.StreamingModeWantAll},
		)
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("snapshot range: %v", err)
	}
	kvs := result.([]gofdb.KeyValue)
	if len(kvs) != 5 {
		t.Fatalf("expected 5 keys, got %d", len(kvs))
	}

	db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "0"), End: gofdb.Key(pfx + "9")})
		return nil, nil
	})
}

// TestRangeIterator_TupleRange verifies iteration over a tuple-based range.
func TestRangeIterator_TupleRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := "iter_tuple_"

	_, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := int64(0); i < 10; i++ {
			key := gofdb.Key(tuple.Tuple{pfx, i}.Pack())
			tr.Set(key, []byte(fmt.Sprintf("val%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Use tuple range [pfx,5 .. pfx,8).
	result, err := db.ReadTransact(func(rtr gofdb.ReadTransaction) (any, error) {
		begin := tuple.Tuple{pfx, int64(5)}.Pack()
		end := tuple.Tuple{pfx, int64(8)}.Pack()
		rr := rtr.GetRange(
			gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)},
			gofdb.RangeOptions{},
		)
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("tuple range: %v", err)
	}
	kvs := result.([]gofdb.KeyValue)
	if len(kvs) != 3 { // 5, 6, 7
		t.Fatalf("expected 3 keys (5,6,7), got %d", len(kvs))
	}

	db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		begin := tuple.Tuple{pfx}.Pack()
		end := append(begin, 0xFF)
		tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)})
		return nil, nil
	})
}
