package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// TestFDB_ReadableIndexViewLatency measures what the planner's readable-index
// view costs on the hottest path there is: a point-lookup SELECT that HITS the
// plan cache. That shape is the right one because the view is resolved BEFORE
// the cache lookup and is part of the cache key, so a cache hit pays for it in
// full — it is the one planning cost caching cannot amortise away.
//
// It exists as a committed test rather than a throwaway probe because the
// conclusion it supports is load-bearing and it is a REGRESSION, which is
// exactly the kind of number that gets rounded away once the measurement is
// deleted. fetchReadableIndexes OPENS the store (see its doc comment) instead
// of doing a speculative range read over the index-state subspace, and the
// store open costs slightly more. Re-running this under a patched
// fetchReadableIndexes reproduces the three-way comparison below.
//
// MEASURED, 500 iterations after a 20-query warmup, three runs each, same
// machine and cluster, back to back:
//
//	store open (current)            2.708 / 2.708 / 2.715 ms per query
//	speculative index-state read    2.648 / 2.638 / 2.640 ms per query
//	view short-circuited entirely   1.432 / 1.438 / 1.426 ms per query
//
// So the readable-index view costs ~1.28 ms of a 2.71 ms cached point-lookup
// SELECT, against ~1.21 ms when it was a speculative read: the change is
// ~70 microseconds SLOWER, about 2.6%. The store open is a header get plus the
// same index-state read that was there before, which is where the extra round
// trip goes; it does not pay for its own transaction twice because it runs
// through the same runInCapturedTx every other path uses.
//
// That regression is the price of correctness, not an accident to be optimised
// away by reverting: the speculative read is CHEAPER precisely because it
// answers a different question — the state on disk, rather than the state
// reconciliation has settled — and gets an evolution-added index wrong.
//
// The absolute numbers are machine-specific; the RATIOS between the three
// conditions are the result. Re-measure all three on one machine, in one
// sitting, before drawing any conclusion from a number recorded elsewhere.
//
// The assertion is deliberately loose. A per-query ceiling tight enough to
// catch a 70 microsecond regression would be a flake on shared hardware; this
// one catches only a catastrophic regression (an extra full scan, a per-query
// index build), and the NUMBERS above are the real record. Read the log line.
func TestFDB_ReadableIndexViewLatency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_rivlat")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rivlat")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rivlat_tmpl "+
			"CREATE TABLE t (id BIGINT, c BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_by_c ON t (c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rivlat/s WITH TEMPLATE rivlat_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_rivlat?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// One connection for the whole measurement: the plan cache is
	// per-connection, so a pool that hands out a fresh connection per query
	// would measure cache MISSES and never reach the shape this test is about.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	for i := 1; i <= 200; i++ {
		mwjoMustExec(t, conn, ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, %d)", i, i%10, i*10))
	}

	const q = "SELECT v FROM t WHERE id = 7"
	lookup := func() {
		t.Helper()
		var v int64
		if err := conn.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("point lookup: %v", err)
		}
		if v != 70 {
			t.Fatalf("point lookup returned %d, want 70", v)
		}
	}

	const warmup = 20
	const iterations = 500
	for i := 0; i < warmup; i++ {
		lookup()
	}
	start := time.Now()
	for i := 0; i < iterations; i++ {
		lookup()
	}
	per := time.Since(start) / iterations
	t.Logf("cached point-lookup SELECT: %.3f ms per query (%d iterations after %d warmup)",
		float64(per.Microseconds())/1000, iterations, warmup)

	// Catastrophe guard only — see the doc comment on why it is not tight.
	if per > 50*time.Millisecond {
		t.Fatalf("cached point-lookup SELECT took %v per query, which is an order of "+
			"magnitude above every condition ever measured for this shape (1.4-2.8 ms). "+
			"Something on the planning path is doing per-query work it did not do before: "+
			"the readable-index view is resolved ahead of the plan-cache lookup, so an "+
			"extra scan or an unbatched read there is paid on every single query, cached "+
			"or not.", per)
	}
}
