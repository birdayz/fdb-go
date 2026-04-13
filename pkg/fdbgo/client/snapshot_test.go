package client

import (
	"bytes"
	"context"
	"testing"
	"time"
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
	rv, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault)
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
