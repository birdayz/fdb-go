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

// TestFDB_LeftJoin_Continuation_Converged is the end-to-end proof for the three
// LEFT-JOIN / FlatMap continuation defects, which all converge on the Java-faithful
// DefaultOnEmpty + OrElse lowering:
//
//	F1  — DefaultOnEmpty had no continuation wrapper (single-result default cursor,
//	      nil StartContinuation): a paging loop re-emitted the default forever /
//	      resume fabricated spurious null rows. Now routed through OrElse.
//	F2  — the in-memory leftOuter flag re-decided empty-vs-nonempty from scratch on
//	      every resume; LEFT OUTER now lowers to DefaultOnEmpty (the flag is retired
//	      from production), so the OrElse USE_INNER/USE_OTHER state carries the
//	      decision across pages.
//	F24 — a LEFT-OUTER null-inner page boundary wrote a stale check_value into the
//	      advanced-outer FlatMap continuation → deterministic mismatch/abort on
//	      resume. The null-supply now flows through DefaultOnEmpty (no FlatMap
//	      check_value on that path), and the check is restart-not-error.
//
// The card-1-probe LEFT JOIN `a LEFT JOIN b ON b.aid = a.id` (index on b.aid) is
// driven under a 1-scanned-row budget so the engine paginates ACROSS inner
// boundaries and resumes purely by internal continuation. The paged result must
// equal the unpaginated one as a multiset — no dropped rows, no duplicates
// (never-advancing paging), and each unmatched outer's (a, NULL) appears EXACTLY
// once. A hard iteration cap catches a continuation that never advances instead of
// hanging the suite.
func TestFDB_LeftJoin_Continuation_Converged(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := leftJoinContDB(t, "converged")

	const q = "SELECT a.id, b.id FROM a LEFT JOIN b ON b.aid = a.id"

	// The exact LEFT-JOIN result (b.id = NULL sentinel -1 for unmatched a): a=1
	// multi-matches b 1,2,3; a=2 → b4; a=3 unmatched; a=4 → b 5,6; a=5 unmatched.
	// Pinned as an absolute expectation so a UNIFORM DefaultOnEmpty correctness
	// regression (e.g. dropping an outer's later inner rows) is caught here, while
	// the paged-vs-unpaged comparison below catches the paging-SPECIFIC defects.
	want := sortNPairs([][2]int64{
		{1, 1}, {1, 2}, {1, 3}, {2, 4}, {3, leftJoinNullSentinel}, {4, 5}, {4, 6}, {5, leftJoinNullSentinel},
	})

	// Reference: unpaginated read.
	unpaged := sortNPairs(readLeftJoinPairs(t, ctx, db, q, 0))
	if fmt.Sprint(unpaged) != fmt.Sprint(want) {
		t.Fatalf("unpaginated LEFT JOIN result wrong:\n got  = %v\n want = %v", unpaged, want)
	}

	// Same query under a 1-scanned-row budget → the driver paginates and resumes
	// via the FlatMap/DefaultOnEmpty continuation internally. maxRows caps the read
	// so a never-advancing continuation fails loudly instead of looping forever.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).
			Build())
	})
	maxRows := len(unpaged)*4 + 16
	paged := sortNPairs(readLeftJoinPairsConn(t, ctx, conn, q, maxRows))

	if len(paged) > maxRows-1 {
		t.Fatalf("paged read hit the safety cap (%d rows) — the continuation never advanced (F1/F2 paging loop)", len(paged))
	}
	if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
		t.Fatalf("LEFT JOIN paged multiset != unpaged (dropped/duplicated/spurious-null rows):\n paged (%d) = %v\n unpaged (%d) = %v",
			len(paged), paged, len(unpaged), unpaged)
	}
}

const leftJoinNullSentinel int64 = -1

func leftJoinContDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/leftjoin_cont_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "leftjoin_cont_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE a (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE b (id BIGINT, aid BIGINT, PRIMARY KEY (id))"+
		" CREATE INDEX b_aid ON b (aid)"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: 1..5. b matches: a=1 → b 1,2,3 (multi); a=2 → b 4 (single); a=4 → b 5,6
	// (multi). a=3 and a=5 have no match → null-extended.
	if _, err := db.ExecContext(ctx, "INSERT INTO a VALUES (1),(2),(3),(4),(5)"); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO b VALUES (1,1),(2,1),(3,1),(4,2),(5,4),(6,4)"); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	return db
}

// readLeftJoinPairs reads (a.id, b.id) with b.id nullable (NULL → sentinel). If
// maxRows > 0 the scan stops after maxRows rows (a guard against a never-advancing
// continuation looping forever).
func readLeftJoinPairs(t *testing.T, ctx context.Context, db *sql.DB, q string, maxRows int) [][2]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	return scanLeftJoinPairs(t, rows, maxRows)
}

func readLeftJoinPairsConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string, maxRows int) [][2]int64 {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	return scanLeftJoinPairs(t, rows, maxRows)
}

func scanLeftJoinPairs(t *testing.T, rows *sql.Rows, maxRows int) [][2]int64 {
	t.Helper()
	var out [][2]int64
	for rows.Next() {
		var a int64
		var b sql.NullInt64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		bv := leftJoinNullSentinel
		if b.Valid {
			bv = b.Int64
		}
		out = append(out, [2]int64{a, bv})
		if maxRows > 0 && len(out) >= maxRows {
			// Stop before hanging; the caller asserts len < cap.
			return out
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func sortNPairs(p [][2]int64) [][2]int64 {
	sort.Slice(p, func(i, j int) bool {
		if p[i][0] != p[j][0] {
			return p[i][0] < p[j][0]
		}
		return p[i][1] < p[j][1]
	})
	return p
}
