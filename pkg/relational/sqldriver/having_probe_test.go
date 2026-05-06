package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

func TestFDB_OrderByAggregateExpression(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_obaggexpr")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_obaggexpr")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE obaggexpr_tmpl CREATE TABLE t (id BIGINT NOT NULL, grp STRING, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_obaggexpr/s WITH TEMPLATE obaggexpr_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_obaggexpr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 30), (4, 'b', 5), (5, 'c', null)")).Error().NotTo(gomega.HaveOccurred())

	scanGroups := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		var result []string
		for rows.Next() {
			var grp string
			g.Expect(rows.Scan(&grp)).To(gomega.Succeed())
			result = append(result, grp)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		return result
	}

	t.Run("order_by_sum_times_2_asc", func(t *testing.T) {
		got := scanGroups("SELECT grp FROM t GROUP BY grp ORDER BY SUM(v) * 2")
		g.Expect(got).To(gomega.Equal([]string{"c", "a", "b"}))
	})

	t.Run("order_by_coalesce_sum", func(t *testing.T) {
		got := scanGroups("SELECT grp FROM t GROUP BY grp ORDER BY COALESCE(SUM(v), 0)")
		g.Expect(got).To(gomega.Equal([]string{"c", "a", "b"}))
	})

	t.Run("having_order_by_sum_times_2_desc", func(t *testing.T) {
		got := scanGroups("SELECT grp FROM t GROUP BY grp HAVING SUM(v) > 10 ORDER BY SUM(v) * 2 DESC")
		g.Expect(got).To(gomega.Equal([]string{"b", "a"}))
	})
}
