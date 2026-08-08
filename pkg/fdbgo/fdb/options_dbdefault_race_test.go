package fdb

import (
	"sync"
	"testing"
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
	// other honored DB defaults so every field of the set is exercised, not just
	// the one the CI report happened to name.
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
			db.applyTxDefaults(tr.t)
		}
	}()

	wg.Wait()
}
