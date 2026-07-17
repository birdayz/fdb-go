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

// TestFDB_UnorderedUnion_Continuation_ResumeAcrossPages is the e2e proof for
// the resumable unordered union (Java UnorderedUnionCursor parity — RFC-180
// A2/E3 closed): a `UNION ALL` driven under a 1-scanned-row budget paginates
// per child through UnionCursorContinuation slots and resumes by
// continuation. The old eager concat DECLINED any continuation and errored on
// a child's out-of-band stop, so this query shape failed loudly under any
// scan budget. The paged result must equal the unpaginated multiset — no
// dropped child suffixes, no duplicated child prefixes. A hard cap catches a
// never-advancing continuation.
func TestFDB_UnorderedUnion_Continuation_ResumeAcrossPages(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := uuContDB(t)

	// Aggregate branches produce unordered outputs, so the planner picks the
	// UNORDERED union (a plain per-PK UNION ALL merges ordered).
	const q = "SELECT g, COUNT(*) AS cnt FROM a GROUP BY g UNION ALL SELECT g, COUNT(*) AS cnt FROM b GROUP BY g"

	if plan := planExplainVia(t, ctx, db, q); !strings.Contains(plan, "UnorderedUnion") {
		t.Fatalf("aggregate UNION ALL must plan as UnorderedUnion (this test pins its continuation), got: %s", plan)
	}

	// a: g 1,1,2,2,3 → (1,2),(2,2),(3,1); b: g 7,7,8 → (7,2),(8,1).
	want := sortNPairs([][2]int64{{1, 2}, {2, 2}, {3, 1}, {7, 2}, {8, 1}})

	unpaged := sortNPairs(readLeftJoinPairs(t, ctx, db, q, 0))
	if fmt.Sprint(unpaged) != fmt.Sprint(want) {
		t.Fatalf("unpaginated aggregate UNION ALL wrong:\n got  = %v\n want = %v", unpaged, want)
	}

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).
			Build())
	})
	maxRows := len(unpaged)*4 + 16
	paged := sortNPairs(readLeftJoinPairsConn(t, ctx, conn, q, maxRows))

	if len(paged) > maxRows-1 {
		t.Fatalf("paged read hit the safety cap (%d rows) — the union continuation never advanced", len(paged))
	}
	if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
		t.Fatalf("UNION ALL paged multiset != unpaged (dropped/duplicated child rows):\n paged (%d) = %v\n unpaged (%d) = %v",
			len(paged), paged, len(unpaged), unpaged)
	}
}

func uuContDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	const dbPath = "/uu_cont"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE uu_cont_tmpl"+
		" CREATE TABLE a (id BIGINT, g BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE b (id BIGINT, g BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE uu_cont_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO a VALUES (1,1),(2,1),(3,2),(4,2),(5,3)"); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO b VALUES (10,7),(11,7),(12,8)"); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	return db
}
