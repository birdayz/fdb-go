package fdb_test

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

func seedWatchKey(t *testing.T, ctx context.Context, db fdb.Database, key fdb.Key) {
	t.Helper()
	if _, err := db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func awaitWatch(t *testing.T, ctx context.Context, watch fdb.FutureNil) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- watch.Get() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		watch.Cancel()
		t.Fatalf("watch did not complete: %v", ctx.Err())
		return ctx.Err()
	}
}

func TestFDB_CommittedWatchesSurviveTransactionTeardown(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{"read_only", "write"} {
		for _, teardown := range []string{"Reset", "OnError", "terminal_OnError", "Cancel"} {
			t.Run(commit+"/"+teardown, func(t *testing.T) {
				t.Parallel()
				db := openTestDB(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				key := fdb.Key(t.Name())
				seedWatchKey(t, ctx, db, key)
				tx, err := db.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Cancel()
				if commit == "write" {
					tx.Set(key, []byte("committed"))
				}
				watch := tx.Watch(key)
				defer watch.Cancel()
				if err := tx.Commit().Get(); err != nil {
					t.Fatal(err)
				}
				// A successor's pending watch is still owned by the transaction.
				pending := tx.Watch(fdb.Key(string(key) + "/pending"))
				defer pending.Cancel()
				switch teardown {
				case "Reset":
					tx.Reset()
				case "OnError":
					if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
						t.Fatal(err)
					}
				case "terminal_OnError":
					if err := tx.OnError(fdb.Error{Code: 2004}).Get(); !isFDBCode(err, 2004) {
						t.Fatalf("terminal OnError: %v", err)
					}
				case "Cancel":
					tx.Cancel()
				}
				if err := awaitWatch(t, ctx, pending); err == nil {
					t.Fatal("uncommitted watch survived transaction teardown")
				}
				_, err = db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
					tx.Set(key, []byte("changed"))
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := awaitWatch(t, ctx, watch); err != nil {
					t.Fatalf("committed watch did not survive %s: %v", teardown, err)
				}
			})
		}
	}
}

func TestFDB_CommittedWatchFutureCancelIsScoped(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := fdb.Key(t.Name())
	seedWatchKey(t, ctx, db, key)
	tx, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Cancel()
	first, sibling := tx.Watch(key), tx.Watch(key)
	defer first.Cancel()
	defer sibling.Cancel()
	if err := tx.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	first.Cancel()
	if err := awaitWatch(t, ctx, first); !isFDBCode(err, 1101) {
		t.Fatalf("cancelled future: %v, want 1101", err)
	}
	tx.Reset()
	if _, err := db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(key, []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := awaitWatch(t, ctx, sibling); err != nil {
		t.Fatalf("cancelling a committed watch also cancelled its sibling: %v", err)
	}
}
