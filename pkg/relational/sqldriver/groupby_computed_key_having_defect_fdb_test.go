package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ComputedGroupKeyRereadInHavingIsWrong is a CHARACTERIZATION test: it
// asserts behaviour that is WRONG, so that the fix flips it rather than
// discovering it. Every assertion below names the correct answer in its failure
// message.
//
// THE DEFECT. A COMPUTED grouping key (`GROUP BY c1 + 1`) re-read in a HAVING
// that is not pushed below the aggregate reads the WRONG SLOT of the aggregate
// output row. On flat columns it is SILENT WRONG ROWS.
//
//	SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200
//	  groups are 101 and 141, so the correct answer is ZERO rows.
//	  measured: TWO rows.
//
// The self-contradiction is visible inside a single result set: projecting the
// key alongside it returns `[101 203] [141 330]` — the key column says 101 and
// 141 while the HAVING that filters `c1 + 1 > 200` admitted both.
//
// WHY, and it is one line. rebaseHavingGroupKeyPredicate hands every
// non-pushable HAVING to rebasePostAggregateGroupKeyValue, which pins a
// reference to the aggregate's output ordinal — but only after
//
//	qfv, ok := gk.Value.(*values.FieldValue)
//	if !ok { continue }
//
// A computed key's Value is an ArithmeticValue, never a FieldValue, so the
// reference matches no key, is never pinned, and survives as an INPUT-relative
// read evaluated against the aggregate's output row. `c1` carries input ordinal
// 1 over [ID C1 C2]; the output row is [(C1 + 1), MAX(C2)], whose slot 1 is the
// AGGREGATE. That predicts 204 > 200 and 331 > 200 (two rows admitted) and
// 204 < 200, 331 < 200 (zero rows admitted) — both measured exactly, in
// opposite directions, which is what identifies the slot rather than merely
// showing that something is off.
//
// NESTED IS THE SAME DEFECT, LOUDER — and only by luck of the ordinal. The
// fused nested root carries ordinal 2 over [ID Q R], the output row is 2 wide,
// so the read is out of range and the executor refuses instead of answering.
// The flat twin's ordinal happens to land inside the row, so nothing catches it.
//
// IT IS PRE-EXISTING AND NOT RFC-230'S. Measured at the parent commit
// 9222f968c in a detached worktree with this exact fixture: byte-identical
// results, both arms. RFC-230 admits a nested path as a BARE grouping key; the
// computed-key arm (sq.groupBy[i].expr != nil) never travelled the refusal it
// retired, and the flat half is on a path RFC-230 does not touch at all.
//
// WHAT FLIPS THIS FILE: matching a post-aggregate reference against a computed
// key by comparing the EXPRESSION, not by asserting the key is a FieldValue —
// so the whole `c1 + 1` subtree rebinds to the key's output slot. That changes
// post-aggregate binding semantics and belongs in its own change with its own
// review, which is why this file pins the defect instead of hiding it.
func TestFDB_ComputedGroupKeyRereadInHavingIsWrong(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/cgkh")
	mustExec(t, setup, ctx, "CREATE DATABASE /cgkh")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cgkh_tmpl "+
		"CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) "+
		"CREATE TYPE AS STRUCT st2(w BIGINT, x BIGINT) "+
		"CREATE TYPE AS STRUCT st3(u st2, v st1) "+
		"CREATE TYPE AS STRUCT st4(s BIGINT, t BIGINT) "+
		"CREATE TABLE nested(id BIGINT, q st4, r st3, PRIMARY KEY(q.s, r.u.w)) "+
		"CREATE TABLE flat(id BIGINT, c1 BIGINT, c2 BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /cgkh/s WITH TEMPLATE cgkh_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///cgkh?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The keys (101, 141) and the aggregates (203, 330) are deliberately far
	// apart, and on OPPOSITE sides of the comparand 200: reading the aggregate
	// where the key was named flips every predicate below, so a wrong slot
	// changes the ROW COUNT rather than coinciding with the right answer.
	mustExec(t, db, ctx, "INSERT INTO flat VALUES "+
		"(1, 100, 200), (2, 100, 201), (3, 100, 202), (4, 100, 203), "+
		"(5, 140, 330), (6, 140, 329), (7, 140, 328), (8, 140, 327)")
	mustExec(t, db, ctx, "INSERT INTO nested VALUES "+
		"(1, (200, 1), ((5, 15), (10, 100))), "+
		"(2, (201, 2), ((5, 15), (10, 100))), "+
		"(3, (202, 3), ((5, 15), (10, 100))), "+
		"(4, (203, 4), ((5, 15), (10, 100))), "+
		"(5, (330, 5), ((5, 15), (10, 140))), "+
		"(6, (329, 6), ((5, 15), (10, 140))), "+
		"(7, (328, 7), ((5, 15), (10, 140))), "+
		"(8, (327, 8), ((5, 15), (10, 140)))")

	count := func(t *testing.T, q string) int {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return n
	}

	t.Run("flat_computed_key_admits_groups_it_must_exclude", func(t *testing.T) {
		q := "SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200"
		if n := count(t, q); n != 2 {
			t.Fatalf("%s returned %d rows; this file PINS the defective 2.\n"+
				"  If it now returns 0, THE DEFECT IS FIXED — the correct answer is "+
				"ZERO rows, because the two grouping keys are 101 and 141 and neither "+
				"exceeds 200. Delete this arm and assert 0.\n"+
				"  Any other number is a THIRD behaviour and needs its own diagnosis.", q, n)
		}
	})

	t.Run("flat_computed_key_excludes_groups_it_must_admit", func(t *testing.T) {
		// The OTHER direction, and it is required: an arm that only ever
		// over-admits is equally consistent with the predicate being dropped
		// altogether. Under-admitting on the mirrored comparison is what says
		// the predicate is being EVALUATED, against the wrong slot.
		q := "SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 < 200 AND max(c2) > 0"
		if n := count(t, q); n != 0 {
			t.Fatalf("%s returned %d rows; this file PINS the defective 0.\n"+
				"  If it now returns 2, THE DEFECT IS FIXED — both keys (101, 141) are "+
				"below 200 and both groups have a positive max. Delete this arm and "+
				"assert 2.", q, n)
		}
	})

	t.Run("the_result_set_contradicts_itself", func(t *testing.T) {
		// The sharpest statement of the defect: the projected key and the
		// HAVING disagree inside ONE result set, so no reading of the data can
		// make this output correct.
		q := "SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200 AND max(c2) > 0"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var got [][2]int64
		for rows.Next() {
			var k, a int64
			if err := rows.Scan(&k, &a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, [2]int64{k, a})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if fmt.Sprint(got) != "[[101 203] [141 330]]" {
			t.Fatalf("%s = %v; this file PINS [[101 203] [141 330]].\n"+
				"  Read it: the KEY column says 101 and 141, and the HAVING filtering "+
				"`c1 + 1 > 200` admitted both. If this is now empty, the defect is "+
				"fixed and this arm asserts empty.", q, got)
		}
	})

	t.Run("a_bare_key_is_the_control_and_is_correct", func(t *testing.T) {
		// Without this the arms above could be read as "HAVING over a grouped
		// query is broken", which is not the finding. A BARE key re-read in the
		// same non-pushable shape binds correctly, because its gk.Value IS a
		// FieldValue and the post-aggregate rebase pins it.
		q := "SELECT max(c2) FROM flat GROUP BY c1 HAVING c1 > 200 AND max(c2) > 0"
		if n := count(t, q); n != 0 {
			t.Fatalf("%s returned %d rows, want 0 — the BARE-key control regressed, "+
				"which would mean the defect is wider than the computed-key arm this "+
				"file characterises", q, n)
		}
		q = "SELECT max(c2) FROM flat GROUP BY c1 HAVING c1 < 200 AND max(c2) > 0"
		if n := count(t, q); n != 2 {
			t.Fatalf("%s returned %d rows, want 2 — the BARE-key control regressed", q, n)
		}
	})

	t.Run("the_nested_twin_is_the_same_defect_reported_loudly", func(t *testing.T) {
		// Same shape, nested key. It fails LOUDLY only because the fused root's
		// input ordinal (2, over [ID Q R]) falls outside the 2-wide output row.
		// That is luck, not a guard: it is the flat arm above with a different
		// ordinal, and it must be fixed by the same change.
		q := "SELECT max(q.s) FROM nested GROUP BY r.v.z + 1 HAVING r.v.z + 1 > 121"
		_, err := db.QueryContext(ctx, q)
		if err == nil {
			t.Fatalf("%s now PLANS. If it returns [330] the defect is fixed for the "+
				"nested arm — check the FLAT arms above moved with it, since a fix that "+
				"only satisfies the loud half leaves the silent half shipping", q)
		}
		if !strings.Contains(err.Error(), "not resolvable in the runtime row") {
			t.Fatalf("%s: got %v.\n"+
				"  This arm pins that the nested twin dies as an input-relative read "+
				"against the aggregate output row. A different error means the shape "+
				"is being turned away somewhere else and this characterization is "+
				"describing the wrong mechanism.", q, err)
		}
	})
}
