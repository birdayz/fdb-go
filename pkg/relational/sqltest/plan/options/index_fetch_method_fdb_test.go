package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/internal/fdbclient"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

var (
	indexFetchBackendOnce sync.Once
	indexFetchBackend     fdb.BackendDatabase
	indexFetchBackendErr  error
)

// indexFetchDB is a schema with an indexed table on a record-layer database of
// its own, armed with a StoreTimer the driver installs, so the counts belong to
// this test alone. Rows have a = id.
func indexFetchDB(t *testing.T, tag string, rows int) (*sql.DB, *recordlayer.StoreTimer) {
	t.Helper()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	indexFetchBackendOnce.Do(func() {
		indexFetchBackend, indexFetchBackendErr = fdbclient.Open(testkit.ClusterFile())
	})
	if indexFetchBackendErr != nil {
		t.Fatalf("open FDB: %v", indexFetchBackendErr)
	}
	key := "index-fetch-method-" + tag
	recordDB := recordlayer.NewFDBDatabaseWithBackend(indexFetchBackend)
	t.Cleanup(sqldriver.RegisterBackend(key, recordDB))
	timer := sqldriver.EnableStoreTimer(key)

	ctx := context.Background()
	dsn := fmt.Sprintf("fdbsql:///FRL/IFM_%s?cluster_file=%s", strings.ToUpper(tag), key)
	setup, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close() })
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/ifm_"+tag)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE ifm_"+tag+
		" CREATE TABLE T (id BIGINT, a BIGINT, c STRING, PRIMARY KEY (id)) CREATE INDEX idx_a ON T(a)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/ifm_"+tag+"/s WITH TEMPLATE ifm_"+tag)
	db, err := sql.Open("fdbsql", dsn+"&schema=S")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for id := 0; id < rows; id++ {
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("INSERT INTO T VALUES (%d, %d, 'v%d')", id, id, id))
	}
	return db, timer
}

func indexFetchConn(t *testing.T, db *sql.DB, options map[api.OptionName]any) *sql.Conn {
	t.Helper()
	return testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		for name, value := range options {
			if err := ec.SetOption(name, value); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestFDB_PlannerOptions_IndexFetchMethodRunsRemoteFetch drives
// INDEX_FETCH_METHOD end to end: each value returns SCAN_AND_FETCH's rows,
// paged through continuations as well as in one page, and Java's counters
// (RemoteFetchTestBase.assertCounters) show which path ran — the mapped read
// for both remote methods, the index scan for SCAN_AND_FETCH and for an unset
// option, whose Go default is SCAN_AND_FETCH (DIVERGENCES.md).
func TestFDB_PlannerOptions_IndexFetchMethodRunsRemoteFetch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const rows = 40
	db, timer := indexFetchDB(t, "runs", rows)
	const q = "SELECT id FROM T WHERE a >= 0 AND c IS NOT NULL"
	want := make([]int64, rows)
	for i := range want {
		want[i] = int64(i)
	}
	unset := indexFetchConn(t, db, nil)
	if plan := testkit.ExplainConn(t, ctx, unset, q); !strings.Contains(plan, "IndexScan(IDX_A") || strings.Contains(plan, "COVERING") {
		t.Fatalf("plan %s must fetch through IDX_A for the counters to mean anything", plan)
	}
	type counts struct{ remoteFetches, remoteEntries, indexScans int64 }
	run := func(conn *sql.Conn) counts {
		t.Helper()
		timer.Reset()
		if got := testkit.ScanInt64Rows(t, ctx, conn, q); !slices.Equal(got, want) {
			t.Fatalf("rows %v, want %v", got, want)
		}
		return counts{
			timer.GetCount(recordlayer.CountRemoteFetch),
			timer.GetCount(recordlayer.EventScanRemoteFetchEntry),
			timer.GetCount(recordlayer.EventScanIndex),
		}
	}
	if got := run(unset); got.remoteFetches != 0 || got.remoteEntries != 0 || got.indexScans == 0 {
		t.Fatalf("unset option: %+v, want an index scan and no remote fetch", got)
	}
	for _, method := range []api.IndexFetchMethod{api.IndexFetchScanAndFetch, api.IndexFetchUseRemoteFetch, api.IndexFetchUseRemoteFetchWithFallback} {
		for _, pageRows := range []int{0, 7} {
			options := map[api.OptionName]any{api.OptIndexFetchMethod: method}
			if pageRows > 0 {
				// Each page resumes the scan from the previous page's continuation.
				options[api.OptExecutionScannedRowsLimit] = pageRows
			}
			got := run(indexFetchConn(t, db, options))
			if method == api.IndexFetchScanAndFetch {
				if got.remoteFetches != 0 || got.remoteEntries != 0 || got.indexScans == 0 {
					t.Fatalf("%v page %d: %+v, want an index scan and no remote fetch", method, pageRows, got)
				}
				continue
			}
			minFetches := int64(1)
			if pageRows > 0 {
				minFetches = rows / int64(pageRows)
			}
			if got.remoteFetches < minFetches || got.remoteEntries < rows || got.indexScans != 0 {
				t.Fatalf("%v page %d: %+v, want >= %d mapped reads, >= %d remote entries and no index scan",
					method, pageRows, got, minFetches, rows)
			}
		}
	}

	// DELETE's access path is the bare index scan, so it fetches remotely too.
	remote := indexFetchConn(t, db, map[api.OptionName]any{api.OptIndexFetchMethod: api.IndexFetchUseRemoteFetch})
	timer.Reset()
	res, err := remote.ExecContext(ctx, "DELETE FROM T WHERE a = 3")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 || timer.GetCount(recordlayer.CountRemoteFetch) == 0 {
		t.Fatalf("DELETE under USE_REMOTE_FETCH: %d rows (%v), %d mapped reads", n, err, timer.GetCount(recordlayer.CountRemoteFetch))
	}
}

// TestFDB_PlannerOptions_IndexFetchMethodKeysThePlanCache pins Java's
// QueryCacheKey, which carries the whole PlannerConfiguration: a plan stamped
// with one method is never served to a connection set to another, each
// method's plan stays cached alongside the others, and SCAN_AND_FETCH set
// explicitly shares the unset option's plan.
func TestFDB_PlannerOptions_IndexFetchMethodKeysThePlanCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := indexFetchDB(t, "cache", 3)
	logger := &testkit.SyncCaptureLogger{}
	var ec *embedded.EmbeddedConnection
	conn := testkit.PinEmbeddedConn(t, db, func(c *embedded.EmbeddedConnection) {
		c.SetPlanLogger(logger)
		ec = c
	})
	const q = "SELECT id FROM T WHERE a = 1"
	set := func(method any) {
		if err := conn.Raw(func(any) error {
			if method == nil {
				ec.SetOptions(nil)
				return nil
			}
			return ec.SetOption(api.OptIndexFetchMethod, method)
		}); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		method any
		want   embedded.PlanCacheEvent
	}{
		{api.IndexFetchUseRemoteFetch, embedded.PlanCacheMiss},
		{api.IndexFetchUseRemoteFetch, embedded.PlanCacheHit},
		{api.IndexFetchUseRemoteFetchWithFallback, embedded.PlanCacheMiss},
		{api.IndexFetchScanAndFetch, embedded.PlanCacheMiss},
		{nil, embedded.PlanCacheHit},
		{api.IndexFetchUseRemoteFetch, embedded.PlanCacheHit},
		{api.IndexFetchUseRemoteFetchWithFallback, embedded.PlanCacheHit},
	}
	for i, step := range steps {
		set(step.method)
		start := len(logger.Snapshot())
		if got := testkit.ScanInt64Rows(t, ctx, conn, q); !slices.Equal(got, []int64{1}) {
			t.Fatalf("step %d (%v): rows %v", i, step.method, got)
		}
		events := logger.Snapshot()[start:]
		if len(events) != 1 || events[0].Cache != step.want {
			t.Fatalf("step %d (%v): cache events %+v, want one %v", i, step.method, events, step.want)
		}
	}
}
