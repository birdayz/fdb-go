package fdbmetrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

type fakeSource struct{ s client.ClientMetricsSnapshot }

func (f fakeSource) Metrics() client.ClientMetricsSnapshot { return f.s }

func TestHandler_TextExposition(t *testing.T) {
	t.Parallel()
	src := fakeSource{s: client.ClientMetricsSnapshot{
		TransactionsCommitStarted:        7,
		TransactionsCommitCompleted:      6,
		TransactionsNotCommitted:         3,
		TransactionsMaybeCommitted:       1,
		TransactionReadVersionsCompleted: 42,
		GRVCacheHits:                     9,
		TransactionRetries:               4,
		ClientConnectionFailures:         2,
		CoordinatorChanges:               1,
		ReadLatency:                      client.LatencyStats{Count: 100, Sum: 1.5, Mean: 0.015, Median: 0.001, P90: 0.005, P99: 0.02, Max: 0.03},
	}}

	rec := httptest.NewRecorder()
	Handler(src).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE fdb_client_transactions_commit_started_total counter",
		"fdb_client_transactions_commit_started_total 7",
		"fdb_client_transactions_commit_completed_total 6",
		"fdb_client_transactions_not_committed_total 3",
		"fdb_client_transactions_maybe_committed_total 1",
		"fdb_client_transaction_read_versions_completed_total 42",
		"# TYPE fdb_client_grv_cache_hits_total counter",
		"fdb_client_grv_cache_hits_total 9",
		"fdb_client_transaction_retries_total 4",
		"fdb_client_transactions_throttled_total 0",
		// RFC-114 counters.
		"# TYPE fdb_client_connection_failures_total counter",
		"fdb_client_connection_failures_total 2",
		"fdb_client_coordinator_changes_total 1",
		// RFC-114 latency summary.
		"# TYPE fdb_client_read_latency_seconds summary",
		`fdb_client_read_latency_seconds{quantile="0.5"} 0.001`,
		`fdb_client_read_latency_seconds{quantile="0.9"} 0.005`,
		`fdb_client_read_latency_seconds{quantile="0.99"} 0.02`,
		"fdb_client_read_latency_seconds_sum 1.5",
		"fdb_client_read_latency_seconds_count 100",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\nbody:\n%s", want, body)
		}
	}
	// Every defined counter and summary renders a TYPE line.
	if got, want := strings.Count(body, "# TYPE "), len(counters)+len(summaries); got != want {
		t.Errorf("rendered %d TYPE lines, want %d", got, want)
	}
}
