package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"github.com/onsi/gomega"
)

// TestFDB_RecursiveCTEStarMetadata pins result-set COLUMN METADATA for
// `SELECT *` directly over a recursive CTE (no projection above the recursive
// plan): the recursive plan's result row carries the CTE's columns. An empty
// column set makes every database/sql Scan fail with "expected 0 destination
// arguments in Scan".
func TestFDB_RecursiveCTEStarMetadata(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/FRL/rcte_star_metadata"
	setup := testkit.OpenDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_star_meta_tmpl "+
			"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_star_meta_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1)")).Error().NotTo(gomega.HaveOccurred())

	for _, tc := range []struct {
		name  string
		query string
		col   string
	}{
		{
			// No column list: the CTE's output column is the seed's (ID).
			name:  "no_column_list",
			query: "WITH RECURSIVE c AS (SELECT id FROM t UNION ALL SELECT id + 1 FROM c WHERE id < 5) SELECT * FROM c ORDER BY id",
			col:   "ID",
		},
		{
			// Column list renames the seed: output column is V.
			name:  "column_list",
			query: "WITH RECURSIVE c(v) AS (SELECT id FROM t UNION ALL SELECT id + 1 FROM c WHERE id < 5) SELECT * FROM c ORDER BY v",
			col:   "V",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			rows, err := db.QueryContext(ctx, tc.query)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer rows.Close()

			cols, err := rows.Columns()
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(cols).To(gomega.Equal([]string{tc.col}),
				"SELECT * over a recursive CTE must surface the CTE's output column (pre-fix: no columns at all)")

			var got []int64
			for rows.Next() {
				var v int64
				g.Expect(rows.Scan(&v)).To(gomega.Succeed())
				got = append(got, v)
			}
			g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5}))
		})
	}
}
