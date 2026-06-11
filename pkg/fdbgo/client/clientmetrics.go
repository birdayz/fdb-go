package client

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// ClientMetrics holds the per-Database transaction counters — the operational
// subset of C++ DatabaseContext's CounterCollection (DatabaseContext.h:585-635),
// with the C++ names and the C++ increment sites (RFC-097):
//
//   - commit started/completed: commitMutations entry (NativeAPI.actor.cpp:6808;
//     the empty/read-only fast path at :6800-6806 does NOT count) / tryCommit
//     success (:6673). Started−Completed = failed or in-flight (intentional
//     asymmetry, matching C++).
//   - the per-error-code retry counters: Transaction::onError's arms
//     (:7749-:7772).
//   - read versions completed (+ per priority): extractReadVersion
//     (:7428-7440) — i.e. per transaction served by a REAL GRV reply; cache
//     hits do not count there in C++ and do not count here.
//
// transactionRetries is a Go-only aggregate (total OnError-sanctioned retries);
// C++ tracks retries per-transaction only (trState->numErrors). It also counts
// codes C++ retries WITHOUT a counter (database_locked 1038,
// blob_granule_request_failed 1079 — :7743-7747, plus the Go-internal 1200 and
// future-proof 1235/1242).
//
// Counters are monotonic; consumers poll Database.Metrics() and diff. There is
// no periodic trace emission (C++ logs TransactionMetrics on a timer; the Go
// analog is the consumer's scrape interval) — the only gap is no persisted
// history if nothing polls before process death.
type ClientMetrics struct {
	transactionsCommitStarted   atomic.Int64
	transactionsCommitCompleted atomic.Int64

	transactionsNotCommitted        atomic.Int64 // 1020 — the conflict counter
	transactionsMaybeCommitted      atomic.Int64 // 1021
	transactionsResourceConstrained atomic.Int64 // 1042, 1078
	transactionsProcessBehind       atomic.Int64 // 1037
	transactionsThrottled           atomic.Int64 // 1051, 1213, 1223
	transactionsTooOld              atomic.Int64 // 1007
	transactionsFutureVersions      atomic.Int64 // 1009

	transactionReadVersionsCompleted          atomic.Int64
	transactionBatchReadVersionsCompleted     atomic.Int64
	transactionDefaultReadVersionsCompleted   atomic.Int64
	transactionImmediateReadVersionsCompleted atomic.Int64

	transactionRetries atomic.Int64 // Go-only aggregate, see doc comment
}

// countRetry records an OnError-sanctioned retry of the given error code,
// mirroring C++ onError's per-code counters (NativeAPI.actor.cpp:7749-7772).
// Codes without a C++ counter still count toward the retry aggregate.
func (m *ClientMetrics) countRetry(code int) {
	m.transactionRetries.Add(1)
	switch code {
	case ErrNotCommitted:
		m.transactionsNotCommitted.Add(1)
	case ErrCommitUnknownResult:
		m.transactionsMaybeCommitted.Add(1)
	case ErrProxyMemoryLimitExceeded, ErrGrvProxyMemoryLimit:
		m.transactionsResourceConstrained.Add(1)
	case ErrProcessBehind:
		m.transactionsProcessBehind.Add(1)
	case ErrBatchTransactionThrottled, ErrTagThrottled, ErrProxyTagThrottled:
		m.transactionsThrottled.Add(1)
	case ErrTransactionTooOld:
		m.transactionsTooOld.Add(1)
	case ErrFutureVersion:
		m.transactionsFutureVersions.Add(1)
	}
}

// countGRVBatchCompleted records n transactions served by one REAL GRV reply
// at the given batcher priority — the C++ extractReadVersion counters
// (:7428-7440). Called from the batcher flush with the batch size; the
// background refresher has no waiters and adds nothing, and cache hits never
// reach here (C++ parity: its cached path returns before the counters).
func (m *ClientMetrics) countGRVBatchCompleted(priority uint32, n int) {
	if n <= 0 {
		return
	}
	m.transactionReadVersionsCompleted.Add(int64(n))
	switch priority {
	case grvPriorityBatch:
		m.transactionBatchReadVersionsCompleted.Add(int64(n))
	case grvPrioritySystemImmediate:
		m.transactionImmediateReadVersionsCompleted.Add(int64(n))
	default:
		m.transactionDefaultReadVersionsCompleted.Add(int64(n))
	}
}

// ClientMetricsSnapshot is a point-in-time copy of the counters. Fields are
// monotonic; diff two snapshots for rates.
type ClientMetricsSnapshot struct {
	TransactionsCommitStarted   int64
	TransactionsCommitCompleted int64

	TransactionsNotCommitted        int64
	TransactionsMaybeCommitted      int64
	TransactionsResourceConstrained int64
	TransactionsProcessBehind       int64
	TransactionsThrottled           int64
	TransactionsTooOld              int64
	TransactionsFutureVersions      int64

	TransactionReadVersionsCompleted          int64
	TransactionBatchReadVersionsCompleted     int64
	TransactionDefaultReadVersionsCompleted   int64
	TransactionImmediateReadVersionsCompleted int64

	TransactionRetries int64
}

// Snapshot returns a point-in-time copy of all counters.
func (m *ClientMetrics) Snapshot() ClientMetricsSnapshot {
	return ClientMetricsSnapshot{
		TransactionsCommitStarted:   m.transactionsCommitStarted.Load(),
		TransactionsCommitCompleted: m.transactionsCommitCompleted.Load(),

		TransactionsNotCommitted:        m.transactionsNotCommitted.Load(),
		TransactionsMaybeCommitted:      m.transactionsMaybeCommitted.Load(),
		TransactionsResourceConstrained: m.transactionsResourceConstrained.Load(),
		TransactionsProcessBehind:       m.transactionsProcessBehind.Load(),
		TransactionsThrottled:           m.transactionsThrottled.Load(),
		TransactionsTooOld:              m.transactionsTooOld.Load(),
		TransactionsFutureVersions:      m.transactionsFutureVersions.Load(),

		TransactionReadVersionsCompleted:          m.transactionReadVersionsCompleted.Load(),
		TransactionBatchReadVersionsCompleted:     m.transactionBatchReadVersionsCompleted.Load(),
		TransactionDefaultReadVersionsCompleted:   m.transactionDefaultReadVersionsCompleted.Load(),
		TransactionImmediateReadVersionsCompleted: m.transactionImmediateReadVersionsCompleted.Load(),

		TransactionRetries: m.transactionRetries.Load(),
	}
}

// logRetryEvent emits the operational slog event for an OnError-sanctioned
// retry (P1.2 remainder): commit_unknown_result at Warn (rare, an ambiguous
// write), everything else at Debug (a conflict storm at Warn would melt the
// log — the COUNTER is the storm signal; rates belong on dashboards). The
// Enabled guard keeps the disabled-level hot path at one branch.
func logRetryEvent(ctx context.Context, logger *slog.Logger, code int, retryCount int) {
	level := slog.LevelDebug
	if code == ErrCommitUnknownResult {
		level = slog.LevelWarn
	}
	if !logger.Enabled(ctx, level) {
		return
	}
	logger.Log(ctx, level, "fdb client transaction retry",
		"fdb_error_code", code,
		"retry_count", retryCount,
	)
}

// countRetryAndLog is the single per-retry hook used by OnError's retryable
// arms: counter + (guarded) operational event. nil-logger-safe for
// hand-constructed test databases.
func (db *database) countRetryAndLog(ctx context.Context, code, retryCount int) {
	db.metrics.countRetry(code)
	if db.logger != nil {
		logRetryEvent(ctx, db.logger, code, retryCount)
	}
}
