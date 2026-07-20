package sqldriver_test

// Pins the boundary of the scalar-subquery-in-a-predicate Go extension.
//
// A NON-correlated aggregate scalar subquery in a WHERE comparison
// (`col <op> (SELECT MAX(x) FROM t)`) is a supported Go read extension (Java's
// grammar has no scalar subquery in an expressionAtom) and returns correct
// rows — the rowdiff harness covers it generatively.
//
// A CORRELATED scalar subquery in a WHERE or HAVING predicate is deliberately
// declined TYPED with 0AF00 ("RFC-180 correct-or-loud", plan_visitor.go): it
// has no lowering yet — its evaluation contract is pre-eval / uncorrelated
// only, so letting it through would reach the executor as an unbindable alias
// (runtime error) or, worse, silently wrong rows. This test locks in the LOUD
// rejection so a future change to the lowering can't regress the correlated
// case to a silent mis-evaluation unnoticed. Java's quantifier lowering is the
// booked follow-up that would turn these into supported queries.
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CorrelatedScalarInPredicate_DeclinesLoud(t *testing.T) {
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

	// The correlated forms must decline LOUDLY (0AF00), never silently return
	// wrong rows.
	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				// Some drivers surface the plan error on the first Next/Scan.
				defer rows.Close()
				if rows.Next() {
					t.Fatalf("correlated scalar subquery returned rows instead of declining: %s", q)
				}
				err = rows.Err()
			}
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s error = %v, want 0AF00 (correlated scalar subquery in predicate unsupported)", name, err)
			}
		})
	}
	rejected("correlated_where",
		"SELECT id FROM t WHERE a > (SELECT MAX(r.b) FROM t AS r WHERE r.c = t.c) ORDER BY id")
	rejected("correlated_having",
		"SELECT o.c FROM t AS o GROUP BY o.c HAVING SUM(o.b) > (SELECT MAX(r.a) FROM t AS r WHERE r.c = o.c)")
}
