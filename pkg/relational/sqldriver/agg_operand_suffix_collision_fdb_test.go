package sqldriver_test

import (
	"fmt"
	"testing"
)

// TestFDB_AggregateOperandSuffixDoesNotCollideOnASharedLeaf pins that an
// aggregate's operand binds to ITS OWN column when two aggregates in one SELECT
// list have arguments whose spellings share a suffix.
//
// WHAT THIS GUARDS. upgradeAggregateOperands matches a parsed argument against
// the aggregate slot's operand TEXT, and it needs more than one candidate
// spelling because the builder strips the SOURCE qualifier when naming a slot —
// `COUNT(a.n.sk)` is named "N.SK" there. The set of candidates is therefore the
// reference as parsed plus the reference with its leading segment removed.
//
// It was briefly EVERY suffix, which is a wider claim and a different rule.
// Over two sources, `SUM(a.n.sk)` and `SUM(b.m.sk)` name different columns of
// different tables, and their all-suffix sets share the bare leaf "SK" — so a
// slot named "SK" could be claimed by either aggregate, and whichever ran second
// would overwrite the first's operand. The failure is a WRONG NUMBER, not an
// error: both operands are BIGINT and both aggregates are well-formed.
//
// The two legs' values are disjoint and their sums differ, so a cross-bound
// operand shows up directly in the assertions below.
//
// MEASURED: this did not reproduce even under the all-suffix rule — the slot
// names carry enough qualification that no live shape collided. That is what
// makes this a pin on a LATENT hazard rather than a regression test, and it is
// worth keeping for exactly that reason: the narrowing that removed the hazard
// is invisible to every other test in the suite, so without this nothing would
// notice it being widened again.
func TestFDB_AggregateOperandSuffixDoesNotCollideOnASharedLeaf(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dupAliasSurfaceDB(t, "aggsuffix")

	// zn.n is nst(sk, co) with rows (11,12) and (21,22).
	// zp.m is nst(sk, co) with rows (31,32), (41,42), (51,52).
	// The two struct columns are named DIFFERENTLY (n / m) and their members
	// share the leaf names, which is the shape that makes the bare leaf a
	// colliding spelling while the one-segment-dropped form stays distinct
	// ("N.SK" vs "M.SK").
	for _, tc := range []struct {
		name     string
		sql      string
		wantRows string
		why      string
	}{{
		name:     "SUM over two struct members sharing the leaf SK",
		sql:      "SELECT SUM(a.n.sk), SUM(b.m.sk) FROM zn AS a, zp AS b",
		wantRows: "[96 246]",
		why: "zn.n.sk sums 11+21=32 over 3 cross-product repetitions = 96; " +
			"zp.m.sk sums 31+41+51=123 over 2 = 246. A cross-bound operand makes " +
			"the two columns equal, which no other assertion in the suite would see.",
	}, {
		name:     "MIN and MAX over the same shared leaf",
		sql:      "SELECT MIN(a.n.sk), MIN(b.m.sk), MAX(a.n.sk), MAX(b.m.sk) FROM zn AS a, zp AS b",
		wantRows: "[11 31 21 51]",
		why: "the extrema of the two legs do not overlap at all, so a collision " +
			"cannot be masked by coincidentally equal aggregates.",
	}, {
		name:     "the CO member, so the collision is not specific to SK",
		sql:      "SELECT SUM(a.n.co), SUM(b.m.co) FROM zn AS a, zp AS b",
		wantRows: "[102 252]",
		why:      "12+22=34 over 3 = 102; 32+42+52=126 over 2 = 252.",
	}, {
		name:     "a struct member's leaf beside a FLAT column of the other leg",
		sql:      "SELECT SUM(a.n.sk), SUM(b.w) FROM zn AS a, zp AS b",
		wantRows: "[96 420]",
		why: "the second operand is not nested at all, so this drives the mixed " +
			"nested/flat pairing rather than two nested arguments.",
	}, {
		name:     "TWO SPELLINGS OF ONE COLUMN must still agree",
		sql:      "SELECT SUM(a.n.sk), SUM(n.sk) FROM zn AS a",
		wantRows: "[32 32]",
		why: "THE CONTROL AGAINST OVER-NARROWING. `a.n.sk` and `n.sk` under a " +
			"single-source FROM are the SAME column, so both aggregates must " +
			"resolve and agree. A narrowing that dropped the leading-segment " +
			"spelling entirely would leave one of them unresolved, and the " +
			"translator's lazy fallback answers NULL or fails — this is the arm " +
			"that catches removing too much rather than too little.",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("%s: %v\n  %s", tc.sql, err, tc.why)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("%s: Columns: %v", tc.sql, err)
			}
			var out []string
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("%s: scan: %v", tc.sql, err)
				}
				out = append(out, fmt.Sprint(vals...))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("%s: iterate: %v", tc.sql, err)
			}
			if fmt.Sprint(out) != tc.wantRows {
				t.Errorf("%s\n  rows = %v, want %s\n  %s", tc.sql, out, tc.wantRows, tc.why)
			}
		})
	}
}
