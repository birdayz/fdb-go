package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_JoinWithCaseWhen — CASE WHEN expression in JOIN query
func TestFDB_JoinWithCaseWhen(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwcw",
		"CREATE TABLE jwcw_orders(id BIGINT, amount BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jwcw_customers(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwcw_orders VALUES (1, 50), (2, 150), (3, 500)"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwcw_customers VALUES (1, 'alice'), (2, 'bob'), (3, 'charlie')"); err != nil {
		t.Fatalf("INSERT customers: %v", err)
	}

	t.Run("case_when_in_join_select", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT c.name,
				CASE WHEN o.amount >= 200 THEN 'premium' ELSE 'standard' END AS tier
			FROM jwcw_customers c
			JOIN jwcw_orders o ON c.id = o.id
			ORDER BY c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "standard" {
			t.Errorf("alice(50): should be standard, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[2][1]) != "premium" {
			t.Errorf("charlie(500): should be premium, got %v", rows[2][1])
		}
	})
}

// TestFDB_MultiTableInsertAndJoin — insert into multiple tables then join
func TestFDB_MultiTableInsertAndJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mtij",
		"CREATE TABLE mt_users(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE mt_posts(id BIGINT, user_id BIGINT, title STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO mt_users VALUES (1, 'alice'), (2, 'bob')"); err != nil {
		t.Fatalf("INSERT users: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mt_posts VALUES
		(10, 1, 'hello'), (20, 1, 'world'), (30, 2, 'test')
	`); err != nil {
		t.Fatalf("INSERT posts: %v", err)
	}

	t.Run("join_count_posts_per_user", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT u.name, COUNT(*) AS post_count
			FROM mt_users u JOIN mt_posts p ON u.id = p.user_id
			GROUP BY u.name ORDER BY u.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 2 {
			t.Errorf("alice: want 2 posts, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "bob" || toInt64(rows[1][1]) != 1 {
			t.Errorf("bob: want 1 post, got %v %v", rows[1][0], rows[1][1])
		}
	})

	t.Run("left_join_users_without_posts", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO mt_users VALUES (3, 'charlie')"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, `
			SELECT u.name FROM mt_users u
			LEFT JOIN mt_posts p ON u.id = p.user_id
			WHERE p.id IS NULL
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "charlie" {
			t.Errorf("want charlie (no posts), got %v", rows)
		}
	})
}

// TestFDB_JoinWithCoalesceAndCase — JOIN with COALESCE and CASE on joined columns
func TestFDB_JoinWithCoalesceAndCase(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwcc",
		"CREATE TABLE jwcc_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jwcc_b(id BIGINT, aid BIGINT, note STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwcc_a VALUES (1, 100), (2, 200), (3, NULL)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwcc_b VALUES (10, 1, 'x'), (20, 3, 'y')"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("left_join_coalesce_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id, COALESCE(a.val, 0), b.note
			FROM jwcc_a a LEFT JOIN jwcc_b b ON a.id = b.aid
			ORDER BY a.id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("id=3: COALESCE(NULL,0)=0, got %v", rows[2][1])
		}
	})

	t.Run("join_case_on_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id,
				CASE WHEN a.val IS NULL THEN 'unknown' ELSE 'known' END
			FROM jwcc_a a JOIN jwcc_b b ON a.id = b.aid
			ORDER BY a.id
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 matched, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "known" {
			t.Errorf("id=1 (val=100): should be known, got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][1]) != "unknown" {
			t.Errorf("id=3 (val=NULL): should be unknown, got %v", rows[1][1])
		}
	})
}

// TestFDB_DeleteWithJoinedFilter — DELETE rows based on conditions involving related data
func TestFDB_DeleteWithJoinedFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dwjf", "CREATE TABLE dwjf_t(id BIGINT, status STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO dwjf_t VALUES
		(1, 'keep', 100), (2, 'delete', 200), (3, 'keep', 300),
		(4, 'delete', 400), (5, 'keep', 500)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_by_status", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dwjf_t WHERE status = 'delete'")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 deleted, got %d", n)
		}
	})

	t.Run("verify_remaining", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM dwjf_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 remaining, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[1][0]) != 3 || toInt64(rows[2][0]) != 5 {
			t.Errorf("want ids 1,3,5 got %v,%v,%v", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	t.Run("aggregate_after_delete", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM dwjf_t")
		if toInt64(rows[0][0]) != 900 {
			t.Errorf("SUM after delete = 100+300+500 = 900, got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectWithMultipleJoins — query joining 3 tables
func TestFDB_SelectWithMultipleJoins(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "smj3",
		"CREATE TABLE mj_countries(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE mj_cities(id BIGINT, country_id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE mj_pop(city_id BIGINT, population BIGINT, PRIMARY KEY(city_id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO mj_countries VALUES (1, 'USA'), (2, 'UK')"); err != nil {
		t.Fatalf("INSERT countries: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO mj_cities VALUES (10, 1, 'NYC'), (20, 1, 'LA'), (30, 2, 'London')"); err != nil {
		t.Fatalf("INSERT cities: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO mj_pop VALUES (10, 8000000), (20, 4000000), (30, 9000000)"); err != nil {
		t.Fatalf("INSERT pop: %v", err)
	}

	t.Run("three_table_join_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT co.name, SUM(p.population)
			FROM mj_countries co
			JOIN mj_cities ci ON co.id = ci.country_id
			JOIN mj_pop p ON ci.id = p.city_id
			GROUP BY co.name ORDER BY co.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "UK" || toInt64(rows[0][1]) != 9000000 {
			t.Errorf("UK: want 9M, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "USA" || toInt64(rows[1][1]) != 12000000 {
			t.Errorf("USA: want 12M (8M+4M), got %v %v", rows[1][0], rows[1][1])
		}
	})
}

// TestFDB_JoinWithBetweenAndOrder — JOIN with BETWEEN filter and ORDER BY
func TestFDB_JoinWithBetweenAndOrder(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwbo",
		"CREATE TABLE jwbo_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jwbo_b(id BIGINT, aid BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwbo_a VALUES (1, 'alice'), (2, 'bob')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwbo_b VALUES (10, 1, 50), (20, 1, 80), (30, 2, 70), (40, 2, 90)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("join_between_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, b.score
			FROM jwbo_a a JOIN jwbo_b b ON a.id = b.aid
			WHERE b.score BETWEEN 60 AND 85
			ORDER BY b.score
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (70,80), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 70 {
			t.Errorf("first score should be 70, got %v", rows[0][1])
		}
	})
}

// TestFDB_JoinWithWhereAndCase — JOIN + WHERE + CASE WHEN in SELECT
func TestFDB_JoinWithWhereAndCase(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwwc",
		"CREATE TABLE jwwc_emp(id BIGINT, name STRING, dept_id BIGINT, salary BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jwwc_dept(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO jwwc_emp VALUES
		(1, 'alice', 1, 100), (2, 'bob', 1, 150), (3, 'charlie', 2, 80)
	`); err != nil {
		t.Fatalf("INSERT emp: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwwc_dept VALUES (1, 'eng'), (2, 'sales')"); err != nil {
		t.Fatalf("INSERT dept: %v", err)
	}

	t.Run("join_where_case", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT e.name, d.name,
				CASE WHEN e.salary >= 100 THEN 'senior' ELSE 'junior' END AS level
			FROM jwwc_emp e JOIN jwwc_dept d ON e.dept_id = d.id
			WHERE d.name = 'eng'
			ORDER BY e.salary DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 eng employees, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][2]) != "senior" {
			t.Errorf("bob(150): should be senior, got %v", rows[0][2])
		}
	})
}

// TestFDB_JoinWithInPredicate — JOIN with IN filter on joined column
func TestFDB_JoinWithInPredicate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwip",
		"CREATE TABLE jwip_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jwip_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwip_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwip_b VALUES (10, 1, 100), (20, 2, 200), (30, 3, 300)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("join_with_in_on_name", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name, b.val
			FROM jwip_a a JOIN jwip_b b ON a.id = b.aid
			WHERE a.name IN ('x', 'z')
			ORDER BY a.name
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "x" || toInt64(rows[0][1]) != 100 {
			t.Errorf("x: want val=100, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_JoinWithUpdateAndVerify — JOIN query, UPDATE, verify via JOIN again
func TestFDB_JoinWithUpdateAndVerify(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwuv",
		"CREATE TABLE jwuv_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jwuv_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jwuv_a VALUES (1, 'alice'), (2, 'bob')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jwuv_b VALUES (10, 1, 100), (20, 2, 200)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("initial_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a.name, b.val FROM jwuv_a a JOIN jwuv_b b ON a.id = b.aid ORDER BY a.name")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
	})

	t.Run("update_then_rejoin", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE jwuv_b SET val = val * 3 WHERE aid = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT a.name, b.val FROM jwuv_a a JOIN jwuv_b b ON a.id = b.aid ORDER BY a.name")
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("alice: 100*3=300, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 200 {
			t.Errorf("bob: unchanged 200, got %v", rows[1][1])
		}
	})
}

// TestFDB_JoinWithLeftAndInnerCompare — compare LEFT vs INNER JOIN results
func TestFDB_JoinWithLeftAndInnerCompare(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jlic",
		"CREATE TABLE jlic_a(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE jlic_b(id BIGINT, aid BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO jlic_a VALUES (1, 'x'), (2, 'y'), (3, 'z')"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jlic_b VALUES (10, 1, 100), (20, 2, 200)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("inner_join_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM jlic_a a JOIN jlic_b b ON a.id = b.aid")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("INNER JOIN: want 2 matched, got %v", rows[0][0])
		}
	})

	t.Run("left_join_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM jlic_a a LEFT JOIN jlic_b b ON a.id = b.aid")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("LEFT JOIN: want 3 (2 matched + 1 unmatched), got %v", rows[0][0])
		}
	})

	t.Run("left_minus_inner", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.name FROM jlic_a a LEFT JOIN jlic_b b ON a.id = b.aid
			WHERE b.id IS NULL
		`)
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "z" {
			t.Errorf("unmatched: want z, got %v", rows)
		}
	})
}

// TestFDB_CoalesceInJoin — COALESCE to provide defaults for NULL from LEFT JOIN
func TestFDB_CoalesceInJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cij",
		"CREATE TABLE cij_emp(id BIGINT, name STRING, dept_id BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE cij_dept(id BIGINT, dname STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO cij_emp VALUES (1,'alice',10),(2,'bob',20),(3,'carol',99)"); err != nil {
		t.Fatalf("INSERT emp: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO cij_dept VALUES (10,'eng'),(20,'sales')"); err != nil {
		t.Fatalf("INSERT dept: %v", err)
	}

	t.Run("coalesce_dept_name", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT e.name, COALESCE(d.dname, 'unassigned') FROM cij_emp e
			LEFT JOIN cij_dept d ON e.dept_id = d.id
			ORDER BY e.id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		wantDept := []string{"eng", "sales", "unassigned"}
		for i, w := range wantDept {
			got := fmt.Sprintf("%v", rows[i][1])
			if got != w {
				t.Errorf("row %d: want dept %s, got %s", i, w, got)
			}
		}
	})
}

// TestFDB_JoinOnCatAndVal — JOIN ON two columns (cat AND val)
func TestFDB_JoinOnCatAndVal(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jocv",
		"CREATE TABLE jocv_a(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jocv_b(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO jocv_a VALUES (1,'x',10),(2,'y',20),(3,'x',30)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO jocv_b VALUES (10,'x',10),(20,'y',25),(30,'x',30)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("join_on_cat_and_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id, b.id FROM jocv_a a
			JOIN jocv_b b ON a.cat = b.cat AND a.val = b.val
			ORDER BY a.id
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 matches, got %d: %v", len(rows), rows)
		}
		// a(1,x,10) matches b(10,x,10); a(3,x,30) matches b(30,x,30)
		if toInt64(rows[0][0]) != 1 || toInt64(rows[0][1]) != 10 {
			t.Errorf("row 0: want [1 10], got %v", rows[0])
		}
		if toInt64(rows[1][0]) != 3 || toInt64(rows[1][1]) != 30 {
			t.Errorf("row 1: want [3 30], got %v", rows[1])
		}
	})

	t.Run("join_on_cat_only_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM jocv_a a JOIN jocv_b b ON a.cat = b.cat
		`)
		// x-x: 2*2=4, y-y: 1*1=1 → total 5
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5, got %v", rows[0][0])
		}
	})
}

// TestFDB_JoinWithWhereOnBothTables — JOIN + WHERE filtering both tables
func TestFDB_JoinWithWhereOnBothTables(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jwwb",
		"CREATE TABLE jwwb_a(id BIGINT, status STRING, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jwwb_b(id BIGINT, aid BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO jwwb_a VALUES
		(1,'active',100),(2,'inactive',200),(3,'active',300)`); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jwwb_b VALUES
		(10,1,50),(11,1,80),(12,2,90),(13,3,30),(14,3,70)`); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("active_and_score_gt_60", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT a.id, b.score FROM jwwb_a a
			JOIN jwwb_b b ON a.id = b.aid
			WHERE a.status = 'active' AND b.score > 60
			ORDER BY b.score
		`)
		// active: ids 1,3; score>60: b(11,1,80), b(14,3,70) → 2 rows
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 70 || toInt64(rows[1][1]) != 80 {
			t.Errorf("want [70 80], got [%v %v]", rows[0][1], rows[1][1])
		}
	})

	t.Run("count_all_joined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM jwwb_a a JOIN jwwb_b b ON a.id = b.aid
		`)
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5, got %v", rows[0][0])
		}
	})
}
