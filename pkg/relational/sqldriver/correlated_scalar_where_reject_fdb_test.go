package sqldriver_test

// Pins the boundary of the scalar-subquery-in-a-predicate Go extension.
//
// A NON-correlated aggregate scalar subquery in a WHERE comparison
// (`col <op> (SELECT MAX(x) FROM t)`) is a supported Go read extension (Java's
// grammar has no scalar subquery in an expressionAtom) and returns correct
// rows — the rowdiff harness covers it generatively.
//
// A CORRELATED scalar in WHERE now lowers through a per-outer LEFT scalar box:
// the inner result is materialized before the comparison and strict cardinality
// is checked per outer row. HAVING still has no per-group lowering and remains a
// typed 0AF00 correct-or-loud boundary.
import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_CorrelatedScalarInPredicate_Boundary(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_corrscw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_corrscw")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE corrscw "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_corrscw/s WITH TEMPLATE corrscw")
	dsn := fmt.Sprintf("fdbsql:///testdb_corrscw?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,c) VALUES (1,10,5,100),(2,3,8,100),(3,50,40,200),(4,1,2,200)")

	// The supported NON-correlated form must work AND return the right rows —
	// this is the extension the rowdiff harness exercises. Subquery MAX(b)=40
	// over the whole table; a>40 only for id=3.
	t.Run("non_correlated_where_ok", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM t WHERE a > (SELECT MAX(b) FROM t) ORDER BY id")
		if err != nil {
			t.Fatalf("non-correlated scalar subquery must be supported, got: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, v)
		}
		if len(got) != 1 || got[0] != 3 {
			t.Errorf("non-correlated scalar subquery rows = %v, want [3]", got)
		}
	})

	t.Run("correlated_where", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM t WHERE a > (SELECT MAX(r.b) FROM t AS r WHERE r.c = t.c) ORDER BY id")
		if err != nil {
			t.Fatalf("correlated WHERE scalar must be supported: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(got) != 2 || got[0] != 1 || got[1] != 3 {
			t.Fatalf("correlated WHERE scalar rows = %v, want [1 3]", got)
		}
	})

	t.Run("correlated_having_still_typed_loud", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT o.c FROM t AS o GROUP BY o.c HAVING SUM(o.b) > "+
				"(SELECT MAX(r.a) FROM t AS r WHERE r.c = o.c)")
		requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
	})
}
