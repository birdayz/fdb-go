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
// runs these shapes against the live Java server at tag 4.12.11.0. Java gets a
// nested grouping key PAST semantic analysis — every nested key produces the
// same outcome as a FLAT key ("Cascades planner could not plan query", no
// SQLSTATE), while a qualifier that resolves to nothing produces 42703
// ("Attempting to query non existing column ZZZ.SK"). Java therefore spends
// 42703 on a reference that does not resolve and does NOT spend it here; the
// layer that turns the nested key away in Java is the planner, not the
// resolver. UNSUPPORTED_QUERY is also the code Java's own visitGroupByItem
// spends on a GROUP BY item it will not take (ExpressionVisitor.java:250-258).
//
// What the measurement does NOT say: Java does not ANSWER a nested grouping
// key at this tag either — its Cascades declines the flat key too, absent a
// matching aggregate index. The claim is about WHICH LAYER refuses, which is
// exactly what the error-code choice turns on.
//
// WHEN NESTED GROUPING KEYS LAND, delete the gate and assert the answers: over
// the seeded (1,1)(1,2)(2,1)(2,2)(1,1) in t1, `GROUP BY n.sk, n.co` is 4 groups
// and each single-key query is 2. Check FIRST that the group-key namer no
// longer renders the flat struct ROOT — on this commit AggregateKeyColumnName
// (cascades/expressions/group_by.go) returns strings.ToUpper(fv.Field), so two
// members of one struct would share an output name and the later key would
// overwrite the earlier, returning too few groups silently.
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

	t.Run("a qualifier Go cannot resolve keeps its own error", func(t *testing.T) {
		t.Parallel()
		// The refusal must not swallow the existence check: a qualifier that
		// resolves to nothing is still 42703, decided where it always was. If
		// this started reporting 0AF00, the new gate would be deciding
		// existence too — and a genuinely misspelled key would be reported as
		// an unsupported feature.
		//
		// WHAT THIS PINS IS CURRENT GO BEHAVIOUR, NOT A SEMANTIC RULE. Go does
		// not resolve the THREE-SEGMENT `alias.struct.member` spelling at all —
		// anywhere, not just in GROUP BY — so `A.N` reaches the existence check
		// as an unresolvable qualifier and that check answers 42703. That is a
		// Go limitation. Java RESOLVES the spelling and answers rows: measured
		// live at tag 4.12.11.0 by conformance/nested_groupby_key_java_probe_test.go,
		// whose three_segment_select_control runs `SELECT a.n.sk FROM t AS a`
		// and gets [[1] [1] [2]] out of Java while Go returns this same 42703.
		//
		// So this assertion says "42703 is what Go produces today, from the
		// existence check", NOT "a three-segment qualifier cannot exist". When
		// Go learns the spelling, this case moves to the 0AF00 group above —
		// it is the same struct descent — and it must not silently keep
		// answering 42703 on the way there.
		const q = "SELECT a.n.sk, COUNT(*) FROM t2 AS a GROUP BY a.n.sk"
		_, err := db.QueryContext(ctx, q)
		if err == nil {
			t.Fatalf("%s planned. Go may have learned the three-segment spelling; "+
				"if so this key is a struct descent and belongs with the 0AF00 "+
				"group above, not here", q)
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Errorf("%s: got %v, want 42703 — existence stays owned by the "+
				"existence check, not by the nested-key refusal.", q, err)
		}
	})
}
