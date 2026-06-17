package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// TestSnapshot_GetReadVersion verifies that Snapshot.GetReadVersion returns
// the same version as the parent transaction.
func TestSnapshot_GetReadVersion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		txRV, err := tx.GetReadVersion(ctx)
		if err != nil {
			return nil, err
		}
		snapRV, err := tx.Snapshot().GetReadVersion(ctx)
		if err != nil {
			return nil, err
		}
		if txRV != snapRV {
			t.Errorf("Snapshot read version %d != Transaction read version %d", snapRV, txRV)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetReadsUncommittedWrite verifies that Snapshot.Get sees
// uncommitted writes from the same transaction (RYW behavior).
func TestSnapshot_GetReadsUncommittedWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("uncommitted"))
		val, err := tx.Snapshot().Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if string(val) != "uncommitted" {
			t.Errorf("Snapshot.Get: got %q, want %q", val, "uncommitted")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetDoesNotConflict verifies that Snapshot reads do NOT cause
// transaction conflicts. This is the whole point of Snapshot — read without
// conflict ranges for monitoring/reporting use cases.
func TestSnapshot_GetDoesNotConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: snapshot-read the key, then write a different key.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	// Snapshot read — should NOT add a read conflict on key.
	_, err = tx1.Snapshot().Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 snapshot Get: %v", err)
	}
	tx1.Set([]byte(t.Name()+"_other"), []byte("x"))

	// Concurrent write to the same key from another transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// tx1 should commit without conflict because the read was a snapshot read.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 should not conflict (snapshot read): %v", err)
	}
}

// TestSnapshot_GetRange verifies Snapshot range reads return correct data
// and interleave correctly with local writes via RYW.
func TestSnapshot_GetRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 5 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
			tx.Set([]byte(prefix+string(rune('A'+i))), []byte{byte(i)})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Write a local key that merges into the range.
		tx.Set([]byte(prefix+"C5"), []byte{99})

		snap := tx.Snapshot()
		begin := []byte(prefix + "A")
		end := []byte(prefix + "Z")
		kvs, more, err := snap.GetRange(ctx, begin, end, 100)
		if err != nil {
			return nil, err
		}
		if more {
			t.Error("expected more=false for 6 keys with limit 100")
		}
		// Should see A, B, C, C5 (local), D, E = 6 keys.
		if len(kvs) != 6 {
			keys := make([]string, len(kvs))
			for i, kv := range kvs {
				keys[i] = string(kv.Key)
			}
			t.Fatalf("expected 6 results, got %d: %v", len(kvs), keys)
		}
		// Verify local write is present.
		if !bytes.Equal(kvs[3].Value, []byte{99}) {
			t.Errorf("C5 value: got %v, want [99]", kvs[3].Value)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetRangeReverse verifies reverse range scan via Snapshot.
func TestSnapshot_GetRangeReverse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 3 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"A"), []byte("a"))
		tx.Set([]byte(prefix+"B"), []byte("b"))
		tx.Set([]byte(prefix+"C"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		snap := tx.Snapshot()
		begin := []byte(prefix + "A")
		end := []byte(prefix + "D")
		kvs, _, err := snap.GetRangeReverse(ctx, begin, end, 100)
		if err != nil {
			return nil, err
		}
		if len(kvs) != 3 {
			t.Fatalf("expected 3 results, got %d", len(kvs))
		}
		// Reverse order: C, B, A.
		if string(kvs[0].Value) != "c" || string(kvs[1].Value) != "b" || string(kvs[2].Value) != "a" {
			t.Errorf("expected reverse order [c, b, a], got [%s, %s, %s]",
				kvs[0].Value, kvs[1].Value, kvs[2].Value)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetKey verifies Snapshot key selector resolution.
func TestSnapshot_GetKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed keys: A, B, C.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"A"), []byte("a"))
		tx.Set([]byte(prefix+"B"), []byte("b"))
		tx.Set([]byte(prefix+"C"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// firstGreaterOrEqual(B) should resolve to B.
		resolved, err := tx.Snapshot().GetKey(ctx, []byte(prefix+"B"), false, 1)
		if err != nil {
			return nil, err
		}
		if string(resolved) != prefix+"B" {
			t.Errorf("firstGreaterOrEqual(B): got %q, want %q", resolved, prefix+"B")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_SnapshotRYWDisable verifies that SetSnapshotRYWDisable
// bypasses the RYW cache for snapshot reads.
func TestSnapshot_SnapshotRYWDisable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed key with committed value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("committed"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetSnapshotRYWDisable()
		tx.Set(key, []byte("local"))

		// With RYW disabled, snapshot read should see committed value, not local.
		val, err := tx.Snapshot().Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if string(val) != "committed" {
			t.Errorf("with RYW disabled: got %q, want %q", val, "committed")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetAfterClear verifies that within the same transaction,
// snapshot read sees the effect of a prior Clear (RYW applies to snapshots).
func TestSnapshot_GetAfterClear(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Clear the key in the same transaction.
		tx.Clear(key)

		// Snapshot read should see nil — RYW merges local clears.
		val, err := tx.Snapshot().Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if val != nil {
			t.Errorf("snapshot read after Clear: got %q, want nil", val)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetRangeAfterClearRange verifies that snapshot range reads
// see the gap created by ClearRange within the same transaction (RYW applies).
func TestSnapshot_GetRangeAfterClearRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 3 keys: A, B, C.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"A"), []byte("a"))
		tx.Set([]byte(prefix+"B"), []byte("b"))
		tx.Set([]byte(prefix+"C"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// ClearRange covering only B: [prefix+B, prefix+C) clears B.
		if err := tx.ClearRange([]byte(prefix+"B"), []byte(prefix+"C")); err != nil {
			return nil, err
		}

		// Snapshot range read should show the gap — only A and C.
		snap := tx.Snapshot()
		kvs, _, err := snap.GetRange(ctx, []byte(prefix+"A"), []byte(prefix+"D"), 100)
		if err != nil {
			return nil, err
		}
		if len(kvs) != 2 {
			keys := make([]string, len(kvs))
			for i, kv := range kvs {
				keys[i] = string(kv.Key)
			}
			t.Fatalf("expected 2 keys (A, C), got %d: %v", len(kvs), keys)
		}
		if string(kvs[0].Key) != prefix+"A" {
			t.Errorf("first key: got %q, want %q", kvs[0].Key, prefix+"A")
		}
		if string(kvs[1].Key) != prefix+"C" {
			t.Errorf("second key: got %q, want %q", kvs[1].Key, prefix+"C")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_GetRangeDoesNotConflict verifies that snapshot range reads do
// NOT add conflict ranges, so a concurrent write to the same range does not
// cause the snapshot-reading transaction to conflict.
func TestSnapshot_GetRangeDoesNotConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 3 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"A"), []byte("a"))
		tx.Set([]byte(prefix+"B"), []byte("b"))
		tx.Set([]byte(prefix+"C"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: snapshot range read, then write an unrelated key.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	// Snapshot range read — should NOT add read conflict range.
	kvs, _, err := tx1.Snapshot().GetRange(ctx, []byte(prefix+"A"), []byte(prefix+"D"), 100)
	if err != nil {
		t.Fatalf("tx1 snapshot GetRange: %v", err)
	}
	if len(kvs) != 3 {
		t.Fatalf("snapshot range: expected 3 keys, got %d", len(kvs))
	}
	tx1.Set([]byte(prefix+"unrelated"), []byte("x"))

	// Concurrent write into the same range from another transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"B"), []byte("modified"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// tx1 should commit without conflict — snapshot range read adds no conflict range.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 should not conflict (snapshot range read): %v", err)
	}
}

// TestSnapshot_GetAfterAtomicAdd verifies that a snapshot read within the same
// transaction sees the accumulated value after a Set + AtomicAdd (RYW merges
// atomic mutations into the local cache).
func TestSnapshot_GetAfterAtomicAdd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_counter")

	// Seed key with value 10 (little-endian int64).
	var initBuf [8]byte
	binary.LittleEndian.PutUint64(initBuf[:], 10)
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, initBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Atomic ADD +5.
		var addBuf [8]byte
		binary.LittleEndian.PutUint64(addBuf[:], 5)
		tx.Atomic(MutAddValue, key, addBuf[:])

		// Snapshot read should see the accumulated value (10 + 5 = 15)
		// because RYW merges atomic mutations.
		val, err := tx.Snapshot().Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if len(val) < 8 {
			t.Fatalf("snapshot read after AtomicAdd: value too short (%d bytes)", len(val))
		}
		got := binary.LittleEndian.Uint64(val)
		if got != 15 {
			t.Errorf("snapshot read after AtomicAdd: got %d, want 15", got)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSnapshot_ConflictAsymmetry verifies the core snapshot semantic: a snapshot
// read does NOT cause conflict, but a regular read on the same key DOES.
// Same setup, same timing, different outcome based on snapshot vs non-snapshot.
func TestSnapshot_ConflictAsymmetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"
	key := []byte(prefix + "key")
	otherKeySnap := []byte(prefix + "other_snap")
	otherKeyRegular := []byte(prefix + "other_regular")

	// Seed key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Part 1: Snapshot read path — should NOT conflict.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	txSnap := db.CreateTransaction()
	txSnap.SetReadVersion(rv)

	// Snapshot read — no conflict range.
	_, err = txSnap.Snapshot().Get(ctx, key)
	if err != nil {
		t.Fatalf("snapshot Get: %v", err)
	}
	txSnap.Set(otherKeySnap, []byte("snap_write"))

	// Concurrent write to same key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write 1: %v", err)
	}

	// Snapshot tx should commit fine.
	if err := txSnap.Commit(ctx); err != nil {
		t.Fatalf("snapshot tx should NOT conflict: %v", err)
	}

	// Part 2: Regular read path — SHOULD conflict.
	rv2, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV2: %v", err)
	}

	txRegular := db.CreateTransaction()
	txRegular.SetReadVersion(rv2)

	// Regular read — adds conflict range.
	_, err = txRegular.Get(ctx, key)
	if err != nil {
		t.Fatalf("regular Get: %v", err)
	}
	txRegular.Set(otherKeyRegular, []byte("regular_write"))

	// Concurrent write to same key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write 2: %v", err)
	}

	// Regular tx should conflict.
	err = txRegular.Commit(ctx)
	if err == nil {
		t.Fatal("regular tx SHOULD conflict — read added conflict range")
	}
	t.Logf("regular tx conflict (expected): %v", err)
}
