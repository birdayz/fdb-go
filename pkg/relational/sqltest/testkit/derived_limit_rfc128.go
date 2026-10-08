package testkit

// RFC-128 — SQL LIMIT/OFFSET is a uniform RecordQueryLimitPlan operator applied
// at its pipeline position, NOT a post-execution hoist. These FDB integration
// tests pin §4 scenarios 1-13: derived-table / CTE / union LIMIT correctness,
// plain top-level LIMIT, multi-page rollover (the continuation envelope), the
// EXPLAIN Limit node + Limit(Sort) ordering gate, plan-cache reuse, scan-bound
// non-regression, and shared-combinator resume. The pre-fix bug returned
// [7,8,9,10] for scenario 1 instead of [5,6,7]; every scenario here is
// revert-proven.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// rfc128DB seeds a single-column table t(id) with ids 1..10 and returns a *sql.DB
// bound to it. Each call uses a unique db/template name (tag) for t.Parallel
// isolation.
func RFC128DB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/FRL/rfc128_" + tag
	setup := OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "rfc128_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE t (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE t2 (id BIGINT, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9),(10)"); err != nil {
		t.Fatalf("seed t: %v", err)
	}
	// t2: v has duplicates so SELECT DISTINCT v is meaningful.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1,10),(2,10),(3,20),(4,20),(5,30),(6,30),(7,40),(8,40)"); err != nil {
		t.Fatalf("seed t2: %v", err)
	}
	return db, ctx
}

// getInts runs a single-int-column query and returns the rows in order.
func GetInts(t *testing.T, ctx context.Context, db queryer, q string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan (%s): %v", q, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (%s): %v", q, err)
	}
	return out
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// explainPlan returns the EXPLAIN PLAN text for q.
func ExplainPlan(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+q)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		for _, v := range vals {
			switch s := v.(type) {
			case string:
				plan.WriteString(s)
			case []byte:
				plan.Write(s)
			}
			plan.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows.Err: %v", err)
	}
	return plan.String()
}
