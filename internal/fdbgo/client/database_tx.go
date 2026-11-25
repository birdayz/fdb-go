package client

import (
	"context"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
)

// TransactionDefaults holds database-level defaults applied to every new
// transaction. Matches C++ DatabaseContext::transactionDefaults.
type TransactionDefaults struct {
	Timeout        int64 // FDB_DB_OPTION_TRANSACTION_TIMEOUT (ms), 0 = disabled
	RetryLimit     int   // FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT
	MaxRetryDelay  int64 // FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY (ms), 0 = use default
	SizeLimit      int64 // FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT; 0/unset → 10 MB default (C++ has no "disabled" state)
	HasRetryLimit  bool
	ReadSystemKeys bool // read \xff/* keys by default
	AccessSysKeys  bool // read+write \xff/* keys by default
	// ReportConflictingKeys is FDB_DB_OPTION_TRANSACTION_REPORT_CONFLICTING_KEYS (702).
	ReportConflictingKeys bool
}

// Database is the public API entry point.
// C++: Database is a Reference<DatabaseContext> — a handle, not state.
//
// Safe for concurrent use by multiple goroutines.
type Database struct {
	db *database
}

// Metrics returns a point-in-time snapshot of this handle's operational
// counters (RFC-097) — the C++ DatabaseContext TransactionMetrics subset.
// Counters are monotonic; poll and diff for rates. This is the export hook:
// Prometheus/OTel consumers are pull-based readers of exactly this shape
// (see pkg/fdbgo/fdbmetrics for a ready-made scrape handler).
func (d *Database) Metrics() ClientMetricsSnapshot {
	return d.db.metrics.Snapshot()
}

// SetHedgeEnabled enables budget-funded speculative read requests (default on).
// Disabled, no read hedges; lone replies still accrue budget (RFC-281).
func (d *Database) SetHedgeEnabled(enabled bool) {
	d.db.hedgeEnabled.Store(enabled)
}

// HedgeEnabled returns whether speculative second requests are active.
func (d *Database) HedgeEnabled() bool {
	return d.db.hedgeEnabled.Load()
}

// SetDialFunc sets a custom dialer for all new connections.
// Must be called before OpenDatabase or any transactions.
func (d *Database) SetDialFunc(fn transport.DialFunc) {
	d.db.dialFn = fn
}

// ConnectionLinger reports the active write-delay and adaptive-linger settings
// of a pooled or most recently dialed connection.
func (d *Database) ConnectionLinger() (minDelay, maxDelay, adaptive time.Duration, waits bool) {
	d.db.connMu.Lock()
	defer d.db.connMu.Unlock()
	for _, c := range d.db.connPool {
		minDelay, maxDelay, waits = c.CoalesceDelay()
		return minDelay, maxDelay, c.AdaptiveLinger(), waits
	}
	if d.db.hasConnLinger {
		return d.db.lastMinDelay, d.db.lastMaxDelay, d.db.lastAdaptive, d.db.lastWaits
	}
	return 0, 0, 0, false
}

// Transact runs a function in a transaction with automatic retry.
// This is the primary API for interacting with FDB.
// The transaction is reused across retries — retryCount and backoff escalate.
func (d *Database) Transact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.createTransaction(true)
	defer tx.endTxSpan()
	defer tx.releaseTransactTimeout()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr // non-retryable
			}
			continue // tx has been reset in place, retryCount/backoff preserved
		}

		// A caller cancellation during fn must not start a new commit.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Commit owns the cancellable GRV / detached-native split; prefetching here
		// would force a GRV on the no-write fast path.
		tx.transactFinalCommit.Store(true)
		err = tx.Commit(ctx)
		tx.transactFinalCommit.Store(false)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue // for commit_unknown_result: self-conflicting applied
		}

		return result, nil
	}
}

// ReadTransact runs a read-only function in a transaction with automatic retry.
// The transaction is never committed — only reads are performed.
func (d *Database) ReadTransact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.createTransaction(true)
	defer tx.endTxSpan()
	defer tx.releaseTransactTimeout()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue
		}
		return result, nil
	}
}

// CreateTransaction creates a new transaction.
// Database-level defaults (timeout, retry limit, system key access) are applied.
func (d *Database) CreateTransaction() *Transaction {
	return d.createTransaction(false)
}

func (d *Database) createTransaction(transactScoped bool) *Transaction {
	tx := &Transaction{
		committedVersion: -1, // C++ TransactionState::committedVersion starts at invalidVersion.
		readVersionState: &readVersionState{},
		db:               d.db,
		txOptions:        txOptions{tenantId: NoTenantID},
		creationTime:     time.Now(),
		transactScoped:   transactScoped,
	}
	// Apply database-level defaults (matches C++ applyTxDefaults). One Load, so every
	// option below comes from the SAME snapshot.
	td := d.db.defaults()
	if td.Timeout > 0 {
		tx.SetTimeout(td.Timeout)
	}
	if td.HasRetryLimit {
		tx.SetRetryLimit(int64(td.RetryLimit))
	}
	if td.MaxRetryDelay > 0 {
		tx.SetMaxRetryDelay(td.MaxRetryDelay)
	}
	// C++ defaults EVERY transaction's sizeLimit to CLIENT_KNOBS->TRANSACTION_SIZE_LIMIT
	// (NativeAPI.actor.cpp:6133) — there is no "disabled" state: the SIZE_LIMIT option
	// floor is 32 and its default/ceiling is 10 MB. A 0/unset DB default must therefore
	// still enforce 10 MB, otherwise the Go client commits an oversized transaction that
	// libfdb_c rejects client-side with transaction_too_large (2101) — a wire divergence.
	// The DB option, when set (>0), lowers the limit; an out-of-range one leaves
	// 10 MB and poisons the txn with 2006, as the tx-level option does.
	tx.SetSizeLimit(transactionSizeLimit)
	if td.SizeLimit > 0 {
		tx.SetSizeLimit(td.SizeLimit)
	}
	if td.ReadSystemKeys {
		tx.SetReadSystemKeys()
	}
	if td.AccessSysKeys {
		tx.SetAccessSystemKeys()
	}
	tx.reportConflictingKeys = td.ReportConflictingKeys
	// Generate the per-transaction trace span (RFC-115 §4) — a real, default-unsampled
	// SpanContext stamped on every request, matching C++ (which never sends a zero span).
	// No SPAN_PARENT can be set yet, so this is regenerateSpan's fresh-span branch.
	tx.initialSpan = newSpanContext(d.db.tracingSampleRate)
	tx.spanContext.Store(&tx.initialSpan)
	return tx
}

// defaults returns the current immutable snapshot of the database-level
// transaction defaults. Safe to call from any goroutine.
func (d *database) defaults() TransactionDefaults {
	if p := d.txDefaults.Load(); p != nil {
		return *p
	}
	return TransactionDefaults{}
}

// mutateDefaults applies fn to a COPY of the current defaults and publishes the
// result atomically, so a concurrent defaults() reader sees either the whole old
// snapshot or the whole new one — never a half-applied option.
func (d *database) mutateDefaults(fn func(*TransactionDefaults)) {
	d.txDefaultsMu.Lock()
	defer d.txDefaultsMu.Unlock()
	next := d.defaults()
	fn(&next)
	d.txDefaults.Store(&next)
}

// SetTransactionTimeout sets the default timeout (in milliseconds) for all
// transactions created from this database. Matches FDB_DB_OPTION_TRANSACTION_TIMEOUT.
func (d *Database) SetTransactionTimeout(ms int64) {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.Timeout = ms })
}

// SetTransactionRetryLimit sets the default retry limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT.
func (d *Database) SetTransactionRetryLimit(retries int64) {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.RetryLimit, td.HasRetryLimit = int(retries), true })
}

// SetTransactionMaxRetryDelay sets the default max retry delay (ms) for all
// transactions. Matches FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY.
func (d *Database) SetTransactionMaxRetryDelay(ms int64) {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.MaxRetryDelay = ms })
}

// SetTransactionSizeLimit sets the default size limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT.
func (d *Database) SetTransactionSizeLimit(limit int64) {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.SizeLimit = limit })
}

// SetDefaultReadSystemKeys makes all new transactions automatically call
// SetReadSystemKeys(), allowing reads of \xff-prefixed system keys.
func (d *Database) SetDefaultReadSystemKeys() {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.ReadSystemKeys = true })
}

// SetDefaultAccessSystemKeys makes all new transactions automatically call
// SetAccessSystemKeys(), allowing reads AND writes of \xff-prefixed system keys.
func (d *Database) SetDefaultAccessSystemKeys() {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.AccessSysKeys = true })
}

// SetMaxWatches sets the cap on concurrently-outstanding watches for this Database
// (FDB_DB_OPTION_MAX_WATCHES). Once the count would exceed it, a new watch fails with
// too_many_watches (1032). Matches C++ DatabaseContext maxOutstandingWatches; default 10000.
// 0 is a REAL cap (no watches allowed — the first fails), NOT "unlimited". An out-of-range value
// (< 0 or > ABSOLUTE_MAX_WATCHES) is REJECTED with invalid_option_value (2006) and the cap is left
// UNCHANGED — C++ extractIntOption(value, 0, ABSOLUTE_MAX_WATCHES) throws on out-of-range, it does
// NOT clamp (NativeAPI.actor.cpp:2092-2102 / :2139).
func (d *Database) SetMaxWatches(n int64) error {
	if n < 0 || n > absoluteMaxWatches {
		return &wire.FDBError{Code: ErrInvalidOptionValue} // cap unchanged
	}
	d.db.maxWatches.Store(n)
	return nil
}

// InvalidateGRVCache resets the GRV cache so the next transaction fetches
// a fresh read version from the GRV proxy. Use after external writes.
func (d *Database) InvalidateGRVCache() {
	d.db.grvCache.invalidate()
}

// GetDBInfo returns the current cluster topology (proxy lists, cluster ID).
// Returns nil if not yet connected.
func (d *Database) GetDBInfo() *DBInfo {
	return d.db.dbInfo.Load()
}

// registerBackgroundGoroutine reserves a db.wg slot for a lazily-started background goroutine (the
// GRV-cache refresher), returning true iff it was reserved. It returns FALSE once Close() has begun, so
// a slot is never Add()ed after Close's db.wg.Wait() is (or is about to be) waiting — which, once the
// topology-monitor slot has drained the counter toward zero, would panic with
// "sync: WaitGroup misuse: Add called concurrently with Wait". The closed check and the Add share
// closeMu with Close's `closed = true`, so the check-and-Add is atomic w.r.t. that store: either the
// Add happens before closed is set (so the counter is still ≥1 from the topology monitor and Close's
// Wait observes this slot) or closed is already set (so we skip). The caller's goroutine MUST
// db.wg.Done() on exit.
func (db *database) registerBackgroundGoroutine() bool {
	db.closeMu.Lock()
	defer db.closeMu.Unlock()
	if db.closed {
		return false
	}
	db.wg.Add(1)
	return true
}

// Close is idempotent. It joins database workers, shared dials and all connection
// teardown, including transports already evicted from the pool.
func (d *Database) Close() error {
	d.db.closeOnce.Do(func() {
		// Set closed BEFORE cancel()/Wait() so a concurrent GRV-cache opt-in can't start the background
		// refresher (register a new wg slot) after we've begun waiting. The refresher launch checks
		// `closed` under closeMu and skips its Add when set (grv.go); taking the same lock here makes the
		// check-and-Add atomic w.r.t. this store, so the Add never races Wait at a zero counter.
		d.db.closeMu.Lock()
		d.db.closed = true
		d.db.closeMu.Unlock()
		d.db.cancel()
		// Native commits may be blocked in SendFrame: close their sockets before joining.
		d.db.connMu.Lock()
		connections := make([]*transport.Conn, 0, len(d.db.connPool))
		for addr, conn := range d.db.connPool {
			connections = append(connections, conn)
			delete(d.db.connPool, addr)
		}
		d.db.connMu.Unlock()
		for _, conn := range connections {
			conn.Close()
		}
		if h := d.db.hooks.Load(); h != nil {
			if hook := h.beforeCloseJoin.Load(); hook != nil {
				(*hook)()
			}
		}
		d.db.wg.Wait()
	})
	return nil
}
