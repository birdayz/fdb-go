package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// A CACHED PLAN OUTLIVES THE STATISTIC IT WAS PLANNED ON.
//
// The freshness gate bounds how old a statistics READ may be. It says nothing
// about how old a PLAN may be: the cache scope is
// dbPath|schema|metaDataVersion|cacheKeyPart, and cacheKeyPart renders "stat"
// from the FLAG (planner_options.go's useCollectedStatistics), not from the
// version the statistic was collected at. Clearing a statistic therefore leaves
// the key byte-identical, so without an explicit invalidation the connection
// keeps serving the statistics-derived plan and `stats clear` does not do the
// one thing an operator runs it for.
//
// This asserts on the plan-cache OUTCOME rather than on the plan text. The
// first version of this test compared EXPLAIN output before and after the
// clear, and passed with the invalidation removed — plan text cannot
// distinguish "re-planned to the same shape" from "served from cache", so the
// assertion that mattered was never made. The middle HIT below is the guard
// against that: if the cache is not live, the final MISS proves nothing.
func TestFDB_StatisticsChangesInvalidateCachedPlans(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const pkRows, fkRows = 200, 10

	dbPath := "/statscache"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE statscache"+
			" CREATE TABLE pkside (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE TABLE fkside (id BIGINT, fk BIGINT, PRIMARY KEY (id))"+
			" CREATE INDEX fkside_by_fk ON fkside (fk)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE statscache")

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s&planner_statistics=true",
		dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := &syncCaptureLogger{}
	conn := installLogger(t, db, logger)

	for r := 0; r < pkRows; r++ {
		if _, e := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO pkside VALUES (%d, %d)", r, r)); e != nil {
			t.Fatalf("insert pkside: %v", e)
		}
	}
	for r := 0; r < fkRows; r++ {
		if _, e := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO fkside VALUES (%d, %d)", r, r%pkRows)); e != nil {
			t.Fatalf("insert fkside: %v", e)
		}
	}

	onConn := func(what string, fn func(*embedded.EmbeddedConnection) error) {
		t.Helper()
		if e := conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("driver conn is %T, not *embedded.EmbeddedConnection", dc)
			}
			return fn(ec)
		}); e != nil {
			t.Fatalf("%s: %v", what, e)
		}
	}

	collect := func() {
		t.Helper()
		onConn("collect", func(ec *embedded.EmbeddedConnection) error {
			rep, cErr := ec.CollectStatistics(ctx, recordlayer.CollectOptions{BatchSize: 100})
			if cErr != nil {
				return cErr
			}
			if got := rep.Collected["PKSIDE"].Count; got != pkRows {
				return fmt.Errorf("collected |pkside|=%d, want %d", got, pkRows)
			}
			return nil
		})
	}

	// The SELECT is what the cache keys on. DML is never cached (PlanCacheSkip),
	// so the inserts above contribute no cacheable entry for this query.
	const q = "SELECT pkside.v, fkside.id FROM pkside, fkside WHERE fkside.fk = pkside.id"
	runQuery := func(what string) {
		t.Helper()
		rows, e := conn.QueryContext(ctx, q)
		if e != nil {
			t.Fatalf("query (%s): %v", what, e)
		}
		rows.Close()
	}

	// Discard everything planned before this point (the INSERTs); from here the
	// captured events are this query's and nothing else's.
	base := len(logger.snapshot())

	// ARM 1 -- COLLECTING must reach the next plan. This is the direction an
	// operator hits first: run `stats collect`, and the very next query on the
	// same connection must be planned on the new counts rather than served from
	// a cache entry built before they existed. Prime the cache BEFORE
	// collecting, so there is an entry for the collection to displace.
	runQuery("before any collection")
	runQuery("before any collection, identical")
	pre := logger.snapshot()[base:]
	if len(pre) != 2 {
		t.Fatalf("want 2 planning events priming the cache, got %d", len(pre))
	}
	if pre[0].Cache != embedded.PlanCacheMiss || pre[1].Cache != embedded.PlanCacheHit {
		t.Fatalf("priming the cache did not produce miss-then-hit (%v, %v); without a live\n"+
			"cache entry here, ARM 1 cannot observe a collection displacing one",
			pre[0].Cache, pre[1].Cache)
	}

	collect()

	runQuery("first, after collect")
	runQuery("second, identical")

	events := logger.snapshot()[base:]
	if len(events) != 4 {
		t.Fatalf("want 4 planning events (2 priming + 2 after collect), got %d — the "+
			"population has to be right before any cache outcome below means anything",
			len(events))
	}
	// ARM 1's assertion: the collection displaced the primed entry, so the very
	// next query re-planned instead of being served a plan built before the
	// statistic existed.
	if events[2].Cache != embedded.PlanCacheMiss {
		t.Fatalf("the SELECT immediately after CollectStatistics was a cache %v — the "+
			"connection served a plan built before the statistic existed, so collecting "+
			"had no effect on the next query", events[2].Cache)
	}
	// THE VACUITY GUARD for ARM 2. If this is not a hit, the plan cache is not
	// serving this query at all and the MISS asserted after the clear would
	// hold for the wrong reason — which is exactly how the first version of
	// this test passed with the bug present.
	if events[3].Cache != embedded.PlanCacheHit {
		t.Fatalf("second identical SELECT cache = %v, want hit — the plan cache is not "+
			"live for this query, so this test cannot observe an invalidation at all",
			events[3].Cache)
	}

	// ARM 2 -- CLEARING must also reach the next plan.
	onConn("clear", func(ec *embedded.EmbeddedConnection) error {
		return ec.ClearStatistics(ctx)
	})

	runQuery("after clear")
	events = logger.snapshot()[base:]
	if len(events) != 5 {
		t.Fatalf("want 5 planning events, got %d", len(events))
	}
	if events[4].Cache != embedded.PlanCacheMiss {
		t.Fatalf("after ClearStatistics the identical SELECT was a cache %v — the plan "+
			"cache outlived the statistic it was planned on, so the connection keeps "+
			"planning as though the cleared statistic were still there", events[4].Cache)
	}
}
