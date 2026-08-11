package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_CorrelatedPrimaryUnnestTableFirstServesRows is the ROW half of
// TestPlanCorrelatedPrimaryUnnestKeepsTableFirstResolution
// (pkg/relational/core/embedded/correlated_primary_unnest_test.go), which can
// only assert that the plan does NOT contain an Explode — its package is a
// metadata-only harness with no store.
//
// `FROM S.TAGS AS E` is ambiguous by spelling: `S` names both a schema holding a
// real table `TAGS` and (in the plan-only twin) a table alias carrying an array
// column `TAGS`. Resolution is TABLE-FIRST, mirroring Java's generateAccess,
// which exhausts the CTE/table/view/function lookups before it ever reaches
// resolveCorrelatedIdentifier (LogicalOperator.java:178-226).
//
// The two readings are distinguished here by ROWS rather than by plan shape, and
// they disagree loudly. Read as the TABLE, the EXISTS is uncorrelated — it is
// true iff any row of TAGS has E = 9 — so every row of T qualifies. Read as the
// per-row ARRAY, it is true only for the T rows whose own array contains 9. The
// fixture is built so those answers are [1 2] and [2]: a misclassification
// cannot produce the expected set by accident, which is what a plan-shape
// assertion cannot guarantee. An absent Explode and a correct row set are
// different claims, and only the second is what a user gets.
func TestFDB_CorrelatedPrimaryUnnestTableFirstServesRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cputf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cputf")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cputf "+
			"CREATE TABLE t (id BIGINT, tags BIGINT ARRAY, PRIMARY KEY (id)) "+
			"CREATE TABLE tags (id BIGINT, e BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cputf/s WITH TEMPLATE cputf")
	dsn := fmt.Sprintf("fdbsql:///testdb_cputf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// T row 1's array does NOT contain 9; row 2's does. The TAGS *table* does
	// carry a row with E = 9. That asymmetry is the whole discriminator.
	for _, stmt := range []string{
		"INSERT INTO t (id, tags) VALUES (1, [1, 2])",
		"INSERT INTO t (id, tags) VALUES (2, [9])",
		"INSERT INTO tags (id, e) VALUES (10, 9)",
	} {
		if _, e := db.ExecContext(ctx, stmt); e != nil {
			t.Fatalf("%s: %v", stmt, e)
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT S.ID FROM T AS S WHERE EXISTS (SELECT E.E FROM S.TAGS AS E WHERE E.E = 9)`)
	if err != nil {
		t.Fatalf("table-first qualified source must plan and run: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if e := rows.Scan(&v); e != nil {
			t.Fatalf("scan: %v", e)
		}
		got = append(got, v)
	}
	if e := rows.Err(); e != nil {
		t.Fatalf("rows: %v", e)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("SELECT S.ID ... EXISTS (SELECT E.E FROM S.TAGS AS E WHERE E.E = 9) = %v, want [1 2] "+
			"— every T row, because S.TAGS is the TABLE and the EXISTS is uncorrelated. "+
			"[2] means the qualified source was misclassified as T's array column.", got)
	}
}
