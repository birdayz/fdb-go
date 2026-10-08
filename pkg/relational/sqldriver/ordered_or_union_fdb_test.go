package sqldriver_test

// An OR whose legs each deliver the ORDER BY merges them in that order and
// dedups on the primary key, as Java's ImplementDistinctUnionRule does over its
// primary-key dedup node, instead of sorting an unordered union. A record
// matching both legs comes back once; paging resumes each leg mid-stream.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_OrderedOrUnionMergesLegs(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const ddl = "CREATE TABLE t (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX t_v ON t (v) CREATE INDEX t_w ON t (w)"
	setup := testkit.OpenDB(t, "/FRL/testdb_oou")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_oou")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE oou "+ddl)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_oou/s WITH TEMPLATE oou")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_OOU?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 3 and 6 satisfy both v = 1 and w = 2.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (1, 1, 0), (2, 0, 2), (3, 1, 2), (4, 0, 0), "+
		"(5, 1, 9), (6, 1, 2), (7, 0, 2), (8, 5, 5)")

	explain := testkit.Explainer(t, db, ctx)
	paged, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer paged.Close()
	if err := paged.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T", dc)
		}
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())
		return nil
	}); err != nil {
		t.Fatalf("set scan limit: %v", err)
	}

	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT id FROM t WHERE v = 1 OR w = 2 ORDER BY id", []string{"1", "2", "3", "5", "6", "7"}},
		{"SELECT * FROM t WHERE v = 1 OR w = 2 ORDER BY id DESC", []string{"7|0|2", "6|1|2", "5|1|9", "3|1|2", "2|0|2", "1|1|0"}},
		{"SELECT id FROM t WHERE id NOT BETWEEN 2 AND 6 ORDER BY id", []string{"1", "7", "8"}},
		{"SELECT id, v FROM t WHERE id BETWEEN 2 AND 3 OR id = 7 OR id = 3 ORDER BY id", []string{"2|0", "3|1", "7|0"}},
	}
	for _, c := range cases {
		plan := explain(c.sql)
		if !strings.Contains(plan, "MergeSortUnion(") || strings.Contains(plan, "InMemorySort") {
			t.Errorf("%s\n  plan %s: the legs deliver the order, so they merge without a sort", c.sql, plan)
		}
		got, err := testkit.QueryRowStrings(t, ctx, db, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if !testkit.EqualRows(got, c.want) {
			t.Errorf("%s\n  plan: %s\n  got  %v\n  want %v", c.sql, plan, got, c.want)
		}
		gotPaged, err := testkit.MhcpkRowsOnConn(ctx, paged, c.sql)
		if err != nil {
			t.Fatalf("%s (paged): %v", c.sql, err)
		}
		if !testkit.EqualRows(gotPaged, c.want) {
			t.Errorf("%s (paged)\n  got  %v\n  want %v", c.sql, gotPaged, c.want)
		}
	}
}
