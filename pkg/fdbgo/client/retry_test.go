package client

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// TestOnError_ResetsRYWCache verifies that OnError (which calls reset()) clears
// the RYW cache. After a retryable error, the transaction starts fresh — any
// writes from the failed attempt should be gone.
func TestOnError_ResetsRYWCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Clean slate.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// tx1: Set key, then force a conflict so we get a retryable error.
	tx1 := db.CreateTransaction()
	rv, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	// Read the key to establish a read conflict range.
	_, err = tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}

	// Set key=A in the first attempt.
	tx1.Set(key, []byte("A"))

	// Spoiler: write the same key from another tx to cause conflict.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("spoiler"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("spoiler: %v", err)
	}

	// tx1 commit should fail with not_committed.
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict")
	}

	// OnError resets the transaction (including RYW cache).
	if retryErr := tx1.OnError(context.Background(), err); retryErr != nil {
		t.Fatalf("OnError should allow retry: %v", retryErr)
	}

	// After reset, the RYW cache is gone. Set key=B.
	tx1.Set(key, []byte("B"))
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("retry commit: %v", err)
	}

	// Verify final value is B, not A.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(result.([]byte)) != "B" {
		t.Fatalf("final value: got %q, want %q", result, "B")
	}
}

// TestOnError_PreservesReadVersionBehavior verifies that after OnError, the
// transaction gets a new read version on the next read. This means it sees
// data committed between the first attempt and the retry.
func TestOnError_PreservesReadVersionBehavior(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed key=original.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("original"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: read the key at a fixed read version.
	tx1 := db.CreateTransaction()
	rv, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	val, err := tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	if string(val) != "original" {
		t.Fatalf("first read: got %q, want %q", val, "original")
	}

	// Spoiler: write key=updated from another tx.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("updated"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("spoiler: %v", err)
	}

	// Force conflict: tx1 tries to commit but read conflict fires.
	tx1.Set(key, []byte("attempt1"))
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict")
	}

	// OnError resets tx1 — it will get a new read version on next read.
	if retryErr := tx1.OnError(context.Background(), err); retryErr != nil {
		t.Fatalf("OnError should allow retry: %v", retryErr)
	}

	// After reset, reading the key should see the "updated" value
	// (committed between first attempt and retry).
	val, err = tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("retry Get: %v", err)
	}
	if string(val) != "updated" {
		t.Fatalf("after OnError read: got %q, want %q — OnError should have cleared read version so new GRV is used", val, "updated")
	}
}

// TestConflictDetectionAcrossRetry verifies the full conflict-retry cycle:
// tx1 reads key, tx2 writes key and commits, tx1 commits (conflict),
// tx1.OnError, tx1 retries and sees tx2's write, commits successfully.
func TestConflictDetectionAcrossRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	otherKey := []byte(t.Name() + "_other")

	// Seed key=v0.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1 reads key (establishes read conflict).
	tx1 := db.CreateTransaction()
	rv, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	val, err := tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	if string(val) != "v0" {
		t.Fatalf("tx1 initial read: got %q, want %q", val, "v0")
	}

	// tx2 writes the same key and commits.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("from_tx2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}

	// tx1 writes a different key, then commits — should conflict on key's read.
	tx1.Set(otherKey, []byte("unrelated"))
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict — tx2 wrote key within tx1's read conflict range")
	}

	// OnError resets for retry.
	if retryErr := tx1.OnError(context.Background(), err); retryErr != nil {
		t.Fatalf("OnError should allow retry: %v", retryErr)
	}

	// Retry: read key again — should see tx2's write.
	val, err = tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 retry Get: %v", err)
	}
	if string(val) != "from_tx2" {
		t.Fatalf("retry read: got %q, want %q", val, "from_tx2")
	}

	// Write otherKey and commit — should succeed (no conflict now).
	tx1.Set(otherKey, []byte("retry_value"))
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 retry commit: %v", err)
	}

	// Verify both values.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		v1, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		v2, err := tx.Get(ctx, otherKey)
		if err != nil {
			return nil, err
		}
		return [2]string{string(v1), string(v2)}, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	vals := result.([2]string)
	if vals[0] != "from_tx2" {
		t.Errorf("key: got %q, want %q", vals[0], "from_tx2")
	}
	if vals[1] != "retry_value" {
		t.Errorf("otherKey: got %q, want %q", vals[1], "retry_value")
	}
}

// TestRetryWithInterleavedWrites verifies that after a failed commit and
// OnError, the RYW cache from the first attempt is completely gone. Only the
// second attempt's writes survive.
func TestRetryWithInterleavedWrites(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Clean slate.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Use Transact which handles retry automatically.
	// First attempt: set key=A, then cause a conflict.
	// Second attempt: set key=B.
	var callCount atomic.Int64
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		n := callCount.Add(1)

		// Read key to establish conflict range.
		_, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}

		if n == 1 {
			tx.Set(key, []byte("A"))
			// Spoiler: write the same key from another tx to cause conflict.
			_, spoilErr := db.Transact(ctx, func(tx2 *Transaction) (any, error) {
				tx2.Set(key, []byte("spoiler"))
				return nil, nil
			})
			if spoilErr != nil {
				return nil, fmt.Errorf("spoiler: %w", spoilErr)
			}
		} else {
			tx.Set(key, []byte("B"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}

	calls := callCount.Load()
	if calls < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", calls)
	}

	// Verify final value is B.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(result.([]byte)) != "B" {
		t.Fatalf("final value: got %q, want %q — first attempt's Set(A) should be gone after retry", result, "B")
	}
}
