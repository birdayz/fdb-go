package client

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

func TestNativeCommit_ResetDropsFenceBeforeItsReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, _, tx, _ := newGatedCommit(t, ctx, true)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, g.reached, "fence reply withheld")
	native := currentNativeCommit(t, tx)
	tx.Reset()
	awaitCommitLifetimeCode(t, ctx, done, ErrTransactionCancelled, "reset public wait")
	waitCommitLifetimeGate(t, ctx, native.finished, "Reset drops native fence without its reply")
	releaseCommitGate(t, g)
	runtime.KeepAlive(tx)
}

func TestNativeCommit_DatabaseCloseJoinsActor(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"commit", "fence", "commit-after-cancel", "fence-after-cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
			defer cancel()
			fence := mode == "fence" || mode == "fence-after-cancel"
			g, db, tx, _ := newGatedCommit(t, ctx, fence)
			done := startGatedCommit(tx, ctx)
			waitCommitLifetimeGate(t, ctx, g.reached, "native reply withheld")
			native := currentNativeCommit(t, tx)
			afterCancel := mode == "commit-after-cancel" || mode == "fence-after-cancel"
			if afterCancel {
				tx.Cancel()
				awaitCommitLifetimeCode(t, ctx, done, ErrTransactionCancelled, "public wait before database close")
				select {
				case <-native.finished:
					t.Fatal("Cancel dropped native actor before database close")
				default:
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- db.Close() }()
			if err := awaitGatedCommit(t, ctx, closed); err != nil {
				t.Fatal(err)
			}
			if !afterCancel {
				if err := awaitGatedCommit(t, ctx, done); err != context.Canceled {
					t.Fatalf("database-close commit = %v, want context.Canceled", err)
				}
			}
			select {
			case <-native.finished:
			default:
				t.Fatal("Database.Close returned before its native actor exited")
			}
			runtime.KeepAlive(tx)
		})
	}
}

func abandonNativeCommit(t *testing.T, ctx context.Context, fence bool) (*commitReplyGate, *nativeCommit, weak.Pointer[Transaction]) {
	t.Helper()
	g, _, tx, _ := newGatedCommit(t, ctx, fence)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, g.reached, "commit reply withheld")
	native := currentNativeCommit(t, tx)
	tx.Cancel()
	awaitCommitLifetimeCode(t, ctx, done, ErrTransactionCancelled, "public wait before abandoning owner")
	select {
	case <-native.finished:
		t.Fatal("Cancel dropped native owner rather than only the public wait")
	default:
	}
	return g, native, weak.Make(tx)
}

func TestNativeCommit_AbandonedOwnerDoesNotLeak(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"commit", "fence"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
			defer cancel()
			g, native, owner := abandonNativeCommit(t, ctx, mode == "fence")
			for {
				runtime.GC()
				if owner.Value() == nil {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("native actor retains its abandoned transaction")
				case <-time.After(time.Millisecond):
				}
			}
			waitCommitLifetimeGate(t, ctx, native.finished, "owner cleanup drops native actor without its reply")
			releaseCommitGate(t, g)
		})
	}
}
