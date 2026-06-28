package sqldriver_test

// Probes explicit NULLS FIRST/LAST ordering (overrides the default tuple NULL
// placement), parameterized LIMIT, and negative LIMIT/OFFSET rejection. Default
// ordering is nulls-first ASC / nulls-last DESC (FDB tuple); the explicit
// modifiers override it.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_OrderNullsLimitProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_onl")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_onl")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE onl CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_onl/s WITH TEMPLATE onl")
	dsn := fmt.Sprintf("fdbsql:///testdb_onl?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (4)") // a NULL

	order := func(q string, args ...any) string {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []string
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a.Valid {
				o = append(o, fmt.Sprintf("%d", a.Int64))
			} else {
				o = append(o, "N")
			}
		}
		return strings.Join(o, ",")
	}
	ck := func(name, want, q string, args ...any) {
		t.Run(name, func(t *testing.T) {
			if got := order(q, args...); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		})
	}

	// defaults: ASC nulls-first, DESC nulls-last.
	ck("default_asc_nulls_first", "N,10,20,30", "SELECT a FROM t ORDER BY a ASC")
	ck("default_desc_nulls_last", "30,20,10,N", "SELECT a FROM t ORDER BY a DESC")
	// explicit overrides.
	ck("asc_nulls_last_override", "10,20,30,N", "SELECT a FROM t ORDER BY a NULLS LAST")
	ck("desc_nulls_first_override", "N,30,20,10", "SELECT a FROM t ORDER BY a DESC NULLS FIRST")
	ck("asc_nulls_first_explicit", "N,10,20,30", "SELECT a FROM t ORDER BY a ASC NULLS FIRST")
	ck("desc_nulls_last_explicit", "30,20,10,N", "SELECT a FROM t ORDER BY a DESC NULLS LAST")
	// parameterized LIMIT.
	ck("param_limit", "N,10", "SELECT a FROM t ORDER BY a LIMIT ?", int64(2))

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "42601") {
				t.Errorf("%s error = %v, want 42601", name, err)
			}
		})
	}
	rejected("negative_limit", "SELECT a FROM t ORDER BY id LIMIT -1")
	rejected("negative_offset", "SELECT a FROM t ORDER BY id LIMIT 2 OFFSET -1")
}
