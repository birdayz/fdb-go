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

	// A JAVA DIVERGENCE, PINNED AS SHAPE — not merely as the answer.
	//
	// Java decides hidden-column membership by DERIVABILITY: Expression
	// .canBeDerivedFrom (Expression.java:254-264) runs Value.pullUp, so a nested
	// `n.sk` IS derivable from a projected `n` and Java appends NO hidden column.
	// Go's sortKeyInOutput uses exact SemanticEqualsUnderAliasMap, so {N}{SK} !=
	// {N} and a hidden column IS appended, named "N" — putting TWO fields named N
	// in the folded row. That is the duplicate-root layout the fold can manufacture
	// from unambiguous SQL, and it is why the re-anchor's ambiguity arm cannot be
	// argued away by "users cannot write it".
	//
	// ROWS ARE CORRECT either way, so an answer assertion CANNOT see this: an
	// earlier version of this test was byte-identical in query and expectation to
	// struct_root_also_projected above and would have stayed green if the
	// divergence were closed — while claiming to be the test that notices.
	//
	// THE OBSERVABLE is the fold's cleanup projection, which
	// translateProjectOverExistsFilter emits ONLY when extraSortCols is non-empty.
	// Measured, holding the sort constant so the wrapper is the only thing that
	// varies:
	//
	//	ORDER BY n.sk (hidden column appended) -> Project([...], InMemorySort([N ASC], FlatMap(...)))
	//	ORDER BY id   (key already projected)  -> FlatMap(...)                       [no Project]
	//
	// So if sortKeyInOutput is taught derivability, extraSortCols goes empty, the
	// cleanup Project disappears, and this test REDDENS — which is what the booking
	// asks for.
	t.Run("java_divergence_the_hidden_column_is_appended_and_shows_in_the_plan", func(t *testing.T) {
		t.Parallel()
		q := "SELECT id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY n.sk"
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected the nested key to still require a sort, got: %s — the "+
				"control below is only meaningful with the sort held constant", plan)
		}
		if !strings.HasPrefix(plan, "Project(") {
			t.Fatalf("no cleanup projection in %s — Go no longer appends a hidden sort "+
				"column for a projected struct root, i.e. sortKeyInOutput now matches "+
				"Java's derivability. That is the DESIRED end state: delete this test, "+
				"and update the TODO booking that asks for it", plan)
		}
	})

	// CONTROL for the assertion above: with the ORDER BY key already projected, no
	// hidden column is appended and the cleanup projection is absent. Without this,
	// "the plan starts with Project(" could be true of every folded query and the
	// pin would hold for a reason unrelated to hidden columns.
	t.Run("no_hidden_column_means_no_cleanup_projection", func(t *testing.T) {
		t.Parallel()
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY id"
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if strings.HasPrefix(plan, "Project(") {
			t.Fatalf("a cleanup projection appeared for a query needing NO hidden sort "+
				"column: %s — the shape assertion above no longer discriminates", plan)
		}
	})

	// THE JOIN ARM, which used to decline with a clean 0AF00 and now plans.
	//
	// It fails INDEPENDENTLY of the three single-table arms above and of each
	// other, because the mechanism is different: those re-anchor the key's root
	// against the fold's own flowed row, while this one emits the key
	// LEG-RELATIVE and leaves the merged re-anchor to the leg-window rebase. A
	// single-table arm can be whole while this is broken and the reverse.
	//
	// THE COLUMN IS PINNED, NOT THE ORDER. n.sk descends 50,40,30 as id ascends
	// 1,2,3, so ordering by n.sk yields [3 2 1] and ordering by ID — or by the
	// struct's OTHER member `co`, which is correlated with id — yields [1 2 3].
	// Ordering by the wrong column still produces plausibly-ordered output, which
	// is why the fixture carries an anti-correlated and a correlated member.
	for _, tc := range []struct {
		name string
		q    string
	}{
		{
			// BARE `n.sk`: two segments, unambiguous because only t1 exposes `n`.
			"nested_key_over_a_join",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.sk",
		},
		{
			// The OTHER struct member. Without this, a fix that fused a constant
			// suffix ordinal — or dropped the suffix and happened to land on a
			// column ordered like sk — would pass every assertion above.
			"nested_key_over_a_join_ordering_by_the_other_member",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.co",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// `co` is correlated with id, `sk` anti-correlated, so the two members
			// have OPPOSITE expected orders — which is what makes "ordered by the
			// wrong member of the right struct" a visible failure rather than a tie.
			want := []int64{3, 2, 1}
			if strings.Contains(tc.q, "n.co") {
				want = []int64{1, 2, 3}
			}
			got := firstCol(t, tc.q)
			if len(got) != len(want) {
				t.Fatalf("got %d rows %v, want %v", len(got), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("got ids %v, want %v for %q.\n"+
						"  The fold emits this key LEG-RELATIVE and the leg-window rebase "+
						"re-states its root on the merged row and fuses the suffix back on. "+
						"A result ordered by ID means the suffix was dropped and the sort "+
						"read the whole struct or the leg's first column; a result in the "+
						"OTHER member's order means the suffix landed on the wrong slot.",
						got, want, tc.q)
				}
			}
		})
	}

	// A SEPARATE, PRE-EXISTING DEFECT, pinned here because this is where it was
	// found and because it is what stops the leg-window re-anchor from covering
	// the projected-root shape over a join.
	//
	// PROJECTING A STRUCT COLUMN THROUGH A JOIN'S MERGED ROW IS BROKEN, and it has
	// nothing to do with ORDER BY, with the nested key, or with this fix. Measured
	// on the production code with this whole change REVERTED, so it is not a
	// consequence of it:
	//
	//	SELECT t1.id, n, EXISTS(...) FROM t1 JOIN t3 ON ...   (no ORDER BY at all)
	//	  -> ordinal resolution: field "N" not resolvable in the runtime row
	//	     (ordinal -1, row columns [ID T1_ID ID N]) — malformed plan
	//	SELECT t1.id, n FROM t1 JOIN t3 ON ...   (no EXISTS either)
	//	  -> returns rows, with the FIRST column 0 instead of the ids
	//
	// The second is a SILENT wrong answer and the more dangerous of the two. Both
	// point at the same root cause the re-anchor had to work around: the positional
	// merge does not model a struct-typed column — the merged row and the leg
	// window both state UNKNOWN for that slot (measured), so the reference stays
	// LAZY (ordinal -1) and resolves by a name the runtime row does not answer to.
	//
	// This asserts the CURRENT loud failure so the shape is not silently forgotten.
	// It is a tripwire, not an endorsement: when the merge learns struct columns
	// this reddens, and the right move is to replace it with the row assertion
	// (ids [3 2 1] ordered by n.sk, the struct projected intact).
	t.Run("projected_struct_root_over_a_join_still_fails_for_a_reason_that_predates_this", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 JOIN t3 ON t3.t1_id = t1.id"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			rows.Close()
			t.Fatalf("projecting a struct column through a join's merged row now works "+
				"for %q. The positional merge has learned struct-typed columns — replace "+
				"this tripwire with the row assertion for the ORDER BY n.sk form, which "+
				"the leg-window re-anchor already handles for every non-projected shape", q)
		}
		if !strings.Contains(err.Error(), "not resolvable in the runtime row") {
			t.Fatalf("expected the pre-existing unresolvable-struct-column failure, got: "+
				"%v — a DIFFERENT failure means this shape changed for some other reason "+
				"and the diagnosis recorded above no longer describes it", err)
		}
	})
}

// TestFDB_NestedCorrelationThroughAJoinsMergedRow is the earning test for the
// wrong-rows defect the leg-window re-anchor removes, and it has NOTHING to do
// with ORDER BY.
//
// `rebaseOuterLegValueOrdinal` dispatched on a reference's frontier pin, not on
// its arity, so a NESTED leg reference took whichever arm its pin selected:
//
//	pinned   -> Resolved.Single() failed -> the rebase declined (loud)
//	unpinned -> the NAME arm looked up fv.Field, which for a nested reference is
//	            the struct ROOT, found it, baked the merged address of the STRUCT
//	            and DROPPED the descent
//
// The second is silent. `t2.t1_id = n.sk` became `t2.t1_id = <the whole struct>`,
// which is a real merged column of the wrong type, so the plan built, ran, and
// compared a BIGINT against a record — matching nothing, with no error.
//
// MEASURED on master, same data, same predicate:
//
//	single table, EXISTS      -> [1 3]     CORRECT
//	single table, NOT EXISTS  -> [2]       CORRECT
//	+ an inner JOIN, EXISTS   -> []        WRONG
//	+ an inner JOIN, NOT EXISTS -> [1 2 3] WRONG
//
// THE SINGLE-TABLE ROWS ARE THE CONTROL AND THEY ARE NOT DECORATION. t3 holds
// exactly one row per t1 id, so the join cannot change which t1 rows qualify:
// the two must agree, and the assertion is that they DO rather than that the
// join side matches a hardcoded list. A fixture whose join silently filtered
// would otherwise make a broken engine look right.
func TestFDB_NestedCorrelationThroughAJoinsMergedRow(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nested_corr")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_nested_corr")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nested_corr_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nested_corr/s WITH TEMPLATE nested_corr_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_nested_corr?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t2.t1_id matches n.sk for ids 1 (sk=50) and 3 (sk=30), not for 2 (sk=40).
	// The values are deliberately NOT ids, so a reference that lost its `.sk` and
	// fell back to anything id-shaped cannot match by accident.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 30), (200, 50)")
	// One t3 row per t1 row: the join is row-preserving, so it CANNOT change the
	// answer. That is what licenses comparing the two sides directly.
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		single string
		joined string
		want   []int64
	}{
		{
			"exists",
			"SELECT t1.id FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = n.sk)",
			"SELECT t1.id FROM t1 JOIN t3 ON t3.t1_id = t1.id " +
				"WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = n.sk)",
			[]int64{1, 3},
		},
		{
			// NOT EXISTS is its own arm: the truncated comparison matched NOTHING,
			// so EXISTS went empty while NOT EXISTS went universal. A test covering
			// only one polarity sees only one of those.
			"not_exists",
			"SELECT t1.id FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = n.sk)",
			"SELECT t1.id FROM t1 JOIN t3 ON t3.t1_id = t1.id " +
				"WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = n.sk)",
			[]int64{2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			single := ids(t, tc.single)
			if fmt.Sprint(single) != fmt.Sprint(tc.want) {
				t.Fatalf("the SINGLE-TABLE control returned %v, want %v. Every verdict "+
					"below is void until this passes: the join's answer is judged by "+
					"agreement with it, so a broken control makes a broken join look right",
					single, tc.want)
			}
			joined := ids(t, tc.joined)
			if fmt.Sprint(joined) != fmt.Sprint(single) {
				t.Fatalf("adding a row-preserving inner JOIN changed the answer: %v vs the "+
					"single-table %v.\n"+
					"  The nested correlation `n.sk` is rebased onto the join's merged row "+
					"by the leg-window re-anchor. An EMPTY join result (with NOT EXISTS "+
					"returning everything) is the suffix-truncation signature: the "+
					"reference lost its `.sk`, so the predicate compared a BIGINT against "+
					"the whole struct and matched nothing — silently, because the "+
					"truncated address is a real merged column.",
					joined, single)
			}
		})
	}
}
