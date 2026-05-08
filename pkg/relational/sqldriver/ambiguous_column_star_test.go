package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_AmbiguousColumnStar reproduces ambiguous_column.yaml tests 7,
// 10, and 12 which exercise SELECT * through cross-joins with
// overlapping column schemas.
func TestFDB_AmbiguousColumnStar(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ambcol_star")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_ambcol_star")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ambcol_star_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_ambcol_star/s WITH TEMPLATE ambcol_star_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ambcol_star?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO a VALUES (1, 'alpha'), (2, 'beta')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO b VALUES (1, 'x'), (2, 'y')")).Error().NotTo(gomega.HaveOccurred())

	// Test 7: SELECT * FROM a, b WHERE a.id = b.id ORDER BY a.id
	// Java expands to all columns (a.id, a.name, b.id, b.name): 4 columns.
	// Go's Cascades path now matches Java.
	t.Run("select_star_cross_join_all_cols", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx,
			"SELECT * FROM a, b WHERE a.id = b.id ORDER BY a.id")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()

		colNames, err := rows.Columns()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(colNames).To(gomega.HaveLen(4), "SELECT * should expand all columns from both sources, got %v", colNames)

		type row struct {
			aID   int64
			aName string
			bID   int64
			bName string
		}
		var results []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.aID, &r.aName, &r.bID, &r.bName)).To(gomega.Succeed())
			results = append(results, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.Equal([]row{
			{1, "alpha", 1, "x"},
			{2, "beta", 2, "y"},
		}))
	})

	// Test 10: CTE + cross join with overlapping columns
	// Java expands to all columns (cx.id, cx.name, b.id, b.name): 4 columns.
	t.Run("select_star_cte_cross_join_all_cols", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx,
			"WITH cx AS (SELECT id, name FROM a) SELECT * FROM cx, b WHERE cx.id = b.id ORDER BY cx.id")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()

		colNames, err := rows.Columns()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(colNames).To(gomega.HaveLen(4), "CTE+cross join SELECT * should expand all columns, got %v", colNames)

		type row struct {
			cxID   int64
			cxName string
			bID    int64
			bName  string
		}
		var results []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.cxID, &r.cxName, &r.bID, &r.bName)).To(gomega.Succeed())
			results = append(results, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.Equal([]row{
			{1, "alpha", 1, "x"},
			{2, "beta", 2, "y"},
		}))
	})

	// Test 12: SELECT a.*, a.* creates duplicate columns in derived table.
	// Should error 22023 (invalid_parameter_value).
	t.Run("select_duplicate_star_derived_table_error", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, err := db.QueryContext(ctx,
			"SELECT id FROM (SELECT a.*, a.* FROM a) AS nested")
		g.Expect(err).To(gomega.HaveOccurred(), "SELECT a.*, a.* in derived table should error")
		g.Expect(err.Error()).To(gomega.ContainSubstring("22023"),
			"expected error code 22023, got: %v", err)
	})
}
