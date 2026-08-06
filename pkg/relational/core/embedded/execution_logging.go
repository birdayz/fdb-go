package embedded

import (
	"context"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ExecutionStats is the diagnostic record emitted once per statement
// EXECUTION, the post-execution counterpart to PlanGenerationInfo (RFC-211).
//
// It exists because a multi-tenant operator cannot attribute cluster load to a
// tenant from planning telemetry alone: PlanGenerationInfo reports what the
// planner decided, never what the statement then consumed.
//
// The scanned counters are a port, not an invention. Java reads exactly these
// off the execution state after the fact — ExecuteState.getRecordsScanned()
// (ExecuteState.java:114) and getBytesScanned() (:122), backed by the mutable
// RecordScanLimiter/ByteScanLimiter pair (ExecuteState.java:44-48) that the
// caller owns and that therefore outlives the limit trip. Go's
// ScanLimiterState is that pair.
//
// The per-statement AGGREGATION is the Go-only read-side extension: Java
// discards the counts at every ExecuteState.reset() (:74, the way each
// continuation segment gets a fresh budget) and surfaces nothing to a SQL
// caller at all — fdb-relational-api/jdbc/grpc carry no metric surface, and
// fdb-relational-core never reads getRecordsScanned() back. Nothing here
// touches the wire: these counters already existed to enforce limits, and this
// only reports them.
type ExecutionStats struct {
	// SQL is the original whitespace-preserved query text, truncated to
	// MaxLoggedSQLLength. Same source and same bound as PlanGenerationInfo.SQL,
	// so the two records join on it.
	SQL string
	// PlanHash is the deterministic hash of the executed physical plan — the
	// join key to the PlanGenerationInfo that produced it. A cached plan
	// executes many times under one planning record, so the hash is what
	// relates the N execution records back to the one plan.
	PlanHash uint64
	// ExecutionDuration is the wall-clock time from the start of Execute to
	// the statement's completion — across every page, not just the first.
	// Java's TOTAL_EXECUTE_QUERY (RelationalMetric.java:88), which
	// AbstractEmbeddedStatement clocks around the same span.
	ExecutionDuration time.Duration
	// RowsReturned is the number of rows actually handed to the caller across
	// all pages. DML reports RowsAffected instead and leaves this zero: a DML
	// statement returns a count, not a result set, so it never passes through
	// the row-returning path this counts.
	RowsReturned int64
	// RowsAffected is the number of records mutated by a DML statement, and is
	// zero for a SELECT.
	RowsAffected int64
	// RecordsScanned is the total records charged against the scan limiter
	// across every page AND every transaction retry. A retried attempt's scan
	// is counted: the cluster served those reads, and cost attribution that
	// hid them would understate exactly the tenants worth knowing about.
	RecordsScanned int64
	// BytesScanned is the total key+value bytes charged across every page and
	// retry, on the same basis as RecordsScanned.
	BytesScanned int64
	// Pages is the number of page fetches this statement performed. Each page
	// is its own transaction in auto-commit, so this is also the auto-commit
	// transaction count before retries.
	Pages int
	// Retries is the number of transaction attempts beyond the first, summed
	// over all pages. An explicit SQL transaction is never retried
	// (runInCapturedTx calls the closure directly), so this is always 0 there.
	Retries int
	// SlowQuery is true when ExecutionDuration exceeded the connection's
	// slow-query threshold — the same knob PlanGenerationInfo.SlowQuery reads,
	// applied to the other half of the statement.
	SlowQuery bool
	// Err is the error that ended the statement, or nil on success. A
	// statement killed by a scan limit reports its consumed counters here
	// alongside the 54F01: the counters are charged per attempt on the way
	// out, not read at a success-only checkpoint.
	Err error
}

// ExecutionStatsLogger receives one callback per statement execution. A nil
// logger is silent. Sampling and log-level policy are the handler's
// responsibility, exactly as for PlanGenerationLogger: the engine always
// emits, and the handler decides volume and sink.
//
// This is deliberately a SECOND interface rather than a second method on
// PlanGenerationLogger. Java can grow MetricCollector because Java has default
// methods and uses them for precisely this (MetricCollector.java:78,89,99,107);
// Go has no equivalent, so adding a method would break every existing
// implementer at compile time. Java also keeps the two mechanisms apart —
// planning through PlanGenerator's finally block into RelationalLoggingUtil,
// execution through AbstractEmbeddedStatement's metricCollector.clock — and
// the lifetimes really do differ: a cached plan is planned once and executed
// many times, so planning and execution records do not pair up 1:1.
type ExecutionStatsLogger interface {
	LogExecutionStats(ctx context.Context, stats ExecutionStats)
}

// execLogScope accumulates one statement's execution record and emits it on
// finish. A nil scope is a no-op throughout, so a nil logger (the production
// default) costs nothing beyond a nil compare in each method — the same
// zero-overhead shape planLogScope has.
type execLogScope struct {
	c   *EmbeddedConnection
	ctx context.Context

	sql      string
	planHash uint64
	start    time.Time

	recordsScanned int64
	bytesScanned   int64
	pages          int
	retries        int
	rowsReturned   int64
	rowsAffected   int64

	// done makes finish idempotent. database/sql may Close a result set more
	// than once, and Execute's own error paths Close before returning, so
	// without this a statement could report twice.
	done bool
}

// beginExecLog starts an execution-stats scope, or returns nil when no logger
// is configured (logging disabled -> zero work beyond the nil compare).
func (c *EmbeddedConnection) beginExecLog(ctx context.Context, sql string, plan plans.RecordQueryPlan) *execLogScope {
	if c == nil || c.execStatsLogger == nil {
		return nil
	}
	s := &execLogScope{
		c:     c,
		ctx:   ctx,
		sql:   truncateSQL(sql),
		start: time.Now(),
	}
	if plan != nil {
		s.planHash = plans.PlanHash(plan)
	}
	return s
}

// execLogSQL returns the canonical query text for an execution record, or ""
// when execution logging is disabled. Gating here is what keeps the disabled
// path free of the substring allocation canonicalTextOf performs.
func (c *EmbeddedConnection) execLogSQL(parseCtx any) string {
	if c == nil || c.execStatsLogger == nil {
		return ""
	}
	return canonicalTextOf(parseCtx)
}

// addScanned charges one execution attempt's scan consumption.
func (s *execLogScope) addScanned(records, bytes int64) {
	if s == nil {
		return
	}
	s.recordsScanned += records
	s.bytesScanned += bytes
}

// addPage records one page fetch.
func (s *execLogScope) addPage() {
	if s == nil {
		return
	}
	s.pages++
}

// addRetries records n transaction attempts beyond the first.
func (s *execLogScope) addRetries(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.retries += n
}

// setRows records the statement's row outcome: rows handed to the caller for a
// result set, or rows mutated for DML.
func (s *execLogScope) setRows(returned, affected int64) {
	if s == nil {
		return
	}
	s.rowsReturned = returned
	s.rowsAffected = affected
}

// finish computes the remaining fields and dispatches to the logger. Safe to
// call on a nil scope, and idempotent on a live one.
func (s *execLogScope) finish(err error) {
	if s == nil || s.done {
		return
	}
	s.done = true
	stats := ExecutionStats{
		SQL:               s.sql,
		PlanHash:          s.planHash,
		ExecutionDuration: time.Since(s.start),
		RowsReturned:      s.rowsReturned,
		RowsAffected:      s.rowsAffected,
		RecordsScanned:    s.recordsScanned,
		BytesScanned:      s.bytesScanned,
		Pages:             s.pages,
		Retries:           s.retries,
		Err:               err,
	}
	if thresh := s.c.slowQueryThresholdMicros; thresh > 0 {
		stats.SlowQuery = stats.ExecutionDuration.Microseconds() > thresh
	}
	s.c.execStatsLogger.LogExecutionStats(s.ctx, stats)
}
