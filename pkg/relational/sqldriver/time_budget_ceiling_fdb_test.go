package sqldriver_test

// Permanent regression coverage for the per-transaction time budget
// (txPageTimeLimit in cascades_generator.go, 4s) intersected with the
// connection-level api.OptExecutionTimeLimit. The connection option only
// NARROWS that ceiling — a larger value can never raise it past 4s — so
// pinning OptExecutionTimeLimit to a handful of milliseconds trips the
// time-limit branch deterministically, independent of how fast the machine
// running the test happens to be.
//
// The invariant under test: a buffered operator (the recursive-CTE DISTINCT
// arm materializes an entire traversal before it can hand back any row —
// see errIfBufferTruncated in executor.go) cannot paginate a continuation,
// so an out-of-band time-limit stop there MUST surface as a 54F01 error —
// never a silently truncated row set presented as a complete answer.
// Streaming shapes (a plain scan, an ORDER BY), by contrast, DO have a
// continuation to resume from and MUST paginate the same stop transparently,
// still returning every row.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func timeBudgetCeilingDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmplName := "ceiling_tmpl" + strings.ReplaceAll(dbPath, "/", "_")
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmplName+
		" CREATE TABLE edges (id BIGINT, parent BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE flat (id BIGINT, val BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmplName); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFDB_TimeBudgetCeiling_RecursionErrorsNotPartial pins that a buffered
// recursive-CTE DISTINCT traversal which cannot fit inside a tiny time
// budget always surfaces 54F01 (SQLSTATE ExecutionLimitReached, caused
// specifically by recordlayer.TimeLimitReached) — and never hands back a
// prefix of the traversal as if it were the complete answer. Resuming a
// DISTINCT traversal by re-executing and skipping the emitted prefix means a
// traversal that cannot fit one budget can never complete by paginating, so
// erroring is the only safe outcome; silently returning partial rows would
// look like success while dropping the rest of the graph.
func TestFDB_TimeBudgetCeiling_RecursionErrorsNotPartial(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := timeBudgetCeilingDB(t, "/ceiling_recursion")

	// No index on parent: every recursion level pays a full scan, which is
	// exactly the shape that has no continuation to fall back on and must
	// error rather than truncate.
	const chain = 300
	var b strings.Builder
	b.WriteString("INSERT INTO edges VALUES (1,0)")
	for i := 2; i <= chain; i++ {
		fmt.Fprintf(&b, ",(%d,%d)", i, i-1)
	}
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const q = "WITH RECURSIVE r(n) AS (" +
		"SELECT id FROM edges WHERE parent = 0 " +
		"UNION " +
		"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n" +
		") SELECT n FROM r"

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionTimeLimit, int64(1)).
			Build())
	})

	rows, qerr := conn.QueryContext(ctx, q)
	var n int
	var iterErr error
	if qerr != nil {
		iterErr = qerr
	} else {
		defer rows.Close()
		for rows.Next() {
			n++
			if n > chain+8 {
				t.Fatal("row runaway")
			}
		}
		iterErr = rows.Err()
	}

	if iterErr == nil {
		t.Fatalf("recursion returned %d/%d rows under a 1ms time budget with NO error — a buffered recursive-CTE arm cannot paginate, so an out-of-band time-limit stop MUST surface as 54F01, never a silent partial result", n, chain)
	}
	if n >= chain {
		t.Fatalf("got an error AND a complete %d-row result — the error path must not also hand back rows as if the traversal had finished", n)
	}
	wantScanLimitReached(t, iterErr, recordlayer.TimeLimitReached)
}

// TestFDB_TimeBudgetCeiling_StreamingShapesPaginateComplete contrasts the
// recursion case above with shapes that DO have a continuation: under the
// same tiny time budget, a plain scan and a streaming ORDER BY must both
// paginate transparently and still return every row — the plain scan in
// scan order, the ORDER BY case in sorted order. A regression that made
// either silently short would be far more serious than the recursion
// erroring, since these operators are supposed to survive an out-of-band
// stop by resuming, not by giving up.
func TestFDB_TimeBudgetCeiling_StreamingShapesPaginateComplete(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := timeBudgetCeilingDB(t, "/ceiling_streaming")

	// A small row count keeps the whole test in the low seconds even at a
	// 1ms budget: ORDER BY re-scans from the top on every page (position
	// replay), so its total cost grows faster than linearly in row count —
	// a 2000-row table pushed this well past 10s, which is too slow for
	// the suite.
	const n = 400
	var b strings.Builder
	b.WriteString("INSERT INTO flat VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		// Reverse val so ORDER BY val actually reorders the rows.
		fmt.Fprintf(&b, "(%d,%d)", i, n-i)
	}
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const millis = 1

	t.Run("PlainScan", func(t *testing.T) {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionTimeLimit, int64(millis)).
				Build())
		})
		rows, err := conn.QueryContext(ctx, "SELECT id FROM flat")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		seen := make(map[int64]bool, n)
		count := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seen[id] = true
			count++
			if count > n+8 {
				t.Fatal("row runaway")
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("a plain scan must paginate a time-limit stop transparently, not error: %v", err)
		}
		if count != n {
			t.Fatalf("plain scan under a 1ms budget returned %d rows, want %d (pagination must be transparent)", count, n)
		}
		if len(seen) != n {
			t.Fatalf("plain scan returned %d rows but only %d distinct ids — duplicates from a mishandled continuation", count, len(seen))
		}
	})

	t.Run("OrderBy", func(t *testing.T) {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionTimeLimit, int64(millis)).
				Build())
		})
		rows, err := conn.QueryContext(ctx, "SELECT val FROM flat ORDER BY val")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		count := 0
		var prev int64 = -1
		for rows.Next() {
			var val int64
			if err := rows.Scan(&val); err != nil {
				t.Fatalf("scan: %v", err)
			}
			count++
			if count > n+8 {
				t.Fatal("row runaway")
			}
			if val <= prev {
				t.Fatalf("ORDER BY val not ascending at row %d: prev=%d val=%d", count, prev, val)
			}
			prev = val
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("a streaming ORDER BY must paginate a time-limit stop transparently, not error: %v", err)
		}
		if count != n {
			t.Fatalf("ORDER BY under a 1ms budget returned %d rows, want %d (pagination must be transparent)", count, n)
		}
	})
}
