package fdbmetrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
)

type fakeSource struct{ s client.ClientMetricsSnapshot }

func (f fakeSource) Metrics() client.ClientMetricsSnapshot { return f.s }

func TestHandler_TextExposition(t *testing.T) {
	t.Parallel()
	src := fakeSource{s: client.ClientMetricsSnapshot{
		TransactionsCommitStarted:        7,
		TransactionsCommitCompleted:      6,
		TransactionsNotCommitted:         3,
		TransactionsThrottled:            22,
		TransactionsMaybeCommitted:       21,
		TransactionReadVersionsCompleted: 42,
		GRVCacheHits:                     9,
		GRVInBandMaybeDelivered:          11,
		TransactionRetries:               4,
		ClientConnectionFailures:         2,
		CoordinatorChanges:               1,
		// Every counter gets a DISTINCT NON-ZERO value AND every expectation below
		// is newline-anchored. All three are load-bearing, and each was wrong here
		// in turn:
		//
		//   - two counters shared the value 1, so crossing their getters passed;
		//   - one was asserted as 0, which every unset counter renders as, so it
		//     held whether its getter was wired or not;
		//   - the values were distinct but the matcher is strings.Contains, and 1,
		//     2 and 4 are PREFIXES of 11-18, 21-22 and 42 -- eleven shadowed
		//     pairs. A symmetric swap still failed on its other half, but a
		//     one-directional mis-wire into a shadowing counter passed: pointing
		//     coordinator_changes at TransactionsTooOld rendered 14, and
		//     "…_total 1" matched it.
		//
		// EVERY expectation below carries a trailing newline -- the TYPE lines and the
		// quantile lines included, not just the _total ones. An earlier pass
		// anchored only _total while the comment claimed all of them, leaving 11 of
		// 34 open.
		TransactionsResourceConstrained:           12,
		TransactionsProcessBehind:                 13,
		TransactionsTooOld:                        14,
		TransactionsFutureVersions:                15,
		TransactionBatchReadVersionsCompleted:     16,
		TransactionDefaultReadVersionsCompleted:   17,
		TransactionImmediateReadVersionsCompleted: 18,
		ReadLatency:        client.LatencyStats{Count: 100, Sum: 1.5, Mean: 0.015, Median: 0.001, P90: 0.005, P99: 0.02, Max: 0.03},
		CommitLatency:      client.LatencyStats{Count: 10, Sum: 0.5, Mean: 0.05, Median: 0.04, P90: 0.06, P99: 0.07, Max: 0.08},
		GRVLatency:         client.LatencyStats{Count: 20, Sum: 0.6, Mean: 0.03, Median: 0.02, P90: 0.05, P99: 0.09, Max: 0.1},
		TransactionLatency: client.LatencyStats{Count: 30, Sum: 0.7, Mean: 0.023, Median: 0.01, P90: 0.03, P99: 0.04, Max: 0.05},
	}}

	rec := httptest.NewRecorder()
	Handler(src).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE fdb_client_transactions_commit_started_total counter\n",
		"fdb_client_transactions_commit_started_total 7\n",
		"fdb_client_transactions_commit_completed_total 6\n",
		"fdb_client_transactions_not_committed_total 3\n",
		"fdb_client_transactions_maybe_committed_total 21\n",
		"fdb_client_transaction_read_versions_completed_total 42\n",
		"# TYPE fdb_client_grv_cache_hits_total counter\n",
		"fdb_client_grv_cache_hits_total 9\n",
		"# TYPE fdb_client_grv_in_band_maybe_delivered_total counter\n",
		"fdb_client_grv_in_band_maybe_delivered_total 11\n",
		"fdb_client_transaction_retries_total 4\n",
		"fdb_client_transactions_throttled_total 22\n",
		// RFC-114 counters.
		"# TYPE fdb_client_connection_failures_total counter\n",
		"fdb_client_connection_failures_total 2\n",
		"fdb_client_coordinator_changes_total 1\n",
		// The remaining counters, so that deleting ANY counterDef reddens this
		// test by name. Without these the exposition named 11 of 18, and the
		// TYPE-count check below cannot see a deletion at all.
		"fdb_client_transactions_resource_constrained_total 12\n",
		"fdb_client_transactions_process_behind_total 13\n",
		"fdb_client_transactions_too_old_total 14\n",
		"fdb_client_transactions_future_versions_total 15\n",
		"fdb_client_transaction_batch_read_versions_completed_total 16\n",
		"fdb_client_transaction_default_read_versions_completed_total 17\n",
		"fdb_client_transaction_immediate_read_versions_completed_total 18\n",
		// and every summary, same reason.
		"# TYPE fdb_client_commit_latency_seconds summary\n",
		"fdb_client_commit_latency_seconds_count 10\n",
		"# TYPE fdb_client_grv_latency_seconds summary\n",
		"fdb_client_grv_latency_seconds_count 20\n",
		"# TYPE fdb_client_transaction_latency_seconds summary\n",
		"fdb_client_transaction_latency_seconds_count 30\n",
		// RFC-114 latency summary.
		"# TYPE fdb_client_read_latency_seconds summary\n",
		"fdb_client_read_latency_seconds{quantile=\"0.5\"} 0.001\n",
		"fdb_client_read_latency_seconds{quantile=\"0.9\"} 0.005\n",
		"fdb_client_read_latency_seconds{quantile=\"0.99\"} 0.02\n",
		"fdb_client_read_latency_seconds_sum 1.5\n",
		"fdb_client_read_latency_seconds_count 100\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\nbody:\n%s", want, body)
		}
	}
	// Every defined counter and summary renders a TYPE line.
	//
	// This does NOT catch a missing counterDef, and cannot: both sides derive
	// from `counters`, so deleting an entry moves them together and the equality
	// still holds. Measured -- removing one fired only the explicit string
	// assertions above, and this check stayed silent. It catches a renderer that
	// skips or duplicates an entry it WAS given.
	//
	// What catches an entry that was never defined is the explicit list above,
	// and only because that list now names ALL of them. It named 11 of 18
	// counters and 1 of 4 summaries when this comment first claimed otherwise:
	// deleting transactions_too_old was invisible to both checks. Verified after
	// filling it in -- deleting that same counterDef, and separately a summary,
	// each fails BY NAME.
	if got, want := strings.Count(body, "# TYPE "), len(counters)+len(summaries); got != want {
		t.Errorf("rendered %d TYPE lines, want %d", got, want)
	}

	// EVERY counter DECLARES ITS ORIGIN, AND EVERY Go-only ONE SAYS SO IN HELP.
	//
	// Iterates `counters` itself rather than a hardcoded name list. The previous
	// version listed five names, so a nineteenth Go-only counter was never
	// examined -- adding one with no provenance passed, and silently falsified
	// the 18/13/5 split in the table's own doc, which nothing checked in the ADD
	// direction.
	//
	// counterOrigin's zero value is invalid, so a counterDef that forgets to
	// declare an origin fails here. That is what makes this fail CLOSED: the
	// iteration cannot miss an entry, and an entry cannot opt out by omission.
	//
	// The classification only matters if it survives into the exposition, since
	// whoever audits it is reading a scrape, not this repository -- which is why
	// the Go-only arm asserts on the rendered HELP LINE rather than on the help
	// string in source.
	for _, c := range counters {
		switch c.origin {
		case originUnset:
			t.Errorf("counter %s declares no origin; counterOrigin's zero value is "+
				"invalid precisely so this cannot pass unnoticed", c.name)
		case originCPPTwin:
			// Nothing to assert in the exposition: a C++ twin is a fact about
			// the other implementation, not about this text.
		case originGoOnly:
			prefix := "# HELP " + c.name + " "
			i := strings.Index(body, prefix)
			if i < 0 {
				t.Errorf("no HELP line for %s; its provenance cannot be checked", c.name)
				continue
			}
			line := body[i:]
			if j := strings.IndexByte(line, '\n'); j >= 0 {
				line = line[:j]
			}
			// "Go aggregate" is transaction_retries' spelling: C++ retries those
			// codes without a counter, which is the same claim.
			if !strings.Contains(line, "Go-only") && !strings.Contains(line, "Go aggregate") {
				t.Errorf("HELP for %s does not declare it Go-only: %q", c.name, line)
			}
		}
	}
}
