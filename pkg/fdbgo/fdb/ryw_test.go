package fdb_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestReadYourOwnWrite_Get demonstrates that the pure Go client does NOT
// support read-your-writes: a Get within the same transaction that did a
// Set returns nil instead of the written value.
//
// The C binding's RYW cache intercepts Get and returns the pending write.
// Our client sends the read to the storage server, which only sees committed data.
//
// This is the root cause of 770/2307 record layer test failures.
// Tracked in: https://github.com/birdayz/fdb-record-layer-go/issues/19
func TestReadYourOwnWrite_Get(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
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

	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
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
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("ryw-clear-key"), []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Second: clear the key and read it in the same tx.
	_, err = db.Transact(func(tr fdb.Transaction) (any, error) {
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
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("ryw-counter"), []byte{10, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Add 5 and read in same tx.
	_, err = db.Transact(func(tr fdb.Transaction) (any, error) {
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
