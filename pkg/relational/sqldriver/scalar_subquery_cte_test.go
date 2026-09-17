package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
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
			"CREATE TABLE t (id BIGINT, g STRING, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
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

// TestFDB_CorrelatedScalarSubqueryNoIndex verifies that correlated
// scalar subqueries work correctly even WITHOUT an index on the
// correlation column. The planner falls back to Filter(Scan) instead
// of IndexScan; the filter must bind the inner row under its alias
// so QOV-based predicates resolve correctly.
func TestFDB_CorrelatedScalarSubqueryNoIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_corrssq_noidx")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_corrssq_noidx")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE corrssq_noidx_tmpl "+
			"CREATE TABLE emp (id BIGINT, fname STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE project (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_corrssq_noidx/s WITH TEMPLATE corrssq_noidx_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_corrssq_noidx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 'Alice')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO emp VALUES (2, 'Bob')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (10, 1)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (11, 1)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (12, 2)")).Error().NotTo(gomega.HaveOccurred())

	t.Run("correlated_count_no_index", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT fname, (SELECT COUNT(*) FROM project WHERE emp_id = emp.id) FROM emp ORDER BY fname")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()

		var results []struct {
			name  string
			count int64
		}
		for rows.Next() {
			var name string
			var count int64
			g.Expect(rows.Scan(&name, &count)).To(gomega.Succeed())
			results = append(results, struct {
				name  string
				count int64
			}{name, count})
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.HaveLen(2))
		g.Expect(results[0].name).To(gomega.Equal("Alice"))
		g.Expect(results[0].count).To(gomega.Equal(int64(2)))
		g.Expect(results[1].name).To(gomega.Equal("Bob"))
		g.Expect(results[1].count).To(gomega.Equal(int64(1)))
	})
}

// TestFDB_CorrelatedScalarSubqueryError verifies that correlated scalar
// subqueries referencing outer tables execute correctly via FlatMap
// when an index exists on the correlation column (IndexScan path).
func TestFDB_CorrelatedScalarSubqueryError(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_corrssq")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_corrssq")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE corrssq_tmpl "+
			"CREATE TABLE emp (id BIGINT, fname STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE project (id BIGINT, emp_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_project_emp ON project (emp_id)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_corrssq/s WITH TEMPLATE corrssq_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_corrssq?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 'Alice')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO emp VALUES (2, 'Bob')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (10, 1)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (11, 1)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO project VALUES (12, 2)")).Error().NotTo(gomega.HaveOccurred())

	t.Run("correlated_scalar_subquery_count", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT fname, (SELECT COUNT(*) FROM project WHERE emp_id = emp.id) FROM emp ORDER BY fname")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()

		var results []struct {
			name  string
			count int64
		}
		for rows.Next() {
			var name string
			var count int64
			g.Expect(rows.Scan(&name, &count)).To(gomega.Succeed())
			results = append(results, struct {
				name  string
				count int64
			}{name, count})
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(results).To(gomega.HaveLen(2))
		g.Expect(results[0].name).To(gomega.Equal("Alice"))
		g.Expect(results[0].count).To(gomega.Equal(int64(2)))
		g.Expect(results[1].name).To(gomega.Equal("Bob"))
		g.Expect(results[1].count).To(gomega.Equal(int64(1)))
	})
}

func TestFDB_ScalarCTEBodySurvivesDerivedBinding(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/scalar_cte_derived_body", "scalar_cte_derived_body",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "WITH c AS (SELECT id AS v FROM t) "+
		"SELECT * FROM (SELECT (SELECT MAX(v) FROM c) AS x FROM t) d")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing derived CTE scalar row: %v", rows.Err())
	}
	var value int64
	if err := rows.Scan(&value); err != nil || value != 7 {
		t.Fatalf("derived CTE scalar = %d, %v; want 7", value, err)
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected trailing row/error: %v", rows.Err())
	}
}

func TestFDB_ScalarCTEBodyRetainsOuterCorrelation(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/scalar_cte_outer_body", "scalar_cte_outer_body",
		"CREATE TABLE outer_t (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE seed (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	for _, statement := range []string{"INSERT INTO outer_t VALUES (7), (9)", "INSERT INTO seed VALUES (1)"} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, scalarFrom := range []string{"c", "c o"} {
		t.Run(scalarFrom, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, "SELECT o.id, "+
				"(WITH c AS (SELECT o.id AS v FROM seed) SELECT (SELECT MAX(v) FROM "+scalarFrom+") FROM seed) "+
				"FROM outer_t o ORDER BY o.id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for _, want := range []int64{7, 9} {
				if !rows.Next() {
					t.Fatalf("missing outer row %d: %v", want, rows.Err())
				}
				var id, value int64
				if err := rows.Scan(&id, &value); err != nil || id != want || value != want {
					t.Fatalf("outer/scalar = %d/%d, %v; want %d/%d", id, value, err, want, want)
				}
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("unexpected trailing row/error: %v", rows.Err())
			}
		})
	}
}

func TestFDB_ScalarCTEBodySurvivesPromotedDerivedUnion(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/scalar_cte_derived_union", "scalar_cte_derived_union",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if _, err := db.Exec("INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`WITH c AS (SELECT id AS v FROM t)
		SELECT d.x FROM (
			SELECT (SELECT MAX(v) FROM c) AS x FROM t
			UNION ALL SELECT 9.5 AS other FROM t
		) d ORDER BY d.x`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for i, want := range []float64{7, 9.5} {
		if !rows.Next() {
			t.Fatalf("missing promoted row %d: %v", i, rows.Err())
		}
		var got any
		if err := rows.Scan(&got); err != nil || got != want {
			t.Fatalf("promoted row %d = %T(%v), err=%v; want float64(%v)", i, got, got, err, want)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected trailing promoted row or error: %v", rows.Err())
	}
}

func TestFDB_ScalarCTECorrelationSurvivesPromotedDerivedUnion(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/scalar_cte_outer_union", "scalar_cte_outer_union",
		"CREATE TABLE outer_t (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE seed (id BIGINT, PRIMARY KEY (id))")
	for _, stmt := range []string{"INSERT INTO outer_t VALUES (7), (9)", "INSERT INTO seed VALUES (1)"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`SELECT o.id,
		(WITH c AS (SELECT o.id AS v FROM seed)
		 SELECT MAX(d.x) FROM (
			SELECT (SELECT MAX(v) FROM c) AS x FROM seed
			UNION ALL SELECT 0.5 AS other FROM seed
		 ) d)
		FROM outer_t o ORDER BY o.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, want := range []int64{7, 9} {
		if !rows.Next() {
			t.Fatalf("missing outer row %d: %v", want, rows.Err())
		}
		var id int64
		var got any
		if err := rows.Scan(&id, &got); err != nil || id != want || got != float64(want) {
			t.Fatalf("outer row = (%d,%T(%v)), err=%v; want (%d,float64(%d))", id, got, got, err, want, want)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected trailing outer row or error: %v", rows.Err())
	}
}

// This nested CTE/scalar/EXISTS composition is a Go extension. MAX over {7,9}
// is 9, so the correlated equality keeps only outer 9 (and its negation only 7).
func TestFDB_ScalarCTEBodySurvivesCorrelatedDerivedExists(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/scalar_cte_derived_exists", "scalar_cte_derived_exists",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if _, err := db.Exec("INSERT INTO t VALUES (7), (9)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		predicate string
		want      int64
	}{
		{"EXISTS", 9},
		{"NOT EXISTS", 7},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			rows, err := db.Query(`WITH c AS (SELECT id AS v FROM t)
				SELECT o.id FROM t o WHERE ` + tc.predicate + ` (
					SELECT o.id FROM (SELECT (SELECT MAX(v) FROM c) AS x FROM t) d
					WHERE d.x = o.id
				) ORDER BY o.id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("missing outer %d: %v", tc.want, rows.Err())
			}
			var got int64
			if err := rows.Scan(&got); err != nil || got != tc.want {
				t.Fatalf("outer row = %d, err=%v; want %d", got, err, tc.want)
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("unexpected trailing row/error: %v", rows.Err())
			}
		})
	}
}

// TestFDB_NestedExistsConsumerAdmission holds the child SQL fixed while varying
// its consumer. These unsupported results are current Go admission contracts,
// not SQL truth claims: the positive predicate has independently derived rows,
// but negated/projected uses must not gain acceptance during ownership repair.
func TestFDB_NestedExistsConsumerAdmission(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/nested_exists_consumer", "nested_exists_consumer",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE flags (k BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE seed (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	for _, statement := range []string{"INSERT INTO t VALUES (7), (9)", "INSERT INTO flags VALUES (50)", "INSERT INTO seed VALUES (1)"} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	const child = `SELECT 1 FROM seed s, t m WHERE f.k > 0 AND EXISTS (SELECT 1 FROM flags nf WHERE nf.k > 0)`
	const positive = `SELECT o.id FROM t o, flags f WHERE EXISTS (` + child + `) ORDER BY o.id`
	for _, tc := range []struct {
		name, sql string
		decline   bool
	}{
		{"positive_predicate", positive, false},
		{"negated_predicate", `SELECT o.id FROM t o, flags f WHERE NOT EXISTS (` + child + `) ORDER BY o.id`, true},
		{"positive_projection", `SELECT o.id, EXISTS (` + child + `) AS present FROM t o, flags f ORDER BY o.id`, true},
		{"negated_projection", `SELECT o.id, NOT EXISTS (` + child + `) AS present FROM t o, flags f ORDER BY o.id`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.decline {
				rows, err := db.QueryContext(ctx, tc.sql)
				if rows != nil {
					rows.Close()
				}
				requireSQLSTATE(t, err, api.ErrCodeUnsupportedOperation)
				t.Log("consumer rejected with SQLSTATE 0A000")
			}
			// A fresh valid query must still work after a declined consumer.
			// Same-planner registration atomicity is a separate builder contract.
			rows, err := db.QueryContext(ctx, positive)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for _, want := range []int64{7, 9} {
				if !rows.Next() {
					t.Fatalf("missing positive row %d: %v", want, rows.Err())
				}
				var got int64
				if err := rows.Scan(&got); err != nil || got != want {
					t.Fatalf("positive row = %d, %v; want %d", got, err, want)
				}
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("unexpected positive row/error: %v", rows.Err())
			}
		})
	}
}
