package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// TestFDB_PaginationLimit_RFC127 pins the RFC-127 (audit P0) fix end-to-end: the internal pagination
// drain (paginatingRows) must not truncate the result set. The bug treated a non-terminal
// StartContinuation (nil bytes) as exhaustion. These integration cases pin the branches the deterministic
// unit test (TestPageContinuationState) asserts in isolation:
//   - LIMIT 0 → exactly 0 rows, no error (the ReturnLimitReached→exhausted branch).
//   - LIMIT N (N>0), incl. a blocking ORDER BY → exactly N rows: the plan yields a BytesContinuation after
//     the first row, so ReturnLimitReached+StartContinuation is never hit for N>0 (Torvalds fix-2).
//   - A full scan and a full ORDER BY (blocking) drain the COMPLETE result set — the regression the bug
//     produced (a short result) is impossible.
func TestFDB_PaginationLimit_RFC127(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/pglim_rfc127"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "pglim_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES "+
		"(1,100),(2,200),(3,300),(4,400),(5,500),(6,600),(7,700),(8,800),(9,900),(10,1000)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	count := func(q string) int {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows.Err: %v", q, err)
		}
		return n
	}

	cases := []struct {
		q    string
		want int
	}{
		{"SELECT id, v FROM t LIMIT 0", 0},                 // ReturnLimitReached → exhausted, 0 rows
		{"SELECT id, v FROM t LIMIT 3", 3},                 // N>0 → BytesContinuation, never trunc
		{"SELECT id, v FROM t", 10},                        // full scan must drain completely
		{"SELECT id, v FROM t ORDER BY v DESC LIMIT 5", 5}, // blocking op + LIMIT N
		{"SELECT id, v FROM t ORDER BY v DESC", 10},        // blocking op full drain must not truncate
	}
	for _, tc := range cases {
		if n := count(tc.q); n != tc.want {
			t.Errorf("%q: got %d rows, want %d", tc.q, n, tc.want)
		}
	}
}
