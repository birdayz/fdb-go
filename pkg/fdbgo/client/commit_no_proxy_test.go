package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// commitWithNoProxies runs one commit attempt against a database that knows no
// commit proxy. The input carries no write conflicts, so the maybe-delivered
// fence (commitDummyTransaction) returns at once and the test observes only the
// proxy wait itself.
func commitWithNoProxies(t *testing.T, ctx context.Context, db *database) (time.Duration, error) {
	t.Helper()
	input := &commitInput{db: db, readVersion: 1, tenantID: NoTenantID}
	start := time.Now()
	_, err := input.commitNative(ctx)
	return time.Since(start), err
}

func requireCode(t *testing.T, err error, want int) {
	t.Helper()
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != want {
		t.Fatalf("commit error = %v, want FDB error %d", err, want)
	}
}

// TestCommit_NoProxies_WaitsForContext pins the C++ shape of a commit made while
// the client knows no commit proxy: tryCommit load-balances over the empty set,
// which is Never() (LoadBalance.actor.h:752-762), raced against
// onProxiesChanged() (NativeAPI.actor.cpp:6646-6649). It does not fail at once.
// Go used to return a Go-internal 1200 immediately, which a bounded retry owner
// counted as a spent attempt during a recovery libfdb_c simply waits through.
// Dropping the native actor ends the wait without pretending a fence completed.
func TestCommit_NoProxies_WaitsForContext(t *testing.T) {
	t.Parallel()
	db := newTestDatabaseStub().db
	const wait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	elapsed, err := commitWithNoProxies(t, ctx, db)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dropped native wait = %v, want context.DeadlineExceeded", err)
	}
	if elapsed < wait {
		t.Fatalf("commit with no proxies returned after %v; it must wait for a proxy change or the context (%v)", elapsed, wait)
	}
}

// TestCommit_NoProxies_WakesOnProxyChange pins the other arm of the race: a
// proxy-set change ends the wait (C++ throws request_maybe_delivered, which
// tryCommit turns into commit_unknown_result), well before the context expires.
func TestCommit_NoProxies_WakesOnProxyChange(t *testing.T) {
	t.Parallel()
	db := newTestDatabaseStub().db
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Broadcast changes until the commit returns: whichever channel it armed,
	// the next broadcast closes it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			db.proxiesChangedMu.Lock()
			close(db.proxiesChanged)
			db.proxiesChanged = make(chan struct{})
			db.proxiesChangedMu.Unlock()
		}
	}()

	elapsed, err := commitWithNoProxies(t, ctx, db)
	requireCode(t, err, ErrCommitUnknownResult)
	if elapsed > 30*time.Second {
		t.Fatalf("commit waited %v; a proxy change must end the wait", elapsed)
	}
}
