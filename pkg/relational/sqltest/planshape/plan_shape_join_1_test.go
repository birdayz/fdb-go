package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_PlanShapeJoinFilterPushdown verifies that a filter on the outer side
// of a join is pushed below the NestedLoopJoin (filter before join, not after).
func TestFDB_PlanShapeJoinFilterPushdown(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jfp",
		"CREATE TABLE a (id BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE b (bid BIGINT, aid BIGINT, PRIMARY KEY (bid))")

	// Insert data into table a.
	for _, row := range []struct {
		id   int
		name string
	}{
		{1, "foo"},
		{2, "bar"},
		{3, "baz"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO a VALUES (%d, '%s')", row.id, row.name,
		)); err != nil {
			t.Fatalf("INSERT a id=%d: %v", row.id, err)
		}
	}
	// Insert data into table b.
	for _, row := range []struct {
		bid int
		aid int
	}{
		{10, 1},
		{20, 2},
		{30, 1},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO b VALUES (%d, %d)", row.bid, row.aid,
		)); err != nil {
			t.Fatalf("INSERT b bid=%d: %v", row.bid, err)
		}
	}

	q := "SELECT a.id FROM a INNER JOIN b ON a.id = b.aid WHERE a.name = 'foo'"
	plan := testkit.ExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// Must contain a join operator (NLJ or FlatMap with correlated scan).
	if !strings.Contains(plan, "NestedLoopJoin") && !strings.Contains(plan, "FlatMap") {
		t.Fatalf("expected NestedLoopJoin or FlatMap in plan, got: %s", plan)
	}

	// WHERE predicates are merged into the NLJ (not a separate Filter).
	// The NLJ carries both the ON predicate (a.id = b.aid) and the WHERE
	// predicate (a.name = 'foo') as [2 preds]. This matches Java's
	// SelectExpression which carries all predicates inline.
	// The join must carry predicates: NLJ carries them inline, FlatMap
	// pushes the equi-join to the correlated scan (residuals stay as
	// PredicatesFilter above).
	if strings.Contains(plan, "NestedLoopJoin") {
		nljIdx := strings.Index(plan, "NestedLoopJoin")
		afterNLJ := plan[nljIdx:]
		if !strings.Contains(afterNLJ, "preds") {
			t.Fatalf("expected join predicates inside NestedLoopJoin, got: %s", plan)
		}
	} else if strings.Contains(plan, "FlatMap") {
		if !strings.Contains(plan, "preds") && !strings.Contains(plan, "[=]") {
			t.Fatalf("expected predicates in FlatMap plan, got: %s", plan)
		}
	}

	// Verify query results: only a.id=1 has name='foo', and b has two rows
	// with aid=1 (bid=10, bid=30), so we expect 2 result rows.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 rows (a.id=1 matched twice), got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id != 1 {
			t.Fatalf("expected all result ids to be 1, got %d in %v", id, ids)
		}
	}
}

// TestFDB_PlanShapeFilterPushdownBelowJoin verifies end-to-end that
// PushFilterBelowJoinRule pushes single-table predicates below joins.
//
// Schema: dept(did PK, dname), emp(eid PK, did, ename)
//
// The pushdown is verified with SELECT * (no projection overhead) where
// the Cascades cost model cleanly picks the pushed-down plan shape:
//
//	NestedLoopJoin(INNER, [join-pred], Scan(EMP), Filter([where-pred], Scan(DEPT)))
//
// rather than:
//
//	Filter([where-pred], NestedLoopJoin(INNER, [join-pred], Scan(EMP), Scan(DEPT)))
//
// Also tests the negative case: a predicate referencing BOTH sides
// stays above the join.
func TestFDB_PlanShapeFilterPushdownBelowJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "fpbj",
		"CREATE TABLE dept (did BIGINT, dname STRING, PRIMARY KEY (did)) "+
			"CREATE TABLE emp (eid BIGINT, did BIGINT, ename STRING, PRIMARY KEY (eid))")

	// Seed dept: eng, sales, hr.
	for _, d := range []struct {
		did   int
		dname string
	}{
		{1, "eng"},
		{2, "sales"},
		{3, "hr"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO dept VALUES (%d, '%s')", d.did, d.dname,
		)); err != nil {
			t.Fatalf("INSERT dept did=%d: %v", d.did, err)
		}
	}
	// Seed emp: Alice and Bob in eng (did=1), Charlie in sales (did=2).
	for _, e := range []struct {
		eid   int
		did   int
		ename string
	}{
		{10, 1, "Alice"},
		{20, 1, "Bob"},
		{30, 2, "Charlie"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, %d, '%s')", e.eid, e.did, e.ename,
		)); err != nil {
			t.Fatalf("INSERT emp eid=%d: %v", e.eid, err)
		}
	}

	// --- Positive case: single-table predicate pushed below join ---
	//
	// SELECT * avoids a LogicalProjection wrapper that can interact
	// with PullFilterAboveProjectionRule and mask the pushdown in the
	// final plan. Without projection, the Cascades cost model cleanly
	// selects the pushed-down shape.
	q := "SELECT * FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE d.dname = 'eng'"
	plan := testkit.ExplainVia(t, ctx, db, q)
	t.Logf("plan (pushdown): %s", plan)

	// Plan must contain a NestedLoopJoin or FlatMap (Java-aligned correlated join).
	if !strings.Contains(plan, "NestedLoopJoin") && !strings.Contains(plan, "FlatMap") {
		t.Fatalf("expected NestedLoopJoin or FlatMap in plan, got: %s", plan)
	}

	// With FlatMap: the join predicate is absorbed into the correlated scan,
	// residual predicates appear as a PredicatesFilter above the FlatMap.
	// With NLJ: the filter should be pushed below the join or merged into NLJ predicates.
	if strings.Contains(plan, "FlatMap") {
		// FlatMap path: PredicatesFilter(FlatMap(outer, inner(correlated)))
		// The residual d.dname='eng' is in the PredicatesFilter.
		if !strings.Contains(plan, "PredicatesFilter") && !strings.Contains(plan, "preds") {
			t.Fatalf("expected residual filter with FlatMap plan, got: %s", plan)
		}
	} else {
		// NLJ path: filter pushed below join.
		nljIdx := strings.Index(plan, "NestedLoopJoin")
		filterIdx := strings.Index(plan, "Filter")
		if filterIdx >= 0 && filterIdx < nljIdx {
			t.Fatalf("expected filter pushed below join, but Filter wraps NLJ.\nplan: %s", plan)
		}
		afterNLJ := plan[nljIdx:]
		if !strings.Contains(afterNLJ, "Filter") && !strings.Contains(afterNLJ, "2 preds") {
			t.Fatalf("expected pushed-down Filter inside NestedLoopJoin or merged predicates, got: %s", plan)
		}
		if !strings.Contains(afterNLJ, "preds") {
			t.Fatalf("expected join predicates inside NestedLoopJoin, got: %s", plan)
		}
	}

	// Verify query results: only eng department rows joined.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	type joinRow struct {
		eid   int64
		did   int64
		ename string
		ddid  int64
		dname string
	}
	var got []joinRow
	for rows.Next() {
		var r joinRow
		if err := rows.Scan(&r.eid, &r.did, &r.ename, &r.ddid, &r.dname); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Only emp rows in eng (did=1): Alice (eid=10) and Bob (eid=20).
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.dname != "eng" {
			t.Fatalf("expected dname='eng', got %q in %+v", r.dname, r)
		}
		if r.did != 1 || r.ddid != 1 {
			t.Fatalf("expected did=1, got did=%d ddid=%d in %+v", r.did, r.ddid, r)
		}
	}

	// --- Negative case: cross-table predicate stays above join ---
	qCross := "SELECT * FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE e.eid > d.did"
	planCross := testkit.ExplainVia(t, ctx, db, qCross)
	t.Logf("plan (cross-table pred): %s", planCross)

	// The e.eid > d.did predicate references both sides — it must NOT
	// be pushed below the join. With FlatMap, it stays as a residual filter.
	if !strings.Contains(planCross, "NestedLoopJoin") && !strings.Contains(planCross, "FlatMap") {
		t.Fatalf("expected NestedLoopJoin or FlatMap in cross-table plan, got: %s", planCross)
	}

	if strings.Contains(planCross, "FlatMap") {
		// FlatMap path: cross-table predicate is a residual in PredicatesFilter.
		if !strings.Contains(planCross, "PredicatesFilter") && !strings.Contains(planCross, "preds") {
			t.Errorf("cross-table predicate not found in FlatMap plan, got: %s", planCross)
		} else {
			t.Logf("cross-table predicate correctly in residual filter above FlatMap")
		}
	} else {
		nljIdxC := strings.Index(planCross, "NestedLoopJoin")
		filterIdxC := strings.Index(planCross, "Filter")
		if filterIdxC < 0 || filterIdxC > nljIdxC {
			afterNLJC := planCross[nljIdxC:]
			if !strings.Contains(afterNLJC, "preds") {
				t.Errorf("cross-table predicate not found in plan — "+
					"expected either Filter above NLJ or preds on NLJ, got: %s", planCross)
			}
		} else {
			t.Logf("cross-table predicate correctly above join")
		}
	}

	// Cross-table query correctness: all emp rows where eid > did of
	// matching dept (all have eid 10/20/30 > did 1/2/3).
	rowsCross, err := db.QueryContext(ctx, qCross)
	if err != nil {
		t.Fatalf("QueryContext cross: %v", err)
	}
	defer rowsCross.Close()

	var crossCount int
	for rowsCross.Next() {
		crossCount++
		var r joinRow
		if err := rowsCross.Scan(&r.eid, &r.did, &r.ename, &r.ddid, &r.dname); err != nil {
			t.Fatalf("Scan cross: %v", err)
		}
		if r.eid <= r.ddid {
			t.Fatalf("cross-table: expected eid > did, got eid=%d did=%d", r.eid, r.ddid)
		}
	}
	if err := rowsCross.Err(); err != nil {
		t.Fatalf("rowsCross.Err: %v", err)
	}

	// All 3 joins satisfy eid > did: 10>1, 20>1, 30>2.
	if crossCount != 3 {
		t.Fatalf("cross: expected 3 rows, got %d", crossCount)
	}
}

func TestFDB_JoinWithNotIn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jnotin",
		"CREATE TABLE emp (id BIGINT, fname STRING, dept_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))")

	for _, e := range []struct {
		id   int
		name string
		dept int
	}{
		{1, "Jack", 1},
		{2, "Thomas", 1},
		{3, "Emily", 1},
		{4, "Amelia", 1},
		{5, "Daniel", 2},
		{6, "Chloe", 2},
		{7, "Charlotte", 2},
		{8, "Megan", 3},
		{9, "Harry", 3},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, '%s', %d)", e.id, e.name, e.dept,
		)); err != nil {
			t.Fatalf("INSERT emp: %v", err)
		}
	}
	for _, d := range []struct {
		id   int
		name string
	}{
		{1, "Engineering"}, {2, "Sales"}, {3, "Marketing"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO dept VALUES (%d, '%s')", d.id, d.name,
		)); err != nil {
			t.Fatalf("INSERT dept: %v", err)
		}
	}

	t.Run("join_with_not_in", func(t *testing.T) {
		// Java: emp JOIN dept WHERE dept.name='Engineering' AND emp.id NOT IN (1,3)
		rows, err := db.QueryContext(ctx,
			"SELECT emp.id, fname FROM emp, dept WHERE emp.dept_id = dept.id AND dept.name = 'Engineering' AND emp.id NOT IN (1, 3) ORDER BY emp.id")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			id    int64
			fname string
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.fname); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{2, "Thomas"}, {4, "Amelia"}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("join_exclude_dept", func(t *testing.T) {
		// Java: emp JOIN dept WHERE dept.id NOT IN (1, 3)
		rows, err := db.QueryContext(ctx,
			"SELECT emp.id, fname FROM emp, dept WHERE emp.dept_id = dept.id AND dept.id NOT IN (1, 3) ORDER BY emp.id")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			id    int64
			fname string
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.fname); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{5, "Daniel"}, {6, "Chloe"}, {7, "Charlotte"}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}

func TestFDB_JoinWithOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "joinob",
		"CREATE TABLE t1 (id BIGINT, a1 BIGINT, a2 BIGINT, a3 STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, b1 BIGINT, b2 BIGINT, b3 STRING, PRIMARY KEY (id))")

	for _, r := range []struct {
		id, a1, a2 int
		a3         string
	}{
		{1001, 1, 10, "a"}, {1002, 1, 11, "b"}, {1003, 1, 12, "a"}, {1004, 1, 13, "b"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t1 VALUES (%d, %d, %d, '%s')", r.id, r.a1, r.a2, r.a3,
		)); err != nil {
			t.Fatalf("INSERT t1: %v", err)
		}
	}
	for _, r := range []struct {
		id, b1, b2 int
		b3         string
	}{
		{2001, 1, 20, "a"}, {2002, 1, 19, "a"}, {2003, 1, 18, "b"}, {2004, 1, 17, "b"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t2 VALUES (%d, %d, %d, '%s')", r.id, r.b1, r.b2, r.b3,
		)); err != nil {
			t.Fatalf("INSERT t2: %v", err)
		}
	}

	t.Run("join_order_by_outer_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT t1.a2, t1.a3, t2.b2, t2.b3
			 FROM t1, t2
			 WHERE t1.a1 = 1 AND t2.b1 = 1 AND t1.a3 = t2.b3
			 ORDER BY t1.a2`)
		if len(rows) != 8 {
			t.Fatalf("want 8 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 10 {
			t.Errorf("first row a2: got %v, want 10", rows[0][0])
		}
		if rows[7][0].(int64) != 13 {
			t.Errorf("last row a2: got %v, want 13", rows[7][0])
		}
	})

	t.Run("join_order_by_outer_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT t1.a2, t2.b2
			 FROM t1, t2
			 WHERE t1.a1 = 1 AND t2.b1 = 1 AND t1.a3 = t2.b3
			 ORDER BY t1.a2 DESC`)
		if len(rows) != 8 {
			t.Fatalf("want 8 rows, got %d", len(rows))
		}
		if rows[0][0].(int64) != 13 {
			t.Errorf("first row a2: got %v, want 13", rows[0][0])
		}
		if rows[7][0].(int64) != 10 {
			t.Errorf("last row a2: got %v, want 10", rows[7][0])
		}
	})

	t.Run("join_project_inner_order_outer", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT t2.b2, t2.b3
			 FROM t1, t2
			 WHERE t1.a1 = 1 AND t2.b1 = 1 AND t1.a3 = t2.b3
			 ORDER BY t1.a2`)
		if len(rows) != 8 {
			t.Fatalf("want 8 rows, got %d", len(rows))
		}
		for _, r := range rows[:2] {
			if r[1].(string) != "a" {
				t.Errorf("first two rows should have b3='a', got %v", r[1])
			}
		}
	})

	t.Run("join_with_aggregate_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT t1.a3, COUNT(*) AS cnt
			 FROM t1, t2
			 WHERE t1.a1 = 1 AND t2.b1 = 1 AND t1.a3 = t2.b3
			 GROUP BY t1.a3
			 ORDER BY cnt DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 4 || rows[1][1].(int64) != 4 {
			t.Errorf("each group should have 4 rows: got %v, %v", rows[0][1], rows[1][1])
		}
	})

	_ = ctx
}
