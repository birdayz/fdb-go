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
			"CREATE TABLE a (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT, name STRING, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
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

	// Test 12, both halves. A repeated qualified star is a legal PRODUCER and
	// an ambiguous name only where it is REFERENCED — Java's expandStar carries
	// no uniqueness rule (SemanticAnalyzer.java:332-347), and AMBIGUOUS_COLUMN
	// comes from resolveIdentifier's lookup returning more than one matching
	// attribute (SemanticAnalyzer.java:417,:422). Both halves are live-JVM
	// measured in conformance/duplicate_star_java_probe_test.go.
	//
	// The producer half is the one that must not regress quietly: rejecting it
	// (Go once answered 22023 here) reports an error on a query Java answers
	// with rows, and reports it on the INNER select — so the outer 42702 that
	// is the real error never happens, and a test asserting only the outer
	// error would still pass with the producer wrongly rejected.
	t.Run("duplicate_star_producer_is_legal", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx, "SELECT a.*, a.* FROM a ORDER BY a.id")
		g.Expect(err).NotTo(gomega.HaveOccurred(), "a repeated qualified star is legal in Java")
		defer rows.Close()
		cols, err := rows.Columns()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(cols).To(gomega.Equal([]string{"ID", "NAME", "ID", "NAME"}))
		type row struct {
			id1   int64
			name1 string
			id2   int64
			name2 string
		}
		var got []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.id1, &r.name1, &r.id2, &r.name2)).To(gomega.Succeed())
			got = append(got, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(got).To(gomega.Equal([]row{
			{1, "alpha", 1, "alpha"},
			{2, "beta", 2, "beta"},
		}))
	})

	// The consumer half: a bare reference to a name the derived row carries
	// twice is 42702, with Java's exact message text.
	t.Run("duplicate_star_derived_bare_reference_is_ambiguous", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, err := db.QueryContext(ctx,
			"SELECT id FROM (SELECT a.*, a.* FROM a) AS nested")
		g.Expect(err).To(gomega.HaveOccurred(),
			"a reference to a doubly-emitted name must be ambiguous")
		g.Expect(err.Error()).To(gomega.ContainSubstring("42702"))
		g.Expect(err.Error()).To(gomega.ContainSubstring("Ambiguous reference ID"))
	})

	// The qualified spelling of the same reference — Java renders the
	// reference AS WRITTEN, so the message carries the qualifier.
	t.Run("duplicate_star_derived_qualified_reference_is_ambiguous", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, err := db.QueryContext(ctx,
			"SELECT nested.id FROM (SELECT a.*, a.* FROM a) AS nested")
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("42702"))
		g.Expect(err.Error()).To(gomega.ContainSubstring("Ambiguous reference NESTED.ID"))
	})

	// The negative that keeps the ambiguity check honest: a name the derived
	// row carries ONCE still resolves, even though the body duplicates a
	// DIFFERENT name. A per-source (rather than per-attribute) duplicate
	// detector would reject this too.
	t.Run("duplicate_star_derived_unique_reference_answers", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx,
			"SELECT name FROM (SELECT a.id, a.id, a.name FROM a) AS nested ORDER BY name")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			g.Expect(rows.Scan(&n)).To(gomega.Succeed())
			names = append(names, n)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(names).To(gomega.Equal([]string{"alpha", "beta"}))
	})

	// Reversed qualified stars over a same-schema self-join: `SELECT t2.*,
	// t1.*` reverses the leg order relative to the FROM clause. The driver's
	// positional-read guard cannot distinguish a reversed same-schema layout
	// by names alone (both legs flow [ID, NAME]); star expansion forces a
	// projection plan whose slots are re-ordered to the SELECT list, so this
	// is correct — if a future fold ever elides that projection, this pin
	// catches the swap.
	t.Run("reversed_qualified_stars_self_join", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx,
			"SELECT t2.*, t1.* FROM a t1, a t2 WHERE t1.id = t2.id AND t1.id = 1")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue())
		var id2, id1 int64
		var name2, name1 string
		g.Expect(rows.Scan(&id2, &name2, &id1, &name1)).To(gomega.Succeed())
		g.Expect([]any{id2, name2, id1, name1}).To(gomega.Equal([]any{int64(1), "alpha", int64(1), "alpha"}))
	})
}
