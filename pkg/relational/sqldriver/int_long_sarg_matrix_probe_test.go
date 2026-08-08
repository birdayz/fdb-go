package sqldriver_test

// Does an INTEGER/LONG (BIGINT) type mismatch cost the index probe? It does
// NOT, and this pins that. The question was filed as open work titled "stop
// INTEGER-to-LONG promotion killing index probes" — a line wrong twice over.
// It was already fixed (sharesIntegerWireEncoding, 41ecb95d1 / #517), and it
// belongs to CQ-27, not CQ-28 (which is the zero-valued FLOAT/DOUBLE item).
// This file is the committed negative result: the refutation is what licenses
// treating the item as closed, so the measurement has to outlive it.
//
// INT and LONG funnel to the SAME FDB tuple encoding (pkg/fdbgo/fdb/tuple
// encodeInt packs any Go integer width as int64, purely by magnitude), so no
// promotion is needed at the encoding boundary and none should be inserted.
// A PromoteValue wrapper over the INDEXED column would stop the SARG matcher's
// AccessorNamePath walk (it unwraps *FieldValue chains only) and degrade the
// probe to a residual full scan — while still returning CORRECT ROWS, which is
// why every cell asserts the plan and not just the rows. An EXPLAIN-only pin
// would equally miss a probe that packs the bound under the wrong tuple type
// and silently matches nothing, so both halves are asserted.
//
// THE CELLS ARE SPLIT INTO TWO NAMED GROUPS, and the split is the point:
//
//   mutation_proven  (10) — verified to go RED when the guard is removed.
//                           Live instruments for THIS mechanism.
//   inert_regression (15) — stay GREEN with both guards removed. Real
//                           coverage against OTHER regressions, but this
//                           mechanism cannot reach them.
//
// Without that split a reader sees 25 green cells and infers 25 live pins.
// The group names carry the distinction so nobody has to read a paragraph to
// avoid the wrong inference, and each group's failure text points at the
// right suspect. See each group's comment for why its cells fall where they do.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_IntLongSargMatrix(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ilsarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ilsarg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ilsarg "+
			"CREATE TABLE ti (id BIGINT, v INTEGER, PRIMARY KEY (id)) "+
			"CREATE TABLE tl (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE drv (id BIGINT, ki INTEGER, kl BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX ti_v ON ti (v) CREATE INDEX tl_v ON tl (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ilsarg/s WITH TEMPLATE ilsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_ilsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// v ∈ {10, 20, 30} in both the INTEGER and the BIGINT table, so every
	// cell of the matrix has the same expected answer for a given operator.
	mwjoMustExec(t, db, ctx, "INSERT INTO ti (id, v) VALUES (1, 10), (2, 20), (3, 30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO tl (id, v) VALUES (1, 10), (2, 20), (3, 30)")
	// One driving row whose INTEGER and BIGINT keys both hold 20, so a
	// correlated probe from drv into ti/tl selects the same single row.
	mwjoMustExec(t, db, ctx, "INSERT INTO drv (id, ki, kl) VALUES (100, 20, 20)")

	explain := func(t *testing.T, q string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %s: %v", q, err)
		}
		return plan
	}
	ids := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d", id))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return out
	}
	// reArmer names what a future change must have done for these cells to
	// stop being sargable. Without it a regression here reads as an
	// unexplained plan-shape flip, and the reader has to re-derive the whole
	// INT/LONG promotion investigation from the plan text alone.
	const reArmer = "RE-ARMED: these cells are sargable ONLY because no PromoteValue is " +
		"inserted over an INT/LONG comparison. Something reintroduced one on the " +
		"INDEXED side. Look for: expr.promoteColumnColumnNumeric losing its " +
		"sharesIntegerWireEncoding skip or its IsConstantValue early return; a new " +
		"promote-insertion site in the predicate builder; or a widen/narrow helper " +
		"extended to the INT/LONG pair. The SARG matcher's AccessorNamePath walks " +
		"*FieldValue chains and stops at any other node, so a wrapped indexed column " +
		"no longer matches the index placeholder and the probe degrades to a residual " +
		"full scan that still returns CORRECT ROWS — which is why the plan, not the " +
		"rows, is the alarm. Note this is Go doing BETTER than Java 4.12.11.0, which " +
		"inserts the promote unconditionally (RelOpValue.promoteOperands) and loses " +
		"the same probe; see DIVERGENCES.md. Removing the guard is a real regression, " +
		"not a return to parity."

	// notThisMechanism is the INERT group's failure text. Those cells cannot
	// be reddened by the INT/LONG promotion guard (see the group comment), so
	// a failure there means something ELSE broke and pointing the reader at
	// the promotion guard would send them down the wrong path.
	const notThisMechanism = "NOT THE INT/LONG PROMOTION GUARD. This cell is in the INERT group: " +
		"either both operand types already agree, or the narrower (INT) side is the " +
		"COMPARAND rather than the indexed column, so no promotion of the indexed " +
		"column is possible and removing the guard leaves this cell green (verified). " +
		"A failure here therefore indicates a DIFFERENT regression — candidates: the " +
		"value-index SARG matcher itself; the comparison-range builder for same-type " +
		"or comparand-side-promoted operands; expr.intLiteralType's fits-int32 rule " +
		"changing what a bare integer literal is typed as; or index selection/costing " +
		"no longer preferring the index. Do NOT start from promoteColumnColumnNumeric."

	// assertProbe pins BOTH halves: the plan really uses the index, and the
	// rows are right. Either alone is insufficient — a full scan returns
	// correct rows, and a mis-typed bound returns an empty index probe.
	// onFail distinguishes the two groups; see their comments.
	assertProbe := func(t *testing.T, q, wantIndex string, want []string, onFail string) {
		t.Helper()
		plan := explain(t, q)
		if !strings.Contains(plan, wantIndex) {
			t.Errorf("expected %s probe\n  query: %s\n  plan:  %s\n%s", wantIndex, q, plan, onFail)
		}
		if got := ids(t, q); !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v\n  query: %s", got, want, q)
		}
	}
	// live = a cell the INT/LONG promotion guard actually protects; its
	// failure re-arms that investigation. inert = a cell this mechanism
	// cannot reach; its failure means something else.
	live := func(t *testing.T, q, wantIndex string, want []string) {
		t.Helper()
		assertProbe(t, q, wantIndex, want, reArmer)
	}
	inert := func(t *testing.T, q, wantIndex string, want []string) {
		t.Helper()
		assertProbe(t, q, wantIndex, want, notThisMechanism)
	}

	ops := []struct {
		name, op string
		want     []string // ti/tl hold v ∈ {10,20,30} at ids 1,2,3; comparand is 20
	}{
		{"eq", "=", []string{"2"}},
		{"gt", ">", []string{"3"}},
		{"gte", ">=", []string{"2", "3"}},
		{"lt", "<", []string{"1"}},
		{"lte", "<=", []string{"1", "2"}},
	}

	// ======================================================================
	// GROUP 1 of 2 — MUTATION_PROVEN (10 cells)
	// ======================================================================
	// Cells the INT/LONG promotion guard demonstrably protects: each one is
	// verified to go RED when the guard is removed, so each is a live
	// instrument rather than a green that happens to hold.
	//
	// What unites them: the INDEXED column is the INT side. maximumType(INT,
	// LONG) is LONG, so a promotion wraps the INT operand — and only when
	// that operand IS the indexed column does the wrapper stop the SARG
	// matcher (AccessorNamePath walks *FieldValue chains and halts at any
	// other node) and cost the probe.
	//
	// Mutations verified, in two independent directions:
	//   - drop sharesIntegerWireEncoding      → the 5 correlated_bigint cells go RED
	//   - drop that AND IsConstantValue       → the 5 long_literal cells also go RED
	t.Run("mutation_proven", func(t *testing.T) {
		t.Parallel()

		// A LONG-typed literal against an INTEGER-indexed column. A bare
		// integer literal is typed INT when it fits int32 (expr.intLiteralType,
		// walk.go), so `v = 20` could never express this; the literal must be
		// explicitly LONG — the `L` suffix (pinned LONG, never re-narrowed) or
		// a magnitude beyond int32.
		t.Run("long_literal", func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name, pred string
				want       []string
			}{
				{"suffix_eq", "v = 20L", []string{"2"}},
				{"suffix_gt", "v > 20L", []string{"3"}},
				{"suffix_lte", "v <= 20L", []string{"1", "2"}},
				{"wide_lt", "v < 3000000000", []string{"1", "2", "3"}},
				{"wide_gt", "v > 3000000000", nil},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					live(t, "SELECT id FROM ti WHERE "+c.pred, "IndexScan(TI_V", c.want)
				})
			}
		})

		// An INTEGER-indexed column against a BIGINT correlated comparand.
		// Neither operand is a compile-time constant, so this is the shape
		// promoteColumnColumnNumeric actually decides about.
		t.Run("correlated_bigint", func(t *testing.T) {
			t.Parallel()
			for _, c := range ops {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					live(t, "SELECT ti.id FROM drv JOIN ti ON ti.v "+c.op+" drv.kl",
						"IndexScan(TI_V", c.want)
				})
			}
		})
	})

	// ======================================================================
	// GROUP 2 of 2 — INERT_REGRESSION (15 cells)
	// ======================================================================
	// These are REAL regression coverage but this mechanism CANNOT redden
	// them — verified: they stay green with both guards removed. They are
	// kept because they guard the SARG path against other regressions, and
	// they are named and separated so nobody reads 25 green cells as 25 live
	// pins on the promotion guard.
	//
	// Why each is unreachable:
	//   - int_literal_vs_int_col:    types already agree (bare literal is INT)
	//   - int_literal_vs_bigint_col: narrower side is the LITERAL, so a
	//                                wrapper never touches the indexed column
	//   - correlated_int:            narrower side is the COMPARAND (drv.ki),
	//                                same reason
	// A failure here is a different bug; see notThisMechanism.
	t.Run("inert_regression", func(t *testing.T) {
		t.Parallel()

		t.Run("int_literal_vs_int_col", func(t *testing.T) {
			t.Parallel()
			for _, c := range ops {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					inert(t, "SELECT id FROM ti WHERE v "+c.op+" 20", "IndexScan(TI_V", c.want)
				})
			}
		})

		t.Run("int_literal_vs_bigint_col", func(t *testing.T) {
			t.Parallel()
			for _, c := range ops {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					inert(t, "SELECT id FROM tl WHERE v "+c.op+" 20", "IndexScan(TL_V", c.want)
				})
			}
		})

		// BIGINT-indexed column vs INTEGER correlated comparand.
		t.Run("correlated_int", func(t *testing.T) {
			t.Parallel()
			for _, c := range ops {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					inert(t, "SELECT tl.id FROM drv JOIN tl ON tl.v "+c.op+" drv.ki",
						"IndexScan(TL_V", c.want)
				})
			}
		})
	})
}
