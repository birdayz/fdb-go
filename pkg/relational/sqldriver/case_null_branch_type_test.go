package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_CaseNullBranchColumnType pins the result-set column TYPE of a CASE
// (and COALESCE) expression that has a literal NULL branch: the NULL carries no
// type and must be ignored, so the result is typed by the concrete branch
// (BIGINT / STRING), NOT UNKNOWN. Regression for RFC-082 / codex / Graefe: a
// literal NULL is `NewNullValue(TypeUnknown)` whose type CODE is unknown, so it
// must be detected by value kind (*values.NullValue) — otherwise commonBranchType
// poisons the whole CASE to UNKNOWN. A non-NULL genuinely-unknown branch DOES
// keep the result UNKNOWN; that's the distinction this test guards.
func TestFDB_CaseNullBranchColumnType(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_casenull")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_casenull")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE casenull_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx, "CREATE SCHEMA /testdb_casenull/s WITH TEMPLATE casenull_tmpl")).
		Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_casenull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()
	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1, 'a'), (2, 'b')")).Error().NotTo(gomega.HaveOccurred())

	colType := func(query string) string {
		rows, err := db.QueryContext(ctx, query)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "query: %s", query)
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(len(cts)).To(gomega.BeNumerically(">=", 2))
		return cts[1].DatabaseTypeName()
	}

	cases := []struct{ name, query, want string }{
		{"case_then_null_else_bigint", "SELECT id, CASE WHEN id = 1 THEN NULL ELSE id END FROM t ORDER BY id", "BIGINT"},
		{"case_then_string_else_null", "SELECT id, CASE WHEN id = 1 THEN s ELSE NULL END FROM t ORDER BY id", "STRING"},
		{"coalesce_null_bigint", "SELECT id, COALESCE(NULL, id) FROM t ORDER BY id", "BIGINT"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(colType(tc.query)).To(gomega.Equal(tc.want),
				"literal NULL branch must not poison the type to UNKNOWN; query: %s", tc.query)
		})
	}
}
