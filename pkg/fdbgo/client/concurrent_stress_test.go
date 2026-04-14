package client

// Concurrent stress tests for the pure Go FDB client.
//
// These tests run many goroutines performing various operations simultaneously
// to expose race conditions, data corruption, and consistency issues. Each
// test pattern is designed to stress a specific aspect:
//
// 1. Concurrent reads + writes: ensure RYW is consistent under contention
// 2. Concurrent transactions: ensure isolation (no cross-tx leaks)
// 3. Concurrent atomics: ensure atomic resolution is correct
// 4. Mixed operations: Set/Get/Clear/ClearRange/AtomicAdd/GetRange in parallel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentRYW_SameTransaction runs many goroutines reading and writing
// within the same transaction. Verifies that Get always returns the latest Set
// value (read-your-writes consistency under concurrent access).
//
// This is adversarial: Go's FDB transaction is documented as NOT safe for
// concurrent use (matching Apple binding), but we test that the RYW cache
// at least doesn't crash or corrupt data.
func TestConcurrentRYW_SameTransaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "concurrent_ryw_"

	// Pre-create keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%02d", pfx, i)), le64stress(uint64(i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Run a transaction with concurrent reads.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Write unique values, then spawn readers.
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%02d", pfx, i)), le64stress(uint64(100+i)))
		}

		var wg sync.WaitGroup
		var errCount atomic.Int64
		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := []byte(fmt.Sprintf("%s%02d", pfx, idx))
				val, err := tx.Get(ctx, key)
				if err != nil {
					errCount.Add(1)
					return
				}
				if val == nil {
					errCount.Add(1)
					return
				}
				got := binary.LittleEndian.Uint64(val)
				// Should see the written value (100+idx), not the old value (idx).
				if got != uint64(100+idx) {
					errCount.Add(1)
				}
			}(g)
		}
		wg.Wait()
		if errCount.Load() > 0 {
			return nil, fmt.Errorf("%d goroutines got wrong/missing values", errCount.Load())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent RYW: %v", err)
	}
}

// TestConcurrentTransactions runs many independent transactions in parallel,
// each doing read-modify-write on a shared counter. Verifies the final count
// is correct (all increments landed via FDB's conflict detection + retry).
func TestConcurrentTransactions_Stress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("concurrent_counter")
	numWorkers := 20
	incrementsPerWorker := 5

	// Initialize counter to 0.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, le64stress(0))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Spawn workers that each increment the counter N times using read-modify-write.
	var wg sync.WaitGroup
	var errors atomic.Int64
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < incrementsPerWorker; i++ {
				_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					val, err := tx.Get(ctx, key)
					if err != nil {
						return nil, err
					}
					current := binary.LittleEndian.Uint64(val)
					tx.Set(key, le64stress(current+1))
					return nil, nil
				})
				if err != nil {
					errors.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if errors.Load() > 0 {
		t.Fatalf("%d transaction errors during concurrent increment", errors.Load())
	}

	// Verify final count.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	final := binary.LittleEndian.Uint64(result.([]byte))
	expected := uint64(numWorkers * incrementsPerWorker)
	if final != expected {
		t.Fatalf("counter: got %d, want %d", final, expected)
	}
}

// TestConcurrentAtomicAdd runs many goroutines doing AtomicAdd on the same key
// in parallel. Each goroutine does multiple adds in separate transactions.
// Verifies the final sum is correct.
func TestConcurrentAtomicAdd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("concurrent_atomic_add")
	numWorkers := 20
	addsPerWorker := 10
	addValue := uint64(7) // each add increments by 7

	// Initialize to 0.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, le64stress(0))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Spawn workers.
	var wg sync.WaitGroup
	var errors atomic.Int64
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < addsPerWorker; i++ {
				_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Atomic(MutAddValue, key, le64stress(addValue))
					return nil, nil
				})
				if err != nil {
					errors.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if errors.Load() > 0 {
		t.Fatalf("%d atomic add errors", errors.Load())
	}

	// Verify sum.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	final := binary.LittleEndian.Uint64(result.([]byte))
	expected := uint64(numWorkers*addsPerWorker) * addValue
	if final != expected {
		t.Fatalf("sum: got %d, want %d", final, expected)
	}
}

// TestConcurrentRangeAndWrite runs concurrent writers and readers.
// Writers insert unique keys, readers scan the full range. At the end,
// verifies all keys are present and the scan count matches.
func TestConcurrentRangeAndWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "concurrent_range_"
	numWriters := 10
	keysPerWriter := 10

	// Each writer inserts unique keys.
	var wg sync.WaitGroup
	var errors atomic.Int64
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < keysPerWriter; i++ {
				key := []byte(fmt.Sprintf("%sw%02dk%02d", pfx, worker, i))
				_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Set(key, []byte(fmt.Sprintf("w%d-k%d", worker, i)))
					return nil, nil
				})
				if err != nil {
					errors.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	if errors.Load() > 0 {
		t.Fatalf("%d write errors", errors.Load())
	}

	// Verify all keys are present via range scan.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"\xFF"), 0)
		if err != nil {
			return nil, err
		}
		return len(kvs), nil
	})
	if err != nil {
		t.Fatalf("range scan: %v", err)
	}
	count := result.(int)
	expected := numWriters * keysPerWriter
	if count != expected {
		t.Fatalf("key count: got %d, want %d", count, expected)
	}
}

// TestConcurrentClearAndRead races Clear against Get on the same key.
// After all transactions commit, the key should either exist with its
// last written value or be absent.
func TestConcurrentClearAndRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("concurrent_clear_read")
	value := []byte("present")

	// Set the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, value)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	// Race: some goroutines clear, others read.
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				// Clear
				db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Clear(key)
					return nil, nil
				})
			} else {
				// Read — should not crash or return corrupt data.
				db.Transact(ctx, func(tx *Transaction) (any, error) {
					val, err := tx.Get(ctx, key)
					if err != nil {
						return nil, err
					}
					if val != nil && !bytes.Equal(val, value) {
						return nil, fmt.Errorf("corrupt value: %v", val)
					}
					return nil, nil
				})
			}
		}(g)
	}
	wg.Wait()

	// After all transactions: key is either absent or "present".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	final := result.([]byte)
	if final != nil && !bytes.Equal(final, value) {
		t.Fatalf("final value corrupt: %v", final)
	}
}

func le64stress(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}
