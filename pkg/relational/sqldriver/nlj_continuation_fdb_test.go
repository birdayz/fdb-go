package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_NLJ_Continuation_ResumeAcrossPages is the end-to-end proof for the
// real nested-loop-join page continuation (RFC-180): the retired
// implementation emitted a fake one-byte marker per row, which a resume
// forwarded RAW to the outer child — a key-value cursor's raw-suffix fallback
// could silently accept it as a scan position and return wrong rows. The
// theta join here plans as NestedLoopJoin (EXPLAIN-pinned, no index to
// absorb the predicate) and is driven under a 1-scanned-row budget so the
// driver paginates mid-join and resumes purely by continuation. The paged
// result must equal the unpaginated one exactly — no dropped rows from a
// restarted outer scan, no duplicates from a restarted inner iteration. A
// hard cap catches a never-advancing continuation.
func TestFDB_NLJ_Continuation_ResumeAcrossPages(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := nljContDB(t)

	// LEFT JOIN on a NON-indexed column: the correlated FlatMap rewrite has
	// no index probe, so the materialized NestedLoopJoin implementation wins.
	const q = "SELECT a.id, b.id FROM a LEFT JOIN b ON b.v = a.id"

	if plan := planExplainVia(t, ctx, db, q); !strings.Contains(plan, "NestedLoopJoin(LEFT OUTER") {
		t.Fatalf("non-indexed LEFT JOIN must plan as a materialized NestedLoopJoin (this test pins the NLJ continuation), got: %s", plan)
	}

	// a: 1..4; b: (id, v) = (1,1),(2,2),(3,3),(4,1). Expected LEFT pairs from
	// the seed: matches on b.v = a.id, unmatched outers null-extended.
	type bRow struct{ id, v int64 }
	bs := []bRow{{1, 1}, {2, 2}, {3, 3}, {4, 1}}
	var want [][2]int64
	for a := int64(1); a <= 4; a++ {
		matched := false
		for _, b := range bs {
			if b.v == a {
				want = append(want, [2]int64{a, b.id})
				matched = true
			}
		}
		if !matched {
			want = append(want, [2]int64{a, leftJoinNullSentinel})
		}
	}
	want = sortNPairs(want)

	unpaged := sortNPairs(readLeftJoinPairs(t, ctx, db, q, 0))
	if fmt.Sprint(unpaged) != fmt.Sprint(want) {
		t.Fatalf("unpaginated LEFT NLJ wrong:\n got  = %v\n want = %v", unpaged, want)
	}

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		// 5 scanned rows per page: the materialized inner side (4 rows of b)
		// re-collects each page and the budget's remainder admits ONE outer
		// step — so the join paginates one outer row per page, resuming via
		// the NLJ continuation's between-outer wrap. (A budget below the
		// inner's size cannot run a materialized NLJ at all: the buffered
		// collect correctly errors 54F01 rather than truncate.)
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 5).
			Build())
	})
	maxRows := len(unpaged)*4 + 16
	paged := sortNPairs(readLeftJoinPairsConn(t, ctx, conn, q, maxRows))

	if len(paged) > maxRows-1 {
		t.Fatalf("paged read hit the safety cap (%d rows) — the NLJ continuation never advanced", len(paged))
	}
	if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
		t.Fatalf("NLJ paged multiset != unpaged (wrong resume position / duplicated inner iteration):\n paged (%d) = %v\n unpaged (%d) = %v",
			len(paged), paged, len(unpaged), unpaged)
	}
}

func nljContDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	const dbPath = "/nlj_cont"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE nlj_cont_tmpl"+
		" CREATE TABLE a (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE b (id BIGINT, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE nlj_cont_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO a VALUES (1),(2),(3),(4)"); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO b VALUES (1,1),(2,2),(3,3),(4,1)"); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	return db
}
