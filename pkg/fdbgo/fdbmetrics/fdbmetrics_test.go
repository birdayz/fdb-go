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
		TransactionRetries:               4,
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
		"fdb_client_transaction_retries_total 4",
		"fdb_client_transactions_throttled_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\nbody:\n%s", want, body)
		}
	}
	// Every defined counter renders HELP+TYPE+value.
	if got := strings.Count(body, "# TYPE "); got != len(counters) {
		t.Errorf("rendered %d TYPE lines, want %d", got, len(counters))
	}
}
