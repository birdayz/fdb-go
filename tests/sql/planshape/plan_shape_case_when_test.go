package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_CaseWhenWithInList(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "casein",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id, col1, col2 int
	}{
		{1, 10, 1},
		{2, 10, 2},
		{3, 10, 3},
		{4, 10, 4},
		{5, 10, 5},
		{6, 20, 6},
		{7, 20, 7},
		{8, 20, 8},
		{9, 20, 9},
		{10, 20, 10},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t1 VALUES (%d, %d, %d)", r.id, r.col1, r.col2,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	// Java standard-tests: CASE WHEN col1=10 THEN 100 WHEN col2 IN (6,7,8,9) THEN 200 ELSE 300 END
	rows, err := db.QueryContext(ctx,
		`SELECT id, CASE WHEN col1 = 10 THEN 100 WHEN col2 IN (6, 7, 8, 9) THEN 200 ELSE 300 END AS newcol
		FROM t1 ORDER BY id`)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	type row struct {
		id     int64
		newcol int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.newcol); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	want := []row{
		{1, 100},
		{2, 100},
		{3, 100},
		{4, 100},
		{5, 100},
		{6, 200},
		{7, 200},
		{8, 200},
		{9, 200},
		{10, 300},
	}
	if len(got) != len(want) {
		t.Fatalf("row count: got %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
		}
	}
}

func TestFDB_CaseWhenWithoutElseReturnsNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "casenoelse",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id, col1, col2 int
	}{
		{1, 10, 1},
		{2, 10, 2},
		{3, 20, 6},
		{4, 20, 7},
		{5, 20, 10},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t1 VALUES (%d, %d, %d)", r.id, r.col1, r.col2,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id, CASE WHEN col1 = 10 THEN 100
		              WHEN col2 IN (6, 7, 8, 9) THEN 200
		         END AS newcol
		FROM t1 ORDER BY id`)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	type row struct {
		id     int64
		newcol *int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.newcol); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	want := []row{
		{1, ptr(int64(100))},
		{2, ptr(int64(100))},
		{3, ptr(int64(200))},
		{4, ptr(int64(200))},
		{5, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("row count: got %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if w.newcol == nil {
			if got[i].newcol != nil {
				t.Errorf("row %d: got %v, want NULL", i, *got[i].newcol)
			}
		} else if got[i].newcol == nil {
			t.Errorf("row %d: got NULL, want %d", i, *w.newcol)
		} else if *got[i].newcol != *w.newcol {
			t.Errorf("row %d: got %d, want %d", i, *got[i].newcol, *w.newcol)
		}
	}
}

func TestFDB_CaseWhenInListCombined(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "casein2",
		"CREATE TABLE orders (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))")

	for _, o := range []struct {
		id, amt int
		s       string
	}{
		{1, 100, "new"},
		{2, 200, "processing"},
		{3, 300, "shipped"},
		{4, 400, "delivered"},
		{5, 50, "cancelled"},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO orders VALUES (%d, '%s', %d)", o.id, o.s, o.amt))
	}

	t.Run("case_in_list", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT id,
			        CASE WHEN status IN ('new', 'processing') THEN 'pending'
			             WHEN status IN ('shipped', 'delivered') THEN 'complete'
			             ELSE 'other'
			        END AS category,
			        amount
			 FROM orders ORDER BY id`)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if rows[0][1].(string) != "pending" {
			t.Errorf("id=1 (new): got %v, want pending", rows[0][1])
		}
		if rows[2][1].(string) != "complete" {
			t.Errorf("id=3 (shipped): got %v, want complete", rows[2][1])
		}
		if rows[4][1].(string) != "other" {
			t.Errorf("id=5 (cancelled): got %v, want other", rows[4][1])
		}
	})

	_ = ctx
}

// TestFDB_CaseWhenJavaPatterns — Java case-when.yamsql patterns
func TestFDB_CaseWhenJavaPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwjp", "CREATE TABLE cw_a(a1 BIGINT, a2 BIGINT, a3 BIGINT, PRIMARY KEY(a1))")
	if _, err := db.ExecContext(ctx, "INSERT INTO cw_a VALUES (1, 10, 10), (2, 11, 20), (3, 12, 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("case_when_comparison", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a3, CASE WHEN a3 > 15 THEN 'foo' ELSE 'bar' END FROM cw_a ORDER BY a1")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		want := []string{"bar", "foo", "foo"}
		for i, w := range want {
			got := fmt.Sprintf("%v", rows[i][1])
			if got != w {
				t.Errorf("row %d: want %s, got %s", i, w, got)
			}
		}
	})

	t.Run("update_with_case_when", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE cw_a SET a2 = CASE WHEN a1 = 1 THEN 4444 END"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT a1, a2 FROM cw_a ORDER BY a1")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 4444 {
			t.Errorf("a1=1: a2 should be 4444, got %v", rows[0][1])
		}
		if rows[1][1] != nil {
			t.Errorf("a1=2: a2 should be NULL (no ELSE), got %v", rows[1][1])
		}
		if rows[2][1] != nil {
			t.Errorf("a1=3: a2 should be NULL (no ELSE), got %v", rows[2][1])
		}
	})

	t.Run("update_with_case_is_null", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE cw_a SET a2 = CASE WHEN a2 IS NULL THEN 8888 ELSE 2222 END"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT a1, a2 FROM cw_a ORDER BY a1")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 2222 {
			t.Errorf("a1=1: was 4444 (not null) -> 2222, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 8888 {
			t.Errorf("a1=2: was NULL -> 8888, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 8888 {
			t.Errorf("a1=3: was NULL -> 8888, got %v", rows[2][1])
		}
	})

	t.Run("nested_case_when", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE cw_a SET a2 = CASE WHEN CASE WHEN a2 = 2222 THEN 8888 ELSE 2222 END > 4000 THEN 4444 ELSE 6666 END"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT a1, a2 FROM cw_a ORDER BY a1")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 4444 {
			t.Errorf("a1=1: inner=8888>4000->4444, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 6666 {
			t.Errorf("a1=2: inner=2222<4000->6666, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 6666 {
			t.Errorf("a1=3: inner=2222<4000->6666, got %v", rows[2][1])
		}
	})

	t.Run("case_when_in_select_with_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT
				CASE WHEN a2 = 4444 THEN 'high' ELSE 'low' END,
				COUNT(*)
			FROM cw_a
			GROUP BY CASE WHEN a2 = 4444 THEN 'high' ELSE 'low' END
		`)
		if len(rows) < 1 {
			t.Fatalf("want at least 1 row, got %d", len(rows))
		}
		t.Logf("CASE WHEN GROUP BY: %v", rows)
	})
}

// TestFDB_MultiBranchCaseWhen — standard-tests.yamsql multi-branch CASE with IN
func TestFDB_MultiBranchCaseWhen(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mbcw", "CREATE TABLE mbcw(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO mbcw VALUES
		(1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5),
		(6, 20, 6), (7, 20, 7), (8, 20, 8), (9, 20, 9), (10, 20, 10),
		(11, 20, 11), (12, 20, 12), (13, 20, 13)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("case_when_in_with_else", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN col1 = 10 THEN 100
			                WHEN col2 IN (6,7,8,9) THEN 200
			                ELSE 300 END AS newcol
			FROM mbcw ORDER BY id
		`)
		if len(rows) != 13 {
			t.Fatalf("want 13 rows, got %d", len(rows))
		}
		for i := 0; i < 5; i++ {
			if toInt64(rows[i][1]) != 100 {
				t.Errorf("id=%d: col1=10 → should be 100, got %v", i+1, rows[i][1])
			}
		}
		for i := 5; i < 9; i++ {
			if toInt64(rows[i][1]) != 200 {
				t.Errorf("id=%d: col2 IN (6-9) → should be 200, got %v", i+1, rows[i][1])
			}
		}
		for i := 9; i < 13; i++ {
			if toInt64(rows[i][1]) != 300 {
				t.Errorf("id=%d: else → should be 300, got %v", i+1, rows[i][1])
			}
		}
	})

	t.Run("case_when_in_no_else_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN col1 = 10 THEN 100
			                WHEN col2 IN (6,7,8,9) THEN 200 END AS newcol
			FROM mbcw ORDER BY id
		`)
		if len(rows) != 13 {
			t.Fatalf("want 13 rows, got %d", len(rows))
		}
		for i := 9; i < 13; i++ {
			if rows[i][1] != nil {
				t.Errorf("id=%d: no ELSE → should be NULL, got %v", i+1, rows[i][1])
			}
		}
	})
}

// TestFDB_CaseWhenWithNull — CASE WHEN NULL edge cases
func TestFDB_CaseWhenWithNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwn", "CREATE TABLE cwn_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO cwn_t VALUES (1, 10), (2, NULL), (3, 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("case_when_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN val IS NULL THEN 'missing' ELSE 'present' END
			FROM cwn_t ORDER BY id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[1][1]) != "missing" {
			t.Errorf("id=2 (NULL): want 'missing', got %v", rows[1][1])
		}
	})

	t.Run("case_no_else_returns_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN val > 20 THEN 'big' END
			FROM cwn_t ORDER BY id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if rows[0][1] != nil {
			t.Errorf("id=1 (val=10): no match, no ELSE → NULL, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[2][1]) != "big" {
			t.Errorf("id=3 (val=30): want 'big', got %v", rows[2][1])
		}
	})

	t.Run("coalesce_in_case", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN COALESCE(val, 0) > 5 THEN 'yes' ELSE 'no' END
			FROM cwn_t ORDER BY id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "yes" {
			t.Errorf("id=1 (val=10>5): want 'yes', got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][1]) != "no" {
			t.Errorf("id=2 (COALESCE(NULL,0)=0<5): want 'no', got %v", rows[1][1])
		}
	})
}

// TestFDB_CaseWhenInSelectProjection — CASE WHEN expressions in SELECT projection
func TestFDB_CaseWhenInSelectProjection(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwsp",
		"CREATE TABLE cwsp_t(id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO cwsp_t VALUES (1,95),(2,75),(3,55),(4,35),(5,85)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("grade_labels", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE
				WHEN score >= 90 THEN 'A'
				WHEN score >= 70 THEN 'B'
				WHEN score >= 50 THEN 'C'
				ELSE 'F'
			END FROM cwsp_t ORDER BY id
		`)
		want := []string{"A", "B", "C", "F", "B"}
		for i, w := range want {
			if fmt.Sprintf("%v", rows[i][1]) != w {
				t.Errorf("id %d: want %s, got %v", i+1, w, rows[i][1])
			}
		}
	})

	t.Run("count_passing", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM cwsp_t WHERE score >= 50
		`)
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4 passing (>=50), got %v", rows[0][0])
		}
	})
}
