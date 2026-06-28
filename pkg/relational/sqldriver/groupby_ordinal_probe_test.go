package sqldriver_test

// Probes GROUP BY by ordinal (`GROUP BY 1`) and by expression. GROUP BY <N> groups
// by the N-th SELECT item (standard SQL positional grouping) and produces correct
// per-group aggregates; GROUP BY <expr> groups by the computed expression. A
// SELECT alias is NOT visible in GROUP BY (42703). (Java visits a GROUP BY integer
// literal as an expression; if it does not treat it as an ordinal this is an
// allowed read-side extension — Go's behavior here is standard-SQL-correct.)

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_GroupByOrdinalProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gbordp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gbordp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gbordp CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gbordp/s WITH TEMPLATE gbordp")
	dsn := fmt.Sprintf("fdbsql:///testdb_gbordp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// g: 1,1,2 ; v: 10,20,30
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, g, v) VALUES (1,1,10),(2,1,20),(3,2,30)")

	pairs := func(q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("%d=%d", a, b))
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	ck := func(name, q, want string) {
		t.Run(name, func(t *testing.T) {
			if got := pairs(q); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		})
	}

	ck("group_by_column", "SELECT g, SUM(v) FROM t GROUP BY g", "1=30 2=30")
	ck("group_by_ordinal_first", "SELECT g, SUM(v) FROM t GROUP BY 1", "1=30 2=30")
	ck("group_by_ordinal_groups_right_col", "SELECT v, COUNT(*) FROM t GROUP BY 1", "10=1 20=1 30=1")
	ck("group_by_expression", "SELECT g + 0, SUM(v) FROM t GROUP BY g + 0", "1=30 2=30")

	t.Run("group_by_select_alias_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT g AS grp, COUNT(*) FROM t GROUP BY grp")
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Errorf("GROUP BY <select-alias> error = %v, want 42703 (alias not visible in GROUP BY)", err)
		}
	})
}
