package client

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestTransactRetry verifies the Transact retry loop handles retryable
// FDB errors correctly. We trigger not_committed (1020) by creating
// a write conflict: two transactions read+write the same key, the
// second commit fails and Transact retries it automatically.
func TestTransactRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("conflict_key"), []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1 reads and writes the key, but doesn't commit yet.
	tx1 := db.CreateTransaction()
	rv, err := db.db.grvBatcher.getReadVersion(db.db, ctx)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)
	_, err = tx1.Get(ctx, []byte("conflict_key"))
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	tx1.Set([]byte("conflict_key"), []byte("v1"))

	// tx2 reads and writes the same key, commits first.
	// This creates the conflict.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		if _, err := tx.Get(ctx, []byte("conflict_key")); err != nil {
			return nil, err
		}
		tx.Set([]byte("conflict_key"), []byte("v2"))
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
	retryErr := tx1.OnError(err)
	if retryErr != nil {
		t.Fatalf("OnError should return nil for 1020, got: %v", retryErr)
	}

	// Now verify Transact handles this automatically: a conflicting
	// write should be retried and eventually succeed.
	attempt := 0
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		attempt++
		// On every attempt, read then write. The first attempt will
		// conflict with the write above; retry should succeed.
		val, err := tx.Get(ctx, []byte("conflict_key"))
		if err != nil {
			return nil, err
		}
		tx.Set([]byte("conflict_key"), []byte("v3"))
		return val, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
	t.Logf("Transact succeeded after %d attempt(s)", attempt)

	// Verify final value.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("conflict_key"))
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Write
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("hello"), []byte("world"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("hello"))
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "world" {
		t.Fatalf("Get: got %q, want %q", result, "world")
	}
}

// openTestDB starts an FDB testcontainer and returns a connected Database.
// Uses a 60s setup context for container creation + configuration, independent
// of the caller's ctx (which may be shorter for the actual test operations).
func openTestDB(t *testing.T, ctx context.Context) *Database {
	t.Helper()

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer setupCancel()

	container, err := tcfdb.Run(setupCtx, "")
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			// Dump container state + logs on failure for debugging.
			state, serr := container.State(ctx)
			if serr == nil {
				t.Logf("=== FDB container state: %s (exit=%d) ===", state.Status, state.ExitCode)
			} else {
				t.Logf("=== FDB container state error: %v ===", serr)
			}
			logCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			logs, lerr := container.Logs(logCtx)
			if lerr == nil {
				logBytes, _ := io.ReadAll(logs)
				if len(logBytes) > 2000 {
					logBytes = logBytes[len(logBytes)-2000:]
				}
				t.Logf("=== FDB container logs (last 2000 bytes) ===\n%s", string(logBytes))
			} else {
				t.Logf("=== FDB container logs error: %v ===", lerr)
			}
		}
		container.Terminate(ctx)
	})

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster string: %v", err)
	}

	// Configure cluster and wait for it to be fully ready.
	// The "configure" command returns before resolvers are initialized.
	// "status minimal" reports "Healthy" only when all roles are up.
	exitCode, _, _ := container.Exec(setupCtx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		t.Fatalf("fdbcli configure exit: %d", exitCode)
	}
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, err := container.Exec(setupCtx, []string{"fdbcli", "--exec", "status minimal"})
		if err != nil || reader == nil {
			continue
		}
		if code == 0 {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "Healthy") {
				break
			}
		}
	}

	// Read internal cluster file for correct cluster key
	_, internalReader, err := container.Exec(setupCtx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		t.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		t.Fatalf("parse internal cluster: %v", err)
	}

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	db, err := openDatabaseFromConfig(setupCtx, connectCF, nil)
	if err != nil {
		t.Fatalf("openDatabaseFromConfig: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}
