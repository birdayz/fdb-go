package embedded

import (
	"strings"
	"testing"
)

// sameShapedLegsDDL is one table read twice. Every arm below joins two
// materializing legs derived from it, so the two legs have the SAME exact row
// type — which is the whole point: record names are not part of type identity
// (Java's Type.Record.equals compares typeCode, nullability and fields), so two
// derived rows with the same columns ARE the same type and nothing downstream
// may rely on a name to tell them apart.
const sameShapedLegsDDL = `CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))`

// TestJoinOfTwoSameShapedMaterializingLegsPlans pins a FlatMap lineage crossing
// that only one of its two branches used to perform.
//
// A FlatMap leg's value is carried to the leg's own output carrier — by the
// child's materializer when it has one, and otherwise by the child's producer
// program plus its layout. BOTH endings leave the value rooted on the CHILD's
// private current row, and both therefore owe the same crossing back to the
// alias the FlatMap's result program addresses that leg through. Only the
// materializer branch paid it.
//
// The omission is invisible while the two legs differ in shape, because the
// child-current field then name-matches exactly one slot in the result
// constructor and the producer bridge resolves it anyway. Give the legs the
// SAME row and the field matches BOTH slots; the bridge declines rather than
// guess (correctly — guessing here reads the other leg's column), and the value
// reaches the output layout still rooted on a one-leg row it cannot address:
//
//	resolution error 42 at reanchor.field: current root and target have
//	different exact types: root RECORD(ID:LONG?), target RECORD(ID:LONG?,ID:LONG?)
//
// That is a hard planning failure, not a decline, so the query returned an
// error instead of an answer.
//
// Each arm asserts the plan, not merely that planning succeeded: the ordinals
// are what prove the two legs stayed distinct through the crossing. `y.id` must
// read #1 (or #2 where the legs are two columns wide), never #0 — collapsing
// both onto one leg's slot is the wrong-column failure the declining bridge was
// protecting against, and it would still "plan".
func TestJoinOfTwoSameShapedMaterializingLegsPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "derived legs, one column each",
			sql:  `SELECT x.id, y.id FROM (SELECT id FROM t) x JOIN (SELECT id FROM t) y ON x.id = y.id`,
			want: "Project([_current.ID#0, _current.ID#1], NestedLoopJoin(INNER, [1 preds], " +
				"Project([_current.ID#0], Scan(T)), Project([_current.ID#0], Scan(T))))",
		},
		{
			name: "derived legs, two columns each",
			sql:  `SELECT x.id, y.id FROM (SELECT id, v FROM t) x JOIN (SELECT id, v FROM t) y ON x.id = y.id`,
			want: "Project([_current.ID#0, _current.ID#2], NestedLoopJoin(INNER, [1 preds], " +
				"Project([_current.ID#0, _current.V#1], Scan(T)), Project([_current.ID#0, _current.V#1], Scan(T))))",
		},
		{
			// The same shape reached through the WITH scope rather than the FROM
			// clause. It is a separate derivation of the leg's row type, and it
			// was the reported reproducer.
			name: "CTE legs",
			sql: `WITH a AS (SELECT id FROM t WHERE v > 20), b AS (SELECT id FROM t WHERE v < 30) ` +
				`SELECT a.id, b.id FROM a JOIN b ON a.id = b.id`,
			want: "Project([_current.ID#0, _current.ID#1], NestedLoopJoin(INNER, [1 preds], " +
				"Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds])), " +
				"Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := explainWithOptions(t, tc.sql, sameShapedLegsDDL, nil)
			if got != tc.want {
				t.Errorf("plan = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// TestSameShapedLegsKeepTheirOwnOrdinals is the VACUITY GUARD for the test
// above, and it has to exist separately: every arm there would still pass if
// the two legs were somehow one leg, as long as the ordinals happened to line
// up. This arm makes the legs observably different in what they SELECT while
// keeping their row TYPE identical, so reading the wrong leg changes the plan's
// filter placement rather than only its ordinals.
func TestSameShapedLegsKeepTheirOwnOrdinals(t *testing.T) {
	t.Parallel()
	const sql = `SELECT x.id, y.id FROM (SELECT id FROM t WHERE v > 20) x ` +
		`JOIN (SELECT id FROM t WHERE v < 30) y ON x.id = y.id`
	got := explainWithOptions(t, sql, sameShapedLegsDDL, nil)
	if strings.Count(got, "PredicatesFilter") != 2 {
		t.Errorf("plan has %d PredicatesFilter, want 2 — one per leg; a collapsed leg would "+
			"drop one of the two independent WHERE clauses:\n%s",
			strings.Count(got, "PredicatesFilter"), got)
	}
	if !strings.Contains(got, "Project([_current.ID#0, _current.ID#1]") {
		t.Errorf("plan does not read the two legs at ordinals 0 and 1:\n%s", got)
	}
}
