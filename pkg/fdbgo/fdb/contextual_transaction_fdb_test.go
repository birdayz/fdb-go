package fdb_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestCreateTransactionInheritsWrappedDatabaseContextForReads(t *testing.T) {
	t.Parallel()
	for _, writable := range []bool{false, true} {
		name := "CreateTransaction"
		if writable {
			name = "CreateWritableTransaction"
		}
		t.Run(name, func(t *testing.T) { testWrappedDatabaseCreatorContext(t, writable) })
	}
}

func testWrappedDatabaseCreatorContext(t *testing.T, writable bool) {
	gate := &grvHoldDialer{replyFileID: types.GetValueReplyFileID, heldCh: make(chan struct{}, 16)}
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()
	db, err := client.OpenDatabaseFromConfig(setupCtx, sharedClusterFile, client.WithAPIVersion(730), client.WithDialFunc(gate.dial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(gate.release)
	key := []byte(t.Name())
	want := []byte("live context value")
	if _, err := db.Transact(setupCtx, func(tx *client.Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := fdb.WrapDatabaseContext(ctx, db)
	create := func() (fdb.WritableTransaction, error) {
		if writable {
			return wrapped.CreateWritableTransaction()
		}
		return wrapped.CreateTransaction()
	}
	live, err := create()
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []fdb.ReadTransaction{live, live.Snapshot()} {
		got, err := read.Get(fdb.Key(key)).Get()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("live inherited context: value=%q err=%v", got, err)
		}
	}

	gate.arm()
	tr, err := create()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	for name, read := range map[string]fdb.ReadTransaction{"transaction": tr, "snapshot": tr.Snapshot()} {
		go func() {
			_, err := read.Get(fdb.Key(key)).Get()
			results <- result{name, err}
		}()
	}
	select {
	case <-gate.heldCh:
	case r := <-results:
		t.Fatalf("%s read completed before a value reply was held: %v", r.name, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("transaction read did not reach the held real-FDB value reply")
	}
	cancel()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case r := <-results:
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("%s read cancellation: got %v, want context.Canceled", r.name, r.err)
			}
		case <-deadline.C:
			t.Fatal("CreateTransaction reads ignored inherited context cancellation while the value reply remained held")
		}
	}
	gate.mu.Lock()
	stillHeld := gate.armed && len(gate.held) > 0
	gate.mu.Unlock()
	if !stillHeld {
		t.Fatal("cancellation test released the value reply before both reads ended")
	}
}

func TestTenantPublicEntriesInheritWrappedDatabaseContext(t *testing.T) {
	t.Parallel()
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()
	db, err := client.OpenDatabaseFromConfig(setupCtx, sharedClusterFile, client.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tenant := fdb.WrapDatabaseContext(ctx, db).OpenTenantById(12345)
	bodyRan := errors.New("tenant body ran despite inherited cancellation")
	t.Run("Transact", func(t *testing.T) {
		_, err := tenant.Transact(func(fdb.WritableTransaction) (any, error) { return nil, bodyRan })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tenant Transact: got %v, want context.Canceled", err)
		}
	})
	t.Run("ReadTransact", func(t *testing.T) {
		_, err := tenant.ReadTransact(func(fdb.ReadTransaction) (any, error) { return nil, bodyRan })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tenant ReadTransact: got %v, want context.Canceled", err)
		}
	})
	tr, err := tenant.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]fdb.ReadTransaction{"transaction": tr, "snapshot": tr.Snapshot()} {
		t.Run(name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() {
				_, err := read.Get(fdb.Key(t.Name())).Get()
				result <- err
			}()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("tenant %s read: got %v, want context.Canceled", name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("tenant %s read ignored inherited cancellation", name)
			}
		})
	}
}
