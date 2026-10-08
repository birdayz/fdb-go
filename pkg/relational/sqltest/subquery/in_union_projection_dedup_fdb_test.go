package sqltest

// An IN-union merges one execution per IN value and DROPS rows whose
// comparison keys tie (Java's UnionCursor). That implements the IN join only
// when the key identifies a row. A projection that loses the primary key, such
// as `SELECT s, b ... WHERE a IN (1, 2) ORDER BY s, b` over an (a, s, b) index,
// ties distinct records on (s, b), and the merge returned one of them. Java
// 4.14.2.0 plans and answers it the same wrong way
// (conformance/in_union_projection_dedup_java_probe_test.go). The arms that keep
// the primary key still merge, over one IN and over two. The index names the
// primary key in its own key: a primary key reached only past the record-type
// coordinate of the entry is no in-union key, in Go as in Java (RFC-257 WS-F 4.3
// item 2).

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

func TestFDB_InUnionMergeKeyMustIdentifyRows(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const ddl = "CREATE TABLE t (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (pk1, pk2)) " +
		"CREATE INDEX t_asb ON t (a, s, b, pk1, pk2)"
	setup := testkit.OpenDB(t, "/FRL/testdb_iupd")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_iupd")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE iupd "+ddl)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_iupd/s WITH TEMPLATE iupd")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_IUPD?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (x, 1) is shared by two a = 1 records and one a = 2 record; a = 3 is
	// outside the IN list.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (1, 1, 1, 1, 'x'), (1, 2, 1, 1, 'x'), "+
		"(2, 1, 2, 1, 'x'), (2, 2, 2, 2, 'y'), (3, 1, 3, 1, 'x'), (4, 1, 1, 3, 'y')")

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
		sql       string
		want      []string
		planHas   string // "" when no merge may serve the query
		planLacks string
	}{
		{
			sql:       "SELECT s, b FROM t WHERE a IN (1, 2) ORDER BY s, b",
			want:      []string{"x|1", "x|1", "x|1", "y|2", "y|3"},
			planLacks: "InUnion(",
		},
		{
			sql:       "SELECT s FROM t WHERE a IN (1, 2) ORDER BY s",
			want:      []string{"x", "x", "x", "y", "y"},
			planLacks: "InUnion(",
		},
		{
			sql:     "SELECT pk1, pk2, s, b FROM t WHERE a IN (1, 2) ORDER BY s, b, pk1, pk2",
			want:    []string{"1|1|x|1", "1|2|x|1", "2|1|x|1", "2|2|y|2", "4|1|y|3"},
			planHas: "InUnion(Map(",
		},
		{
			sql:     "SELECT pk1, pk2, b FROM t WHERE a IN (1, 2) AND s IN ('x', 'y') ORDER BY b, pk1, pk2",
			want:    []string{"1|1|1", "1|2|1", "2|1|1", "2|2|2", "4|1|3"},
			planHas: "bindings=2",
		},
	}
	for _, c := range cases {
		plan := explain(c.sql)
		if c.planHas != "" && !strings.Contains(plan, c.planHas) {
			t.Errorf("%s\n  plan %s lacks %q: the merge no longer serves a key that identifies rows", c.sql, plan, c.planHas)
		}
		if c.planLacks != "" && strings.Contains(plan, c.planLacks) {
			t.Errorf("%s\n  plan %s merges on a key that ties distinct records", c.sql, plan)
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
			t.Errorf("%s (paged)\n  plan: %s\n  got  %v\n  want %v", c.sql, plan, gotPaged, c.want)
		}
	}
}
