package fdb_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// TestReadYourOwnWrite_Get demonstrates that the pure Go client does NOT
// support read-your-writes: a Get within the same transaction that did a
// Set returns nil instead of the written value.
//
// The C binding's RYW cache intercepts Get and returns the pending write.
// Our client sends the read to the storage server, which only sees committed data.
//
// This is the root cause of 770/2307 record layer test failures.
// Tracked in: https://fdb.dev/issues/19
func TestReadYourOwnWrite_Get(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		key := fdb.Key("ryw-test-get")
		tr.Set(key, []byte("written-in-same-tx"))

		val := tr.Get(key).MustGet()
		if val == nil {
			t.Error("RYW broken: Get after Set in same tx returned nil (server doesn't see uncommitted writes)")
			return nil, nil
		}
		if string(val) != "written-in-same-tx" {
			t.Errorf("RYW broken: got %q, want %q", val, "written-in-same-tx")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_GetRange demonstrates that GetRange within the same
// transaction does not see pending Set operations.
func TestReadYourOwnWrite_GetRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-range-a"), []byte("1"))
		tr.Set(fdb.Key("ryw-range-b"), []byte("2"))
		tr.Set(fdb.Key("ryw-range-c"), []byte("3"))

		rr := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key("ryw-range-a"), End: fdb.Key("ryw-range-d")},
			fdb.RangeOptions{},
		)
		kvs, err := rr.GetSliceWithError()
		if err != nil {
			t.Fatalf("GetRange: %v", err)
		}
		if len(kvs) == 0 {
			t.Error("RYW broken: GetRange after 3 Sets in same tx returned 0 results")
			return nil, nil
		}
		if len(kvs) != 3 {
			t.Errorf("RYW broken: GetRange returned %d results, want 3", len(kvs))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_Clear demonstrates that Clear within the same
// transaction is not visible to subsequent Get.
func TestReadYourOwnWrite_Clear(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// First: write and commit a key.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-clear-key"), []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Second: clear the key and read it in the same tx.
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Clear(fdb.Key("ryw-clear-key"))

		val := tr.Get(fdb.Key("ryw-clear-key")).MustGet()
		if val != nil {
			t.Error("RYW broken: Get after Clear in same tx still returns the old value")
			return nil, nil
		}
		// With RYW, val should be nil (cleared). Without RYW, val is "exists" (stale).
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_AtomicAdd demonstrates that atomic Add is not visible
// to Get within the same transaction.
func TestReadYourOwnWrite_AtomicAdd(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Seed: set counter to 10 (little-endian int64).
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-counter"), []byte{10, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Add 5 and read in same tx.
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Add(fdb.Key("ryw-counter"), []byte{5, 0, 0, 0, 0, 0, 0, 0})

		val := tr.Get(fdb.Key("ryw-counter")).MustGet()
		if val == nil {
			t.Fatal("Get returned nil")
		}
		// With RYW: C binding resolves Add locally → returns 15.
		// Without RYW: server returns committed value 10 (Add not yet committed).
		got := int64(val[0]) | int64(val[1])<<8 | int64(val[2])<<16 | int64(val[3])<<24 |
			int64(val[4])<<32 | int64(val[5])<<40 | int64(val[6])<<48 | int64(val[7])<<56
		if got == 10 {
			t.Error("RYW broken: atomic Add(5) not visible in same tx — got 10 (stale), want 15")
		} else if got != 15 {
			t.Errorf("unexpected counter value: got %d, want 15", got)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_SetClearGetRange tests the pattern used by PermutedMinMax:
// Set 3 keys, Clear 1, then GetRange should only return the 2 remaining.
func TestReadYourOwnWrite_SetClearGetRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-scgr-a"), []byte("1"))
		tr.Set(fdb.Key("ryw-scgr-b"), []byte("2"))
		tr.Set(fdb.Key("ryw-scgr-c"), []byte("3"))

		tr.Clear(fdb.Key("ryw-scgr-b"))

		rr := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key("ryw-scgr-a"), End: fdb.Key("ryw-scgr-d")},
			fdb.RangeOptions{},
		)
		kvs, err := rr.GetSliceWithError()
		if err != nil {
			t.Fatalf("GetRange: %v", err)
		}
		if len(kvs) != 2 {
			t.Errorf("Set 3, Clear 1, GetRange returned %d results, want 2", len(kvs))
			for i, kv := range kvs {
				t.Logf("  [%d] key=%q val=%q", i, kv.Key, kv.Value)
			}
			return nil, nil
		}
		if string(kvs[0].Key) != "ryw-scgr-a" || string(kvs[1].Key) != "ryw-scgr-c" {
			t.Errorf("wrong keys: got %q and %q", kvs[0].Key, kvs[1].Key)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_SetClearIterator tests that the RangeIterator correctly
// filters cleared entries across multiple batches.
func TestReadYourOwnWrite_SetClearIterator(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-iter-a"), []byte("1"))
		tr.Set(fdb.Key("ryw-iter-b"), []byte("2"))
		tr.Set(fdb.Key("ryw-iter-c"), []byte("3"))

		tr.Clear(fdb.Key("ryw-iter-b"))

		rr := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key("ryw-iter-a"), End: fdb.Key("ryw-iter-d")},
			fdb.RangeOptions{Mode: fdb.StreamingModeSmall},
		)
		iter := rr.Iterator()
		var keys []string
		for iter.Advance() {
			kv := iter.MustGet()
			keys = append(keys, string(kv.Key))
		}
		if len(keys) != 2 {
			t.Errorf("iterator: got %d keys, want 2: %v", len(keys), keys)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_PermutedMinMaxPattern reproduces the exact PermutedMinMax
// test failure: save 3 records (via Set), delete the highest (via Clear),
// reverse scan with limit=1 to find the new max.
// Uses tuple-packed keys to match the real index format.
func TestReadYourOwnWrite_PermutedMinMaxPattern(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		prefix := fdb.Key("ryw-pmm-")

		// Simulate 3 VALUE index entries: [group=100, value=1], [group=100, value=5], [group=100, value=10]
		key1 := append(append([]byte(nil), prefix...), []byte{1}...)
		key5 := append(append([]byte(nil), prefix...), []byte{5}...)
		key10 := append(append([]byte(nil), prefix...), []byte{10}...)

		// INSERT all 3
		tr.Set(fdb.Key(key1), []byte{})
		tr.Set(fdb.Key(key5), []byte{})
		tr.Set(fdb.Key(key10), []byte{})

		// Verify max = key10 (reverse scan limit=1)
		rr := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(append(append([]byte(nil), prefix...), 0xFF))},
			fdb.RangeOptions{Reverse: true, Limit: 1},
		)
		kvs, err := rr.GetSliceWithError()
		if err != nil {
			t.Fatalf("first scan: %v", err)
		}
		if len(kvs) != 1 || kvs[0].Key[len(prefix)] != 10 {
			t.Fatalf("first scan: expected key10, got %v", kvs)
		}

		// DELETE key10 (standard index clear)
		tr.Clear(fdb.Key(key10))

		// Also Get to check (like permuted maintainer does)
		val := tr.Get(fdb.Key(key10)).MustGet()
		if val != nil {
			t.Errorf("Get after Clear should return nil, got %v", val)
		}

		// Reverse scan limit=1 should now return key5
		rr2 := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(append(append([]byte(nil), prefix...), 0xFF))},
			fdb.RangeOptions{Reverse: true, Limit: 1},
		)
		kvs2, err := rr2.GetSliceWithError()
		if err != nil {
			t.Fatalf("second scan: %v", err)
		}
		if len(kvs2) != 1 {
			t.Errorf("second scan: expected 1 result, got %d", len(kvs2))
			return nil, nil
		}
		if kvs2[0].Key[len(prefix)] != 5 {
			t.Errorf("second scan: expected key5, got key=%v (last byte=%d)", kvs2[0].Key, kvs2[0].Key[len(prefix)])
		}

		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_SetClearGetRangeReverse tests reverse scan after
// Set + Clear — the PermutedMinMax getExtremum pattern.
func TestReadYourOwnWrite_SetClearGetRangeReverse(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-rev-a"), []byte("1"))
		tr.Set(fdb.Key("ryw-rev-b"), []byte("2"))
		tr.Set(fdb.Key("ryw-rev-c"), []byte("3"))

		tr.Clear(fdb.Key("ryw-rev-c"))

		rr := tr.GetRange(
			fdb.KeyRange{Begin: fdb.Key("ryw-rev-a"), End: fdb.Key("ryw-rev-d")},
			fdb.RangeOptions{Reverse: true, Limit: 1},
		)
		kvs, err := rr.GetSliceWithError()
		if err != nil {
			t.Fatalf("GetRange reverse: %v", err)
		}
		if len(kvs) != 1 {
			t.Errorf("expected 1 result, got %d", len(kvs))
			return nil, nil
		}
		if string(kvs[0].Key) != "ryw-rev-b" {
			t.Errorf("reverse scan after clear: got key=%q, want ryw-rev-b", kvs[0].Key)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_AtomicAddThenGet verifies that atomic Add is visible
// to a subsequent Get in the same transaction via the RYW cache.
func TestReadYourOwnWrite_AtomicAddThenGet(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Seed counter = 100.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		buf := make([]byte, 8)
		buf[0] = 100
		tr.Set(fdb.Key("ryw-atomic-add"), buf)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Add 7, then read in same tx.
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		param := make([]byte, 8)
		param[0] = 7
		tr.Add(fdb.Key("ryw-atomic-add"), param)

		val := tr.Get(fdb.Key("ryw-atomic-add")).MustGet()
		if val == nil {
			t.Fatal("Get returned nil after Add")
		}
		got := int(val[0])
		if got != 107 {
			t.Errorf("expected 107 (100+7), got %d", got)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_SetThenAtomicAddThenGet verifies that Set followed by
// atomic Add is correctly resolved by the RYW cache (no server round-trip needed).
func TestReadYourOwnWrite_SetThenAtomicAddThenGet(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		buf := make([]byte, 8)
		buf[0] = 50
		tr.Set(fdb.Key("ryw-set-add"), buf)

		param := make([]byte, 8)
		param[0] = 25
		tr.Add(fdb.Key("ryw-set-add"), param)

		val := tr.Get(fdb.Key("ryw-set-add")).MustGet()
		if val == nil {
			t.Fatal("Get returned nil")
		}
		if int(val[0]) != 75 {
			t.Errorf("expected 75 (50+25), got %d", val[0])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_MultipleAtomicsThenGet verifies that multiple atomic
// operations on the same key accumulate correctly in the RYW cache.
func TestReadYourOwnWrite_MultipleAtomicsThenGet(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		buf := make([]byte, 8)
		buf[0] = 10
		tr.Set(fdb.Key("ryw-multi-atomic"), buf)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		one := make([]byte, 8)
		one[0] = 1
		tr.Add(fdb.Key("ryw-multi-atomic"), one)
		tr.Add(fdb.Key("ryw-multi-atomic"), one)
		tr.Add(fdb.Key("ryw-multi-atomic"), one)

		val := tr.Get(fdb.Key("ryw-multi-atomic")).MustGet()
		if val == nil {
			t.Fatal("Get returned nil")
		}
		if int(val[0]) != 13 {
			t.Errorf("expected 13 (10+1+1+1), got %d", val[0])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestReadYourOwnWrite_CompareAndClearThenGet verifies CompareAndClear
// atomic is visible via RYW: if current value matches, key becomes absent.
func TestReadYourOwnWrite_CompareAndClearThenGet(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("ryw-cac"), []byte("match"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.CompareAndClear(fdb.Key("ryw-cac"), []byte("match"))

		val := tr.Get(fdb.Key("ryw-cac")).MustGet()
		if val != nil {
			t.Errorf("expected nil after CompareAndClear, got %q", val)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}
