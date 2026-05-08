package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

func TestFDB_GroupByDerivedTableComputedExpr(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_gbderived")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_gbderived")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE gbderived_tmpl "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_gbderived/s WITH TEMPLATE gbderived_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_gbderived?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t1 VALUES "+
			"(1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5), "+
			"(6, 20, 6), (7, 20, 7), (8, 20, 8), (9, 20, 9), (10, 20, 10), "+
			"(11, 20, 11), (12, 20, 12), (13, 20, 13)")).Error().NotTo(gomega.HaveOccurred())

	// derived_table_group_by test 4: x.col1 + 10 through derived + GROUP BY
	t.Run("derived_col1_plus_10", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT x.col1 + 10 FROM (SELECT col1 FROM t1) AS x GROUP BY x.col1 ORDER BY 1")
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
		g.Expect(results).To(gomega.Equal([]int64{20, 30}))
	})

	// derived_table_group_by test 6: x.col1 + x.col1 through derived + GROUP BY
	t.Run("derived_col1_plus_col1", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT x.col1 + x.col1 FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 ORDER BY 1")
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
		g.Expect(results).To(gomega.Equal([]int64{20, 40}))
	})

	// derived_table_group_by test 7: nested aggregate in derived + outer filter
	t.Run("nested_derived_agg_plus_literal", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			`SELECT G + 4 FROM (
				SELECT MIN(x.col2) AS G FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1
			) AS Y WHERE G > 5`)
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
		g.Expect(results).To(gomega.Equal([]int64{10}))
	})

	// group_by_proj_expr test 1: a+b in projection, both in GROUP BY
	t.Run("a_plus_b_grouped", func(t *testing.T) {
		setup2 := openTestDB(t, "/testdb_gbpe")
		g.Expect(setup2.ExecContext(ctx, "CREATE DATABASE /testdb_gbpe")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup2.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE gbpe_tmpl "+
				"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup2.ExecContext(ctx,
			"CREATE SCHEMA /testdb_gbpe/s WITH TEMPLATE gbpe_tmpl")).Error().NotTo(gomega.HaveOccurred())

		dsn2 := fmt.Sprintf("fdbsql:///testdb_gbpe?cluster_file=%s&schema=s", clusterFilePath)
		db2, err := sql.Open("fdbsql", dsn2)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer db2.Close()

		g.Expect(db2.ExecContext(ctx,
			"INSERT INTO t VALUES "+
				"(1, 1, 1, 10), (2, 1, 1, 20), (3, 1, 2, 30), "+
				"(4, 2, 1, 40), (5, 2, 1, 50), (6, 2, 2, 60)")).Error().NotTo(gomega.HaveOccurred())

		rows, err := db2.QueryContext(ctx,
			"SELECT a, b, a+b, MAX(c), MIN(c), COUNT(c), AVG(c) FROM t GROUP BY a, b ORDER BY a, b")
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()
		type row struct {
			a, b, ab, maxC, minC, countC int64
			avgC                         float64
		}
		var results []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.a, &r.b, &r.ab, &r.maxC, &r.minC, &r.countC, &r.avgC)).To(gomega.Succeed())
			results = append(results, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.Equal([]row{
			{1, 1, 2, 20, 10, 2, 15.0},
			{1, 2, 3, 30, 30, 1, 30.0},
			{2, 1, 3, 50, 40, 2, 45.0},
			{2, 2, 4, 60, 60, 1, 60.0},
		}))
	})
}
