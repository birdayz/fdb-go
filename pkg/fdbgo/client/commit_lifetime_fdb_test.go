package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

const commitLifetimeTimeout = 30 * time.Second

type pausedCommit struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func installOneShotCommitPause(t *testing.T, db *Database) *pausedCommit {
	t.Helper()
	p := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
	var used atomic.Bool
	hook := func() {
		if !used.CompareAndSwap(false, true) {
			return
		}
		close(p.reached)
		<-p.release
	}
	db.db.beforeCommitProxySelect.Store(&hook)
	t.Cleanup(func() { db.db.beforeCommitProxySelect.Store(nil) })
	return p
}

func (p *pausedCommit) unblock() { p.once.Do(func() { close(p.release) }) }

func waitCommitLifetimeGate(t *testing.T, ctx context.Context, gate <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-gate:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
	}
}

func awaitCommitLifetimeResult(t *testing.T, ctx context.Context, result <-chan error, what string) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
	}
}

func readCommitLifetimeValue(t *testing.T, ctx context.Context, db *Database, key []byte) []byte {
	t.Helper()
	v, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("independent read %q: %v", key, err)
	}
	if v == nil {
		return nil
	}
	return append([]byte(nil), v.([]byte)...)
}

func requireCommitLifetimeCode(t *testing.T, err error, code int, what string) {
	t.Helper()
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != code {
		t.Fatalf("%s: got %v, want FDB error %d", what, err, code)
	}
}

func distinctCommitLifetimeReadVersion(t *testing.T, ctx context.Context, db *Database, after int64, key []byte) int64 {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("advance-version"))
		return nil, nil
	}); err != nil {
		t.Fatalf("advance cluster version: %v", err)
	}
	for {
		tx := db.CreateTransaction()
		rv, err := tx.GetReadVersion(ctx)
		tx.Cancel()
		if err != nil {
			t.Fatalf("get replacement read version: %v", err)
		}
		if rv != after {
			return rv
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("timed out obtaining a read version distinct from %d: %v", after, ctx.Err())
		}
	}
}

func TestFDBDetachedCommitResetPreservesBothGenerations(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := []byte(fmt.Sprintf("commit-lifetime/%s/", t.Name()))
	oldKey, newKey := append(append([]byte(nil), prefix...), "old"...), append(append([]byte(nil), prefix...), "new"...)
	advanceKey := append(append([]byte(nil), prefix...), "advance"...)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	pause := installOneShotCommitPause(t, db)
	defer pause.unblock()

	tx.SetPriority(PriorityBatch)
	oldRV, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("old GetReadVersion: %v", err)
	}
	tx.Set(oldKey, []byte("old-value"))
	oldDone := make(chan error, 1)
	go func() { oldDone <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, pause.reached, "detached old commit")

	tx.Reset()
	newRV := distinctCommitLifetimeReadVersion(t, ctx, db, oldRV, advanceKey)
	tx.SetPriority(PrioritySystemImmediate)
	tx.SetReadVersion(newRV)
	tx.Set(newKey, []byte("new-value"))
	if got, err := tx.GetCommittedVersion(); err == nil || got != 0 {
		requireCommitLifetimeCode(t, err, 2015, "replacement committed version before either completion")
	}
	if got := readCommitLifetimeValue(t, ctx, db, newKey); got != nil {
		t.Fatalf("replacement write escaped before its commit: %q", got)
	}

	pause.unblock()
	awaitCommitLifetimeResult(t, ctx, oldDone, "old detached commit")
	if got := readCommitLifetimeValue(t, ctx, db, oldKey); !bytes.Equal(got, []byte("old-value")) {
		t.Fatalf("detached old mutation not durable: got %q", got)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newKey); got != nil {
		t.Fatalf("replacement write escaped after old completion: %q", got)
	}
	if got, err := tx.GetCommittedVersion(); err == nil || got != 0 {
		requireCommitLifetimeCode(t, err, 2015, "stale completion published into replacement")
	}
	if _, err := tx.GetVersionstamp(); err == nil {
		t.Fatal("stale completion published a versionstamp into replacement")
	} else {
		requireCommitLifetimeCode(t, err, 2015, "replacement versionstamp after stale completion")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("replacement commit: %v", err)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newKey); !bytes.Equal(got, []byte("new-value")) {
		t.Fatalf("replacement mutation not durable: got %q", got)
	}
}

func TestFDBDetachedCommitCancelDoesNotReviveHandle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(fmt.Sprintf("commit-lifetime/%s/old", t.Name()))
	tx := db.CreateTransaction()
	pause := installOneShotCommitPause(t, db)
	defer pause.unblock()

	tx.Set(key, []byte("old-value"))
	oldDone := make(chan error, 1)
	go func() { oldDone <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, pause.reached, "detached commit before Cancel")
	tx.Cancel()
	pause.unblock()
	awaitCommitLifetimeResult(t, ctx, oldDone, "detached commit after Cancel")

	if got := readCommitLifetimeValue(t, ctx, db, key); !bytes.Equal(got, []byte("old-value")) {
		t.Fatalf("actual detached outcome was not durable: got %q", got)
	}
	if _, err := tx.GetVersionstamp(); err == nil {
		t.Fatal("canceled handle was revived with an old versionstamp")
	} else {
		requireCommitLifetimeCode(t, err, ErrTransactionCancelled, "versionstamp on canceled handle")
	}
	if _, err := tx.Get(ctx, key); err == nil {
		t.Fatal("old completion revived canceled transaction")
	} else {
		requireCommitLifetimeCode(t, err, ErrTransactionCancelled, "read on canceled handle")
	}
}

func TestFDBOlderDetachedCompletionCannotOverwriteNewerCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := []byte(fmt.Sprintf("commit-lifetime/%s/", t.Name()))
	oldKey := append(append([]byte(nil), prefix...), "old"...)
	newKey := append(append([]byte(nil), prefix...), "new"...)
	newestKey := append(append([]byte(nil), prefix...), "newest"...)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	pause := installOneShotCommitPause(t, db)
	defer pause.unblock()

	tx.Set(oldKey, []byte("old-value"))
	oldDone := make(chan error, 1)
	go func() { oldDone <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, pause.reached, "first-generation commit")

	tx.Reset()
	tx.Set(newKey, []byte("new-value"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("second-generation commit (one-shot seam must not block it): %v", err)
	}
	newVersion, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("second-generation committed version: %v", err)
	}
	newStamp, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatalf("second-generation versionstamp: %v", err)
	}

	tx.Set(newestKey, []byte("newest-value"))
	pause.unblock()
	awaitCommitLifetimeResult(t, ctx, oldDone, "first-generation detached commit")
	if got := readCommitLifetimeValue(t, ctx, db, oldKey); !bytes.Equal(got, []byte("old-value")) {
		t.Fatalf("first-generation mutation not durable: got %q", got)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newKey); !bytes.Equal(got, []byte("new-value")) {
		t.Fatalf("second-generation mutation not durable: got %q", got)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newestKey); got != nil {
		t.Fatalf("newest-generation write escaped before commit: %q", got)
	}
	if got, err := tx.GetCommittedVersion(); err != nil || got != newVersion {
		t.Fatalf("old completion overwrote newer committed version: got (%d, %v), want %d", got, err, newVersion)
	}
	if got, err := tx.GetVersionstamp(); err != nil || !bytes.Equal(got, newStamp) {
		t.Fatalf("old completion overwrote newer versionstamp: got (%x, %v), want %x", got, err, newStamp)
	}
	local, err := tx.Get(ctx, newestKey)
	if err != nil || !bytes.Equal(local, []byte("newest-value")) {
		t.Fatalf("old completion wiped newest-generation local write: got (%q, %v)", local, err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("newest-generation commit: %v", err)
	}
	if got := readCommitLifetimeValue(t, ctx, db, newestKey); !bytes.Equal(got, []byte("newest-value")) {
		t.Fatalf("newest-generation mutation not durable: got %q", got)
	}
}
