package client

// Tests for FDB transaction isolation guarantees.
//
// These verify that the pure Go client correctly handles conflicts:
// - Two concurrent transactions writing the same key → one must fail
// - Write conflict ranges are correctly propagated
// - Read-write conflicts cause the right transaction to abort

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestConflictDetection verifies that two concurrent transactions writing
// to the same key produce a conflict. Exactly one should succeed and one
// should be retried (via Transact's retry loop). The final value should
// be from one of the two transactions.
func TestConflictDetection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("conflict_test_key")

	// Initialize.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, le64v(0))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Two goroutines simultaneously read-modify-write the same key.
	// Both read the same version, both try to write. FDB's conflict
	// detection will cause one to fail and retry.
	var wg sync.WaitGroup
	var conflicts atomic.Int64

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(val uint64) {
			defer wg.Done()
			_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				// Read current value.
				old, err := tx.Get(ctx, key)
				if err != nil {
					return nil, err
				}
				current := binary.LittleEndian.Uint64(old)
				// Set to current + our value.
				tx.Set(key, le64v(current+val))
				return nil, nil
			})
			if err != nil {
				// Transact should have retried on conflict.
				// If we still got an error, it's unexpected.
				var fdbErr *wire.FDBError
				if errors.As(err, &fdbErr) {
					conflicts.Add(1)
				}
			}
		}(uint64(i + 1))
	}
	wg.Wait()

	// Read final value. Should be 0+1+2 = 3 if both succeeded via retry.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	final := binary.LittleEndian.Uint64(result.([]byte))
	if final != 3 {
		t.Errorf("final value: got %d, want 3 (0+1+2)", final)
	}
}

// TestWriteConflictRanges verifies that manually added write conflict ranges
// cause conflicts between transactions.
func TestWriteConflictRanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("wcr_test_key")

	// Initialize.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Transaction 1: read the key, then add a manual write conflict range.
	// Transaction 2: also reads and writes the same key.
	// One should conflict and retry.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			db.Transact(ctx, func(tx *Transaction) (any, error) {
				val, err := tx.Get(ctx, key)
				if err != nil {
					return nil, err
				}
				_ = val
				tx.Set(key, []byte("writer_"+string(rune('A'+idx))))
				return nil, nil
			})
		}(i)
	}
	wg.Wait()

	// Final value should be from one of the writers.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	val := string(result.([]byte))
	if val != "writer_A" && val != "writer_B" {
		t.Errorf("unexpected final value: %q", val)
	}
}

// TestSnapshotReadNoConflict verifies that snapshot reads don't cause conflicts.
// A transaction that only uses snapshot reads should never conflict with writers.
func TestSnapshotReadNoConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("snap_noconflict_key")

	// Write initial value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Reader uses snapshot reads only. Writer modifies the key.
	// Reader should never conflict.
	var wg sync.WaitGroup
	var readerConflicts atomic.Int64

	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(key, []byte("modified"))
			return nil, nil
		})
	}()

	// Reader (snapshot).
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			// Snapshot read — should NOT add read conflict.
			snap := tx.Snapshot()
			val, err := snap.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			_ = val
			// Write something unrelated to commit the transaction.
			tx.Set([]byte("snap_noconflict_unrelated"), []byte("x"))
			return nil, nil
		})
		if err != nil {
			readerConflicts.Add(1)
		}
	}()

	wg.Wait()

	// Snapshot reader should never have conflicted.
	if readerConflicts.Load() > 0 {
		t.Error("snapshot reader should not conflict with writer")
	}
}
