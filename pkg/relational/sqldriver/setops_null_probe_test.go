package sqldriver_test

// Probes set-operation support + NULL handling, pinning CONFORMANCE with Java:
// only UNION ALL is supported. Java's QueryVisitor rejects UNION (distinct) with
// the identical message "only UNION ALL is supported" (UNSUPPORTED_QUERY / 0AF00),
// and INTERSECT/EXCEPT are not in Java's grammar (syntax error). UNION ALL itself
// concatenates without dedup and preserves NULLs from both sides.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_SetOpsNullProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_setopsnull")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_setopsnull")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE setopsnull "+
			"CREATE TABLE a (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_setopsnull/s WITH TEMPLATE setopsnull")
	dsn := fmt.Sprintf("fdbsql:///testdb_setopsnull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a.v = 1,2,3,NULL ; b.v = 2,3,4,NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, v) VALUES (1,1),(2,2),(3,3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (4)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, v) VALUES (1,2),(2,3),(3,4)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id) VALUES (4)")

	// UNION ALL: concatenation, no dedup, both NULLs preserved → 8 rows, 2 nulls.
	t.Run("union_all_preserves_all_and_nulls", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT v FROM a UNION ALL SELECT v FROM b")
		if err != nil {
			t.Fatalf("UNION ALL: %v", err)
		}
		defer rows.Close()
		total, nulls := 0, 0
		for rows.Next() {
			var v sql.NullInt64
			_ = rows.Scan(&v)
			total++
			if !v.Valid {
				nulls++
			}
		}
		if total != 8 || nulls != 2 {
			t.Errorf("UNION ALL = %d rows (%d null), want 8 (2 null)", total, nulls)
		}
	})

	rejected := func(name, q, wantCode string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded; Java rejects it too", name)
			}
			if !strings.Contains(err.Error(), wantCode) {
				t.Errorf("%s error = %v, want SQLSTATE %s", name, err, wantCode)
			}
		})
	}
	// UNION (distinct): rejected with the same message/code as Java
	// (QueryVisitor: UNSUPPORTED_QUERY "only UNION ALL is supported").
	rejected("union_distinct_rejected", "SELECT v FROM a UNION SELECT v FROM b", "0AF00")
	// INTERSECT / EXCEPT: not in Java's grammar → syntax error in both engines.
	rejected("intersect_rejected", "SELECT v FROM a INTERSECT SELECT v FROM b", "42601")
	rejected("except_rejected", "SELECT v FROM a EXCEPT SELECT v FROM b", "42601")
}
