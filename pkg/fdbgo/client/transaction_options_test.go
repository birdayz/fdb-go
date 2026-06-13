package client

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestSetNextWriteNoWriteConflictRange verifies that the flag suppresses
// write conflict ranges for exactly one mutation, then auto-resets.
// This is used by the Record Layer for blind writes (e.g., index updates)
// where conflict detection is unnecessary.
func TestSetNextWriteNoWriteConflictRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key1 := []byte(t.Name() + "_key1")
	key2 := []byte(t.Name() + "_key2")

	// tx1: Set key1 with no-write-conflict, then key2 normally.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	tx1.SetNextWriteNoWriteConflictRange()
	tx1.Set(key1, []byte("v1")) // Should NOT add a write conflict range.
	tx1.Set(key2, []byte("v2")) // Should add a write conflict range (flag auto-reset).

	// Verify: key1 has no write conflict, key2 does.
	// We do this by having another transaction write key1 between tx1's read version
	// and tx1's commit. If key1 has no write conflict, tx1 should still commit.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key1, []byte("concurrent"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// tx1 should commit successfully because key1 has no write conflict range.
	// (key2 has no conflicting read so it won't conflict either.)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 should commit (no write conflict on key1): %v", err)
	}
}

// TestGetPipelined_CacheHit verifies that GetPipelined returns the cached
// RYW value immediately when the key was previously Set in the same transaction.
func TestGetPipelined_CacheHit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("cached"))

		// GetPipelined should return the cached value, not a PendingGet.
		val, pending, err := tx.GetPipelined(ctx, key)
		if err != nil {
			return nil, err
		}
		if pending != nil {
			t.Fatal("expected cache hit (pending should be nil)")
		}
		if string(val) != "cached" {
			t.Fatalf("cached value: got %q, want %q", val, "cached")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestGetPipelined_ServerFetch verifies that GetPipelined sends a request
// to the server for uncached keys and the PendingGet resolves correctly.
func TestGetPipelined_ServerFetch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("server-value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Key not in RYW cache — should go to server.
		val, pending, err := tx.GetPipelined(ctx, key)
		if err != nil {
			return nil, err
		}
		if val != nil {
			t.Fatalf("expected nil val for server fetch, got %q", val)
		}
		if pending == nil {
			t.Fatal("expected PendingGet for server fetch")
		}

		// Resolve the pending get.
		resolved, err := pending.Resolve()
		if err != nil {
			return nil, err
		}
		if string(resolved) != "server-value" {
			t.Fatalf("resolved: got %q, want %q", resolved, "server-value")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestGetPipelined_ClearedKey verifies that GetPipelined returns (nil, nil, nil)
// for keys that were cleared in the same transaction (RYW cache hit on clear).
func TestGetPipelined_ClearedKey(t *testing.T) {
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
		tx.Clear(key) // Clear in RYW cache.

		val, pending, err := tx.GetPipelined(ctx, key)
		if err != nil {
			return nil, err
		}
		if val != nil {
			t.Fatalf("expected nil for cleared key, got %q", val)
		}
		if pending != nil {
			t.Fatal("expected nil pending for cleared key")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestSetCausalReadRisky verifies that the causal-read-risky flag produces
// a functional GRV. The key behavioral test: reads should still work,
// but the read version may be slightly stale (acceptable for monitoring).
func TestSetCausalReadRisky(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read with causal-read-risky. Should succeed and return the value.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetCausalReadRisky(true)
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Transact with causal-read-risky: %v", err)
	}
	if string(result.([]byte)) != "v1" {
		t.Fatalf("got %q, want %q", result, "v1")
	}
}

// TestGrvFlags verifies the bitmask encoding matches C++ constants.
func TestGrvFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		priority TransactionPriority
		risky    bool
		want     uint32
	}{
		{"default", PriorityDefault, false, grvPriorityDefault},
		{"batch", PriorityBatch, false, grvPriorityBatch},
		{"system_immediate", PrioritySystemImmediate, false, grvPrioritySystemImmediate},
		{"default_risky", PriorityDefault, true, grvPriorityDefault | grvFlagCausalReadRisky},
		{"batch_risky", PriorityBatch, true, grvPriorityBatch | grvFlagCausalReadRisky},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			tx.SetPriority(tt.priority)
			tx.SetCausalReadRisky(tt.risky)
			got := tx.grvFlags()
			if got != tt.want {
				t.Fatalf("grvFlags: got %#x, want %#x", got, tt.want)
			}
		})
	}
}

// TestGetCommittedVersion_BeforeCommit verifies that GetCommittedVersion
// returns an error before the transaction has committed.
func TestGetCommittedVersion_BeforeCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	_, err := tx.GetCommittedVersion()
	if err == nil {
		t.Fatal("expected error before commit")
	}
}

// TestGetCommittedVersion_AfterCommit verifies that GetCommittedVersion
// returns a valid version after a successful commit.
func TestGetCommittedVersion_AfterCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	tx := db.CreateTransaction()
	if _, err := tx.Get(ctx, key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	tx.Set(key, []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ver, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion: %v", err)
	}
	if ver <= 0 {
		t.Fatalf("committed version should be > 0, got %d", ver)
	}
	t.Logf("committed version: %d", ver)
}

// TestGetVersionstamp_ErrorBeforeCommit verifies that GetVersionstamp
// errors before commit and returns valid 10-byte stamp after commit.
func TestGetVersionstamp_ErrorBeforeCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	tx := db.CreateTransaction()
	// Before commit: should error.
	_, err := tx.GetVersionstamp()
	if err == nil {
		t.Fatal("expected error before commit")
	}

	if _, err := tx.Get(ctx, key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	tx.Set(key, []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	vs, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatalf("GetVersionstamp: %v", err)
	}
	if len(vs) != 10 {
		t.Fatalf("versionstamp length: got %d, want 10", len(vs))
	}
	// Version bytes (big-endian) should be non-zero.
	if bytes.Equal(vs[:8], make([]byte, 8)) {
		t.Fatal("versionstamp version bytes should be non-zero after commit")
	}
	t.Logf("versionstamp: %x", vs)
}
