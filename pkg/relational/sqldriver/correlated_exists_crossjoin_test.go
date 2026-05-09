package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_CorrelatedExistsCrossJoin exercises correlated EXISTS combined
// with a cross-join: the EXISTS references outer table e from a query
// that cross-joins emp AS e with project AS p.
func TestFDB_CorrelatedExistsCrossJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_correxcj")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_correxcj")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE correxcj_tmpl "+
			"CREATE TABLE emp (id BIGINT NOT NULL, fname STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE project (pid BIGINT NOT NULL, emp_id BIGINT, pname STRING, PRIMARY KEY (pid))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_correxcj/s WITH TEMPLATE correxcj_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_correxcj?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO emp VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Charlie')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO project VALUES (10, 1, 'P1'), (20, 2, 'P2'), (30, 2, 'P3')")).Error().NotTo(gomega.HaveOccurred())

	// Test 11: cross-join with correlated EXISTS
	rows, err := db.QueryContext(ctx, `
		SELECT e.fname FROM emp AS e, project AS p
		WHERE e.id = p.emp_id
		  AND EXISTS (SELECT 1 FROM project WHERE emp_id = e.id)
		ORDER BY e.id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).NotTo(gomega.HaveOccurred())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Semi-join optimization: the cross-join is subsumed by the
	// correlated EXISTS (same table, same column pair). Go collapses
	// to a simple EXISTS semi-join, matching Java's 2-row result.
	g.Expect(names).To(gomega.Equal([]string{"Alice", "Bob"}))
}

// TestFDB_NestedCorrelatedExists exercises nested EXISTS: the outer
// query scans emp, the outer EXISTS scans project, and the inner EXISTS
// correlates with emp.id from the outermost scope.
func TestFDB_NestedCorrelatedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nestexists")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_nestexists")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nestexists_tmpl "+
			"CREATE TABLE emp (id BIGINT NOT NULL, fname STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE project (pid BIGINT NOT NULL, emp_id BIGINT, pname STRING, PRIMARY KEY (pid))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_nestexists/s WITH TEMPLATE nestexists_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_nestexists?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO emp VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Charlie')")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO project VALUES (10, 1, 'P1'), (20, 2, 'P2'), (30, 2, 'P3')")).Error().NotTo(gomega.HaveOccurred())

	// Test 21: nested EXISTS — inner EXISTS correlates with outermost emp.id
	rows, err := db.QueryContext(ctx, `
		SELECT fname FROM emp
		WHERE EXISTS (
		  SELECT 1 FROM project
		  WHERE EXISTS (SELECT 1 FROM project WHERE emp_id = emp.id)
		)
		ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).NotTo(gomega.HaveOccurred())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Alice", "Bob"}))
}
