package fdb

import (
	"context"
	"errors"
	"sync"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
)

// TestDatabaseDefaults_ConcurrentSetAndApply pins the concurrency contract the
// `Database` doc comment asserts ("safe for concurrent use by multiple goroutines"):
// a database-level transaction default may be set while another goroutine is
// starting a transaction. libfdb_c gets this for free — fdb_database_set_option
// and transaction creation both run confined to the single network thread — so
// the pure-Go client must serialize them explicitly instead of racing on the
// shared internalDB.
//
// The observed failure was a real -race abort in CI: NewFDBDatabaseWithBackend
// writes readSystemKeys on a process-shared Database while a concurrent query's
// TransactCtx reads the same defaults in applyTxDefaults.
//
// That CI run (31238513459) reported four failing sqldriver tests —
// TestFDB_StoreTimerExporter_IndexScansAreCounted, TestFDB_UpdateSetNullIndexProbe,
// TestFDB_ScalarMathProbe/power, TestFDB_StoreTimerExporter_CountsRealSQLWork.
// They are held to be four symptoms of ONE cause, not four defects: the whole log
// contained exactly ONE "WARNING: DATA RACE" (measured, `grep -c`), and the four are
// simply what was in flight when the detector aborted the shared test binary. They
// were not individually re-run after the fix.
//
// That claim is falsifiable, and these are the observations that would break it:
//   - any of the four reds on a -race lane at a commit that CONTAINS this fix;
//   - any of the four reds with "--- FAIL" but WITHOUT an accompanying
//     "race detected during execution of test" — that is an ordinary assertion
//     failure and therefore an independent cause;
//   - a race report whose reader/writer frames are anything other than
//     applyTxDefaults against a DatabaseOptions setter — that is a second racing
//     pair this fix does not address.
//
// If any of those is seen, the single-cause reasoning is wrong and the specific test
// needs its own root-cause, not a re-run.
//
// This test only fails under -race. If it stops failing with the synchronization
// removed, the defaults have been made unreachable from one of the two sides and
// the invariant needs re-checking.
func TestDatabaseDefaults_ConcurrentSetAndApply(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	db := Database{d: idb}
	opts := db.Options()

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: the NewFDBDatabaseWithBackend side of the reported race, plus the
	// other facade defaults, including persistent and non-persistent fields from
	// the same immutable snapshot.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = opts.SetReadSystemKeys()
			_ = opts.SetTransactionTimeout(int64(i%7) + 1)
			_ = opts.SetTransactionRetryLimit(int64(i % 5))
			_ = opts.SetTransactionMaxRetryDelay(int64(i%3) + 1)
			_ = opts.SetTransactionSizeLimit(int64(i%11) + 1)
			_ = opts.SetTransactionBypassUnreadable()
			_ = opts.SetTransactionCausalReadRisky()
			_ = opts.SetSnapshotRywDisable()
			_ = opts.SetSnapshotRywEnable()
		}
	}()

	// Reader: the TransactCtx side — applyTxDefaults on a fresh transaction.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tr, _ := newBareOptionsTx()
			db.applyTxDefaults(tr.t, true)
		}
	}()

	wg.Wait()
}

func TestDatabaseDefaults_CapturedSnapshotKeepsBothGroups(t *testing.T) {
	t.Parallel()
	db := Database{d: &internalDB{}}
	opts := db.Options()
	if err := opts.SetTransactionRetryLimit(0); err != nil {
		t.Fatal(err)
	}
	captured := make(chan struct{})
	apply := make(chan struct{})
	applied := make(chan *client.Transaction, 1)
	go func() {
		snapshot := db.d.defaults()
		close(captured)
		<-apply
		tr, inner := newBareOptionsTx()
		snapshot.apply(tr.t, true)
		applied <- inner
	}()
	<-captured
	// Publish (retry=2, bypass=false), then (retry=2, bypass=true) while
	// the captured (retry=0, bypass=false) generation is parked before apply.
	if err := opts.SetTransactionRetryLimit(2); err != nil {
		t.Error(err)
	}
	if err := opts.SetTransactionBypassUnreadable(); err != nil {
		t.Error(err)
	}
	close(apply)
	old := <-applied
	defer old.Cancel()
	if old.BypassUnreadable() {
		t.Fatal("captured generation picked up a later non-persistent default")
	}
	var fe *wire.FDBError
	if err := old.OnError(context.Background(), &wire.FDBError{Code: 1020}); !errors.As(err, &fe) || fe.Code != 1020 {
		t.Fatalf("captured generation lost its no-retry default: %v", err)
	}

	tr, fresh := newBareOptionsTx()
	defer fresh.Cancel()
	db.applyTxDefaults(tr.t, true)
	if !fresh.BypassUnreadable() {
		t.Fatal("fresh generation lost the new non-persistent default")
	}
	for retry := 0; retry <= 2; retry++ {
		err := fresh.OnError(context.Background(), &wire.FDBError{Code: 1020})
		if retry < 2 && err != nil {
			t.Fatalf("fresh retry %d: %v", retry, err)
		}
		if retry == 2 && (!errors.As(err, &fe) || fe.Code != 1020) {
			t.Fatalf("fresh generation lost its retry limit: %v", err)
		}
	}
}
