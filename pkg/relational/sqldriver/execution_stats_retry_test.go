package sqldriver

// The retry dimension of post-execution query statistics (RFC-211).
//
// It lives in a separate file, in the non-`_test` package, for one reason: the
// only deterministic way to force a transaction conflict in this tree is the
// SimFDB `retryOnceBackend` that sim_sql_page_commit_retry_test.go established,
// and that lives here. Real FDB gives no way to make a 1020 happen on demand —
// a test that raced two connections and hoped for a conflict would be the flaky
// test CLAUDE.md forbids, not a measurement. The rest of the RFC-211 coverage
// runs against real FDB in execution_stats_fdb_test.go.
//
// What has to be proven is two-part, and the second part is the one a weaker
// test would miss:
//
//  1. a retried page reports Retries >= 1 — the counter is wired at all; and
//  2. a retried page's scan is CHARGED — the re-executed attempt's records are
//     added, not silently dropped when its transaction rolled back.
//
// (2) is the whole point of counting retries. An operator attributing cluster
// load needs the reads the cluster actually served, and a statement that
// scanned a page twice cost twice. Reporting the retry count while quietly
// discarding the retried attempt's scan would give the most misleading possible
// answer: a visible retry with no cost attached to it.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// retryStatsCapture is a concurrency-safe ExecutionStatsLogger.
type retryStatsCapture struct {
	mu     sync.Mutex
	events []embedded.ExecutionStats
}

func (c *retryStatsCapture) LogExecutionStats(_ context.Context, s embedded.ExecutionStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, s)
}

func (c *retryStatsCapture) takeOne(t *testing.T) embedded.ExecutionStats {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 1 {
		t.Fatalf("want exactly 1 execution-stats event, got %d: %+v", len(c.events), c.events)
	}
	ev := c.events[0]
	c.events = nil
	return ev
}

// TestExecutionStats_RetryIsCountedAndCharged pins both halves.
//
// The fault ordinal is SWEPT rather than fixed. A fixed ordinal would be a
// guess about how many transactions the connection makes before the query's
// first page — the same guess sim_sql_page_commit_retry_test.go declined to
// make — and the property is supposed to hold wherever inside the statement the
// conflict lands. The sweep is also what lets the test fail loudly when NOTHING
// was exercised: if no ordinal ever produced a retry, the assertion at the end
// says so instead of passing vacuously.
func TestExecutionStats_RetryIsCountedAndCharged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const query = "SELECT id FROM t"

	// runOnce opens a fresh schema+data, optionally arms a retryable failure at
	// transaction ordinal n (n < 0 means no fault), and returns the statement's
	// stats. A fresh database per run keeps the sweep's iterations independent.
	runOnce := func(t *testing.T, seed uint64, armAt int64) embedded.ExecutionStats {
		t.Helper()
		db, backend := openRetryOnceSchema(t, seed,
			"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
		for i := 0; i < pageRetryRows; i++ {
			mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, a) VALUES (%d, %d)", i, i))
		}

		capture := &retryStatsCapture{}
		conn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
			// The small per-page scan budget is what forces MULTIPLE pages, and
			// therefore multiple transactions for a fault to land in. With one
			// page there is only one transaction and the sweep has nothing to
			// sweep over.
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, pageRetryScanLimit).
				Build())
			ec.SetExecutionStatsLogger(capture)
		})

		if armAt >= 0 {
			backend.arm(armAt)
		}
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%q with fault armed at %d: %v", query, armAt, err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows (fault at %d): %v", armAt, err)
		}
		rows.Close()
		backend.disarm()

		// Every run must return the complete result. A retry that lost rows
		// would make any statistic about it meaningless.
		if n != pageRetryRows {
			t.Fatalf("fault at %d: got %d rows, want %d", armAt, n, pageRetryRows)
		}
		return capture.takeOne(t)
	}

	// Baseline: no fault. This is the reference the charged-scan claim is
	// measured against, so it is asserted rather than assumed.
	base := runOnce(t, 9500, -1)
	if base.Retries != 0 {
		t.Fatalf("baseline Retries = %d, want 0 with no fault armed", base.Retries)
	}
	if base.RecordsScanned != pageRetryRows {
		t.Fatalf("baseline RecordsScanned = %d, want %d", base.RecordsScanned, pageRetryRows)
	}

	sawRetry := false
	for n := int64(0); n < 8; n++ {
		s := runOnce(t, uint64(9600+n), n)
		if s.Retries == 0 {
			// The fault landed outside the statement's own page transactions
			// (setup, catalog, store open). Nothing to assert; not a failure.
			continue
		}
		sawRetry = true
		if s.Retries != 1 {
			t.Errorf("fault at %d: Retries = %d, want exactly 1 (one page failed once)", n, s.Retries)
		}
		// The charged-scan half. The retried page re-scanned its records, so
		// the statement consumed strictly more than the clean run did.
		if s.RecordsScanned <= base.RecordsScanned {
			t.Errorf("fault at %d: RecordsScanned = %d, want more than the clean run's %d — "+
				"the retried attempt's reads were served by the cluster and must be charged",
				n, s.RecordsScanned, base.RecordsScanned)
		}
		if s.BytesScanned <= base.BytesScanned {
			t.Errorf("fault at %d: BytesScanned = %d, want more than the clean run's %d",
				n, s.BytesScanned, base.BytesScanned)
		}
		// Rows RETURNED must be unaffected: a retry re-reads, it does not
		// re-emit. This is the dimension that separates "scanned" from
		// "returned" on the retry axis specifically.
		if s.RowsReturned != int64(pageRetryRows) {
			t.Errorf("fault at %d: RowsReturned = %d, want %d — a retry must not double-count emitted rows",
				n, s.RowsReturned, pageRetryRows)
		}
	}
	if !sawRetry {
		t.Fatalf("no armed ordinal in 0..7 produced a page retry; the retry dimension was never exercised, " +
			"so this test proved nothing")
	}
}
