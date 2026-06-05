package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_JoinColumnLabelsUnqualified pins the result-set column labels for
// joins to Java's behaviour: the FROM-alias qualifier must NOT leak into the
// user-visible column metadata. Regression for the RFC-077 7.6 source-anchored
// join rework, which made join-leg projections carry a navigation Child and so
// turned `SELECT u.name` over a join into the column label "U.NAME" instead of
// "NAME". The bug shipped green because the Go↔Java conformance suite that
// compares column metadata was excluded from `just test` and PR CI.
//
// Java ground truth (fdb-relational 4.11.1.0):
//
//	SELECT u.name, o.total  -> [NAME, TOTAL]   (qualified ref, no alias)
//	SELECT name, total      -> [NAME, TOTAL]   (bare ref)
//	SELECT u.name AS un      -> [UN]            (explicit alias wins)
//	SELECT u.name (1 table)  -> [NAME]          (single-source, was already ok)
//	SELECT *  over join      -> [UID, NAME, OID, UID, TOTAL]  (bare, dup kept)
//	SELECT u.* over join     -> [UID, NAME]
func TestFDB_JoinColumnLabelsUnqualified(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_joincols")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_joincols")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE joincols_tmpl "+
			"CREATE TABLE Users (uid BIGINT NOT NULL, name STRING, PRIMARY KEY (uid)) "+
			"CREATE TABLE Orders (oid BIGINT NOT NULL, uid BIGINT, total BIGINT, PRIMARY KEY (oid))")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_joincols/s WITH TEMPLATE joincols_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_joincols?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Users VALUES (1, 'alice'), (2, 'bob')")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Orders VALUES (10, 1, 100), (11, 1, 200), (12, 2, 300)")).
		Error().NotTo(gomega.HaveOccurred())

	queryColumns := func(query string) []string {
		rows, err := db.QueryContext(ctx, query)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "query: %s", query)
		defer rows.Close()
		cols, err := rows.Columns()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return cols
	}

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"join_qualified_ref", "SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", []string{"NAME", "TOTAL"}},
		{"join_bare_ref", "SELECT name, total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", []string{"NAME", "TOTAL"}},
		{"join_explicit_alias", "SELECT u.name AS un, o.total AS ot FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", []string{"UN", "OT"}},
		{"single_table_qualified", "SELECT u.name FROM Users u ORDER BY u.uid", []string{"NAME"}},
		{"star_over_join", "SELECT * FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", []string{"UID", "NAME", "OID", "UID", "TOTAL"}},
		{"qualified_star_over_join", "SELECT u.* FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", []string{"UID", "NAME"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(queryColumns(tc.query)).To(gomega.Equal(tc.want),
				"column labels must match Java (unqualified); query: %s", tc.query)
		})
	}
}
