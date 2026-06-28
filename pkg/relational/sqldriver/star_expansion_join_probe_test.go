package sqldriver_test

// Probes SELECT * / qualified-star expansion across a JOIN: `*` expands all columns
// of the first table then the second in declaration order; `a.*` expands only that
// table's columns; and `b.*, a.id` preserves SELECT-LIST order (b's columns then
// a.id) — confirming the plain-projection path honors output order (the
// keys-first column-order bug is GROUP-BY-specific).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_StarExpansionJoinProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sej")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sej")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sej "+
		"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE b (bid BIGINT NOT NULL, y BIGINT, PRIMARY KEY (bid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sej/s WITH TEMPLATE sej")
	dsn := fmt.Sprintf("fdbsql:///testdb_sej?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (bid, y) VALUES (100, 9)")

	colsAndRow := func(q string) ([]string, []string) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if !rows.Next() {
			t.Fatalf("%q: no rows", q)
		}
		vals := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range vals {
			ptr[i] = &vals[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out := make([]string, len(cols))
		for i := range vals {
			out[i] = fmt.Sprintf("%v", vals[i])
		}
		return cols, out
	}
	ck := func(name, q, wantCols, wantRow string) {
		t.Run(name, func(t *testing.T) {
			cols, row := colsAndRow(q)
			if strings.Join(cols, ",") != wantCols {
				t.Errorf("%s cols = %v, want %s", name, cols, wantCols)
			}
			if strings.Join(row, ",") != wantRow {
				t.Errorf("%s row = %v, want %s", name, row, wantRow)
			}
		})
	}

	ck("star_both_tables", "SELECT * FROM a JOIN b ON a.x = b.bid", "ID,X,BID,Y", "1,100,100,9")
	ck("qualified_star_a", "SELECT a.* FROM a JOIN b ON a.x = b.bid", "ID,X", "1,100")
	ck("qualified_star_then_col", "SELECT b.*, a.id FROM a JOIN b ON a.x = b.bid", "BID,Y,ID", "100,9,1")
}
