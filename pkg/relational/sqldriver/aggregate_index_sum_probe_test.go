package sqldriver_test

// Probes SUM/MIN/MAX aggregate (materialized) indexes — verifies each fires an
// AggregateIndex scan (the optimization, per NO FAKE CHECKBOXES) AND returns the
// correct grouped value. The existing RFC-106a test only pins COUNT.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_AggregateIndexSumMinMax(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggidxp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggidxp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggidxp "+
			"CREATE TABLE ga (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM ga GROUP BY g "+
			"CREATE INDEX min_by_g AS SELECT MIN(v) FROM ga GROUP BY g "+
			"CREATE INDEX max_by_g AS SELECT MAX(v) FROM ga GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggidxp/s WITH TEMPLATE aggidxp")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggidxp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO ga (id,g,v) VALUES (1,1,10),(2,1,30),(3,2,5),(4,2,25),(5,2,15)")

	check := func(name, q string, want map[int64]int64) {
		t.Run(name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "AggregateIndex") {
				t.Fatalf("%s must use an AggregateIndex scan, got: %s", name, plan)
			}
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			got := map[int64]int64{}
			var keys []int64
			for rows.Next() {
				var g, a int64
				if err := rows.Scan(&g, &a); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got[g] = a
				keys = append(keys, g)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			if len(got) != len(want) {
				t.Fatalf("%s: %d groups, want %d", name, len(got), len(want))
			}
			for g, w := range want {
				if got[g] != w {
					t.Errorf("%s: g=%d => %d, want %d", name, g, got[g], w)
				}
			}
		})
	}

	check("sum", "SELECT g, SUM(v) FROM ga GROUP BY g", map[int64]int64{1: 40, 2: 45})
	check("min", "SELECT g, MIN(v) FROM ga GROUP BY g", map[int64]int64{1: 10, 2: 5})
	check("max", "SELECT g, MAX(v) FROM ga GROUP BY g", map[int64]int64{1: 30, 2: 25})
}
