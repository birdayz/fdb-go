package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_ScalarSubqueryCTE verifies that a scalar subquery can reference
// a CTE defined in the outer WITH clause. Regression test for the bug
// where `(SELECT MIN(v) FROM high)` returned NULL instead of the correct
// aggregate because the Cascades planner didn't propagate CTE scope to
// scalar subquery planning.
func TestFDB_ScalarSubqueryCTE(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ssq_cte")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_ssq_cte")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ssq_cte_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, g STRING, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_ssq_cte/s WITH TEMPLATE ssq_cte_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ssq_cte?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 30), (4, 'b', 40), (5, 'c', null)")).
		Error().NotTo(gomega.HaveOccurred())

	t.Run("scalar_subquery_references_cte", func(t *testing.T) {
		// The scalar subquery (SELECT MIN(v) FROM high) references CTE "high".
		// Expected: [1, 30] because high = {30, 40}, MIN = 30.
		rows, err := db.QueryContext(ctx,
			"WITH high AS (SELECT v FROM t WHERE v > 25) "+
				"SELECT id, (SELECT MIN(v) FROM high) FROM t WHERE id = 1")
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()

		g.Expect(rows.Next()).To(gomega.BeTrue(), "expected one row")
		var id int64
		var minV sql.NullInt64
		g.Expect(rows.Scan(&id, &minV)).To(gomega.Succeed())
		g.Expect(id).To(gomega.Equal(int64(1)))
		g.Expect(minV.Valid).To(gomega.BeTrue(), "MIN(v) should not be NULL")
		g.Expect(minV.Int64).To(gomega.Equal(int64(30)))
		g.Expect(rows.Next()).To(gomega.BeFalse(), "expected exactly one row")
	})

	t.Run("scalar_subquery_references_cte_max", func(t *testing.T) {
		// Same shape but with MAX.
		rows, err := db.QueryContext(ctx,
			"WITH high AS (SELECT v FROM t WHERE v > 25) "+
				"SELECT id, (SELECT MAX(v) FROM high) FROM t WHERE id = 2")
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()

		g.Expect(rows.Next()).To(gomega.BeTrue(), "expected one row")
		var id int64
		var maxV sql.NullInt64
		g.Expect(rows.Scan(&id, &maxV)).To(gomega.Succeed())
		g.Expect(id).To(gomega.Equal(int64(2)))
		g.Expect(maxV.Valid).To(gomega.BeTrue(), "MAX(v) should not be NULL")
		g.Expect(maxV.Int64).To(gomega.Equal(int64(40)))
		g.Expect(rows.Next()).To(gomega.BeFalse(), "expected exactly one row")
	})
}
