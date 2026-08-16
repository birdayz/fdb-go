package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_GroupByNestedPathKey is RFC-230's acceptance criterion, and it is
// STANDALONE on purpose: the corpus arm that asserts the same rows
// (`groupby-tests.yamsql:61`) runs inside a shuffled block behind a DDL gate
// that has moved twice, so an acceptance criterion resting on it alone is one
// re-gating away from being reported as satisfied by a suite that never ran it.
//
// THE SHAPE IS THE REPRODUCER, NOT A COUSIN OF IT. Two properties of
// `select max(q.s) from nested group by r.v.z having r.v.z > 120` drive the
// whole design and a simpler query loses both:
//
//   - the grouping key is NEVER PROJECTED. It appears only as a grouping value
//     and as a HAVING reference. Any implementation that works by materialising
//     the key as an output column passes a test that projects it;
//   - `r` is a struct COLUMN, not a table alias — `r.v.z` is a three-segment
//     descent into one column of one source, so the leading segments cannot be
//     read as `source . column`.
//
// WHAT WAS ACTUALLY BROKEN, measured on this branch before the fix by disabling
// the refusal that stood in front of it: the ladder in upgradeAggregateOperands
// minted a qualified grouping key as `colRef{table: Qualifier, col: Bare}`,
// i.e. as `table.column`. For `r.v.z` that asks the scope for a source named
// "R.V", nothing answers, and the key degraded to a flat dotted
// FieldValue{"R.V.Z"} — `ordinal resolution: field "R.V.Z" not resolvable in
// the runtime row (ordinal -1, row columns [ID Q R]) — malformed plan`. A
// degraded MINT, not a missing capability: the executor already evaluates the
// grouping key Value against the row (Java's StreamGrouping.evalGroupingKey),
// so nothing downstream of the mint needed changing, and nothing was.
//
// Java answers this query `[{330}]` off index i2. Go answers the same rows
// through an in-memory sort over a base-record streaming aggregation, because
// aggColumnMatches offers a length-1 candidate and a nested path can never
// match it — correct rows, different access path. That is the sanctioned
// read-side fallback, and it is recorded here so "the rows match" is not read
// as "the plans match".
func TestFDB_GroupByNestedPathKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/gbnpk")
	mustExec(t, setup, ctx, "CREATE DATABASE /gbnpk")
	// The corpus schema verbatim (groupby-tests.yamsql:22-29) — a two-level
	// struct (st3.v is an st1, whose member is z) so the key is a genuine
	// three-segment descent, plus the index the Java plan uses.
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE gbnpk_tmpl "+
		"CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) "+
		"CREATE TYPE AS STRUCT st2(w BIGINT, x BIGINT) "+
		"CREATE TYPE AS STRUCT st3(u st2, v st1) "+
		"CREATE TYPE AS STRUCT st4(s BIGINT, t BIGINT) "+
		"CREATE TABLE nested(id BIGINT, q st4, r st3, PRIMARY KEY(q.s, r.u.w)) "+
		"CREATE INDEX i2 AS SELECT r.v.z FROM nested ORDER BY r.v.z")
	mustExec(t, setup, ctx, "CREATE SCHEMA /gbnpk/s WITH TEMPLATE gbnpk_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///gbnpk?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The corpus rows verbatim. Two z groups (100 and 140) whose max(q.s)
	// differ (203 and 330), so a key that collapsed to one group, or that read
	// the struct root instead of the member, produces a different number.
	mustExec(t, db, ctx, "INSERT INTO nested VALUES "+
		"(1, (200, 1), ((5, 15), (10, 100))), "+
		"(2, (201, 2), ((5, 15), (10, 100))), "+
		"(3, (202, 3), ((5, 15), (10, 100))), "+
		"(4, (203, 4), ((5, 15), (10, 100))), "+
		"(5, (330, 5), ((5, 15), (10, 140))), "+
		"(6, (329, 6), ((5, 15), (10, 140))), "+
		"(7, (328, 7), ((5, 15), (10, 140))), "+
		"(8, (327, 8), ((5, 15), (10, 140)))")

	rowsOf := func(t *testing.T, q string, cols int) [][]int64 {
		t.Helper()
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rs.Close()
		var out [][]int64
		for rs.Next() {
			row := make([]int64, cols)
			ptrs := make([]any, cols)
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, row)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return out
	}
	eq := func(t *testing.T, q string, got [][]int64, want string) {
		t.Helper()
		if fmt.Sprint(got) != want {
			t.Fatalf("%s\n  rows = %v\n  want = %s", q, got, want)
		}
	}

	t.Run("unprojected_key_with_having", func(t *testing.T) {
		// THE acceptance query, and the only one in this file whose key is
		// never projected. Java: [{330}].
		q := "SELECT max(q.s) FROM nested GROUP BY r.v.z HAVING r.v.z > 120"
		eq(t, q, rowsOf(t, q, 1), "[[330]]")
	})

	t.Run("unprojected_key_no_having", func(t *testing.T) {
		// The control that says the HAVING did the filtering rather than the
		// grouping having collapsed to one group: both groups, in key order.
		q := "SELECT max(q.s) FROM nested GROUP BY r.v.z"
		eq(t, q, rowsOf(t, q, 1), "[[203] [330]]")
	})

	t.Run("projected_key", func(t *testing.T) {
		// The key ALSO reads correctly as an output column. It went through a
		// different gate than the unprojected shape — a leaf-vs-top-level-field
		// existence check that answered "no such column R.V.Z" because Z is a
		// struct MEMBER — so it needs its own arm.
		q := "SELECT r.v.z, max(q.s) FROM nested GROUP BY r.v.z"
		eq(t, q, rowsOf(t, q, 2), "[[100 203] [140 330]]")
	})

	t.Run("alias_qualified_key", func(t *testing.T) {
		// FOUR segments: the alias segment is peeled and the remaining descent
		// resolves. This is the spelling whose single-source prefix strip used
		// to flatten the surviving segments into one dotted "bare" name.
		q := "SELECT a.r.v.z, max(a.q.s) FROM nested AS a GROUP BY a.r.v.z"
		eq(t, q, rowsOf(t, q, 2), "[[100 203] [140 330]]")
	})

	t.Run("order_by_the_nested_key", func(t *testing.T) {
		// DESC, so an ORDER BY silently dropped (or bound to the wrong slot)
		// shows up as the ascending sequence rather than as equal output.
		q := "SELECT max(q.s) FROM nested GROUP BY r.v.z ORDER BY r.v.z DESC"
		eq(t, q, rowsOf(t, q, 1), "[[330] [203]]")
		q = "SELECT max(q.s) FROM nested GROUP BY r.v.z ORDER BY r.v.z"
		eq(t, q, rowsOf(t, q, 1), "[[203] [330]]")
	})

	t.Run("having_over_an_aggregate_beside_the_nested_key", func(t *testing.T) {
		// The HAVING reference to the nested key is pushed BELOW the aggregate
		// by PushFilterThroughGroupByRule, where an input-relative nested read
		// is the correct read. A HAVING on the AGGREGATE cannot be pushed, so
		// this arm exercises the post-aggregate filter with a nested grouping
		// key present — the combination the pushdown hides in the arm above.
		q := "SELECT r.v.z, max(q.s) FROM nested GROUP BY r.v.z HAVING max(q.s) > 250"
		eq(t, q, rowsOf(t, q, 2), "[[140 330]]")
		q = "SELECT r.v.z FROM nested GROUP BY r.v.z HAVING r.v.z > 120 AND max(q.s) > 250"
		eq(t, q, rowsOf(t, q, 1), "[[140]]")
	})

	t.Run("correlated_scalar_subquery_grouped_by_the_nested_key", func(t *testing.T) {
		// The correlated-scalar arm runs its OWN group-key resolution
		// (resolveCorrelatedGroupKeyValues) and its own grouped ORDER BY
		// binding, neither of which is reachable from the main ladder — so its
		// behaviour could not be predicted from the arms above and it is
		// asserted rather than assumed. It resolved the key from the JOINED
		// qualifier and spent 42703 "no FROM source aliased as N2.R.V".
		q := "SELECT id, (SELECT max(n2.id) FROM nested AS n2 WHERE n2.id = nested.id " +
			"GROUP BY n2.r.v.z ORDER BY n2.r.v.z LIMIT 1) FROM nested"
		eq(t, q, rowsOf(t, q, 2), "[[1 1] [2 2] [3 3] [4 4] [8 8] [7 7] [6 6] [5 5]]")
	})

	t.Run("plan_shape_is_the_read_side_fallback", func(t *testing.T) {
		// Recorded, not aspirational: Go does NOT match index i2 for a nested
		// grouping key (aggColumnMatches offers a length-1 candidate; a nested
		// path has length >= 2 and the length gate rejects it before comparing
		// a segment), so it groups over a base-record scan ordered by an
		// in-memory sort. Asserting the shape here is what makes a later
		// aggregate-index match a visible, deliberate change rather than a
		// silent one — and it pins that there is exactly ONE sort, below the
		// aggregation, so the grouping order is being CONSUMED rather than
		// re-established above it.
		var plan string
		if err := db.QueryRowContext(ctx,
			"EXPLAIN SELECT max(q.s) FROM nested GROUP BY r.v.z ORDER BY r.v.z").
			Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		want := "Project([_current.MAX(Q.S)#1], StreamingAgg(keys=[_current.R#2.V#1.Z#1], " +
			"InMemorySort([_current.R#2.V#1.Z#1 ASC], Scan(NESTED))))"
		if plan != want {
			t.Fatalf("plan shape moved.\n  got:  %s\n  want: %s\n"+
				"  A SECOND sort above the aggregation means the ordering the "+
				"aggregation provides stopped being recognised — the grouping "+
				"key renders as its resolved accessor path, and the provided-vs-"+
				"requested ordering match compares renderings.", plan, want)
		}
	})
}
