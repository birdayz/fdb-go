package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// rangeIssueDB opens a database on the shared container. This file runs in its own fast-lane
// target (BUILD.bazel), apart from client_test's helpers.
func rangeIssueDB(t *testing.T, ctx context.Context) *Database {
	t.Helper()
	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithAPIVersion(730))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func rangeIssueKey(prefix []byte, i int) []byte {
	return append(bytes.Clone(prefix), []byte(fmt.Sprintf("/%02d", i))...)
}

func rangeIssueSeed(t *testing.T, ctx context.Context, db *Database, prefix []byte, n int) {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < n; i++ {
			tx.Set(rangeIssueKey(prefix, i), []byte{byte(i)})
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func fdbCode(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return 0
}

// releaseOnCleanup returns a channel parked reads wait on; it is closed by release or at
// cleanup, so a failed test cannot leave a read holding its lease.
func releaseOnCleanup(t *testing.T) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// StartGetRange returns at once, like fdb_transaction_get_range: N reads over a cold location
// cache have all N location lookups in flight before the first wait.
func TestStartGetRange_LocationLookupsOverlap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	const n = 5
	prefixes := make([][]byte, n)
	for i := range prefixes {
		prefixes[i] = []byte(fmt.Sprintf("%s/%d/", t.Name(), i))
		rangeIssueSeed(t, ctx, db, prefixes[i], 1)
	}
	// A fresh database: nothing is in its location cache.
	cold := rangeIssueDB(t, ctx)
	defer cold.Close()
	tx := cold.CreateTransaction()
	defer tx.Cancel()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	var inflight atomic.Int32
	held, release := releaseOnCleanup(t)
	hook := func() {
		inflight.Add(1)
		<-held
	}
	cold.db.afterLocationSend.Store(&hook)
	started := make(chan []*PendingRange, 1)
	go func() {
		reads := make([]*PendingRange, n)
		for i := range reads {
			reads[i] = tx.StartGetRange(ctx, prefixes[i], append(bytes.Clone(prefixes[i]), 0xff), 0, ByteLimitUnlimited, false)
		}
		started <- reads
	}()
	deadline := time.Now().Add(20 * time.Second)
	for inflight.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := inflight.Load()
	release()
	if got != n {
		t.Fatalf("location lookups in flight before the first wait = %d, want %d", got, n)
	}
	for i, p := range <-started {
		kvs, _, err := p.Get()
		if err != nil || len(kvs) != 1 || !bytes.Equal(kvs[0].Key, rangeIssueKey(prefixes[i], 0)) {
			t.Fatalf("read %d = %v, %v", i, kvs, err)
		}
	}
}

// GetRangeRequests counts client getRange calls, not storage requests: C++ increments it once
// at the top of Transaction::getRangeInternal (NativeAPI.actor.cpp:5808).
func TestGetRangeRequests_OncePerRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	prefix := []byte(t.Name() + "/")
	// 120 rows of 1 KB exceed one reply's REPLY_BYTE_LIMIT (80 KB), so the read takes two replies.
	const rows = 120
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < rows; i++ {
			tx.Set(rangeIssueKey(prefix, i), bytes.Repeat([]byte{'v'}, 1000))
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	before := db.Metrics().TransactionGetRangeRequests
	kvs, _, err := tx.GetRange(ctx, prefix, append(bytes.Clone(prefix), 0xff), 0)
	if err != nil || len(kvs) != rows {
		t.Fatalf("read = %d rows, %v", len(kvs), err)
	}
	if got := db.Metrics().TransactionGetRangeRequests - before; got != 1 {
		t.Fatalf("GetRangeRequests for one multi-reply read = %d, want 1", got)
	}
}

// A panic on a started read's goroutine fails that read (RFC-110) instead of aborting the host,
// and fails the commit: the read never recorded its conflict.
func TestStartGetRange_PanicFailsTheRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	prefix := []byte(t.Name())
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.afterRangeReply = func() { panic("injected") }
	before := db.Metrics().RecoveredPanics
	_, _, err := tx.StartGetRange(ctx, prefix, append(bytes.Clone(prefix), 0xff), 0, ByteLimitUnlimited, false).Get()
	if fdbCode(err) != ErrInternalError {
		t.Fatalf("read = %v, want internal_error", err)
	}
	if db.Metrics().RecoveredPanics == before {
		t.Fatal("the panic was not counted by the backstop")
	}
	tx.Set(append(bytes.Clone(prefix), 'w'), []byte("x"))
	if err := tx.Commit(ctx); fdbCode(err) != ErrInternalError {
		t.Fatalf("commit = %v, want the failed read's internal_error", err)
	}
}

// Commit waits for an issued range read that nobody consumes, and that read's
// conflict counts (ReadYourWrites.actor.cpp:1359-1360, :1767, :388).
func TestStartGetRange_CommitWaitsForUnconsumedRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	prefix := []byte(t.Name())
	rangeIssueSeed(t, ctx, db, prefix, 2)

	tx := db.CreateTransaction()
	defer tx.Cancel()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	held, release := releaseOnCleanup(t)
	tx.afterRangeReply = func() { <-held }
	waiting := make(chan int, 1)
	tx.beforeIssuedReadWait = func(n int) {
		select {
		case waiting <- n:
		default:
		}
	}
	tx.StartGetRange(ctx, rangeIssueKey(prefix, 0), rangeIssueKey(prefix, 1), 0, ByteLimitUnlimited, false)
	tx.Set(append(bytes.Clone(prefix), []byte("/w")...), []byte("x"))

	// A concurrent writer changes the key the unconsumed read covers.
	if _, err := db.Transact(ctx, func(w *Transaction) (any, error) {
		w.Set(rangeIssueKey(prefix, 0), []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	committed := make(chan error, 1)
	go func() { committed <- tx.Commit(ctx) }()
	select {
	case n := <-waiting:
		if n != 1 {
			t.Fatalf("commit waited for %d issued reads, want 1", n)
		}
	case err := <-committed:
		t.Fatalf("commit returned %v without waiting for the issued range read", err)
	case <-time.After(30 * time.Second):
		t.Fatal("commit neither waited for the issued read nor returned")
	}
	release()
	if err := <-committed; fdbCode(err) != ErrNotCommitted {
		t.Fatalf("commit = %v, want not_committed: the unconsumed read's conflict must count", err)
	}
}

// The rows reflect the write map when the read was issued, not when it ran:
// C++ builds the read's RYWIterator before its first wait (:382, WriteMap.h:158).
func TestStartGetRange_IssueTimeWriteView(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	prefix := []byte(t.Name())
	rangeIssueSeed(t, ctx, db, prefix, 3)
	end := append(bytes.Clone(prefix), 0xff)

	for _, withWrites := range []bool{false, true} {
		t.Run(fmt.Sprintf("writes_at_issue=%v", withWrites), func(t *testing.T) {
			tx := db.CreateTransaction()
			defer tx.Cancel()
			if withWrites {
				tx.Set(rangeIssueKey(prefix, 1), []byte("local"))
			}
			held, release := releaseOnCleanup(t)
			tx.afterRangeReply = func() { <-held }
			p := tx.StartGetRange(ctx, prefix, end, 0, ByteLimitUnlimited, false)
			tx.afterRangeReply = nil
			// Writes after the issue must not join it.
			tx.Set(rangeIssueKey(prefix, 7), []byte("late"))
			tx.Clear(rangeIssueKey(prefix, 0))
			release()
			kvs, _, err := p.Get()
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"\x00", "\x01", "\x02"}
			if withWrites {
				want[1] = "local"
			}
			var got []string
			for _, kv := range kvs {
				got = append(got, string(kv.Value))
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("rows = %q, want the issue-time view %q", got, want)
			}
			// A key written after the issue keeps its read conflict, as the
			// issue-time iterator saw an unmodified range there.
			found := false
			for _, r := range tx.readConflicts {
				if bytes.Compare(r.Begin, rangeIssueKey(prefix, 7)) <= 0 && bytes.Compare(rangeIssueKey(prefix, 7), r.End) < 0 {
					found = true
				}
			}
			if !found {
				t.Fatalf("read conflicts %v lost the key written after issue", tx.readConflicts)
			}
		})
	}
}

// A watch whose read fails adds no read conflict: C++ adds it only on the
// read's success (ReadYourWrites.actor.cpp:384-388), and a failed watch read
// does not fail the commit (:1299-1302).
func TestWatchSetup_FailedReadAddsNoConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := rangeIssueDB(t, ctx)
	defer db.Close()
	prefix := []byte(t.Name() + "/")
	// A pending versionstamped key makes its whole candidate-stamp range unreadable
	// (ReadYourWrites.actor.cpp:2263-2277). key is inside that range but carries no write
	// of its own, so a successful read of it would conflict.
	template := append(append(bytes.Clone(prefix), make([]byte, 10)...), 's')
	template = binary.LittleEndian.AppendUint32(template, uint32(len(prefix)))
	key := append(append(bytes.Clone(prefix), bytes.Repeat([]byte{1}, 10)...), 's')

	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.Atomic(MutSetVersionstampedKey, template, []byte("v"))
	if _, _, _, _, _, err := tx.WatchSetup(ctx, key); fdbCode(err) != ErrAccessedUnreadable {
		t.Fatalf("watch setup = %v, want accessed_unreadable", err)
	}
	if _, err := db.Transact(ctx, func(w *Transaction) (any, error) {
		w.Set(key, []byte("other"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit = %v; the failed watch read must not leave a read conflict", err)
	}
}
