package sqldriver_test

// RFC-202 gate (d), twin-plan direction: an ON-source INCLUDE index and its
// AS-SELECT twin produce the SAME plan for the same query — the D2 "one
// generator" property observed at the planner, not just in stored metadata.
// Two templates declare the identically-NAMED index through the two DDL
// forms; the EXPLAIN strings must be byte-identical.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_OnSourceIndexPlans_TwinOfAsSelect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ostw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ostw")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE ostw_on
		CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))
		CREATE INDEX x ON T(a) INCLUDE (b, c)`)
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE ostw_as
		CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))
		CREATE INDEX x AS SELECT a, b, c FROM T ORDER BY a`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ostw/s_on WITH TEMPLATE ostw_on")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ostw/s_as WITH TEMPLATE ostw_as")

	open := func(schema string) *sql.DB {
		db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_ostw?cluster_file=%s&schema=%s", clusterFilePath, schema))
		if err != nil {
			t.Fatalf("sql.Open(%s): %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	dbOn := open("s_on")
	dbAs := open("s_as")

	queries := []string{
		// Covering: key + INCLUDE columns only — both must plan COVERING(X …).
		"SELECT a, b, c FROM T WHERE a = 5",
		// Prefix-bounded ordered read.
		"SELECT b FROM T WHERE a = 5 ORDER BY a",
	}
	for _, q := range queries {
		var planOn, planAs string
		if err := dbOn.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&planOn); err != nil {
			t.Fatalf("explain on-source %q: %v", q, err)
		}
		if err := dbAs.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&planAs); err != nil {
			t.Fatalf("explain as-select %q: %v", q, err)
		}
		if planOn != planAs {
			t.Errorf("plans diverge for %q:\n on-source: %s\n as-select: %s\n— the two DDL forms build one index shape (RFC-202 D2) and must plan identically", q, planOn, planAs)
		}
		if !strings.Contains(planOn, "COVERING") {
			t.Errorf("plan for %q = %s\nwant a COVERING scan over X — the INCLUDE columns ride in the entry value and the fetch is eliminated", q, planOn)
		}
	}
}
