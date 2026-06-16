package client

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
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

	// grvCacheHits counts transactions served a read version from the GRV cache
	// (the USE_GRV_CACHE opt-in fast path, RFC-104) — the complement of
	// transactionReadVersionsCompleted (real GRV replies, cache hits excluded).
	// Go-only observability + the deterministic test seam that distinguishes a
	// cache-off transaction (grvCacheHits stays 0) from a stale/served one.
	grvCacheHits atomic.Int64

	transactionRetries atomic.Int64 // Go-only aggregate, see doc comment

	// recoveredPanics counts panics recovered by the background-goroutine
	// backstop (RFC-110) — the analog of C++ Net2::run's SevError "TaskError"
	// events: a long-lived/background goroutine hit a panic and survived instead
	// of aborting the host. A steady climb means a loop is stuck re-panicking
	// (deterministic bug); the rate is the storm signal (the per-occurrence log
	// is rate-limited). recoveredPanicsConsecutiveMax is the high-water of any
	// single loop's consecutive-panic streak (unmaskable across loops — a healthy
	// loop's reset cannot hide a stuck one), so a dashboard can alert "a loop
	// re-panicked N× in a row." Both are Go-only (C++ has no goroutine model).
	recoveredPanics               atomic.Int64
	recoveredPanicsConsecutiveMax atomic.Int64

	// clientConnectionFailures and coordinatorChanges (RFC-114) are Go-only
	// observability — C++ has no DatabaseContext CounterCollection twin; it emits
	// connection-lifecycle TraceEvents and drives IFailureMonitor. The precedent
	// for a Go-only observability counter is recoveredPanics above. Incremented at
	// the single connection-failure sink (handleConnError, topology.go) and on a
	// followed coordinator forward (topology.go) respectively.
	clientConnectionFailures atomic.Int64
	coordinatorChanges       atomic.Int64

	// Latency distributions (RFC-114) — ports of DatabaseContext's
	// DDSketch<double> members, sampled in SECONDS to match C++ now()-as-double.
	// Each zero value is ready to use (lazy bucket map), so no constructor.
	readLatency   latencySketch // GetValue round-trip       (C++ readLatencies,   NativeAPI.actor.cpp:3698)
	commitLatency latencySketch // commit round-trip          (C++ commitLatencies, :6681)
	grvLatency    latencySketch // GRV round-trip             (C++ GRVLatencies,    :7417)
	totalLatency  latencySketch // commit: now-creationTime   (C++ latencies,       :6682)
}

// observeReadLatency/Commit/GRV/Total record one latency sample (RFC-114). nil-safe
// for hand-constructed test databases is unnecessary — the sketches are value
// fields of ClientMetrics, always present.
func (m *ClientMetrics) observeReadLatency(d time.Duration)   { m.readLatency.addSample(d.Seconds()) }
func (m *ClientMetrics) observeCommitLatency(d time.Duration) { m.commitLatency.addSample(d.Seconds()) }
func (m *ClientMetrics) observeGRVLatency(d time.Duration)    { m.grvLatency.addSample(d.Seconds()) }
func (m *ClientMetrics) observeTotalLatency(d time.Duration)  { m.totalLatency.addSample(d.Seconds()) }

// countConnectionFailure records one connection/dial failure (RFC-114), the
// single sink handleConnError routes through.
func (m *ClientMetrics) countConnectionFailure() { m.clientConnectionFailures.Add(1) }

// countCoordinatorChange records one followed coordinator forward (RFC-114).
func (m *ClientMetrics) countCoordinatorChange() { m.coordinatorChanges.Add(1) }

// LatencyStats is a point-in-time summary of one latency distribution (RFC-114),
// in SECONDS. Unlike the monotonic counters, these are instantaneous distribution
// reads — diffing two snapshots is not meaningful. Median/P90/P99 are DDSketch
// quantiles (±0.5% relative error). C++ emits p90/p98 only for the aggregate
// transaction latency; Go exposes per-category median/p90/p99 — a local-metric
// superset (p99 is the conventional Prometheus tail, vs C++'s trace-only p98).
type LatencyStats struct {
	Count  int64
	Sum    float64 // seconds — the summary _sum (and Mean = Sum/Count)
	Mean   float64
	Median float64 // p50
	P90    float64
	P99    float64
	Max    float64
}

// countRecoveredPanic records one recovered background-goroutine panic (RFC-110)
// and raises the consecutive-streak high-water mark. consecutive is the calling
// backstop's current streak (≥1).
func (m *ClientMetrics) countRecoveredPanic(consecutive int) {
	m.recoveredPanics.Add(1)
	for {
		cur := m.recoveredPanicsConsecutiveMax.Load()
		if int64(consecutive) <= cur {
			return
		}
		if m.recoveredPanicsConsecutiveMax.CompareAndSwap(cur, int64(consecutive)) {
			return
		}
	}
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

// countGRVCacheHit records one transaction served a read version from the GRV
// cache (USE_GRV_CACHE opt-in fast path, RFC-104). The complement of
// countGRVBatchCompleted, which counts only real GRV replies.
func (m *ClientMetrics) countGRVCacheHit() {
	m.grvCacheHits.Add(1)
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
	GRVCacheHits                              int64 // RFC-104: served from the GRV cache (opt-in)

	TransactionRetries int64

	RecoveredPanics               int64 // RFC-110: panics recovered by the goroutine backstop
	RecoveredPanicsConsecutiveMax int64 // RFC-110: high-water of any loop's consecutive-panic streak

	ClientConnectionFailures int64 // RFC-114: connection/dial failures (Go-only observability)
	CoordinatorChanges       int64 // RFC-114: followed coordinator forwards (Go-only observability)

	// Latency distributions (RFC-114), seconds. Instantaneous reads, not monotonic.
	ReadLatency        LatencyStats // GetValue round-trip
	CommitLatency      LatencyStats // commit round-trip
	GRVLatency         LatencyStats // GRV round-trip
	TransactionLatency LatencyStats // total transaction latency (C++ "latencies")
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
		GRVCacheHits: m.grvCacheHits.Load(),

		TransactionRetries: m.transactionRetries.Load(),

		RecoveredPanics:               m.recoveredPanics.Load(),
		RecoveredPanicsConsecutiveMax: m.recoveredPanicsConsecutiveMax.Load(),

		ClientConnectionFailures: m.clientConnectionFailures.Load(),
		CoordinatorChanges:       m.coordinatorChanges.Load(),

		ReadLatency:        m.readLatency.stats(),
		CommitLatency:      m.commitLatency.stats(),
		GRVLatency:         m.grvLatency.stats(),
		TransactionLatency: m.totalLatency.stats(),
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
