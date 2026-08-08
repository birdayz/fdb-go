package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_NestedSortKeyThroughTheProjectedExistsFold is the earning test for the
// fold's nested-ORDER-BY fix.
//
// The projected-EXISTS fold used to re-derive a nested sort key from a RENDERED
// name. `n.sk` resolves to ONE FieldValue whose Field is the struct ROOT (`N`)
// and whose resolved path is `[N, SK]`, so rendering produced either `N` — and
// the fold sorted by the whole struct — or `T1.N.SK`, whose last-dot split
// yielded `SK`, a column no row has. The fix carries the resolved path and
// re-states its ROOT ordinal in the layout the fold evaluates against.
//
// EVERY ARM BELOW IS A DISTINCT FAILURE MODE, not a restatement of one. The
// crossing of "nested ORDER BY" with "projected EXISTS" broke in three different
// ways depending only on what else the SELECT list projected, and an earlier
// deliverable list named two of them — which is the same omission, one layer
// out, as the design error that let this survive several review laps:
//
//	SELECT id, EXISTS(...)          -> struct/struct ordering comparison error
//	SELECT id, n, EXISTS(...)       -> planner could not plan the query
//	SELECT id, n AS nn, EXISTS(...) -> planner could not plan the query
//
// They must therefore be able to fail INDEPENDENTLY; a single fixture covering
// "a nested key in the fold" would satisfy the sentence and not the property.
//
// The assertions pin the COLUMN, not merely the order. n.sk descends 50,40,30 as
// id ascends 1,2,3, so ordering by n.sk yields [3 2 1] while ordering by id — or
// by whatever slot a mis-derived root ordinal lands on — yields [1 2 3]. Ordering
// by the wrong column still produces a plausibly-ordered result set, so a
// row-order assertion over a coincidentally-sorted column proves nothing.
//
// `nst` carries a SECOND field for that reason. With a single-field struct,
// `ORDER BY n` and `ORDER BY n.sk` produce the same row order, so the claim above
// would be caught only incidentally — by struct/struct comparison happening to
// error today — and would silently stop being tested the moment struct ordering
// is implemented. `co` is CORRELATED with id (1,2,3) while `sk` is ANTI-correlated
// (50,40,30), so sorting by the wrong member of the same struct is now visible as
// [1 2 3] rather than hidden behind a tie.
func TestFDB_NestedSortKeyThroughTheProjectedExistsFold(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nsk_fold")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_nsk_fold")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nsk_fold_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nsk_fold/s WITH TEMPLATE nsk_fold_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_nsk_fold?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// n.sk runs OPPOSITE to id, so the two orderings are distinguishable.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	firstCol := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []int64
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			id, ok := vals[0].(int64)
			if !ok {
				t.Fatalf("first column is %T, want int64", vals[0])
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		q    string
	}{
		{
			"nested_key_not_projected",
			"SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 ORDER BY n.sk",
		},
		{
			// The struct ROOT is also projected. Distinct arm: the projected `N`
			// and the sort key's source column interact in the fold's output
			// membership test, and this shape failed as a planning error rather
			// than as the struct-comparison error above.
			"struct_root_also_projected",
			"SELECT id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 ORDER BY n.sk",
		},
		{
			// Same, but the projected root is ALIASED, so it cannot be matched to
			// the key by output name even incidentally.
			"struct_root_projected_under_an_alias",
			"SELECT id, n AS nn, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 ORDER BY n.sk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := firstCol(t, tc.q)
			want := []int64{3, 2, 1}
			if len(got) != len(want) {
				t.Fatalf("got %d rows %v, want %v", len(got), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("got ids %v, want %v — [1 2 3] means the sort ordered by "+
						"ID (or by another slot the re-anchored root landed on), not by "+
						"n.sk. This arm fails independently of the others by design",
						got, want)
				}
			}
		})
	}

	// A JAVA DIVERGENCE, pinned here because it is plan-shape only and would
	// otherwise go unrecorded.
	//
	// Java decides hidden-column membership by DERIVABILITY: Expression
	// .canBeDerivedFrom runs Value.pullUp, so a nested `n.sk` IS derivable from a
	// projected `n` and Java appends NO hidden column for the two shapes above
	// that project the struct root. Go's sortKeyInOutput uses exact
	// SemanticEqualsUnderAliasMap, so {N}{SK} != {N} and a hidden column IS
	// appended — named "N", which puts TWO fields named N in the folded row. That
	// is precisely the duplicate-root layout the fold is documented as able to
	// manufacture from unambiguous SQL, and it is why the re-anchor's ambiguity
	// arm cannot be argued away by "users cannot write it".
	//
	// ROWS ARE CORRECT either way, which is why this is a shape divergence and not
	// a bug: the sort resolves to the appended column and orders by the same
	// values Java orders by. It is pinned so that closing it (teaching
	// sortKeyInOutput derivability, matching canBeDerivedFrom) is a deliberate
	// change with a test that notices, rather than a silent plan-shape drift.
	t.Run("struct_root_projected_still_orders_correctly_despite_the_extra_column", func(t *testing.T) {
		t.Parallel()
		got := firstCol(t, "SELECT id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h "+
			"FROM t1 ORDER BY n.sk")
		want := []int64{3, 2, 1}
		for i := range want {
			if i >= len(got) || got[i] != want[i] {
				t.Fatalf("got %v, want %v — the appended hidden column (Go appends one where "+
					"Java derives instead) must not change the ANSWER, only the shape", got, want)
			}
		}
	})

	// The JOIN arm DECLINES, and declines LOUDLY. A nested key over a merged row
	// needs the leg-window re-anchor, which refuses multi-accessor paths today, so
	// the fold bails and the query is rejected as unsupported rather than planned
	// with a key that reads a pre-merge ordinal against the merged row.
	//
	// The assertion is that it is a CLEAN refusal, not that it fails: the shape
	// previously produced `malformed plan` from the executor. If this ever returns
	// rows, the leg-window re-anchor has landed and this must become a row
	// assertion — ids [3 2 1], same as above.
	t.Run("nested_key_over_a_join_declines_cleanly", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.sk"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			rows.Close()
			t.Fatalf("the JOIN arm now plans %q — the leg-window re-anchor has landed. "+
				"Replace this with the row assertion: ids [3 2 1] ordered by n.sk", q)
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("expected a clean 0AF00 unsupported-shape refusal, got: %v — a "+
				"non-0AF00 failure means the fold planned something and the executor "+
				"rejected it, which is the silent-wrong-column class this fix removes", err)
		}
	})
}
