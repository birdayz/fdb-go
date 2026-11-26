package testkit

// Batch probe: derived tables (FROM subquery), GROUP BY + HAVING, and
// CASE/COALESCE/NULLIF expressions. Unique db paths to stay parallel-safe.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func DgcOpen(t *testing.T, dbpath, tpl, ddl string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	setup := OpenDB(t, dbpath)
	MustExecCtx(t, setup, ctx, "CREATE DATABASE "+dbpath)
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+tpl+" "+ddl)
	MustExecCtx(t, setup, ctx, "CREATE SCHEMA "+dbpath+"/s WITH TEMPLATE "+tpl)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbpath), clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

func DgcInts(t *testing.T, db *sql.DB, ctx context.Context, q string, sortIt bool) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if sortIt {
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	}
	return out
}

func DgcEq(g, w []int64) bool {
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
