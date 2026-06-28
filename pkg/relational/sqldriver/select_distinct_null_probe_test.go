package sqldriver_test

// Probes SELECT DISTINCT NULL semantics: DISTINCT treats two NULLs as equal (they
// collapse to a single NULL), and multi-column DISTINCT distinguishes (NULL, 5)
// from (NULL, NULL). COUNT(DISTINCT ...) is conformantly rejected (0AF00).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_SelectDistinctNullProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_distnullp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_distnullp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE distnullp "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_distnullp/s WITH TEMPLATE distnullp")
	dsn := fmt.Sprintf("fdbsql:///testdb_distnullp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: 10,10,20,NULL,NULL ; b: 1,1,2,5,NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,10,1),(2,10,1),(3,20,2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,b) VALUES (4,5)") // a NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (5)")     // a,b NULL

	t.Run("distinct_single_col_nulls_collapse", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT DISTINCT a FROM t")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a.Valid {
				out = append(out, fmt.Sprintf("%d", a.Int64))
			} else {
				out = append(out, "NULL")
			}
		}
		sort.Strings(out)
		// {10,10,20,NULL,NULL} → {10,20,NULL} (dups + the two NULLs collapse).
		if strings.Join(out, ",") != "10,20,NULL" {
			t.Errorf("DISTINCT a = %v, want [10 20 NULL] (NULLs collapse to one)", out)
		}
	})

	t.Run("distinct_multi_col", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT DISTINCT a, b FROM t")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		f := func(n sql.NullInt64) string {
			if n.Valid {
				return fmt.Sprintf("%d", n.Int64)
			}
			return "_"
		}
		var out []string
		for rows.Next() {
			var a, b sql.NullInt64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, f(a)+","+f(b))
		}
		sort.Strings(out)
		// (10,1)×2 collapse; (20,2); (NULL,5); (NULL,NULL) distinct → 4 pairs.
		if strings.Join(out, " ") != "10,1 20,2 _,5 _,_" {
			t.Errorf("DISTINCT a,b = %v, want [10,1 20,2 _,5 _,_]", out)
		}
	})

	t.Run("count_distinct_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT COUNT(DISTINCT a) FROM t")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("COUNT(DISTINCT a) error = %v, want 0AF00 (DISTINCT aggregates not supported)", err)
		}
	})
}
