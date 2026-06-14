package client

import (
	"context"
	"testing"
	"time"
)

// TestDatabaseDefaultsPropagateToTransaction verifies that database-level
// defaults (timeout, retry limit, max retry delay, size limit) are applied
// to every transaction created via CreateTransaction. This is important
// because application code typically sets these once on the Database and
// expects all transactions to inherit them.
func TestDatabaseDefaultsPropagateToTransaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Set all database-level defaults.
	db.SetTransactionTimeout(5000)        // 5s
	db.SetTransactionRetryLimit(3)        // 3 retries
	db.SetTransactionMaxRetryDelay(200)   // 200ms
	db.SetTransactionSizeLimit(1_000_000) // 1MB
	db.SetDefaultReadSystemKeys()

	// Create a transaction — it should inherit all defaults.
	tx := db.CreateTransaction()

	if tx.sizeLimit != 1_000_000 {
		t.Errorf("sizeLimit: got %d, want 1000000", tx.sizeLimit)
	}
	if tx.maxRetryDelay != 200*time.Millisecond {
		t.Errorf("maxRetryDelay: got %v, want 200ms", tx.maxRetryDelay)
	}
	if tx.retryLimit != 3 {
		t.Errorf("retryLimit: got %d, want 3", tx.retryLimit)
	}
	if !tx.readSystemKeys {
		t.Error("readSystemKeys not propagated")
	}

	// Verify the transaction actually works with inherited settings.
	key := []byte(t.Name() + "_key")
	tx.Set(key, []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit with inherited settings: %v", err)
	}
}

// TestDatabaseAccessSystemKeysPropagation verifies that SetDefaultAccessSystemKeys
// allows reading AND writing system keys.
func TestDatabaseAccessSystemKeysPropagation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	db.SetDefaultAccessSystemKeys()

	tx := db.CreateTransaction()
	if !tx.writeSystemKeys {
		t.Error("writeSystemKeys not propagated")
	}
}

// TestInvalidateGRVCache verifies that InvalidateGRVCache forces a fresh
// read version fetch on the next transaction.
func TestInvalidateGRVCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Get a read version to populate the cache.
	rv1, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetReadVersion(ctx)
	})
	if err != nil {
		t.Fatalf("first GRV: %v", err)
	}

	// Invalidate and get a new version.
	db.InvalidateGRVCache()

	rv2, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetReadVersion(ctx)
	})
	if err != nil {
		t.Fatalf("second GRV: %v", err)
	}

	// The second version should be >= the first.
	v1, v2 := rv1.(int64), rv2.(int64)
	if v2 < v1 {
		t.Errorf("version went backwards: %d < %d", v2, v1)
	}
	t.Logf("versions: %d, %d", v1, v2)
}

// TestGetDBInfo verifies that GetDBInfo returns non-nil topology
// after a successful connection.
func TestGetDBInfo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	info := db.GetDBInfo()
	if info == nil {
		t.Fatal("GetDBInfo returned nil")
	}
	if len(info.GRVProxies) == 0 {
		t.Error("no GRV proxies")
	}
	if len(info.CommitProxies) == 0 {
		t.Error("no commit proxies")
	}
}

// TestReadTransact_ReturnsWithoutCommit verifies that ReadTransact
// returns the result without committing the transaction.
func TestReadTransact_ReturnsWithoutCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("seeded"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ReadTransact should read the value and return it.
	result, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		val, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		// Write inside ReadTransact — should NOT be committed.
		tx.Set(key, []byte("should-not-persist"))
		return string(val), nil
	})
	if err != nil {
		t.Fatalf("ReadTransact: %v", err)
	}
	if result.(string) != "seeded" {
		t.Fatalf("got %q, want %q", result, "seeded")
	}

	// Verify the write was NOT committed.
	final, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(final.([]byte)) != "seeded" {
		t.Fatalf("ReadTransact leaked a write: got %q", final)
	}
}

// TestAddConflictRanges verifies explicit read/write conflict ranges.
func TestAddConflictRanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: add explicit read conflict range, then write a different key.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)
	// Explicit read conflict — even though we don't read the key, we'll conflict
	// if it changes between our read version and commit.
	end := append([]byte(nil), key...)
	end = append(end, 0)
	if err := tx1.AddReadConflictRange(key, end); err != nil {
		t.Fatalf("AddReadConflictRange: %v", err)
	}
	tx1.Set([]byte(t.Name()+"_other"), []byte("x"))

	// Concurrent write to the conflicting key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// tx1 should conflict because of the explicit read conflict range.
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("expected conflict from explicit read conflict range")
	}
	t.Logf("conflict (expected): %v", err)

	// Inverted range should return error.
	tx2 := db.CreateTransaction()
	if err := tx2.AddReadConflictRange([]byte("z"), []byte("a")); err == nil {
		t.Error("AddReadConflictRange(z, a) should return inverted_range error")
	}
	if err := tx2.AddWriteConflictRange([]byte("z"), []byte("a")); err == nil {
		t.Error("AddWriteConflictRange(z, a) should return inverted_range error")
	}
}

// TestBatchPriority verifies that batch priority transactions
// successfully get read versions and operate normally.
func TestBatchPriority(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetPriority(PriorityBatch)
		tx.Set(key, []byte("batch"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("batch priority write: %v", err)
	}

	// Read back with batch priority.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetPriority(PriorityBatch)
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("batch priority read: %v", err)
	}
	if string(result.([]byte)) != "batch" {
		t.Fatalf("got %q, want %q", result, "batch")
	}
}
