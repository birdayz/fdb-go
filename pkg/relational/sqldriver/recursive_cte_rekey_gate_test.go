package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_RecursiveCTERekeyGate is the RFC-173 Slice 1 gate: after retiring the
// source-name reverse-map, a RENAMED recursive CTE keys its temp table under the
// CTE's OUTPUT column names (not the seed's source columns). Both legs normalize
// to those OUTPUT names before the temp-table insert.
//
// The load-bearing invariant it pins: the recursive-body normalization must NEVER
// persist a QUALIFIED datum key (e.g. "B.ID") into the temp table. The recursive
// body here is a self-join (walk AS a, t AS b), whose merged row carries qualified
// keys; recursiveRemapValues reads the qualified key but projects the BARE output
// name, so only bare output keys land in the temp table. If a qualified key were
// persisted, it would collide with the NEXT recursion level's same-qualified join
// side and clobber the live row — stalling the recursion one level early and
// dropping the DEEPEST node. The deep linear chain below makes that stall visible:
// the deepest descendant (id=8) is only reached if every level advances.
func TestFDB_RecursiveCTERekeyGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_rekey_gate"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_rekey_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_rekey_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Deep linear chain: 1 -> 2 -> 3 -> ... -> 8 (node k has parent k-1), plus
	// an off-chain sibling (100, parent 3) to prove the join-body branch is real.
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, -1), (2, 1), (3, 2), (4, 3), (5, 4), (6, 5), (7, 6), (8, 7), (100, 3)")).
		Error().NotTo(gomega.HaveOccurred())

	// Renamed recursive CTE whose recursive branch is a self-join. `a.cur` resolves
	// to the CTE's OUTPUT column CUR (post-flip: field CUR, not the source ID),
	// so the temp table must be keyed under CUR/ORIG for the join predicate to match.
	query := `WITH RECURSIVE walk(cur, orig) AS (
		SELECT id, parent FROM t WHERE id = 1
		UNION ALL
		SELECT b.id, b.parent FROM walk AS a, t AS b WHERE b.parent = a.cur
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

	// Full transitive closure from the root: the whole chain 1..8 AND the
	// off-chain node 100 (child of 3). If a qualified key had stalled the
	// recursion one level early, the deepest node 8 (and 100) would be missing;
	// if a qualified key had polluted the rows, duplicates would appear. Exactly
	// these nine, once each, proves neither happened.
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8, 100}))

	// UNION DISTINCT variant over the same renamed+joined shape: dedup keys on the
	// OUTPUT column CUR (not the inert source key the rename projects from), so
	// re-derivations collapse and the closure is produced exactly once.
	distinctQuery := `WITH RECURSIVE walk(cur, orig) AS (
		SELECT id, parent FROM t WHERE id = 1
		UNION
		SELECT b.id, b.parent FROM walk AS a, t AS b WHERE b.parent = a.cur
	)
	SELECT cur FROM walk ORDER BY cur`

	drows, err := db.QueryContext(ctx, distinctQuery)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer drows.Close()

	var dgot []int64
	for drows.Next() {
		var v int64
		g.Expect(drows.Scan(&v)).To(gomega.Succeed())
		dgot = append(dgot, v)
	}
	g.Expect(drows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(dgot).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8, 100}))
}

// TestFDB_RecursiveCTEComputedColumn_RFC173 pins the dual-window differential's
// first catch: a recursive CTE whose recursive leg projects a COMPUTED expression
// (`SELECT n + 1`). The leg's normalization WRAP re-read the leg output by its
// LOGICAL column name ("N + 1"), but the physical row is keyed by the PHYSICAL
// name (values.ProjectionColumnName → "(N + 1)") — a mismatch the tolerant name
// map absorbed as a silent NULL, so `WHERE n < 10` stopped the recursion one
// level early and count(*) returned 2 instead of 10. Every suite recursive CTE
// used bare columns (logical == physical), so this dimension was unprobed until
// the §5 differential flagged it. The fix KEEPS the wrap — it is what strips
// qualified keys before the temp-table insert (see normalizeLegToOutputColumns)
// — but makes it read by the PHYSICAL names (legPhysicalOutputNames, via the
// shared values.ProjectionColumnName naming contract), so reader and writer
// cannot drift.
func TestFDB_RecursiveCTEComputedColumn_RFC173(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_computed_col"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_computed_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_computed_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1)")).Error().NotTo(gomega.HaveOccurred())

	// Depth counter 1..10: every level must advance (a one-level stall returns 2).
	var count int64
	g.Expect(db.QueryRowContext(ctx,
		"WITH RECURSIVE c AS (SELECT id AS n FROM t UNION ALL SELECT n + 1 FROM c WHERE n < 10) SELECT count(*) FROM c",
	).Scan(&count)).To(gomega.Succeed())
	g.Expect(count).To(gomega.Equal(int64(10)), "computed-column recursive CTE must recurse the full depth (silent one-level stall = 2)")

	// And the actual values, ordered — proving each level computed n+1 correctly
	// (not NULLs padding a correct count).
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE c AS (SELECT id AS n FROM t UNION ALL SELECT n + 1 FROM c WHERE n < 10) SELECT n FROM c ORDER BY n")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var n int64
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		got = append(got, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}))
}

// TestFDB_RecursiveCTEStarSeedAliases_RFC173 pins the twice-flagged (codex P2 +
// Graefe pre-existing corner) star-seed alias drop: a projection-less seed
// (`SELECT * FROM t`) exposed no projection columns, so an explicit CTE
// column-alias list (`cte(a, b)`) never length-matched the alias gate and was
// silently dropped — the temp table stayed keyed by the base columns and a
// recursive reference to `a` was a silent NULL under the name model / a loud
// OrdinalResolutionError under the ordinal model. The fix derives the seed
// schema from the operator's output (table columns for a scan) so the alias
// list applies and the seed normalizes onto it.
func TestFDB_RecursiveCTEStarSeedAliases_RFC173(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/rcte_star_seed"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_star_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_star_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE cte(a, b) AS (SELECT * FROM t UNION ALL SELECT a + 1, b FROM cte WHERE a < 3) SELECT a, b FROM cte ORDER BY a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type row struct{ a, b int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.a, &r.b)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{1, 10}, {2, 10}, {3, 10}}),
		"star-seed CTE aliases must apply: recursion counts a to 3 with b carried")
}
