package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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

	// Walk the chain from the root: one recursion level per node. The
	// TRAVERSAL ORDER level_order clause PINS the level-union plan — the
	// cursor under test — now that the cost model may prefer the DFS join
	// for an unconstrained recursion (its legs match Java's insert-free
	// shape).
	const q = "WITH RECURSIVE r(n) AS (" +
		"SELECT id FROM edges WHERE parent = 0 " +
		"UNION ALL " +
		"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n" +
		") TRAVERSAL ORDER level_order SELECT n FROM r"

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

// TestFDB_RecursiveDFS_Continuation_ResumeAcrossPages is the end-to-end
// proof for the RecursiveCursor port (Java cursors/RecursiveCursor.java):
// a TRAVERSAL ORDER pre_order/post_order recursive CTE plans as the DFS
// join, paginates mid-traversal under a small scanned-rows budget, and
// resumes purely from the RecursiveContinuation token (per-depth child
// continuations + primary-key check values). DFS emission ORDER is part
// of the contract — the paged sequence must equal the unpaginated one
// EXACTLY, not as a multiset (a wrong resume depth reorders or repeats
// subtrees).
func TestFDB_RecursiveDFS_Continuation_ResumeAcrossPages(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/rec_dfs_cont"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rec_dfs_cont_tmpl"+
		" CREATE TABLE edges (id BIGINT, parent BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rec_dfs_cont_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Tree: 1 → {10, 20}; 10 → {40, 50, 70}; 20 → {100, 210}; 50 → {250}.
	// Children enumerate in PK order, so the DFS orders are deterministic.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO edges VALUES (1,0),(10,1),(20,1),(40,10),(50,10),(70,10),(100,20),(210,20),(250,50)"); err != nil {
		t.Fatalf("seed edges: %v", err)
	}

	for _, tc := range []struct {
		name, order string
		want        []int64
	}{
		{"preorder", "pre_order", []int64{1, 10, 40, 50, 250, 70, 20, 100, 210}},
		{"postorder", "post_order", []int64{40, 250, 50, 70, 10, 100, 210, 20, 1}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := "WITH RECURSIVE r(n) AS (" +
				"SELECT id FROM edges WHERE parent = 0 " +
				"UNION ALL " +
				"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n" +
				") TRAVERSAL ORDER " + tc.order + " SELECT n FROM r"

			unpaged := readInt64Col(t, ctx, db.QueryContext, q, 0)
			if fmt.Sprint(unpaged) != fmt.Sprint(tc.want) {
				t.Fatalf("unpaginated %s DFS wrong:\n got  = %v\n want = %v", tc.order, unpaged, tc.want)
			}

			// The budget must clear the resume floor: a checkpoint stores
			// each level's PRE-YIELD position (Java buildContinuation reads
			// childContinuationBefore), so a resume re-descends the saved
			// path (~depth scans) before new work — a budget below that
			// floor can never progress. 16 clears the floor while breaking
			// the ~60-scan traversal into several pages.
			conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionScannedRowsLimit, 16).
					Build())
			})
			paged := readInt64Col(t, ctx, conn.QueryContext, q, len(tc.want)*4+16)
			if fmt.Sprint(paged) != fmt.Sprint(tc.want) {
				t.Fatalf("paged %s DFS diverged (ORDER included):\n got  = %v\n want = %v\n(repeat = re-descended subtree; missing = dropped node; reorder = wrong resume depth)", tc.order, paged, tc.want)
			}
		})
	}
}

// TestFDB_RecursiveDFS_BelowFloorBudgetIsLoud pins the paging driver's
// liveness tripwire: a DFS resume re-descends the saved path (~depth
// scans — checkpoints store PRE-YIELD positions, Java's design), so a
// per-page scan budget below that floor can never progress. The driver
// must surface 54F01 — never an infinite internal page loop (the
// pre-guard behavior hung the statement forever).
func TestFDB_RecursiveDFS_BelowFloorBudgetIsLoud(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/rec_dfs_floor"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rec_dfs_floor_tmpl"+
		" CREATE TABLE edges (id BIGINT, parent BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rec_dfs_floor_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// A 12-deep chain: the DFS re-descent floor (~12 scans) dwarfs a
	// 3-scan page budget.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO edges VALUES (1,0),(2,1),(3,2),(4,3),(5,4),(6,5),(7,6),(8,7),(9,8),(10,9),(11,10),(12,11)"); err != nil {
		t.Fatalf("seed edges: %v", err)
	}
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 3).
			Build())
	})
	rows, qerr := conn.QueryContext(ctx,
		"WITH RECURSIVE r(n) AS ("+
			"SELECT id FROM edges WHERE parent = 0 "+
			"UNION ALL "+
			"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n"+
			") TRAVERSAL ORDER pre_order SELECT n FROM r")
	var iterErr error
	if qerr == nil {
		n := 0
		for rows.Next() {
			n++
			if n > 64 {
				t.Fatal("safety cap — the below-floor page loop leaked rows")
			}
		}
		iterErr = rows.Err()
		rows.Close()
	} else {
		iterErr = qerr
	}
	if iterErr == nil || !strings.Contains(iterErr.Error(), "54F01") {
		t.Fatalf("a below-floor page budget must surface 54F01 (liveness tripwire), got: %v", iterErr)
	}
}

// TestFDB_RecursiveDistinct_CycleTerminates pins the DISTINCT recursion
// extension end-to-end (Java rejects recursive UNION distinct — "only
// UNION ALL is supported"): a CYCLIC graph terminates only through the
// cross-level seen-set, on the level union and the DFS join alike. The
// replay RESUME's deterministic proof is executor-level
// (TestRecursiveDistinctReplay_ResumesMidStream) — an eager cursor only
// breaks pages under the wall-clock page limit, which no option forces
// deterministically.
func TestFDB_RecursiveDistinct_CycleTerminates(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/rec_dist_cont"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rec_dist_cont_tmpl"+
		" CREATE TABLE edge2 (eid BIGINT, src BIGINT, dst BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rec_dist_cont_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 1→2→3→4→2: the 4→2 edge closes a cycle only DISTINCT terminates.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO edge2 VALUES (1,1,2),(2,2,3),(3,3,4),(4,4,2)"); err != nil {
		t.Fatalf("seed edge2: %v", err)
	}

	for _, tc := range []struct {
		name, traversal string
	}{
		// The explicit level_order clause pins the level union — a
		// clause-less recursion leaves the choice to the cost model.
		{"level_union", " TRAVERSAL ORDER level_order"},
		{"dfs", " TRAVERSAL ORDER pre_order"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := "WITH RECURSIVE r(n) AS (" +
				"SELECT dst FROM edge2 WHERE src = 1 " +
				"UNION " +
				"SELECT e.dst FROM edge2 AS e, r WHERE e.src = r.n" +
				")" + tc.traversal + " SELECT n FROM r"

			want := []int64{2, 3, 4}
			got := readInt64Col(t, ctx, db.QueryContext, q, len(want)*6+16)
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("DISTINCT recursion over a cycle wrong:\n got  = %v\n want = %v\n(a hang here is the cycle escaping the seen-set)", got, want)
			}
		})
	}
}
