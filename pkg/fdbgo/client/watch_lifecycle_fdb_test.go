package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func committedWatchPoll(t *testing.T, ctx, parent context.Context, db *Database, rec *sendRecorder, key []byte, write bool) (*Transaction, context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tx := db.CreateTransaction()
	t.Cleanup(tx.Cancel)
	if write {
		tx.Set(key, []byte("committed"))
	}
	value, rv, span, watchCtx, watchCancel, err := tx.WatchSetup(parent, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		watchCancel()
		tx.ReleaseWatch()
		t.Fatal(err)
	}
	version, err := tx.AwaitWatchCommit(watchCtx, tx.WatchActivationFor(watchCtx), rv)
	if err != nil {
		watchCancel()
		tx.ReleaseWatch()
		t.Fatal(err)
	}
	tx.watchMu.Lock()
	registered := len(tx.watchCancels)
	tx.watchMu.Unlock()
	if registered != 0 {
		watchCancel()
		tx.ReleaseWatch()
		t.Fatalf("successful commit retained %d watch registrations", registered)
	}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		watchCancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("watch poll did not stop during cleanup")
		}
	})
	rec.start()
	go func() {
		defer close(stopped)
		done <- tx.WatchPoll(watchCtx, watchCancel, key, value, version, span)
	}()
	waitForRecorded(t, ctx, rec, "WatchValueRequest")
	if recording := rec.stop(); len(recording.problems) != 0 {
		t.Fatalf("watch request recording: %v", recording.problems)
	}
	if outstanding := db.db.outstandingWatches.Load(); outstanding != 1 {
		t.Fatalf("watch poll holds %d slots, want 1", outstanding)
	}
	return tx, watchCtx, watchCancel, done
}

func TestCommittedWatchPollSurvivesTransactionReuse(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{"read_only", "write"} {
		for _, teardown := range []string{"Reset", "OnError"} {
			t.Run(commit+"/"+teardown, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				db, rec := openRecordedDB(t, ctx)
				key := []byte(t.Name())
				tx, watchCtx, _, done := committedWatchPoll(t, ctx, ctx, db, rec, key, commit == "write")
				if teardown == "Reset" {
					tx.Reset()
				} else if err := tx.OnError(ctx, &wire.FDBError{Code: ErrNotCommitted}); err != nil {
					t.Fatal(err)
				}
				if err := watchCtx.Err(); err != nil {
					t.Fatalf("%s cancelled committed poll: %v", teardown, err)
				}
				if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Set(key, []byte("changed"))
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("committed poll after %s: %v", teardown, err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if outstanding := db.db.outstandingWatches.Load(); outstanding != 0 {
					t.Fatalf("completed poll leaked %d slots", outstanding)
				}
			})
		}
	}
}

func TestCommittedWatchPollRetainsScopedAndParentCancellation(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"scoped", "parent"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			parent, cancelParent := context.WithCancel(ctx)
			defer cancelParent()
			db, rec := openRecordedDB(t, ctx)
			_, _, watchCancel, done := committedWatchPoll(t, ctx, parent, db, rec, []byte(t.Name()), false)
			if source == "scoped" {
				watchCancel()
			} else {
				cancelParent()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s cancellation: %v", source, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if outstanding := db.db.outstandingWatches.Load(); outstanding != 0 {
				t.Fatalf("cancelled poll leaked %d slots", outstanding)
			}
		})
	}
}
