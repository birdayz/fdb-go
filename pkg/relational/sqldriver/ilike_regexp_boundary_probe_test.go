package sqldriver_test

// Pins the case-insensitive/regex matching boundary: LIKE is case-sensitive with %
// and _ wildcards; ILIKE and REGEXP are not in the grammar (42601). The supported
// case-insensitive idiom is UPPER(col) LIKE 'PATTERN%'.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_IlikeRegexpBoundaryProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ilrp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ilrp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ilrp CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ilrp/s WITH TEMPLATE ilrp")
	dsn := fmt.Sprintf("fdbsql:///testdb_ilrp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1,'Apple'),(2,'banana')")

	count := func(where string) (int, error) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}
	matches := func(name, where string, want int) {
		t.Run(name, func(t *testing.T) {
			n, err := count(where)
			if err != nil {
				t.Fatalf("%s: %v", where, err)
			}
			if n != want {
				t.Errorf("%s matched %d, want %d", where, n, want)
			}
		})
	}
	rejected := func(name, where string) {
		t.Run(name, func(t *testing.T) {
			_, err := count(where)
			if err == nil || !strings.Contains(err.Error(), "42601") {
				t.Errorf("%s error = %v, want 42601 (not in grammar)", where, err)
			}
		})
	}

	matches("like_case_sensitive_match", "s LIKE 'A%'", 1)
	matches("like_case_sensitive_nomatch", "s LIKE 'a%'", 0)
	matches("like_underscore_wildcard", "s LIKE 'App__'", 1) // 'Apple' = App + le
	matches("upper_like_case_insensitive", "UPPER(s) LIKE 'A%'", 1)
	rejected("ilike_unsupported", "s ILIKE 'a%'")
	rejected("regexp_unsupported", "s REGEXP 'App'")
}
