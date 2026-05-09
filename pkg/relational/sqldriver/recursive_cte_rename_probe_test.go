package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_RecursiveCTERename reproduces recursive_cte.yaml test 6:
// column-list rename on a recursive CTE. The CTE defines columns
// (node, up) but the seed projects (id, parent). The recursive branch
// references a.up — that must map to the second CTE column.
func TestFDB_RecursiveCTERename(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_rename"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_rename_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_rename_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert hierarchy: 1(root, parent=-1) -> {10,20}; 10 -> {40,50,70}; 20 -> {100,210}; 50 -> {250}
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10), (70, 10), (100, 20), (210, 20), (250, 50)")).
		Error().NotTo(gomega.HaveOccurred())

	// recursive_cte.yaml test 6: column-list rename, walk ancestors.
	t.Run("ancestors_up_chain", func(t *testing.T) {
		query := `WITH RECURSIVE ancestors(node, up) AS (
			SELECT id, parent FROM t WHERE id = 250
			UNION ALL
			SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.up
		)
		SELECT node FROM ancestors ORDER BY node DESC`

		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()

		var results []int64
		for rows.Next() {
			var v int64
			g.Expect(rows.Scan(&v)).To(gomega.Succeed())
			results = append(results, v)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		// Walk from 250 up to root: 250 -> 50 -> 10 -> 1
		g.Expect(results).To(gomega.Equal([]int64{250, 50, 10, 1}))
	})

	// Single-column rename: full tree traversal with renamed column.
	t.Run("descendants_single_col_rename", func(t *testing.T) {
		query := `WITH RECURSIVE desc2(node) AS (
			SELECT id FROM t WHERE parent = -1
			UNION ALL
			SELECT b.id FROM desc2 AS a, t AS b WHERE b.parent = a.node
		)
		SELECT node FROM desc2 ORDER BY node`

		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()

		var results []int64
		for rows.Next() {
			var v int64
			g.Expect(rows.Scan(&v)).To(gomega.Succeed())
			results = append(results, v)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		// All 9 nodes in the tree.
		g.Expect(results).To(gomega.Equal([]int64{1, 10, 20, 40, 50, 70, 100, 210, 250}))
	})
}
