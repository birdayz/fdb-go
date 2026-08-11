package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_GroupByNestedPathRefusedCleanly pins that a GROUP BY key which
// descends INTO a struct column is refused at PLAN time, for every shape,
// rather than planning and dying in the executor.
//
// WHAT WAS WRONG. Nothing validated the grouping KEY. The only thing that ever
// turned a nested key away was validateGroupByProjection's existence check,
// which compares a PROJECTED column's BARE LEAF against the union of source
// field names — so `n.sk` was refused only when no column named SK existed
// anywhere on the sources. Two consequences, both measured on the parent
// commit and both pinned below:
//
//   - a table that ALSO declares a flat `sk` satisfied the leaf check on the
//     strength of an unrelated column, so `SELECT n.sk FROM t2 GROUP BY n.sk`
//     planned and died with `ordinal resolution: field "N.SK" not resolvable in
//     the runtime row (ordinal -1, row columns [ID SK N]) — malformed plan`;
//   - with NOTHING projected the leaf check never ran at all, so
//     `SELECT COUNT(*) FROM t1 GROUP BY n.sk` escaped the same way over the
//     table with no flat `sk` anywhere. The refusal was never about the table's
//     shape; it was about whether the key happened to be projected.
//
// WHY 0AF00 AND NOT 42703. The reference is well-formed — `n.sk` answers in
// SELECT, WHERE and ORDER BY over the same table, asserted below — so
// "undefined column" would name a column that demonstrably exists.
//
// MEASURED, not inferred: conformance/nested_groupby_key_java_probe_test.go
// runs these shapes against the live Java server at tag 4.12.11.0. Given an
// index over the path, JAVA ANSWERS a nested grouping key —
// `SELECT COUNT(*) FROM T_NG3 GROUP BY n.sk` returns [[2] [1]], the same rows
// as its indexed FLAT twin `GROUP BY k` — while Go answers 0AF00. The vendored
// corpus agrees statically: `select max(q.s) from nested group by r.v.z having
// r.v.z > 120` → `[{330}]` over index i2 (groupby-tests.yamsql:29,61-62). This
// is a capability Java has and Go lacks, which is what UNSUPPORTED_QUERY
// reports. Java reserves 42703 for a qualifier resolving to nothing
// (`GROUP BY zzz.sk` → "Attempting to query non existing column ZZZ.SK") and
// does not spend it here. UNSUPPORTED_QUERY is also the code Java's own
// visitGroupByItem spends on a GROUP BY item it will not take
// (ExpressionVisitor.java:250-258).
//
// A PLANNER DECLINE OUT OF JAVA IS NOT A NESTING SIGNATURE, and a prior version
// of this note read it as one. Java's Cascades has no physical sort, so a GROUP
// BY plans only when an index supplies the key's ordering; without one it
// declines with "Cascades planner could not plan query" and no SQLSTATE — for a
// FLAT key exactly as much as a nested one. The probe's unindexed rows are all
// in that state and decide nothing on their own; the indexed rows above are
// what carry the conclusion.
//
// WHAT ACTUALLY BLOCKS THE FEATURE IN GO is ONE INPUT, not a missing
// capability. Measured by disabling rejectNestedPathGroupKey and running these
// shapes: `SELECT COUNT(*) FROM t1 GROUP BY n.sk` dies with `ordinal
// resolution: field "N.SK" not resolvable in the runtime row (ordinal -1, row
// columns [ID N]) — malformed plan`, and the projected spelling dies earlier
// with 42703 out of validateGroupByProjection's leaf check. That output fits
// two different causes — a pipeline that cannot address nested keys, or a
// working pipeline handed a degraded value — and it is the second:
// upgradeAggregateOperands mints the key as `colRef{table: Qualifier, col:
// Bare}`, reading a qualified key as `table.column` and never as
// `column.member`, so a nested key degrades to a flat dotted FieldValue no
// runtime row can answer. RFC-230 carries that arm.
//
// The group-key NAMER is NOT the blocker, and the sentence that used to stand
// here saying it was — that AggregateKeyColumnName returns
// strings.ToUpper(fv.Field), so two members of one struct share an output name
// — was already false when it was written. That function takes the resolved
// PATH for a nested key (cascades/expressions/group_by.go), pinned by
// expressions/group_by_naming_test.go's
// TestAggregateKeyColumnName_NestedKeyTakesTheResolvedPath.
//
// WHEN NESTED GROUPING KEYS LAND, delete the gate and assert the answers: over
// the seeded (1,1)(1,2)(2,1)(2,2)(1,1) in t1, `GROUP BY n.sk, n.co` is 4 groups
// and each single-key query is 2.
func TestFDB_GroupByNestedPathRefusedCleanly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/gnpr"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE gnpr_tmpl "+
			"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id)) "+
			// t2 differs from t1 in ONE way: it also declares a FLAT column
			// whose name equals the struct member's LEAF. That single
			// difference is what armed the escape, so the two tables are a
			// controlled comparison rather than two fixtures.
			"CREATE TABLE t2 (id BIGINT, sk BIGINT, n gst, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE gnpr_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// BOTH tables MUST be non-empty. The escape this pins surfaced from the
	// EXECUTOR, so an empty table hides it completely: the plan is built, no row
	// is ever evaluated, and the query returns zero rows with no error — a green
	// that proves only that the table was empty.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	// The flat `sk` values are disjoint from the struct's so a wrong-slot read
	// is visible if any of these shapes ever starts answering.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	t.Run("the nested reference itself still answers everywhere else", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL THAT MAKES THE REFUSAL MEAN SOMETHING. If `n.sk` stopped
		// resolving generally, the gate below would pass while saying nothing
		// about GROUP BY — and "undefined column" would have been the honest
		// code after all. Each row count is load-bearing: a refusal that
		// answered zero rows would pass a bare error check.
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"SELECT n.sk FROM t1", 5},
			{"SELECT id FROM t1 ORDER BY n.sk, n.co", 5},
			{"SELECT n.sk FROM t2 ORDER BY n.sk", 3},
			{"SELECT n.sk FROM t2 WHERE n.sk = 1", 2},
		} {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("a nested reference outside GROUP BY stopped working: %s: %v\n"+
					"  This pin's premise is that GROUP BY is the odd one out and the "+
					"reference is valid. If nested paths are broken generally, the gap "+
					"is elsewhere and the 0AF00 below is describing the wrong thing.", tc.query, err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			iterErr := rows.Err()
			rows.Close()
			if iterErr != nil {
				t.Fatalf("%s: iterate: %v", tc.query, iterErr)
			}
			if n != tc.want {
				t.Errorf("%s returned %d rows, want %d", tc.query, n, tc.want)
			}
		}
	})

	t.Run("every nested grouping key is refused at plan time", func(t *testing.T) {
		t.Parallel()
		// The list spans the three dimensions the old behaviour got wrong
		// INDEPENDENTLY, so a fix that only satisfies one of them fails here:
		//   - the table: t1 (no flat SK) and t2 (flat SK present);
		//   - the projection: the key projected, and nothing projected but COUNT(*);
		//   - the member: SK, which collides with a flat column, and CO, which
		//     collides with nothing.
		for _, q := range []string{
			"SELECT n.sk, COUNT(*) FROM t1 GROUP BY n.sk",
			"SELECT n.sk, n.co, COUNT(*) FROM t1 GROUP BY n.sk, n.co",
			"SELECT n.sk FROM t1 GROUP BY n.sk",
			"SELECT COUNT(*) FROM t1 GROUP BY n.sk",
			"SELECT n.sk FROM t2 GROUP BY n.sk",
			"SELECT n.sk, COUNT(*) FROM t2 GROUP BY n.sk",
			"SELECT COUNT(*) FROM t2 GROUP BY n.sk",
			"SELECT COUNT(*) FROM t2 GROUP BY n.co",
			"SELECT n.sk FROM t2 GROUP BY n.sk, id",
			"SELECT n.sk, COUNT(*) FROM t2 AS a GROUP BY n.sk",
			// QUOTED spellings of the same descent. The gate resolves the key
			// through the semantic layer rather than matching text, so a
			// delimited identifier must land on the same refusal — a gate that
			// compared spellings would let these through.
			"SELECT COUNT(*) FROM t2 GROUP BY n.\"SK\"",
			"SELECT COUNT(*) FROM t2 GROUP BY \"N\".\"SK\"",
		} {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Errorf("NESTED-PATH GROUP BY NOW PLANS: %s\n"+
					"  If it answers correct groups, nested grouping keys have landed — "+
					"delete this gate and assert the answers named in this file's header, "+
					"after confirming AggregateKeyColumnName no longer renders the flat "+
					"struct root. If it answers rows WITHOUT grouping correctly, that is "+
					"worse than the refusal it replaced.", q)
				continue
			}
			// The CODE, not merely that it errors: the shape this replaced also
			// errored — from the executor, as internal state — so "it errors" was
			// already true while the defect was fully present.
			if !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("nested-path GROUP BY refused with the WRONG error.\n"+
					"  query: %s\n  got:   %v\n  want:  0AF00 (unsupported_query).\n"+
					"  A 42703 would claim the column does not exist, which the control "+
					"above disproves. An 'ordinal resolution ... malformed plan' means the "+
					"key escaped the semantic layer again and the defect is back.", q, err)
			}
			if strings.Contains(err.Error(), "not resolvable in the runtime row") {
				t.Errorf("the key ESCAPED to the executor again: %s\n  %v", q, err)
			}
		}
	})

	t.Run("legitimate grouping keys are untouched", func(t *testing.T) {
		t.Parallel()
		// Without these the gate could pass by refusing far too much — a
		// refusal keyed on "the reference is qualified", or on "some source has
		// a struct column", would take every one of these with it. The row
		// counts are asserted rather than just the absence of an error, because
		// a key silently dropped from the grouping returns the WRONG number of
		// groups, not an error.
		for _, tc := range []struct {
			query string
			want  int
		}{
			// A flat column sharing the struct member's leaf name — the exact
			// column whose presence used to defeat the refusal — must still group.
			{"SELECT sk, COUNT(*) FROM t2 GROUP BY sk", 3},
			// Table-qualified: the same two-segment SPELLING as `n.sk`, resolving
			// to a source column instead of a struct descent.
			{"SELECT t2.sk, COUNT(*) FROM t2 GROUP BY t2.sk", 3},
			{"SELECT id, COUNT(*) FROM t2 GROUP BY id", 3},
			// Grouping by the struct column ITSELF is not a descent and keeps
			// working: (1,1) appears twice in t1, so 4 groups over 5 rows.
			{"SELECT n, COUNT(*) FROM t1 GROUP BY n", 4},
			// Qualified by a DERIVED table's alias — a two-segment key whose
			// root is not a base-table source at all.
			{"SELECT x.sk, COUNT(*) FROM (SELECT sk FROM t2) AS x GROUP BY x.sk", 3},
			{"SELECT sk, COUNT(*) FROM t2 GROUP BY sk HAVING COUNT(*) > 0", 3},
		} {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Errorf("a LEGITIMATE grouping key was refused: %s: %v\n"+
					"  The refusal must key on the semantic layer's struct DESCENT, not "+
					"on the reference being qualified and not on the table declaring a "+
					"struct column somewhere.", tc.query, err)
				continue
			}
			n := 0
			for rows.Next() {
				n++
			}
			iterErr := rows.Err()
			rows.Close()
			if iterErr != nil {
				t.Errorf("%s: iterate: %v", tc.query, iterErr)
				continue
			}
			if n != tc.want {
				t.Errorf("%s returned %d groups, want %d", tc.query, n, tc.want)
			}
		}
	})

	t.Run("a quoted-lowercase nested path is refused EARLIER, and consistently", func(t *testing.T) {
		t.Parallel()
		// A NEGATIVE RESULT, pinned because it is what makes the gate's lack of
		// a case-folding retry correct rather than lucky.
		//
		// resolveColumnRefStructural retries a failed verbatim lookup in the
		// FOLDED spelling; the nested-key gate does not. That asymmetry would be
		// a hole — a path that resolves only through the retry would pass the
		// existence check and then slip past a gate that never saw it as nested
		// — except that `n."sk"` does not resolve through the retry either. It
		// resolves NOWHERE: SELECT and ORDER BY refuse it with the same 42703,
		// asserted here so the fact is measured rather than assumed.
		//
		// IF THIS SUBTEST GOES RED because quoted-lowercase references start
		// resolving, the gate needs the same folded retry its sibling has —
		// otherwise this exact spelling becomes the one nested grouping key that
		// escapes to the executor.
		for _, q := range []string{
			`SELECT n."sk" FROM t2`,
			`SELECT id FROM t2 ORDER BY n."sk"`,
			`SELECT COUNT(*) FROM t2 GROUP BY n."sk"`,
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Errorf("%s now RESOLVES.\n"+
					"  Quoted-lowercase references reaching the resolver means the "+
					"nested-key gate must gain the folded retry that "+
					"resolveColumnRefStructural already has, or this spelling escapes "+
					"to the executor as a malformed plan.", q)
				continue
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s: got %v, want 42703.\n"+
					"  The point of this control is that GROUP BY refuses this spelling "+
					"for the SAME reason SELECT does, so it is not a grouping gap.", q, err)
			}
		}
	})

	t.Run("the three-segment spelling reaches the SAME refusal", func(t *testing.T) {
		t.Parallel()
		// THE TRIPWIRE THAT FIRED, flipped to what the fix actually does.
		//
		// This arm used to assert 42703 for `GROUP BY a.n.sk`, and it said so
		// explicitly as a statement about Go and not about semantics: Go did not
		// resolve the three-segment `alias.struct.member` spelling ANYWHERE, so
		// "A.N" reached the existence check as an unresolvable qualifier and that
		// check answered 42703 — while Java resolved the spelling and answered
		// rows ([[1] [1] [2]] for three_segment_select_control, measured live at
		// 4.12.11.0). It carried the instruction that when Go learned the
		// spelling the case would move to the 0AF00 group, because it is the same
		// struct descent, and must not silently keep answering 42703 on the way.
		// Go learned it; this is that move.
		//
		// The two spellings of ONE key must agree about WHICH LAYER refuses them.
		// Before the fix the descent test behind the gate could see only two
		// segments, so for `a.n.sk` it was really asking whether a source called
		// "A.N" exists, answering no, and letting the key fall through to
		// undefined-column — a capability gap reported as a missing column.
		for _, q := range []string{
			"SELECT a.n.sk, COUNT(*) FROM t2 AS a GROUP BY a.n.sk",
			"SELECT t2.n.sk, COUNT(*) FROM t2 GROUP BY t2.n.sk",
			"SELECT COUNT(*) FROM t2 AS a GROUP BY a.n.co",
		} {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Errorf("%s now PLANS. If nested grouping keys have landed, this "+
					"arm belongs with the answers named in this file's header.", q)
				continue
			}
			if !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s: got %v, want 0AF00.\n"+
					"  A 42703 here means the three-segment spelling is being decided by "+
					"the EXISTENCE check again — the two arities of one key disagreeing "+
					"about which layer refuses them, which is the defect this arm was "+
					"flipped from.", q, err)
			}
			// The message must name the key AS WRITTEN. A refusal that reported
			// the two-segment spelling would hide which reference was rejected.
			if !strings.Contains(err.Error(), "A.N.SK") &&
				!strings.Contains(err.Error(), "T2.N.SK") &&
				!strings.Contains(err.Error(), "A.N.CO") {
				t.Errorf("%s: refusal %v does not name the key as written", q, err)
			}
		}
	})

	t.Run("a qualifier that resolves to nothing keeps its own error", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL THAT MAKES THE FLIP ABOVE SAFE, and it is the half the
		// original arm was really protecting: the nested-key refusal must not
		// swallow the existence check. A qualifier resolving to nothing is still
		// 42703, decided where it always was. If these started reporting 0AF00,
		// a genuinely misspelled key would be reported as an unsupported feature.
		//
		// Java draws the same line — `GROUP BY zzz.sk` gets "Attempting to query
		// non existing column ZZZ.SK", 42703, out of the live server.
		for _, q := range []string{
			"SELECT COUNT(*) FROM t2 GROUP BY zzz.sk",
			// THREE segments, so it travels the same carrier as the arm above
			// and differs only in that the leading segment names nothing.
			"SELECT COUNT(*) FROM t2 AS a GROUP BY zzz.n.sk",
			// The leading segment resolves and the MEMBER does not: still an
			// existence question, not a capability one.
			"SELECT COUNT(*) FROM t2 AS a GROUP BY a.n.zzz",
		} {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Errorf("%s planned, and it names nothing", q)
				continue
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s: got %v, want 42703 — existence stays owned by the "+
					"existence check, not by the nested-key refusal.", q, err)
			}
		}
	})
}
