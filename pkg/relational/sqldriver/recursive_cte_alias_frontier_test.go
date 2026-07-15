package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// These tests pin the alias/frontier naming contract for
// recursive-CTE leg normalization. A leg's normalization wrap
// (normalizeLegToOutputColumns) re-reads the leg's output by its PHYSICAL
// column names. On the positional frontier the physical slot names are the
// OUTPUT names — ALIAS-preferring (executeProjection's posNames) — so a leg whose top projection carries an alias must be
// re-read by that alias, not by values.ProjectionColumnName of the projected
// value (the source column / computed rendering). Reading the source name
// against an alias-named positional row is a GetByName miss, and the ordinal
// model is loud on a miss by design (no name-map fallback):
// OrdinalResolutionError on a valid recursive CTE.
//
// The pre-fix suite only probed alias-free legs where the two renderings
// coincide (`SELECT n + 1` — physical "(N + 1)" both sides; bare-column
// renames; star seeds), so this dimension was green while broken.

// TestFDB_RecursiveCTEAliasedComputedColumn pins the recursive-branch
// shape: `SELECT n + 1 AS n`. The recursive leg's wrap always fires; the leg's
// positional row is slot-named by the alias N while the wrap (pre-fix) read
// "(N + 1)" — OrdinalResolutionError at level 1 of the recursion.
func TestFDB_RecursiveCTEAliasedComputedColumn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_alias_computed"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_alias_computed_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_alias_computed_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1)")).Error().NotTo(gomega.HaveOccurred())

	// Identical to the pinned alias-free depth counter
	// (TestFDB_RecursiveCTEComputedColumn) except the recursive branch
	// aliases its computed column: `SELECT n + 1 AS n`.
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c AS (SELECT id AS n FROM t UNION ALL SELECT n + 1 AS n FROM c WHERE n < 10) SELECT n FROM c ORDER BY n")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var n int64
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		got = append(got, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred(),
		"aliased computed recursive branch must not error (pre-fix: OrdinalResolutionError on \"(N + 1)\")")
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}))

	// UNION DISTINCT variant: the dedup path rides the same normalization wrap.
	drows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c AS (SELECT id AS n FROM t UNION SELECT n + 1 AS n FROM c WHERE n < 10) SELECT n FROM c ORDER BY n")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer drows.Close()
	var dgot []int64
	for drows.Next() {
		var n int64
		g.Expect(drows.Scan(&n)).To(gomega.Succeed())
		dgot = append(dgot, n)
	}
	g.Expect(drows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(dgot).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}))
}

// TestFDB_RecursiveCTEColumnListRenamesAliasedSeed pins the seed shape:
// an explicit CTE column list `c(v)` renames a seed whose projection carries
// its own alias (`SELECT id AS x`). The seed wrap fires (outCols V != seedOut
// X); the seed's positional row is slot-named X (the alias) while the wrap
// (pre-fix) read the SOURCE column ID — OrdinalResolutionError before the
// first row lands in the temp table.
func TestFDB_RecursiveCTEColumnListRenamesAliasedSeed(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_alias_seed_rename"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_alias_seed_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_alias_seed_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1)")).Error().NotTo(gomega.HaveOccurred())

	// Column list renames the aliased seed (X -> V); alias-free recursive branch.
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c(v) AS (SELECT id AS x FROM t UNION ALL SELECT v + 1 FROM c WHERE v < 5) SELECT v FROM c ORDER BY v")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred(),
		"column-list rename of an aliased seed must not error (pre-fix: OrdinalResolutionError on \"ID\")")
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5}))
}

// TestFDB_RecursiveCTEColumnListAndAliasedBranches combines both axes —
// a table-backed seed (FROM-less SELECT is unsupported, matching Java's
// visitSimpleTable rejection): the column list renames an aliased seed AND
// the recursive branch aliases its computed column. Both wraps fire; both
// must read the alias-named frontier slots.
func TestFDB_RecursiveCTEColumnListAndAliasedBranches(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_alias_both"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_alias_both_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_alias_both_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c(v) AS (SELECT id AS x FROM t UNION ALL SELECT v + 1 AS v FROM c WHERE v < 5) SELECT * FROM c ORDER BY v")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{"V"}),
		"SELECT * over the CTE must surface the column-list name V")
	var got []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5}))
}

// TestFDB_RecursiveCTEAliasedJoinBodyColumn pins the join-body-with-
// alias shape (`SELECT b.id AS cur, b.parent AS orig FROM walk a, t b`): a
// join body is OFF the positional frontier (merged rows carry no Positional),
// so leg re-reads resolve against the name-keyed Datum — which stores BOTH the
// qualified source key and the alias key. This must keep working whichever
// name the wrap reads (alias-preferring after the fix), and the wrap must
// still strip qualified keys from the temp table (the rekey-gate invariant):
// the deep chain only completes if every level advances.
func TestFDB_RecursiveCTEAliasedJoinBodyColumn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_alias_join_body"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_alias_join_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_alias_join_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Deep linear chain 1 -> 2 -> ... -> 6: a one-level stall drops node 6.
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, -1), (2, 1), (3, 2), (4, 3), (5, 4), (6, 5)")).
		Error().NotTo(gomega.HaveOccurred())

	query := `WITH RECURSIVE walk(cur, orig) AS (
		SELECT id, parent FROM t WHERE id = 1
		UNION ALL
		SELECT b.id AS cur, b.parent AS orig FROM walk AS a, t AS b WHERE b.parent = a.cur
	)
	SELECT cur FROM walk ORDER BY cur`

	rows, err := db.QueryContext(ctx, query)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6}))
}

// TestFDB_RecursiveCTEDuplicateAliases pins DUPLICATE output aliases in
// a recursive leg: `SELECT a + 1 AS x, b + 1 AS x` emits
// two slots BOTH named X, and any name-based re-read collapses them —
// positional GetByName resolves the FIRST matching slot (b silently copied a:
// rows (2,2),(3,3) instead of (2,11),(3,12)), while a name-keyed row would be
// last-wins (a copied b and the recursion stalled). The leg-normalization wrap
// therefore reads a projection-top leg by ORDINAL
// (values.NewFieldValueWithResolvedOrdinal — Java's FieldValue.ofOrdinalNumber
// model; ordinal position is authoritative on the frontier), so
// read i hits emitted slot i by construction and duplicate names cease to
// matter. Duplicate SELECT output names are legal SQL and preserved
// positionally everywhere else in the engine (positionalTypeFromNames keeps
// them as distinct fields).
func TestFDB_RecursiveCTEDuplicateAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_dup_alias"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_dup_alias_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_dup_alias_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())

	type row struct{ a, b int64 }
	want := []row{{1, 10}, {2, 11}, {3, 12}}

	// Column-list form.
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c(a, b) AS (SELECT id, v FROM t UNION ALL SELECT a + 1 AS x, b + 1 AS x FROM c WHERE a < 3) SELECT a, b FROM c ORDER BY a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.a, &r.b)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal(want),
		"duplicate aliases must stay positional: b advances independently of a (name-collapsed reads give (2,2),(3,3))")

	// Seed-alias form (no column list) — the corpus shape
	// recursive_cte_duplicate_alias_branch; sum(b)=33 only if b advances.
	var sum int64
	g.Expect(db.QueryRowContext(ctx,
		"WITH RECURSIVE c AS (SELECT id AS a, v AS b FROM t UNION ALL SELECT a + 1 AS x, b + 1 AS x FROM c WHERE a < 3) SELECT sum(b) FROM c",
	).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum).To(gomega.Equal(int64(33)), "sum(b): 10+11+12; first-match collapse gives 15, last-wins stall gives 21")
}
