package sqldriver_test

// Probes LIKE ... ESCAPE: an ESCAPE char makes the following %/_ literal, so
// `50\%off` ESCAPE '\' matches only the literal '50%off' (not '50Xoff'), and
// `a\_b` ESCAPE '\' matches only 'a_b' (not 'aXb'). Java's grammar has the same
// `(ESCAPE escape=STRING_LITERAL)?` production — conformant.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_LikeEscapeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_likeescp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_likeescp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE likeescp CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_likeescp/s WITH TEMPLATE likeescp")
	dsn := fmt.Sprintf("fdbsql:///testdb_likeescp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1,'50%off'),(2,'50Xoff'),(3,'a_b'),(4,'aXb')")

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
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("plain_percent_is_wildcard", "SELECT id FROM t WHERE s LIKE '50%off'", []int64{1, 2})
	ck("escaped_percent_is_literal", `SELECT id FROM t WHERE s LIKE '50\%off' ESCAPE '\'`, []int64{1})
	ck("plain_underscore_is_wildcard", "SELECT id FROM t WHERE s LIKE 'a_b'", []int64{3, 4})
	ck("escaped_underscore_is_literal", `SELECT id FROM t WHERE s LIKE 'a\_b' ESCAPE '\'`, []int64{3})
}
