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

	// derived_table_group_by test 5 (index 4): x.col1 + x.col2 where col2 is
	// NOT in GROUP BY must error 42803. Java rejects this because col2 is
	// neither grouped nor aggregated.
	t.Run("derived_col1_plus_col2_ungrouped_42803", func(t *testing.T) {
		_, err := db.QueryContext(ctx,
			"SELECT x.col1 + x.col2 FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1")
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("42803"))
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

	// group_by_validation test 18: GROUP BY x.col1 AS z with derived table.
	// The alias z must be usable in SELECT (MAX(z)) and ORDER BY (ORDER BY z).
	// Pre-fix: errored 42703 "column Z does not exist" because the scope
	// walker didn't recognise GROUP BY aliases and the Cascades sort key
	// referenced a non-existent field.
	t.Run("group_by_alias_derived_max_z", func(t *testing.T) {
		// Use a separate DB/schema to match YAML test data:
		// t1 rows: (1,10,100), (2,10,200), (3,20,300)
		setupA := openTestDB(t, "/testdb_gbalias")
		g.Expect(setupA.ExecContext(ctx, "CREATE DATABASE /testdb_gbalias")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setupA.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE gbalias_tmpl "+
				"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setupA.ExecContext(ctx,
			"CREATE SCHEMA /testdb_gbalias/s WITH TEMPLATE gbalias_tmpl")).Error().NotTo(gomega.HaveOccurred())

		dsnA := fmt.Sprintf("fdbsql:///testdb_gbalias?cluster_file=%s&schema=s", clusterFilePath)
		dbA, openErr := sql.Open("fdbsql", dsnA)
		g.Expect(openErr).NotTo(gomega.HaveOccurred())
		defer dbA.Close()

		g.Expect(dbA.ExecContext(ctx,
			"INSERT INTO t1 VALUES (1, 10, 100), (2, 10, 200), (3, 20, 300)")).Error().NotTo(gomega.HaveOccurred())

		rows, err := dbA.QueryContext(ctx,
			`SELECT MAX(z) FROM (SELECT col1 FROM t1) AS x GROUP BY x.col1 AS z ORDER BY z`)
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
		// MAX(z) where z is the group key (col1):
		// col1=10 group → MAX(10) = 10
		// col1=20 group → MAX(20) = 20
		// ORDER BY z (ASC) → [10, 20]
		g.Expect(results).To(gomega.Equal([]int64{10, 20}))
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

	// group_by_multi test 9: SELECT expr AS alias FROM t GROUP BY expr HAVING ... ORDER BY agg
	// The GROUP BY expression (amt/100) is an ArithmeticValue. The projection
	// references it as a FieldValue whose name is the raw SQL text. The aggregate
	// executor stores the group key under ExplainValue (with outer parens). If
	// the projection can't find the value, it returns NULL for every row.
	t.Run("expr_group_by_with_having_order_by_agg", func(t *testing.T) {
		setup4 := openTestDB(t, "/testdb_gbexpr")
		g.Expect(setup4.ExecContext(ctx, "CREATE DATABASE /testdb_gbexpr")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup4.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE gbexpr_tmpl "+
				"CREATE TABLE sales (id BIGINT NOT NULL, region STRING, category STRING, amt BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup4.ExecContext(ctx,
			"CREATE SCHEMA /testdb_gbexpr/s WITH TEMPLATE gbexpr_tmpl")).Error().NotTo(gomega.HaveOccurred())

		dsn4 := fmt.Sprintf("fdbsql:///testdb_gbexpr?cluster_file=%s&schema=s", clusterFilePath)
		db4, err := sql.Open("fdbsql", dsn4)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer db4.Close()

		g.Expect(db4.ExecContext(ctx,
			"INSERT INTO sales VALUES "+
				"(1, 'east', 'a', 100), (2, 'east', 'a', 50), (3, 'east', 'b', 200), "+
				"(4, 'west', 'a', 300), (5, 'west', 'b', 400), (6, 'west', 'b', 25)")).Error().NotTo(gomega.HaveOccurred())

		rows, err := db4.QueryContext(ctx,
			"SELECT amt / 100 AS bucket FROM sales GROUP BY amt / 100 HAVING COUNT(*) >= 1 ORDER BY MAX(amt) DESC")
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()
		var results []int64
		for rows.Next() {
			var v sql.NullInt64
			g.Expect(rows.Scan(&v)).To(gomega.Succeed())
			if !v.Valid {
				t.Fatal("got NULL, expected integer value")
			}
			results = append(results, v.Int64)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.Equal([]int64{4, 3, 2, 1, 0}))
	})

	// group_by_proj_expr test 2: no aggregates, just expression on group cols
	t.Run("a_times_100_plus_b_no_agg", func(t *testing.T) {
		setup3 := openTestDB(t, "/testdb_gbpe2")
		g.Expect(setup3.ExecContext(ctx, "CREATE DATABASE /testdb_gbpe2")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup3.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE gbpe2_tmpl "+
				"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setup3.ExecContext(ctx,
			"CREATE SCHEMA /testdb_gbpe2/s WITH TEMPLATE gbpe2_tmpl")).Error().NotTo(gomega.HaveOccurred())

		dsn3 := fmt.Sprintf("fdbsql:///testdb_gbpe2?cluster_file=%s&schema=s", clusterFilePath)
		db3, err := sql.Open("fdbsql", dsn3)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer db3.Close()

		g.Expect(db3.ExecContext(ctx,
			"INSERT INTO t VALUES "+
				"(1, 1, 1, 10), (2, 1, 1, 20), (3, 1, 2, 30), "+
				"(4, 2, 1, 40), (5, 2, 1, 50), (6, 2, 2, 60)")).Error().NotTo(gomega.HaveOccurred())

		rows, err := db3.QueryContext(ctx,
			"SELECT a, b, a*100+b FROM t GROUP BY a, b ORDER BY a, b")
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()
		type row struct{ a, b, expr int64 }
		var results []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.a, &r.b, &r.expr)).To(gomega.Succeed())
			results = append(results, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.Equal([]row{
			{1, 1, 101}, {1, 2, 102}, {2, 1, 201}, {2, 2, 202},
		}))
	})

	// Cross-join with derived table: t.id must resolve to the outer
	// table's id, not the derived table's (which shares the same
	// underlying record type).
	t.Run("cross_join_derived_qualified_column", func(t *testing.T) {
		setupCJ := openTestDB(t, "/testdb_cjderived")
		g.Expect(setupCJ.ExecContext(ctx, "CREATE DATABASE /testdb_cjderived")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setupCJ.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE cjderived_tmpl "+
				"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(setupCJ.ExecContext(ctx,
			"CREATE SCHEMA /testdb_cjderived/s WITH TEMPLATE cjderived_tmpl")).Error().NotTo(gomega.HaveOccurred())

		dsnCJ := fmt.Sprintf("fdbsql:///testdb_cjderived?cluster_file=%s&schema=s", clusterFilePath)
		dbCJ, openErr := sql.Open("fdbsql", dsnCJ)
		g.Expect(openErr).NotTo(gomega.HaveOccurred())
		defer dbCJ.Close()

		g.Expect(dbCJ.ExecContext(ctx,
			"INSERT INTO t VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30), (4, 2, 40), (5, 3, 50)")).Error().NotTo(gomega.HaveOccurred())

		// SELECT t.id FROM t, (SELECT id FROM t WHERE id <= 2) AS x ORDER BY t.id
		// Outer table t has 5 rows, derived table x has 2 rows.
		// Cross product: 10 rows, t.id cycling through all 5 values twice.
		rows, err := dbCJ.QueryContext(ctx,
			"SELECT t.id FROM t, (SELECT id FROM t WHERE id <= 2) AS x ORDER BY t.id")
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
		g.Expect(results).To(gomega.Equal([]int64{1, 1, 2, 2, 3, 3, 4, 4, 5, 5}))
	})
}
