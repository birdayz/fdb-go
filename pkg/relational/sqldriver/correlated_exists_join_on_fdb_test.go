package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_CorrelatedExistsJoinOnEnforced pins the inner ON of a CORRELATED
// EXISTS whose inner FROM is an explicit `JOIN … ON`. The correlated-EXISTS
// front-end fallback (buildCorrelatedExists) rebuilt the inner join tree and
// dropped the ON, so EXISTS went silently true over an EMPTY inner join.
//
// Data is deliberately ON-DISCRIMINATING so a dropped ON flips the answer:
//
//	e: (eid=1, fid=99), (eid=2, fid=88)
//	f: (fid=88)                          -- matches fid=88, NOT fid=99
//	p: (1, 10), (2, 20), (3, 30)
//	q: (1), (2)                          -- no q(3): p.id=3 is only visible to the FROM p forms
//
// The correlation `e.eid = p.id` selects e-row eid=p.id:
//   - p.id=1 -> e.fid=99 -> `e JOIN f ON f.fid=e.fid` is EMPTY (no f with 99) -> EXISTS false
//   - p.id=2 -> e.fid=88 -> the join has a matching f(88) row            -> EXISTS true
//   - p.id=3 -> no e.eid=3 -> the inner join has no rows at all          -> EXISTS false
//
// A plan that DROPS the INNER ON makes the inner the bare cross-product e×f, which
// is non-empty whenever the correlated e-row exists, so it would report EXISTS true
// for p.id=1 (the silent-wrong bug). Java 4.12.11.0 returns the polarities asserted
// below. Do NOT weaken the data to a shape where f always matches — that masks the
// bug (the answer is identical with or without the ON).
//
// The LEFT-JOIN case (left_join_preserves_row) is the DUAL: an OUTER join's ON is
// NOT a WHERE — the preserved e-row survives even with no f match — so p.id=1 must
// read EXISTS TRUE. Folding an outer-join ON into the filter (the regression this
// guards) would drop that preserved row and answer false.
func TestFDB_CorrelatedExistsJoinOnEnforced(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_join_on"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cejo_tmpl "+
		"CREATE TABLE p (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE q (qid BIGINT NOT NULL, PRIMARY KEY (qid)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, fid BIGINT, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cejo_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO q VALUES (1), (2)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (1, 99), (2, 88)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (88)")

	type idBool struct {
		v int64
		e bool
	}
	queryVBool := func(t *testing.T, sqlText string) []idBool {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr != nil {
			t.Fatalf("query %q: %v", sqlText, qerr)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if serr := rows.Scan(&r.v, &r.e); serr != nil {
				t.Fatalf("scan %q: %v", sqlText, serr)
			}
			out = append(out, r)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows.Err %q: %v", sqlText, rerr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].v < out[j].v })
		return out
	}

	// q0: PROJECTED EXISTS over a JOIN-in-outer-FROM (implementJoinWithExistential,
	// the 2-leg fold arm). p×q keeps one row per p (q.qid=p.id). Java = the
	// polarities below; EXISTS true for BOTH rows means the ON f.fid=e.fid was
	// dropped.
	t.Run("projected_join_in_from", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id) "+
			"FROM p, q WHERE q.qid = p.id")
		want := []idBool{{10, false}, {20, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v (inner ON f.fid=e.fid dropped if v=10 reads true)", got, want)
		}
	})

	// q3: PROJECTED EXISTS over a SINGLE outer table (implementExistentialSelect).
	// Same ON, same discrimination — the bug is not exclusive to join-in-FROM.
	t.Run("projected_single_outer", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id) FROM p")
		want := []idBool{{10, false}, {20, true}, {30, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// q4: the correlation lives IN the ON, with NO WHERE. Previously this took the
	// no-WHERE early return and produced a bare cross-product with no filter at
	// all — dropping both the ON and the correlation.
	t.Run("correlation_in_on_no_where", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid AND e.eid = p.id) FROM p")
		want := []idBool{{10, false}, {20, true}, {30, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// q5: NOT EXISTS — the opposite polarity. A dropped ON flips these too.
	t.Run("not_exists_projected", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, NOT EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id) FROM p")
		want := []idBool{{10, true}, {20, false}, {30, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// LEFT-JOIN inner: an OUTER join's ON is NOT a WHERE. The preserved e-row
	// survives with no f match, so EXISTS is true whenever the correlated e-row
	// exists, regardless of the ON:
	//   - p.id=1 -> e(1,99), no f match, e PRESERVED -> EXISTS true
	//   - p.id=2 -> e(2,88), f(88) matches           -> EXISTS true
	//   - p.id=3 -> no e.eid=3, LEFT join empty       -> EXISTS false
	// The P1 regression this guards folded the outer-join ON into the inner
	// filter, dropping p.id=1's preserved row and wrongly reading EXISTS false.
	// Contrast with projected_single_outer above (same p.id=1 reads INNER-join
	// false): LEFT preserves, INNER drops.
	t.Run("left_join_preserves_row", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e LEFT JOIN f ON f.fid = e.fid WHERE e.eid = p.id) FROM p")
		want := []idBool{{10, true}, {20, true}, {30, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v (outer-join ON folded into filter drops the preserved p.id=1 row)", got, want)
		}
	})

	// WHERE-EXISTS filter form over the single outer table — only the p rows whose
	// correlated e-row has a matching f survive (p.id=2 -> v=20).
	t.Run("where_exists_filter", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id)")
		if qerr != nil {
			t.Fatalf("where-exists query: %v", qerr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if serr := rows.Scan(&v); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got = append(got, v)
		}
		if fmt.Sprint(got) != fmt.Sprint([]int64{20}) {
			t.Errorf("WHERE EXISTS got %v, want [20] (v=10's inner join is empty)", got)
		}
	})

	// WHERE NOT EXISTS + JOIN-ON (the polarity × join-in-inner combination @claude
	// flagged as inferable-but-unpinned). NOT EXISTS is the mirror of q5's projected
	// form as a filter: p.id=1's inner join is empty → NOT EXISTS true → kept;
	// p.id=2 matches → NOT EXISTS false → excluded; p.id=3 no e → kept. A dropped ON
	// makes p.id=1's inner non-empty → NOT EXISTS false → wrongly excludes v=10.
	t.Run("where_not_exists_join_on", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, "SELECT p.v FROM p WHERE NOT EXISTS "+
			"(SELECT 1 FROM e JOIN f ON f.fid = e.fid WHERE e.eid = p.id)")
		if qerr != nil {
			t.Fatalf("where-not-exists query: %v", qerr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if serr := rows.Scan(&v); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got = append(got, v)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if fmt.Sprint(got) != fmt.Sprint([]int64{10, 30}) {
			t.Errorf("WHERE NOT EXISTS got %v, want [10 30] (v=20's inner join matches → excluded)", got)
		}
	})

	// A bare RIGHT-JOIN-direct inner (the mirror of left_join_preserves_row; @claude
	// flagged RIGHT's executor RIGHT→LEFT normalization warrants an end-to-end row).
	// The ON f.fid=e.fid is inner-inner (stays on the RIGHT node); the correlation
	// e.eid=p.id lifts. e RIGHT JOIN f = {(e2,f88)} (f88 matches e2; no unmatched f):
	//   p.id=1 → e.eid=1 → no row → false; a DROPPED ON (bare e×f) → e1 matches → true.
	//   p.id=2 → e.eid=2 → EXISTS true; p.id=3 → false.
	t.Run("right_join_direct_inner", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e RIGHT JOIN f ON f.fid = e.fid WHERE e.eid = p.id) FROM p")
		want := []idBool{{10, false}, {20, true}, {30, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v (RIGHT-join inner ON dropped if v=10 reads true)", got, want)
		}
	})
}

// TestFDB_CorrelatedExistsInnerThenOuterJoinLevel pins that an INNER-join ON is
// enforced at ITS join level, not globally below the whole inner join — the case
// where an INNER `JOIN … ON` is FOLLOWED by a RIGHT/FULL join. A globally-folded
// inner-inner ON (`e.fid = f.fid`) evaluates NULL→false on a RIGHT/FULL-preserved
// row (whose e/f columns are NULL), wrongly dropping a row that must keep EXISTS
// true.
//
// Data:
//
//	e: (eid=1, fid=100)
//	f: (fid=200)         -- e JOIN f ON e.fid=f.fid is EMPTY (100 != 200)
//	g: (gid=5)           -- RIGHT/FULL JOIN preserves this g row with NULL e/f
//	p: (5, 50), (7, 70)
//
// `RIGHT/FULL JOIN g ON g.gid = f.fid WHERE g.gid = p.id`: the g(5) row is
// preserved (no f match), and the WHERE correlation keeps only the p whose id = 5:
//   - p.id=5 -> g(5) preserved AND g.gid=5=p.id -> EXISTS true
//   - p.id=7 -> g(5) preserved but g.gid=5 != 7 -> EXISTS false
//
// Java 4.12.11.0 returns [[50 true] [70 false]]. The P2 regression this guards
// (folding the INNER `e.fid=f.fid` into one filter below the whole inner join)
// returns [[50 false] [70 false]] — the preserved g row's NULL e.fid fails the
// misplaced ON.
func TestFDB_CorrelatedExistsInnerThenOuterJoinLevel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_mixed_join"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cemj_tmpl "+
		"CREATE TABLE p (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, fid BIGINT, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid)) "+
		"CREATE TABLE g (gid BIGINT NOT NULL, PRIMARY KEY (gid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cemj_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO p VALUES (5, 50), (7, 70)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (1, 100)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (200)")
	mustExec(t, db, ctx, "INSERT INTO g VALUES (5)")

	type idBool struct {
		v int64
		e bool
	}
	queryVBool := func(t *testing.T, sqlText string) []idBool {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr != nil {
			t.Fatalf("query %q: %v", sqlText, qerr)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if serr := rows.Scan(&r.v, &r.e); serr != nil {
				t.Fatalf("scan %q: %v", sqlText, serr)
			}
			out = append(out, r)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows.Err %q: %v", sqlText, rerr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].v < out[j].v })
		return out
	}

	// INNER e JOIN f (empty), then RIGHT JOIN g preserves g(5). The inner-inner ON
	// e.fid=f.fid must apply at the e-f join level (before the RIGHT join), not
	// below the whole inner join.
	t.Run("inner_then_right", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.fid = f.fid "+
			"RIGHT JOIN g ON g.gid = f.fid WHERE g.gid = p.id) FROM p")
		want := []idBool{{50, true}, {70, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v (INNER ON folded below the whole join drops the preserved g row for p.id=5)", got, want)
		}
	})

	// Same with FULL JOIN — the g(5) row is preserved on the right side of the
	// full outer join.
	t.Run("inner_then_full", func(t *testing.T) {
		got := queryVBool(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.fid = f.fid "+
			"FULL JOIN g ON g.gid = f.fid WHERE g.gid = p.id) FROM p")
		want := []idBool{{50, true}, {70, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// TestFDB_CorrelatedExistsOnCorrelationBeforeRightFull pins the correlation
// sibling of the join-level ON placement: a CORRELATED conjunct in an INNER-join
// ON is lifted to the EXISTS level, which applies it AFTER the whole inner plan.
// That is correct only when no LATER join preserves the other side with NULLs on
// this join's columns. A later RIGHT/FULL join preserves g-side rows with NULL e,
// so the lifted `e.eid = p.id` evaluates NULL→false and rejects those preserved
// rows — EXISTS wrongly false. Reproducing the ON's join-level placement is not
// something this front-end fallback can do, so it DECLINES cleanly (0A000). A
// later LEFT/INNER join does not preserve NULL-e rows, so the lift is kept.
//
// Data:
//
//	e: (eid=1, fid=100)
//	f: (fid=200)
//	g: (gid=5)
//	p: (1, 10)
func TestFDB_CorrelatedExistsOnCorrelationBeforeRightFull(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_on_before_rf"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cebrf_tmpl "+
		"CREATE TABLE p (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, fid BIGINT, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid)) "+
		"CREATE TABLE g (gid BIGINT NOT NULL, PRIMARY KEY (gid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cebrf_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (1, 100)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (200)")
	mustExec(t, db, ctx, "INSERT INTO g VALUES (5)")

	requireDecline := func(t *testing.T, sqlText string) {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr == nil {
			// Some plan errors only surface on iteration; force it.
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a clean decline (0A000), got no error for %q", sqlText)
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUnsupportedOperation)
	}

	// Correlated INNER-join ON (`e.eid = p.id`) before a later RIGHT join: the
	// RIGHT join preserves g rows with NULL e, so lifting the ON would drop them.
	// Decline. (Without the decline the lift returns EXISTS false for the
	// preserved g row — a silent-wrong answer.)
	t.Run("decline_before_right", func(t *testing.T) {
		requireDecline(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.eid = p.id "+
			"RIGHT JOIN g ON g.gid = f.fid) FROM p")
	})

	// Same before a later FULL join.
	t.Run("decline_before_full", func(t *testing.T) {
		requireDecline(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.eid = p.id "+
			"FULL JOIN g ON g.gid = f.fid) FROM p")
	})

	// A later LEFT join preserves the LEFT (e/f) side — which HAS the correlation
	// column e — so the lift is safe and must NOT decline. e JOIN f ON e.eid=p.id
	// = {(1,100,200)}; LEFT JOIN g (no g.gid=200) keeps it -> EXISTS true. This
	// pins that the fix is surgical (no over-decline of the common shape).
	t.Run("safe_before_left", func(t *testing.T) {
		var v int64
		var ex bool
		if serr := db.QueryRowContext(ctx, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.eid = p.id "+
			"LEFT JOIN g ON g.gid = f.fid) FROM p").Scan(&v, &ex); serr != nil {
			t.Fatalf("later-LEFT lift must NOT decline: %v", serr)
		}
		if v != 10 || !ex {
			t.Errorf("later-LEFT: got (%d,%v), want (10,true)", v, ex)
		}
	})

	// WHERE-EXISTS form of the decline must ALSO surface 0A000 (unsupported), not
	// 42703 (undefined-column). The WHERE-EXISTS planning path maps a
	// CorrelatedExistsError from the predicate walk; an intentional decline must be
	// distinguished from a genuine resolution failure so both the projected and
	// WHERE-EXISTS positions report the same 0A000.
	t.Run("decline_where_exists_before_right", func(t *testing.T) {
		requireDecline(t, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON e.eid = p.id "+
			"RIGHT JOIN g ON g.gid = f.fid)")
	})
}

// TestFDB_CorrelatedExistsNestedSubqueryInOnDeclines pins the classifier
// robustness boundary: a JOIN ON that itself contains a NESTED subquery (an
// EXISTS/scalar subquery) inside a correlated EXISTS is DECLINED cleanly (0A000).
// This front-end fallback cannot place it correctly — the ON conjunct references
// a GENERATED subquery alias, so lifting it to the outer level misclassifies it
// as a correlation (and the downstream nested-EXISTS hoist would DROP the whole
// `e JOIN f` tree, returning EXISTS true over an empty inner join); while keeping
// it on the join node ORPHANS the nested subquery's plan (the join node carries
// no ExistsSubqueries slot, so the nested EXISTS evaluates as a dead always-false
// predicate). Neither placement is correct, so it declines correct-or-conservative.
//
// Repro: `SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON EXISTS (SELECT 1
// FROM h WHERE h.hid = p.id)) FROM p`. With e EMPTY the correct answer is EXISTS
// false (the inner join has no rows). The pre-fix lift silently DROPPED the join
// tree and returned EXISTS true whenever h matched — a silent-wrong answer.
func TestFDB_CorrelatedExistsNestedSubqueryInOnDeclines(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_nested_on"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cenon_tmpl "+
		"CREATE TABLE p (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid)) "+
		"CREATE TABLE h (hid BIGINT NOT NULL, PRIMARY KEY (hid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cenon_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// e is intentionally EMPTY (the silent-wrong case); h matches p.id=1.
	mustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (200)")
	mustExec(t, db, ctx, "INSERT INTO h VALUES (1)")

	requireDecline := func(t *testing.T, sqlText string) {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a clean decline (0A000), got no error for %q", sqlText)
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUnsupportedOperation)
	}

	// Nested EXISTS inside the JOIN ON.
	t.Run("nested_exists_in_on", func(t *testing.T) {
		requireDecline(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON EXISTS "+
			"(SELECT 1 FROM h WHERE h.hid = p.id)) FROM p")
	})

	// Nested EXISTS in the ON alongside an inner-inner conjunct — still declined.
	t.Run("nested_exists_in_on_with_conjunct", func(t *testing.T) {
		requireDecline(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.fid = 200 AND EXISTS "+
			"(SELECT 1 FROM h WHERE h.hid = p.id)) FROM p")
	})

	// WHERE-EXISTS form — must ALSO surface 0A000, not 42703 (undefined-column).
	t.Run("nested_exists_in_on_where_form", func(t *testing.T) {
		requireDecline(t, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON EXISTS "+
			"(SELECT 1 FROM h WHERE h.hid = p.id))")
	})

	// The nested EXISTS in the ON references the CURRENT leg `f` (not the outer),
	// so WalkPredicate FAILS first (the nested planner's scope lacks `f`) before
	// the onAddedSubquery check. The projected form already maps to 0A000, but the
	// WHERE-EXISTS form previously surfaced the raw 42703; the pre-walk
	// subquery/EXISTS detection now declines it 0A000 in BOTH forms. Correlated
	// via `WHERE e.eid = p.id` so it reaches the correlated fallback.
	t.Run("nested_exists_in_on_refs_current_leg_projected", func(t *testing.T) {
		requireDecline(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON EXISTS "+
			"(SELECT 1 FROM h WHERE h.hid = f.fid) WHERE e.eid = p.id) FROM p")
	})
	t.Run("nested_exists_in_on_refs_current_leg_where_form", func(t *testing.T) {
		requireDecline(t, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON EXISTS "+
			"(SELECT 1 FROM h WHERE h.hid = f.fid) WHERE e.eid = p.id)")
	})
}

// TestFDB_CorrelatedExistsCteInnerNoWhereNoOn pins the CTE-safe fast path of the
// correlated-EXISTS fallback. A projected EXISTS whose inner FROM is a CTE and
// which enters the fallback ONLY because its (ignored) SELECT list references an
// outer column — with NO WHERE and NO ON — must return the bare CTE scan WITHOUT
// resolving the inner source as a catalog table. A CTE is not a catalog table, so
// reaching Analyzer.ResolveTable rejects it ("table not found"). The fast path
// therefore runs BEFORE any table resolution.
//
// `WITH c AS (SELECT order_id FROM ord) SELECT o.order_id, EXISTS (SELECT
// o.order_id FROM c) FROM ord AS o`: c has both order rows, so EXISTS is true for
// every outer row. Java 4.12.11.0 returns [[1 true] [2 true]].
func TestFDB_CorrelatedExistsCteInnerNoWhereNoOn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_cte_inner"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cecte_tmpl "+
		"CREATE TABLE ord (order_id BIGINT NOT NULL, cust_id BIGINT, PRIMARY KEY (order_id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cecte_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO ord VALUES (1, 10), (2, 20)")

	type idBool struct {
		id int64
		e  bool
	}
	rows, qerr := db.QueryContext(ctx, "WITH c AS (SELECT order_id FROM ord) "+
		"SELECT o.order_id, EXISTS (SELECT o.order_id FROM c) FROM ord AS o")
	if qerr != nil {
		t.Fatalf("CTE-inner projected EXISTS must plan (fast path before table resolution): %v", qerr)
	}
	defer rows.Close()
	var got []idBool
	for rows.Next() {
		var r idBool
		if serr := rows.Scan(&r.id, &r.e); serr != nil {
			t.Fatalf("scan: %v", serr)
		}
		got = append(got, r)
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("rows.Err: %v", rerr)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].id < got[j].id })
	want := []idBool{{1, true}, {2, true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v (CTE inner resolved as a catalog table = 'table not found')", got, want)
	}
}

// TestFDB_CorrelatedExistsCteInnerWithPredicate pins the round-8 fix: a CTE
// inner source with an ON or a WHERE (which skips the no-WHERE/no-ON fast path
// and reaches the scope/resolver) must resolve the CTE via the enclosing query's
// CTE registry, not Analyzer.ResolveTable against the catalog. Before the fix the
// CTE inner failed "table not found" once any ON or WHERE was present — a
// regression over baseline, where a CTE-inner-with-ON planned (the ON was
// silently dropped, and a trivial `ON 1=1` returned correct rows).
//
// Data: ord = {1, 2}; the CTE c = SELECT order_id FROM ord = {1, 2}. t = {100}
// (no order_id matches). The correlation that forces the fallback is the ignored
// SELECT-list ref o.order_id. Java 4.12.11.0 semantics.
func TestFDB_CorrelatedExistsCteInnerWithPredicate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_cte_pred"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cectp_tmpl "+
		"CREATE TABLE ord (order_id BIGINT NOT NULL, cust_id BIGINT, PRIMARY KEY (order_id)) "+
		"CREATE TABLE t (tid BIGINT NOT NULL, PRIMARY KEY (tid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cectp_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO ord VALUES (1, 10), (2, 20)")
	mustExec(t, db, ctx, "INSERT INTO t VALUES (100)")

	type idBool struct {
		id int64
		e  bool
	}
	queryIDBool := func(t *testing.T, sqlText string) []idBool {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr != nil {
			t.Fatalf("CTE-inner EXISTS must plan (CTE resolved via registry, not catalog): %v\n  sql: %s", qerr, sqlText)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if serr := rows.Scan(&r.id, &r.e); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			out = append(out, r)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows.Err: %v", rerr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}
	eq := func(t *testing.T, got []idBool, want []idBool) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}

	// The exact round-8 regression: CTE inner + a trivial `ON 1=1`, no WHERE. Must
	// PLAN (baseline returned correct rows; HEAD errored "table not found").
	t.Run("cte_on_trivial_plans", func(t *testing.T) {
		eq(t, queryIDBool(t, "WITH c AS (SELECT order_id FROM ord) "+
			"SELECT o.order_id, EXISTS (SELECT o.order_id FROM c JOIN t ON 1 = 1) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// CTE column in an ON with NO match: c.order_id in {1,2}, t.tid=100 -> the inner
	// join is EMPTY -> EXISTS false. Proves the CTE column resolves AND the ON is
	// ENFORCED (a dropped ON would cross-join c×t -> non-empty -> wrongly true).
	t.Run("cte_on_column_enforced_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "WITH c AS (SELECT order_id FROM ord) "+
			"SELECT o.order_id, EXISTS (SELECT o.order_id FROM c JOIN t ON c.order_id = t.tid) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})

	// CTE column in a WHERE that keeps rows (positive polarity).
	t.Run("cte_where_column_true", func(t *testing.T) {
		eq(t, queryIDBool(t, "WITH c AS (SELECT order_id FROM ord) "+
			"SELECT o.order_id, EXISTS (SELECT o.order_id FROM c WHERE c.order_id > 0) FROM ord AS o"),
			[]idBool{{1, true}, {2, true}})
	})

	// CTE column in a WHERE that filters all out (negative polarity) -> EXISTS false.
	// Proves the WHERE is ENFORCED (a dropped WHERE would keep c non-empty -> true).
	t.Run("cte_where_column_enforced_false", func(t *testing.T) {
		eq(t, queryIDBool(t, "WITH c AS (SELECT order_id FROM ord) "+
			"SELECT o.order_id, EXISTS (SELECT o.order_id FROM c WHERE c.order_id > 100) FROM ord AS o"),
			[]idBool{{1, false}, {2, false}})
	})
}

// TestFDB_CorrelatedExistsOnReferencesLaterInnerAlias pins the round-10 ON-scoping
// fix: a JOIN ON is walked against SQL LEFT-TO-CURRENT visibility ({primary +
// legs[0..i]}), NOT the full inner set. When an earlier ON references an alias
// that is REUSED as a LATER inner join source, per-join scope correctly binds it
// to the OUTER source (the later leg isn't in scope yet) — but the lifted
// correlation's QOV(name) then collides with the same-named inner leg at runtime
// (ambiguous). That outer/inner alias collision is DECLINED cleanly (0A000)
// rather than mis-answered.
//
// Before the fix the FULL-scope resolver bound the earlier ON's `p` to the later
// inner `JOIN base AS p`, misclassified the correlation as inner, and returned
// wrong rows (EXISTS true for outer rows that must be false) — silent-wrong.
//
// The SELECT o.oid (outer o, no inner o) forces the correlated fallback; the
// earlier ON `p.pk = e.eid` references `p`, reused as the later `JOIN base AS p`.
func TestFDB_CorrelatedExistsOnReferencesLaterInnerAlias(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_later_alias"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cela_tmpl "+
		"CREATE TABLE base (pk BIGINT NOT NULL, pval BIGINT, PRIMARY KEY (pk)) "+
		"CREATE TABLE other (oid BIGINT NOT NULL, PRIMARY KEY (oid)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE cela_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO base VALUES (1, 10), (2, 20)")
	mustExec(t, db, ctx, "INSERT INTO other VALUES (99)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (1)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (1)")

	requireDecline := func(t *testing.T, sqlText string) {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a clean decline (0A000), got no error for %q", sqlText)
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUnsupportedOperation)
	}

	t.Run("decline_on_references_later_inner_alias", func(t *testing.T) {
		requireDecline(t, "SELECT p.pk, EXISTS (SELECT o.oid FROM e JOIN f ON p.pk = e.eid "+
			"JOIN base AS p ON 1 = 1) FROM base AS p, other AS o")
	})
}

// TestFDB_CorrelatedExistsOnErrorCodeConsistency pins that the PROJECTED and
// WHERE-EXISTS forms of a correlated EXISTS agree on the SQLSTATE for the same
// inner-ON shape: a GENUINE resolution failure (a missing column in the ON)
// reports 42703 (undefined-column) in BOTH forms, while a DELIBERATE decline of
// an unsupported shape reports 0A000 in BOTH forms. Before the fix the projected
// path mapped ANY CorrelatedExistsError to 0A000, so a genuine missing column in
// a projected EXISTS's ON wrongly reported 0A000 instead of 42703.
func TestFDB_CorrelatedExistsOnErrorCodeConsistency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_corr_exists_errcode"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ceec_tmpl "+
		"CREATE TABLE p (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE e (eid BIGINT NOT NULL, PRIMARY KEY (eid)) "+
		"CREATE TABLE f (fid BIGINT NOT NULL, PRIMARY KEY (fid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE ceec_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10)")
	mustExec(t, db, ctx, "INSERT INTO e VALUES (1)")
	mustExec(t, db, ctx, "INSERT INTO f VALUES (1)")

	getErr := func(t *testing.T, sqlText string) error {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, sqlText)
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected an error for %q", sqlText)
		}
		return qerr
	}

	// GENUINE missing column in the ON (f.nope) -> 42703 in BOTH forms.
	t.Run("projected_genuine_missing_column_42703", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON f.nope = e.eid WHERE e.eid = p.id) FROM p"),
			api.ErrCodeUndefinedColumn)
	})
	t.Run("where_genuine_missing_column_42703", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON f.nope = e.eid WHERE e.eid = p.id)"),
			api.ErrCodeUndefinedColumn)
	})

	// DELIBERATE decline (nested subquery in the ON) -> 0A000 in BOTH forms.
	t.Run("projected_deliberate_decline_0A000", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON EXISTS (SELECT 1 FROM f AS g WHERE g.fid = p.id)) FROM p"),
			api.ErrCodeUnsupportedOperation)
	})
	t.Run("where_deliberate_decline_0A000", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v FROM p WHERE EXISTS (SELECT 1 FROM e JOIN f ON EXISTS (SELECT 1 FROM f AS g WHERE g.fid = p.id))"),
			api.ErrCodeUnsupportedOperation)
	})

	// A DELIBERATE correlated-SCALAR-subquery decline (a correlated scalar with no
	// inner WHERE — "WHERE clause required for correlation") is a Message-only
	// CorrelatedExistsError with no wrapped cause → must report 0A000, NOT 42703.
	// The projected-EXISTS SQLSTATE unification routes scalar errors through
	// mapPredicateWalkError too; without the Cause==nil deliberate-decline arm a
	// genuine-failure remap would report these unsupported shapes as 42703.
	t.Run("correlated_scalar_no_where_decline_0A000", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v, (SELECT p.id FROM f) FROM p"),
			api.ErrCodeUnsupportedOperation)
	})

	// A top-level WHERE with a correlated-scalar deliberate decline must ALSO give
	// 0A000 (the WHERE-EXISTS mapping branch honors the same Cause==nil rule as
	// mapPredicateWalkError — every path agrees).
	t.Run("where_scalar_decline_0A000", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v FROM p WHERE p.id = (SELECT p.id FROM f)"),
			api.ErrCodeUnsupportedOperation)
	})

	// An IN-subquery inside a JOIN ON is CLEANLY DECLINED (0AF00 "subquery in a
	// JOIN ON clause is not supported") by the ON-shape guard BEFORE the correlated
	// fallback — NOT the raw 42703 a dropped ON would give. Correct-or-conservative:
	// the ON is never silently dropped. (The 0AF00-vs-0A000 nuance is two flavors of
	// "unsupported"; the point is it's a clean decline, never a wrong answer.)
	t.Run("in_subquery_in_on_declines_cleanly", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v, EXISTS (SELECT 1 FROM e JOIN f ON e.eid IN "+
			"(SELECT g.fid FROM f AS g WHERE g.fid = f.fid) WHERE e.eid = p.id) FROM p"),
			api.ErrCodeUnsupportedQuery)
	})

	// A deliberate unsupported decline that WRAPS a NON-semantic cause (e.g. an
	// unsupported aggregate shape) must report 0A000, NOT 42703. Error
	// classification is by the recognized-cause TYPE: a genuine resolution error
	// (ColumnNotFound/SourceNotFound) → 42703; anything else (unsupported shape,
	// wrapped or not) → 0A000. A wrong 42703 here means the mapper defaulted a
	// non-resolution failure to undefined-column.
	t.Run("wrapped_unsupported_cause_scalar_0A000", func(t *testing.T) {
		requireSQLSTATE(t, getErr(t, "SELECT p.v, (SELECT COUNT(DISTINCT f.fid) FROM f WHERE f.fid = p.id) FROM p"),
			api.ErrCodeUnsupportedOperation)
	})
}
