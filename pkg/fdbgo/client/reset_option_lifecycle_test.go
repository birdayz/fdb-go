package client

import (
	"testing"
	"time"
)

// txWithDB builds a minimal active transaction bound to a database with zero-value option defaults, so
// reset() can run applyOptionDefaults (which needs tx.db.txDefaults).
func txWithDB() *Transaction {
	tx := &Transaction{db: &database{}}
	tx.state.Store(int32(txStateActive))
	return tx
}

// TestReset_OptionPersistenceSplit pins RFC-171 / #9,#14: NON-persistent options are cleared to their DB
// defaults on BOTH reset paths; PERSISTENT options (timeout / retryLimit / maxRetryDelay) revert to DB
// defaults ONLY on a user Reset() (reset(true)) and are PRESERVED on the OnError retry (reset(false)).
// Pre-RFC-171 Go preserved EVERYTHING on both paths — the divergence this fixes.
func TestReset_OptionPersistenceSplit(t *testing.T) {
	t.Parallel()

	t.Run("non-persistent cleared on OnError retry", func(t *testing.T) {
		t.Parallel()
		tx := txWithDB()
		tx.SetReadSystemKeys()
		tx.SetAccessSystemKeys()
		tx.SetSizeLimit(100)
		tx.SetPriority(PriorityBatch)
		tx.SetTag("t")
		tx.SetSnapshotRYWDisable()

		tx.reset(false) // OnError retry — non-persistent options STILL clear

		if tx.readSystemKeys || tx.writeSystemKeys {
			t.Error("readSystemKeys/writeSystemKeys must clear on retry (non-persistent)")
		}
		if tx.sizeLimit != transactionSizeLimit {
			t.Errorf("sizeLimit must revert to the 10MB DB default, got %d", tx.sizeLimit)
		}
		if tx.priority != 0 {
			t.Errorf("priority must clear on retry, got %v", tx.priority)
		}
		if len(tx.tags) != 0 {
			t.Errorf("tags must clear on retry, got %v", tx.tags)
		}
		if tx.snapshotRYWDisableCount != 0 {
			t.Errorf("snapshotRYWDisableCount must clear on retry, got %d", tx.snapshotRYWDisableCount)
		}
	})

	t.Run("persistent PRESERVED on OnError retry", func(t *testing.T) {
		t.Parallel()
		tx := txWithDB()
		tx.SetTimeout(5000)
		tx.SetRetryLimit(7)
		tx.SetMaxRetryDelay(500)

		tx.reset(false) // OnError retry — persistent options survive

		if tx.timeout != 5000*time.Millisecond {
			t.Errorf("timeout must be PRESERVED on retry, got %v", tx.timeout)
		}
		if !tx.hasRetryLimit || tx.retryLimit != 7 {
			t.Errorf("retryLimit must be PRESERVED on retry, got hasRetryLimit=%v retryLimit=%d", tx.hasRetryLimit, tx.retryLimit)
		}
		if tx.maxRetryDelay != 500*time.Millisecond {
			t.Errorf("maxRetryDelay must be PRESERVED on retry, got %v", tx.maxRetryDelay)
		}
	})

	t.Run("persistent REVERT to DB defaults on user Reset", func(t *testing.T) {
		t.Parallel()
		tx := txWithDB() // db has no timeout / retry-limit / max-retry-delay defaults
		tx.SetTimeout(5000)
		tx.SetRetryLimit(7)
		tx.SetMaxRetryDelay(500)

		tx.reset(true) // user Reset — persistent options revert to DB defaults

		if tx.timeout != 0 {
			t.Errorf("timeout must revert to the DB default (0) on user Reset, got %v", tx.timeout)
		}
		if tx.hasRetryLimit {
			t.Errorf("retryLimit must revert to the no-limit DB default on user Reset, got retryLimit=%d", tx.retryLimit)
		}
		if tx.maxRetryDelay != 0 {
			t.Errorf("maxRetryDelay must revert to the DB default (0) on user Reset, got %v", tx.maxRetryDelay)
		}
	})

	t.Run("persistent per-txn override survives retry but DB default wins after Reset", func(t *testing.T) {
		t.Parallel()
		// A DB-level timeout default of 3000ms; a per-txn override of 5000ms.
		tx := txWithDB()
		tx.db.txDefaults.Timeout = 3000
		tx.SetTimeout(5000)

		tx.reset(false) // retry → per-txn override preserved
		if tx.timeout != 5000*time.Millisecond {
			t.Errorf("retry: per-txn timeout override must survive, got %v", tx.timeout)
		}
		tx.reset(true) // user Reset → reverts to the DB default (3000ms), NOT the per-txn 5000ms
		if tx.timeout != 3000*time.Millisecond {
			t.Errorf("user Reset: timeout must revert to the DB default 3000ms, got %v", tx.timeout)
		}
	})
}

// TestReset_ClearsGrvCacheAndWriteConflictFlags pins that the non-persistent GRV-cache options
// (USE_GRV_CACHE 1101 / SKIP_GRV_CACHE 1102) and writeConflictsDisabled are cleared on BOTH reset paths,
// matching C++ TransactionOptions::clear (NativeAPI.actor.cpp:6148-6149). Leaving useGrvCache set would
// serve a stale cached GRV on the retried txn; leaving writeConflictsDisabled set would silently drop
// write conflict ranges C++ would add on the fresh state. Revert-proof: drop the three clears in
// applyOptionDefaults → these assertions red.
func TestReset_ClearsGrvCacheAndWriteConflictFlags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		userReset bool
	}{
		{"OnError retry (reset false)", false},
		{"user Reset (reset true)", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := txWithDB()
			tx.SetUseGrvCache()
			tx.SetSkipGrvCache()
			tx.SetWriteConflictsDisabled()

			tx.reset(tc.userReset)

			if tx.useGrvCache {
				t.Error("useGrvCache must clear (non-persistent, TransactionOptions::clear)")
			}
			if tx.skipGrvCache {
				t.Error("skipGrvCache must clear (non-persistent, TransactionOptions::clear)")
			}
			if tx.writeConflictsDisabled {
				t.Error("writeConflictsDisabled must clear (non-persistent)")
			}
		})
	}
}

// TestReset_UnlimitedDBRetryDefaultStaysUnlimited pins that a DB default of "unlimited retries"
// is stored as HasRetryLimit=true with RetryLimit=-1 (SetTransactionRetryLimit(-1)). A raw copy on user
// Reset would leave hasRetryLimit=true, retryLimit=-1, and the next OnError's `retryCount >= retryLimit`
// check (0 >= -1) would immediately STOP retrying. Routing through SetRetryLimit collapses the negative to
// hasRetryLimit=false (truly unlimited), exactly as CreateTransaction does. Revert-proof: replace the
// SetRetryLimit call with a raw `hasRetryLimit=td.HasRetryLimit; retryLimit=td.RetryLimit` → red.
func TestReset_UnlimitedDBRetryDefaultStaysUnlimited(t *testing.T) {
	t.Parallel()
	tx := txWithDB()
	tx.db.txDefaults.HasRetryLimit = true
	tx.db.txDefaults.RetryLimit = -1 // unlimited
	tx.SetRetryLimit(3)              // per-txn override

	tx.reset(true) // user Reset → revert to the DB default, which is "unlimited"

	if tx.hasRetryLimit {
		t.Errorf("user Reset must collapse the unlimited (-1) DB retry default to hasRetryLimit=false, got retryLimit=%d", tx.retryLimit)
	}
}
