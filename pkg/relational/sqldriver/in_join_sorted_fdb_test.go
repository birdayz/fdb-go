package sqldriver_test

// An InJoin over a sorted IN source delivers the IN column in the source's
// order and the inner's order within each value, as Java's
// OrderingProperty.visitInJoinPlan claims, so the ORDER BY needs no sort. A
// repeated IN value runs once; paging resumes mid-value. Over a non-covering
// index the InJoin runs under the fetch (Fetch(InJoin(Covering))), where Java
// merges an IN-union for ASC (DIVERGENCES.md, RFC-191); the IN list is given
// out of order so a plan that ran the values in list order fails.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_SortedInJoinDeliversTheOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const ddl = "CREATE TABLE t5 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX i5 AS SELECT col1 FROM t5 ORDER BY col1 " +
		"CREATE TABLE tbl (id BIGINT, k BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id, k)) " +
		"CREATE INDEX ia ON tbl (a)"
	setup := openTestDB(t, "/FRL/testdb_isj")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_isj")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE isj "+ddl)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_isj/s WITH TEMPLATE isj")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_ISJ?cluster_file=%s&schema=S", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t5 VALUES (1, 20, 1), (2, 10, NULL), (3, 20, 3), "+
		"(4, NULL, NULL), (5, 10, 5), (6, 30, 6), (7, 10, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tbl VALUES (1, 1, 10, 100), (1, 2, 20, 200), (2, 1, 30, 300), "+
		"(3, 5, 40, 400), (4, 1, 50, 500), (5, 1, 20, 600)")

	explain := mwjoExplainer(t, db, ctx)
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
		plan string
	}{
		{
			"SELECT id, col1 FROM t5 WHERE col1 IN (20, 10) ORDER BY col1 DESC",
			[]string{"1|20", "3|20", "2|10", "5|10", "7|10"},
			"InJoin(",
		},
		{
			"SELECT id, col1 FROM t5 WHERE col1 IN (10, 20, 10) ORDER BY col1 DESC, id",
			[]string{"1|20", "3|20", "2|10", "5|10", "7|10"},
			"InJoin(",
		},
		{
			"SELECT id FROM t5 WHERE col1 IN (30, 10) ORDER BY col1 DESC",
			[]string{"6", "2", "5", "7"},
			"InJoin(",
		},
		{
			"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a, id, k",
			[]string{"1|1|10|100", "1|2|20|200", "5|1|20|600", "2|1|30|300"},
			"Fetch(InJoin(IndexScan(IA, [=] COVERING), binding ASC))",
		},
		{
			"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a DESC, id DESC, k DESC",
			[]string{"2|1|30|300", "5|1|20|600", "1|2|20|200", "1|1|10|100"},
			"Fetch(InJoin(IndexScan(IA, [=] COVERING) REVERSE, binding DESC))",
		},
	}
	for _, c := range cases {
		plan := explain(c.sql)
		if !strings.Contains(plan, c.plan) || strings.Contains(plan, "InMemorySort") {
			t.Errorf("%s\n  plan %s: a sorted InJoin delivers the order without a sort", c.sql, plan)
		}
		got, err := mmRows(t, ctx, db, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if !mmEqRows(got, c.want) {
			t.Errorf("%s\n  plan: %s\n  got  %v\n  want %v", c.sql, plan, got, c.want)
		}
		gotPaged, err := mhcpkRowsOnConn(ctx, paged, c.sql)
		if err != nil {
			t.Fatalf("%s (paged): %v", c.sql, err)
		}
		if !mmEqRows(gotPaged, c.want) {
			t.Errorf("%s (paged)\n  got  %v\n  want %v", c.sql, gotPaged, c.want)
		}
	}
}
