package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_GroupByNestedPathKeyShapes is the SHAPE coverage for a GROUP BY key
// that descends INTO a struct column. The acceptance criterion lives in
// groupby_nested_path_key_fdb_test.go (the corpus query, on the corpus schema);
// this file drives the dimensions that used to fail INDEPENDENTLY of each
// other, so a change that satisfies one of them still fails here.
//
// It was a refusal pin, and every arm below is the same arm with its verdict
// moved. Keeping the shapes rather than starting over is deliberate: the list
// was chosen because each entry isolated a way the old behaviour went wrong,
// and that is exactly the list a new capability must answer.
//
// THE THREE DIMENSIONS, unchanged from when they were refusals:
//   - the TABLE: t1 (no flat SK) and t2 (a flat SK sharing the path's leaf);
//   - the PROJECTION: the key projected, and nothing projected but COUNT(*);
//   - the MEMBER: SK, which collides with a flat column, and CO, which does not.
//
// WHAT WAS ACTUALLY WRONG, and it was ONE INPUT rather than a missing
// capability. Nothing validated the grouping KEY, so a nested key either died
// in the executor as internal state, or was turned away by
// validateGroupByProjection's existence check comparing a PROJECTED column's
// BARE LEAF against the union of top-level source field names. Both are
// downstream of the mint: the group-key ladder in upgradeAggregateOperands
// minted a qualified key as `colRef{table: Qualifier, col: Bare}`, reading a
// qualified key as `table.column` and never as `column.member`, so a nested key
// degraded to a flat dotted FieldValue no runtime row can answer. The executor
// was already correct — it evaluates the grouping key VALUE against the row,
// Java's StreamGrouping.evalGroupingKey — so nothing below the mint changed.
//
// Java answers these shapes: conformance/nested_groupby_key_java_probe_test.go
// runs them against the live server at tag 4.12.11.0 and, given an index over
// the path, `SELECT COUNT(*) FROM T_NG3 GROUP BY n.sk` returns [[2] [1]] — the
// same rows as its indexed FLAT twin `GROUP BY k`. A Java PLANNER DECLINE is
// not evidence either way: Java's Cascades has no physical sort, so it declines
// a FLAT key just as readily when no index supplies the ordering.
//
// GO'S PLAN DIFFERS FROM JAVA'S AND THE ROWS DO NOT. Go does not match an
// aggregate index for a nested grouping key (aggColumnMatches offers a
// length-1 candidate; a nested path has length >= 2), so it groups over a
// base-record scan under an in-memory sort. That is the sanctioned read-side
// fallback, recorded here so "the shapes answer" is not read as "the plans
// match"; the plan shape itself is pinned in the acceptance file.
func TestFDB_GroupByNestedPathKeyShapes(t *testing.T) {
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
			// difference is what used to arm the escape, and it is now what
			// makes a wrong-column read visible — the two tables are a
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

	// BOTH tables MUST be non-empty. What these arms pin surfaced from the
	// EXECUTOR, so an empty table hides it completely: the plan is built, no row
	// is ever evaluated, and the query returns zero rows with no error — a green
	// that proves only that the table was empty.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	// The flat `sk` values are disjoint from the struct's, so a wrong-slot read
	// changes the group COUNT rather than coinciding with the right answer.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	countGroups := func(t *testing.T, q string) (int, error) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}

	t.Run("the nested reference itself answers everywhere else", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL THAT MAKES THE REST MEAN SOMETHING. If `n.sk` stopped
		// resolving generally, the grouping arms below would fail and read as a
		// group-key defect. Each row count is load-bearing: a query answering
		// zero rows would pass a bare error check.
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"SELECT n.sk FROM t1", 5},
			{"SELECT id FROM t1 ORDER BY n.sk, n.co", 5},
			{"SELECT n.sk FROM t2 ORDER BY n.sk", 3},
			{"SELECT n.sk FROM t2 WHERE n.sk = 1", 2},
		} {
			n, err := countGroups(t, tc.query)
			if err != nil {
				t.Fatalf("a nested reference outside GROUP BY stopped working: %s: %v\n"+
					"  If nested paths are broken generally, the gap is elsewhere and "+
					"the grouping arms below are describing the wrong thing.", tc.query, err)
			}
			if n != tc.want {
				t.Errorf("%s returned %d rows, want %d", tc.query, n, tc.want)
			}
		}
	})

	t.Run("every nested grouping key groups by the member it names", func(t *testing.T) {
		t.Parallel()
		// The group COUNT is what is asserted, because that is the number that
		// moves when the key reads the wrong thing: the struct ROOT gives one
		// group per distinct struct value, the colliding FLAT column on t2
		// gives three, and a key silently dropped gives one.
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"SELECT n.sk, COUNT(*) FROM t1 GROUP BY n.sk", 2},
			{"SELECT n.sk, n.co, COUNT(*) FROM t1 GROUP BY n.sk, n.co", 4},
			{"SELECT n.sk FROM t1 GROUP BY n.sk", 2},
			{"SELECT COUNT(*) FROM t1 GROUP BY n.sk", 2},
			{"SELECT n.sk FROM t2 GROUP BY n.sk", 2},
			{"SELECT n.sk, COUNT(*) FROM t2 GROUP BY n.sk", 2},
			{"SELECT COUNT(*) FROM t2 GROUP BY n.sk", 2},
			{"SELECT COUNT(*) FROM t2 GROUP BY n.co", 2},
			// Two keys, one nested and one flat, so a nested key that captured
			// the whole grouping (or lost its own slot) changes this number.
			{"SELECT n.sk FROM t2 GROUP BY n.sk, id", 3},
			{"SELECT n.sk, COUNT(*) FROM t2 AS a GROUP BY n.sk", 2},
			// QUOTED spellings of the same descent. Resolution goes through the
			// semantic layer rather than matching text, so a delimited
			// identifier must land on the same answer.
			{"SELECT COUNT(*) FROM t2 GROUP BY n.\"SK\"", 2},
			{"SELECT COUNT(*) FROM t2 GROUP BY \"N\".\"SK\"", 2},
		} {
			n, err := countGroups(t, tc.query)
			if err != nil {
				t.Errorf("a nested grouping key was refused: %s: %v", tc.query, err)
				continue
			}
			if n != tc.want {
				t.Errorf("%s returned %d groups, want %d.\n"+
					"  THREE over t2 means the key read the flat column sharing its "+
					"leaf; ONE means the key was dropped from the grouping; a count "+
					"matching the number of distinct STRUCT values means it read the "+
					"root instead of the member.", tc.query, n, tc.want)
			}
		}
	})

	t.Run("the three-segment spelling answers the same as the two", func(t *testing.T) {
		t.Parallel()
		// The two spellings of ONE key must agree, and for a long time they did
		// not agree about anything: `a.n.sk` was not resolvable ANYWHERE in Go,
		// so "A.N" reached the existence check as an unresolvable qualifier and
		// the key was reported as an undefined column, while `n.sk` was reported
		// as an unsupported feature. Java resolved both. Both now answer.
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"SELECT a.n.sk, COUNT(*) FROM t2 AS a GROUP BY a.n.sk", 2},
			{"SELECT t2.n.sk, COUNT(*) FROM t2 GROUP BY t2.n.sk", 2},
			{"SELECT COUNT(*) FROM t2 AS a GROUP BY a.n.co", 2},
		} {
			n, err := countGroups(t, tc.query)
			if err != nil {
				t.Errorf("%s: %v\n"+
					"  The alias-qualified spelling is the same struct descent with "+
					"its source named; a failure here and a pass on the two-segment "+
					"twin means the leading segment is being folded into the lookup "+
					"instead of peeled.", tc.query, err)
				continue
			}
			if n != tc.want {
				t.Errorf("%s returned %d groups, want %d — and its two-segment twin "+
					"returns %d", tc.query, n, tc.want, tc.want)
			}
		}
	})

	t.Run("legitimate non-nested grouping keys are untouched", func(t *testing.T) {
		t.Parallel()
		// The descent arm must key on the semantic layer's struct DESCENT, not
		// on the reference being qualified and not on some source declaring a
		// struct column. The row counts are asserted rather than the absence of
		// an error, because a key silently re-routed returns the WRONG number of
		// groups, not an error.
		for _, tc := range []struct {
			query string
			want  int
		}{
			// A flat column sharing the struct member's leaf name.
			{"SELECT sk, COUNT(*) FROM t2 GROUP BY sk", 3},
			// Table-qualified: the same two-segment SPELLING as `n.sk`,
			// resolving to a source column instead of a struct descent.
			{"SELECT t2.sk, COUNT(*) FROM t2 GROUP BY t2.sk", 3},
			{"SELECT id, COUNT(*) FROM t2 GROUP BY id", 3},
			// Grouping by the struct column ITSELF is not a descent: (1,1)
			// appears twice in t1, so 4 groups over 5 rows.
			{"SELECT n, COUNT(*) FROM t1 GROUP BY n", 4},
			// Qualified by a DERIVED table's alias — a two-segment key whose
			// root is not a base-table source at all.
			{"SELECT x.sk, COUNT(*) FROM (SELECT sk FROM t2) AS x GROUP BY x.sk", 3},
			{"SELECT sk, COUNT(*) FROM t2 GROUP BY sk HAVING COUNT(*) > 0", 3},
		} {
			n, err := countGroups(t, tc.query)
			if err != nil {
				t.Errorf("a LEGITIMATE grouping key was refused: %s: %v", tc.query, err)
				continue
			}
			if n != tc.want {
				t.Errorf("%s returned %d groups, want %d", tc.query, n, tc.want)
			}
		}
	})

	t.Run("a quoted-lowercase nested path is refused, and consistently", func(t *testing.T) {
		t.Parallel()
		// A NEGATIVE RESULT, kept because it is what says the descent arm needs
		// no case-folding retry of its own rather than that one was forgotten.
		//
		// resolveColumnRefStructural retries a failed verbatim lookup in the
		// FOLDED spelling. `n."sk"` does not resolve through that retry either:
		// it resolves NOWHERE, so SELECT, ORDER BY and GROUP BY all refuse it
		// with the same 42703. That is the fact, measured rather than assumed.
		//
		// IF THIS GOES RED because quoted-lowercase references start resolving,
		// the GROUP BY arm must answer the same rows as the SELECT arm — the
		// three clauses agreeing is the property, not the refusal.
		for _, q := range []string{
			`SELECT n."sk" FROM t2`,
			`SELECT id FROM t2 ORDER BY n."sk"`,
			`SELECT COUNT(*) FROM t2 GROUP BY n."sk"`,
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Errorf("%s now RESOLVES.\n"+
					"  Check that GROUP BY answers the same member SELECT does; the "+
					"property is that the three clauses agree.", q)
				continue
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s: got %v, want 42703.\n"+
					"  The point of this control is that GROUP BY refuses this spelling "+
					"for the SAME reason SELECT does, so it is not a grouping gap.", q, err)
			}
		}
	})

	t.Run("a qualifier that resolves to nothing keeps its own error", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL THAT MAKES THE FLIP ABOVE SAFE. Admitting nested keys must
		// not admit keys that name nothing: a misspelled qualifier is still
		// 42703, decided where it always was. Java draws the same line —
		// `GROUP BY zzz.sk` gets "Attempting to query non existing column
		// ZZZ.SK", 42703, out of the live server.
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
					"existence check, not by the descent arm.", q, err)
			}
		}
	})
}
