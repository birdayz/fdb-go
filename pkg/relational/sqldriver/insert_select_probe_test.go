package sqldriver_test

// Probes INSERT ... SELECT (wire-relevant — writes records derived from a query).
// Go (like Java) supports only the POSITIONAL form `INSERT INTO dst SELECT ...`
// (SELECT columns map positionally to dst's columns); an explicit column list
// `INSERT INTO dst (cols) SELECT ...` is rejected with 0AF00, matching Java's
// QueryVisitor (`!isInsertFromSelect || ctx.columns == null`). Covers row copy
// with a WHERE filter + int→DOUBLE coercion (stored wire type correct), and
// INSERT...SELECT over a JOIN.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_InsertSelectProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inssel")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inssel")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inssel "+
			"CREATE TABLE src (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dst (id BIGINT NOT NULL, x DOUBLE, y BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX dst_x ON dst (x) "+
			"CREATE TABLE lk (id BIGINT NOT NULL, label BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inssel/s WITH TEMPLATE inssel")
	dsn := fmt.Sprintf("fdbsql:///testdb_inssel?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO src (id, a, b) VALUES (1,10,100),(2,20,200),(3,30,300)")
	mwjoMustExec(t, db, ctx, "INSERT INTO lk (id, label) VALUES (1,7),(3,9)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("positional_with_filter_and_coercion", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO dst SELECT id, a, b FROM src WHERE a > 15")
		if got := ids("SELECT id FROM dst"); !eq(got, []int64{2, 3}) {
			t.Errorf("dst ids after INSERT...SELECT WHERE a>15 = %v, want [2 3]", got)
		}
		if got := ids("SELECT id FROM dst WHERE x = 20.0"); !eq(got, []int64{2}) {
			t.Errorf("dst WHERE x=20.0 = %v, want [2] (int a stored as double x)", got)
		}
		if got := ids("SELECT id FROM dst WHERE x = 20"); !eq(got, []int64{2}) {
			t.Errorf("dst WHERE x=20 (int lit, cross-type fix) = %v, want [2]", got)
		}
	})

	t.Run("positional_over_join", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "DELETE FROM dst")
		mwjoMustExec(t, db, ctx, "INSERT INTO dst SELECT s.id, s.a, l.label FROM src s JOIN lk l ON l.id = s.id")
		if got := ids("SELECT id FROM dst"); !eq(got, []int64{1, 3}) {
			t.Errorf("dst ids after INSERT...SELECT JOIN = %v, want [1 3]", got)
		}
		var y1, y3 int64
		if err := db.QueryRowContext(ctx, "SELECT y FROM dst WHERE id = 1").Scan(&y1); err != nil {
			t.Fatalf("scan y1: %v", err)
		}
		if err := db.QueryRowContext(ctx, "SELECT y FROM dst WHERE id = 3").Scan(&y3); err != nil {
			t.Fatalf("scan y3: %v", err)
		}
		if y1 != 7 || y3 != 9 {
			t.Errorf("dst.y from join = (%d,%d), want (7,9)", y1, y3)
		}
	})

	t.Run("column_list_form_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dst (id, x, y) SELECT id, a, b FROM src")
		if err == nil {
			t.Fatal("INSERT INTO dst (cols) SELECT ... succeeded; Java rejects it")
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("column-list INSERT...SELECT error = %v, want SQLSTATE 0AF00", err)
		}
	})
}
