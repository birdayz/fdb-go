package factory_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestFDB_NestedPathOnAMultiLegOuterSide pins a three-way join whose third leg
// is a LEFT OUTER join and whose ON condition reads a NESTED path from the
// already-joined (multi-leg) side:
//
//	SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id
//	                         LEFT JOIN nt AS r ON m.n.sk = r.n.sk
//
// This was found by the nested generator and pinned here as KNOWN-BROKEN: the
// engine named it a planner/executor bug in its own message and refused the
// query with
//
//	correlated FieldValue "N" (correlation "M") ... (*RowEvalContext (multi-leg
//	row cannot serve a source-relative ordinal)) — no frontier row resolved
//
// IT IS NOW FIXED, and this file is its regression pin. The cause was the gated
// join's predicate bake identifying a leg reference by ACCESSOR COUNT: a
// user-written nested descent arrives as one unpinned FieldValue whose root is
// leg-relative and whose remaining accessors are the descent, so the arity test
// declined exactly the references the bake exists to rewrite. The node kept its
// leg correlation, nothing bound it at execution, and the read fell through to
// the multi-leg positional row and tripped its correct-or-loud guard.
// Machinery-ownership is the FRONTIER PIN, never the arity.
//
// The three conditions that were jointly required are why the arms stay a SET
// rather than collapsing to the one query:
//
//   - a MULTI-LEG row on the outer side (the two-way forms always answered),
//   - a LEFT OUTER third leg (the three-way INNER form always answered),
//   - the nested path read FROM that multi-leg side (a nested path on the
//     NULL-extended right side answered, and so did a flat column on the left).
//
// ALL THREE defect arms flipped together, which is the evidence that a CLASS
// closed rather than one query: no arm of this file still refuses.
//
// THE ROWS WERE DERIVED FROM THE FIXTURE, NOT BLESSED FROM THE OUTPUT, because
// an unbound correlation that starts resolving can resolve to the wrong leg and
// return plausible rows. nestedProbeInsert seeds
//
//	id | sk  | n.sk | n.co | n.dp.sk
//	 1 | 100 |  11  | 'a'  |  111
//	 2 | 200 |  22  | 'b'  |  222
//	 3 | 300 |  33  | 'c'  |  333
//
// `l.id = m.id` is an equality on the PRIMARY KEY, so it pairs each row with
// itself: three (l, m) pairs, l and m the same row. `n.sk` is unique across the
// three rows, so a nested-key LEFT JOIN matches exactly one r per pair and
// null-extends none; `m.n.sk` (11/22/33) and `r.sk` (100/200/300) are disjoint,
// so that arm matches nothing and null-extends everything.
//
// TWO ARMS PROJECT r.id, AND THAT IS THE POINT. On this fixture every candidate
// mis-resolution — reading the flat `sk` instead of the nested one, reading the
// enclosing STRUCT because the descent was dropped, reading `n.dp.sk` — still
// yields three rows of `l.id`, so `[1 2 3]` alone cannot tell a correct descent
// from a dropped one. Projecting the null-extendable side separates them: a
// dropped descent turns the matching arm's r.id into NULL and the non-matching
// arm's into a match.
//
// MEASURED AGAINST THE ACTUAL PRE-FIX DEFECT, which is BOTH mutations at once —
// resolve the root by DISPLAY name (a fused node is named after its leaf, so
// `m.n.sk` resolves to the flat `sk`) AND drop the descent. Under that
// combination NINE of these ten arms are GREEN, and exactly one fires:
//
//	-multileg-side-only-projecting-r   want [1|NULL 2|NULL 3|NULL]
//	                                   got  [1|1 2|2 3|3]
//
// The mechanism is the fixture's two key columns. `-both-sides` degrades from
// `m.n.sk = r.n.sk` to `m.sk = r.sk` — both sides degrade TOGETHER, `sk` is
// unique, so it still matches 1:1 and still produces 1|1,2|2,3|3. The wrong read
// COINCIDES with the right answer, which is why even the projecting form of that
// arm cannot see it. `-multileg-side-only` degrades from `m.n.sk = r.sk` to
// `m.sk = r.sk`: the correct query compares DISJOINT domains (11/22/33 against
// 100/200/300) and must null-extend, while the degraded one matches. Only there
// does the wrong column change the answer.
//
// So the whole `[1 2 3]` population — and one of the two projecting arms — is
// blind to the exact defect this file exists for, and a single arm carries the
// detection. That is the argument for keeping it, and it is a stronger one than
// "more coverage": a pair whose two sides degrade together cannot check itself.
//
// What this fixture CANNOT separate is `l` from `m` — the
// join condition makes them the same row, so both bindings are correct by
// construction here; the multiplicity discrimination for that axis lives in
// sqldriver's TestFDB_OuterMultilegNestedOnPredicate, whose fixture collides
// n.sk deliberately.
func TestFDB_NestedPathOnAMultiLegOuterSide(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db := openFactorySchema(t, ctx, "zznestouter", nestedProbeDDL)
	if _, err := db.ExecContext(ctx, nestedProbeInsert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		// --- the shapes that always worked, kept as the no-regression half ---
		{
			name:  "two-way-inner-nested-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			want:  []string{"1", "2", "3"},
		},
		{
			name:  "two-way-outer-nested-on",
			query: "SELECT l.id FROM nt AS l LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			want:  []string{"1", "2", "3"},
		},
		{
			// Three legs, nested ON, but the third leg is INNER: the multi-leg
			// row served the ordinal fine, so outer-ness was load-bearing.
			name: "three-way-inner-nested-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// Three legs, OUTER third leg, but a FLAT column on the multi-leg
			// side: so it was the NESTING that broke it, not the outer join.
			name: "three-way-outer-flat-on",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.sk = r.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// Nested on the NULL-EXTENDED side only. That side is a single-leg
			// row, so it resolved. In `(l JOIN m) LEFT JOIN r` the PRESERVED
			// side is the composite (l, m) and `r` is the null-extended one, so
			// a nested path on `r` is the opposite of the failing case.
			name: "three-way-outer-nested-on-null-extended-side",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},

		// --- the three arms that were the defect, now value assertions ------
		{
			// DERIVED: pairs (l,m) = (1,1),(2,2),(3,3); m.n.sk 11/22/33 each
			// matches exactly one r by n.sk, so no row is null-extended.
			name: "three-way-outer-nested-on-both-sides",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// The same shape PROJECTING THE NULL-EXTENDABLE SIDE, which is what
			// makes it a test of the descent rather than of the row count.
			// DERIVED: r is the row whose n.sk equals m.n.sk, i.e. r.id = l.id.
			// A dropped descent (comparing the whole `n` struct, or the flat
			// `sk` 100/200/300) matches nothing and would show NULL here while
			// still showing three rows of l.id above.
			name: "three-way-outer-nested-on-both-sides-projecting-r",
			query: "SELECT l.id, r.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			want: []string{"1|1", "2|2", "3|3"},
		},
		{
			// DERIVED: m.n.sk is 11/22/33 and r.sk is 100/200/300 — disjoint, so
			// nothing matches and the LEFT JOIN null-extends every pair. Three
			// rows of l.id, all r columns NULL.
			name: "three-way-outer-nested-on-multileg-side-only",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
		{
			// The discriminating twin of the arm above, and the sharper of the
			// two: reading the FLAT `m.sk` instead of the nested `m.n.sk` would
			// match r 1:1 and produce 1|1, 2|2, 3|3 — while the l.id-only form
			// returns the identical [1 2 3] either way.
			name: "three-way-outer-nested-on-multileg-side-only-projecting-r",
			query: "SELECT l.id, r.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON m.n.sk = r.sk ORDER BY l.id",
			want: []string{"1|NULL", "2|NULL", "3|NULL"},
		},
		{
			// The nested path reads the FIRST leg rather than the second; the
			// composite row was what could not serve it, so which leg is
			// immaterial. DERIVED: l.n.sk 11/22/33 matches exactly one r each.
			// `l.id = m.id` makes l and m the same row, so this arm cannot
			// distinguish an l-binding from an m-binding — and does not need to,
			// because both are the same value here.
			name: "three-way-outer-nested-on-first-leg",
			query: "SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id " +
				"LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			want: []string{"1", "2", "3"},
		},
	} {
		got, err := queryRowStrings(ctx, db, tc.query)
		if err != nil {
			t.Errorf("%s: %s\n  %v", tc.name, tc.query, err)
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: WRONG ROWS.\n  %s\n  want %v\n  got  %v", tc.name, tc.query, tc.want, got)
			continue
		}
		t.Logf("OK              %s", tc.name)
	}
}
