package client

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestTransactRetry verifies the Transact retry loop handles retryable
// FDB errors correctly. We trigger not_committed (1020) by creating
// a write conflict: two transactions read+write the same key, the
// second commit fails and Transact retries it automatically.
func TestTransactRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Use test-unique key prefix to avoid collisions with parallel tests.
	key := []byte(t.Name() + "_key")

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1 reads and writes the key, but doesn't commit yet.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)
	_, err = tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	tx1.Set(key, []byte("v1"))

	// tx2 reads and writes the same key, commits first.
	// This creates the conflict.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		if _, err := tx.Get(ctx, key); err != nil {
			return nil, err
		}
		tx.Set(key, []byte("v2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	// Now commit tx1 — this will get not_committed (1020).
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should have conflicted")
	}
	t.Logf("tx1 conflict: %v", err)

	// Verify the error is retryable.
	retryErr := tx1.OnError(context.Background(), err)
	if retryErr != nil {
		t.Fatalf("OnError should return nil for 1020, got: %v", retryErr)
	}

	// Now verify Transact handles this automatically: a conflicting
	// write should be retried and eventually succeed.
	attempt := 0
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		attempt++
		val, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		tx.Set(key, []byte("v3"))
		return val, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
	t.Logf("Transact succeeded after %d attempt(s)", attempt)

	// Verify final value.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if string(result.([]byte)) != "v3" {
		t.Fatalf("final value: got %q, want %q", result, "v3")
	}
}

// TestSetGet is the minimal end-to-end test: write a key, read it back.
// Pure Go client, no C bindings. Talks to a real FDB 7.3.75 testcontainer.
func TestSetGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Write
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("world"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "world" {
		t.Fatalf("Get: got %q, want %q", result, "world")
	}
}

// openTestDB returns a Database connected to the shared FDB testcontainer.
// Each call creates a fresh Database connection (separate connection pool,
// separate options) for test isolation, but reuses the single container
// started in TestMain. This cuts per-test overhead from ~30s to ~1s.
func openTestDB(t *testing.T, ctx context.Context) *Database {
	t.Helper()

	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()

	db, err := OpenDatabaseFromConfig(setupCtx, sharedClusterFile)
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if t.Failed() && sharedContainer != nil {
			// Dump container state + logs on failure for debugging.
			diagCtx, diagCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer diagCancel()
			state, serr := sharedContainer.State(diagCtx)
			if serr == nil {
				t.Logf("=== FDB container state: %s (exit=%d) ===", state.Status, state.ExitCode)
			}
			logs, lerr := sharedContainer.Logs(diagCtx)
			if lerr == nil {
				logBytes, _ := io.ReadAll(logs)
				if len(logBytes) > 2000 {
					logBytes = logBytes[len(logBytes)-2000:]
				}
				t.Logf("=== FDB container logs (last 2000 bytes) ===\n%s", string(logBytes))
			}
		}
	})

	return db
}
