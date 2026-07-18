package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func recursiveCteContDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	const dbPath = "/rec_cte_cont"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rec_cte_cont_tmpl"+
		" CREATE TABLE edges (id BIGINT, parent BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rec_cte_cont_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// A 12-node chain 1→2→…→12: each recursion level scans the edges
	// table (a store read per level), so a small scanned-rows budget
	// forces page breaks MID-recursion.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO edges VALUES (1,0),(2,1),(3,2),(4,3),(5,4),(6,5),(7,6),(8,7),(9,8),(10,9),(11,10),(12,11)"); err != nil {
		t.Fatalf("seed edges: %v", err)
	}
	return db
}

func readInt64Col(t *testing.T, ctx context.Context, q func(context.Context, string, ...any) (*sql.Rows, error), query string, cap int) []int64 {
	t.Helper()
	rows, err := q(ctx, query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
		if cap > 0 && len(out) >= cap {
			t.Fatalf("hit the safety cap (%d rows) — the recursive continuation never advanced", cap)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestFDB_RecursiveCTE_Continuation_ResumeAcrossPages is the end-to-end
// proof for the RecursiveUnionCursor continuation port (RFC-181 C1): a
// UNION ALL recursive CTE walking a stored chain, driven under a small
// scanned-rows budget, paginates MID-recursion and resumes purely by
// continuation — the token carries the phase, the active leg's position
// (including the owning TempTableInsert's partial next-frontier), and
// the scan-frontier snapshot. The paged result must equal the
// unpaginated one exactly: the pre-port executor declined every resume
// (and before the stopgap, silently re-seeded from a raw token).
func TestFDB_RecursiveCTE_Continuation_ResumeAcrossPages(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := recursiveCteContDB(t)

	// Walk the chain from the root: one recursion level per node.
	const q = "WITH RECURSIVE r(n) AS (" +
		"SELECT id FROM edges WHERE parent = 0 " +
		"UNION ALL " +
		"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n" +
		") SELECT n FROM r"

	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

	unpaged := readInt64Col(t, ctx, db.QueryContext, q, 0)
	sort.Slice(unpaged, func(i, j int) bool { return unpaged[i] < unpaged[j] })
	if fmt.Sprint(unpaged) != fmt.Sprint(want) {
		t.Fatalf("unpaginated recursion wrong:\n got  = %v\n want = %v", unpaged, want)
	}

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		// A tiny per-page scan budget: every level scans the edges table,
		// so the driver breaks pages mid-recursion and resumes via the
		// RecursiveCursorContinuation.
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 3).
			Build())
	})
	paged := readInt64Col(t, ctx, conn.QueryContext, q, len(want)*4+16)
	sort.Slice(paged, func(i, j int) bool { return paged[i] < paged[j] })
	if fmt.Sprint(paged) != fmt.Sprint(want) {
		t.Fatalf("paged recursion diverged from unpaginated:\n got  = %v\n want = %v\n(duplicates = replayed levels; missing = dropped frontier)", paged, want)
	}
}
