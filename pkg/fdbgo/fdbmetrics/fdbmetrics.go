// Package fdbmetrics exposes a pure-Go FDB client's operational counters
// (client.Database.Metrics, RFC-097) in the Prometheus text exposition
// format, ready to scrape — with zero dependencies.
//
// Usage:
//
//	http.Handle("/metrics", fdbmetrics.Handler(db))
//
// Deliberately NOT a prometheus.Collector: that would pull
// github.com/prometheus/client_golang into the module for ~14 monotonic
// counters. A user who wants a Collector writes a trivial one over
// db.Metrics():
//
//	prometheus.NewCounterFunc(opts, func() float64 {
//	    return float64(db.Metrics().TransactionsNotCommitted)
//	})
package fdbmetrics

import (
	"fmt"
	"io"
	"net/http"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// MetricsSource is the part of *client.Database this package consumes —
// accepted as an interface so tests and wrappers can substitute snapshots.
type MetricsSource interface {
	Metrics() client.ClientMetricsSnapshot
}

// Handler returns an http.Handler that renders the database's counters in
// the Prometheus text exposition format (text/plain; version=0.0.4). All
// metrics are monotonic counters.
func Handler(src MetricsSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// A write error here means the scraper hung up mid-response —
		// nothing actionable; the next scrape retries.
		_ = WriteText(w, src.Metrics())
	})
}

// counterDef binds the exposition name + help text to a snapshot field.
type counterDef struct {
	name string
	help string
	get  func(s client.ClientMetricsSnapshot) int64
}

// counters uses the C++ TransactionMetrics names, snake_cased with the
// conventional fdb_client prefix and Prometheus _total suffix.
var counters = []counterDef{
	{
		"fdb_client_transactions_commit_started_total", "Commits sent (read-only fast-path commits excluded, matching C++).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsCommitStarted },
	},
	{
		"fdb_client_transactions_commit_completed_total", "Commits acknowledged successful.",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsCommitCompleted },
	},
	{
		"fdb_client_transactions_not_committed_total", "Commit conflicts (FDB error 1020).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsNotCommitted },
	},
	{
		"fdb_client_transactions_maybe_committed_total", "Ambiguous commits (commit_unknown_result, 1021).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsMaybeCommitted },
	},
	{
		"fdb_client_transactions_resource_constrained_total", "Proxy memory-limit retries (1042/1078).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsResourceConstrained },
	},
	{
		"fdb_client_transactions_process_behind_total", "process_behind retries (1037).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsProcessBehind },
	},
	{
		"fdb_client_transactions_throttled_total", "Ratekeeper/tag throttle retries (1051/1213/1223).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsThrottled },
	},
	{
		"fdb_client_transactions_too_old_total", "transaction_too_old retries (1007).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsTooOld },
	},
	{
		"fdb_client_transactions_future_versions_total", "future_version retries (1009).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionsFutureVersions },
	},
	{
		"fdb_client_transaction_read_versions_completed_total", "Transactions served by a real GRV reply (cache hits excluded, matching C++).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionReadVersionsCompleted },
	},
	{
		"fdb_client_transaction_batch_read_versions_completed_total", "BATCH-priority GRV completions.",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionBatchReadVersionsCompleted },
	},
	{
		"fdb_client_transaction_default_read_versions_completed_total", "DEFAULT-priority GRV completions.",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionDefaultReadVersionsCompleted },
	},
	{
		"fdb_client_transaction_immediate_read_versions_completed_total", "SYSTEM_IMMEDIATE-priority GRV completions.",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionImmediateReadVersionsCompleted },
	},
	{
		"fdb_client_transaction_retries_total", "All OnError-sanctioned retries (Go aggregate; includes codes C++ retries without a counter).",
		func(s client.ClientMetricsSnapshot) int64 { return s.TransactionRetries },
	},
}

// WriteText renders one snapshot in the Prometheus text exposition format.
// Returns the first writer error (the exposition itself cannot fail).
func WriteText(w io.Writer, s client.ClientMetricsSnapshot) error {
	for _, c := range counters {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n",
			c.name, c.help, c.name, c.name, c.get(s)); err != nil {
			return err
		}
	}
	return nil
}
