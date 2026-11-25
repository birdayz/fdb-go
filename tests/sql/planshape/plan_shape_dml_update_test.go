package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_UpdateExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "updexpr1",
		"CREATE TABLE items (id BIGINT, qty BIGINT, price BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO items VALUES (1, 10, 100)",
		"INSERT INTO items VALUES (2, 20, 200)",
		"INSERT INTO items VALUES (3, 30, 300)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("update_set_arithmetic", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE items SET qty = qty + 5 WHERE id = 1")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1 affected, got %d", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT qty FROM items WHERE id = 1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 15 {
			t.Fatalf("want qty=15, got %v", rows)
		}
	})

	t.Run("update_multiple_columns", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE items SET qty = qty * 2, price = price - 10 WHERE id = 2")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT qty, price FROM items WHERE id = 2")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 40 || toInt64(rows[0][1]) != 190 {
			t.Errorf("want (40, 190), got (%v, %v)", rows[0][0], rows[0][1])
		}
	})

	t.Run("update_all_rows", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE items SET price = price + 1")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 affected, got %d", n)
		}
	})

	t.Run("update_no_match", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE items SET qty = 0 WHERE id = 999")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("want 0 affected, got %d", n)
		}
	})
}

// TestFDB_UpdateWithExpressions — UPDATE with arithmetic and conditional expressions
func TestFDB_UpdateWithExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "updex", "CREATE TABLE upd_t(id BIGINT, val BIGINT, status STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO upd_t VALUES
		(1, 100, 'active'), (2, 200, 'active'), (3, 300, 'inactive'), (4, 400, 'active'), (5, 500, 'inactive')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_arithmetic", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE upd_t SET val = val + 50 WHERE status = 'active'"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM upd_t WHERE status = 'active' ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 active, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 150 {
			t.Errorf("id=1: 100+50=150, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 250 {
			t.Errorf("id=2: 200+50=250, got %v", rows[1][1])
		}
	})

	t.Run("update_with_case", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE upd_t SET status = CASE WHEN val > 300 THEN 'premium' ELSE status END"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, status FROM upd_t WHERE status = 'premium' ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 premium (val>300: id=4(450), id=5(500)), got %d: %v", len(rows), rows)
		}
	})

	t.Run("update_multiply", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE upd_t SET val = val * 2 WHERE id = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM upd_t WHERE id = 1")
		if toInt64(rows[0][0]) != 300 {
			t.Errorf("id=1: 150*2=300, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateMultiColumn — UPDATE setting multiple columns at once
func TestFDB_UpdateMultiColumn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "updmc", "CREATE TABLE umc_t(id BIGINT, a BIGINT, b STRING, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO umc_t VALUES (1, 10, 'old', 100), (2, 20, 'old', 200)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_two_columns", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE umc_t SET a = 99, b = 'new' WHERE id = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT a, b, c FROM umc_t WHERE id = 1")
		if toInt64(rows[0][0]) != 99 {
			t.Errorf("a should be 99, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[0][1]) != "new" {
			t.Errorf("b should be 'new', got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 100 {
			t.Errorf("c should be unchanged (100), got %v", rows[0][2])
		}
	})

	t.Run("update_all_rows", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE umc_t SET c = c + 1000")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 updated, got %d", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT c FROM umc_t ORDER BY id")
		if toInt64(rows[0][0]) != 1100 {
			t.Errorf("id=1: c=100+1000=1100, got %v", rows[0][0])
		}
		if toInt64(rows[1][0]) != 1200 {
			t.Errorf("id=2: c=200+1000=1200, got %v", rows[1][0])
		}
	})
}

// TestFDB_UpdateWithWhereAndVerify — UPDATE with various WHERE and verify results
func TestFDB_UpdateWithWhereAndVerify(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uwv", "CREATE TABLE uwv_t(id BIGINT, status STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO uwv_t VALUES
		(1, 'pending', 10), (2, 'active', 20), (3, 'pending', 30),
		(4, 'active', 40), (5, 'done', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_by_status", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE uwv_t SET score = score + 100 WHERE status = 'pending'")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 updated, got %d", n)
		}
	})

	t.Run("verify_update", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, score FROM uwv_t WHERE status = 'pending' ORDER BY id")
		if toInt64(rows[0][1]) != 110 {
			t.Errorf("id=1: 10+100=110, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 130 {
			t.Errorf("id=3: 30+100=130, got %v", rows[1][1])
		}
	})

	t.Run("update_status_and_score", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE uwv_t SET status = 'done', score = 0 WHERE status = 'pending'"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM uwv_t WHERE status = 'done'")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 done (2 former pending + 1 original), got %v", rows[0][0])
		}
	})

	t.Run("aggregate_after_updates", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT status, COUNT(*), SUM(score) FROM uwv_t GROUP BY status ORDER BY status")
		t.Logf("final state: %v", rows)
		if len(rows) < 2 {
			t.Fatalf("want at least 2 groups, got %d", len(rows))
		}
	})
}

// TestFDB_UpdateSetArithmeticWithIndex — UPDATE arithmetic on indexed column
func TestFDB_UpdateSetArithmeticWithIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "usawi",
		"CREATE TABLE usai_t(id BIGINT, balance BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX sum_balance AS SELECT SUM(balance) FROM usai_t")
	if _, err := db.ExecContext(ctx, "INSERT INTO usai_t VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("initial_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(balance) FROM usai_t")
		if toInt64(rows[0][0]) != 600 {
			t.Errorf("initial SUM should be 600, got %v", rows[0][0])
		}
	})

	t.Run("update_add_50", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE usai_t SET balance = balance + 50 WHERE id = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT SUM(balance) FROM usai_t")
		if toInt64(rows[0][0]) != 650 {
			t.Errorf("after +50: SUM should be 650, got %v", rows[0][0])
		}
	})

	t.Run("update_subtract", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE usai_t SET balance = balance - 100 WHERE id = 3"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT balance FROM usai_t WHERE id = 3")
		if toInt64(rows[0][0]) != 200 {
			t.Errorf("id=3: 300-100=200, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateWithArithmeticAndWhere — UPDATE SET with arithmetic and WHERE
func TestFDB_UpdateWithArithmeticAndWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uwaw", "CREATE TABLE uwaw_t(id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO uwaw_t VALUES (1, 10, 5), (2, 20, 3), (3, 30, 7)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("double_price_where_qty_gt_4", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE uwaw_t SET price = price * 2 WHERE qty > 4")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 updated (qty=5,7), got %d", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, price FROM uwaw_t ORDER BY id")
		if toInt64(rows[0][1]) != 20 {
			t.Errorf("id=1: 10*2=20, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 20 {
			t.Errorf("id=2: unchanged 20, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 60 {
			t.Errorf("id=3: 30*2=60, got %v", rows[2][1])
		}
	})

	t.Run("verify_sum_after_update", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(price), SUM(price * qty) FROM uwaw_t")
		if toInt64(rows[0][0]) != 100 {
			t.Errorf("SUM(price) = 20+20+60 = 100, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateSetToExpression — UPDATE SET to computed expression
func TestFDB_UpdateSetToExpression(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uste", "CREATE TABLE uste_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO uste_t VALUES (1, 10, 20, 0), (2, 30, 40, 0)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("set_c_to_a_plus_b", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE uste_t SET c = a + b"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, c FROM uste_t ORDER BY id")
		if toInt64(rows[0][1]) != 30 {
			t.Errorf("id=1: c=10+20=30, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 70 {
			t.Errorf("id=2: c=30+40=70, got %v", rows[1][1])
		}
	})

	t.Run("verify_sum_c", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(c) FROM uste_t")
		if toInt64(rows[0][0]) != 100 {
			t.Errorf("SUM(c) = 30+70 = 100, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateConditionalAndVerifyAggregate — UPDATE with CASE, verify via aggregate
func TestFDB_UpdateConditionalAndVerifyAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ucva", "CREATE TABLE ucva_t(id BIGINT, score BIGINT, grade STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ucva_t VALUES
		(1, 95, ''), (2, 85, ''), (3, 75, ''), (4, 65, ''), (5, 55, '')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_grade_via_case", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `
			UPDATE ucva_t SET grade = CASE
				WHEN score >= 90 THEN 'A'
				WHEN score >= 80 THEN 'B'
				WHEN score >= 70 THEN 'C'
				ELSE 'F'
			END
		`); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
	})

	t.Run("verify_grade_counts", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grade, COUNT(*) FROM ucva_t GROUP BY grade ORDER BY grade")
		if len(rows) != 4 {
			t.Fatalf("want 4 grades, got %d: %v", len(rows), rows)
		}
		t.Logf("grades: %v", rows)
	})
}

// TestFDB_UpdateWithCoalesce — UPDATE SET using COALESCE
func TestFDB_UpdateWithCoalesce(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uwcoal", "CREATE TABLE uwcoal_t(id BIGINT, val BIGINT, backup BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO uwcoal_t VALUES (1, NULL, 99), (2, 50, 99), (3, NULL, NULL)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_coalesce", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE uwcoal_t SET val = COALESCE(val, backup, 0)"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM uwcoal_t ORDER BY id")
		if toInt64(rows[0][1]) != 99 {
			t.Errorf("id=1: COALESCE(NULL, 99, 0) = 99, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 50 {
			t.Errorf("id=2: COALESCE(50, 99, 0) = 50, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 0 {
			t.Errorf("id=3: COALESCE(NULL, NULL, 0) = 0, got %v", rows[2][1])
		}
	})
}

// TestFDB_UpdateConditionalArithmetic — UPDATE with CASE and arithmetic in SET
func TestFDB_UpdateConditionalArithmetic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uca",
		"CREATE TABLE uca_t(id BIGINT, price BIGINT, status STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO uca_t VALUES (1,100,'active'),(2,200,'inactive'),(3,50,'active'),(4,300,'inactive')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("double_active_prices", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE uca_t SET price = price * 2 WHERE status = 'active'")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 rows affected, got %d", n)
		}

		rows := testkit.CollectRows(t, db, "SELECT id, price FROM uca_t WHERE status = 'active' ORDER BY id")
		if toInt64(rows[0][1]) != 200 || toInt64(rows[1][1]) != 100 {
			t.Errorf("want [200 100], got [%v %v]", rows[0][1], rows[1][1])
		}
	})

	t.Run("inactive_unchanged", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, price FROM uca_t WHERE status = 'inactive' ORDER BY id")
		if toInt64(rows[0][1]) != 200 || toInt64(rows[1][1]) != 300 {
			t.Errorf("inactive should be unchanged: got [%v %v]", rows[0][1], rows[1][1])
		}
	})
}

// TestFDB_UpdateCaseWhenTier — UPDATE SET col = CASE WHEN ... tiered classification
func TestFDB_UpdateCaseWhenTier(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ucwt",
		"CREATE TABLE ucwt_t(id BIGINT, score BIGINT, tier STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ucwt_t VALUES (1,95,'?'),(2,75,'?'),(3,55,'?'),(4,35,'?')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("classify_tiers", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
			UPDATE ucwt_t SET tier = CASE
				WHEN score >= 90 THEN 'A'
				WHEN score >= 70 THEN 'B'
				WHEN score >= 50 THEN 'C'
				ELSE 'D'
			END
		`)
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}

		rows := testkit.CollectRows(t, db, "SELECT id, tier FROM ucwt_t ORDER BY id")
		want := []string{"A", "B", "C", "D"}
		for i, w := range want {
			if fmt.Sprintf("%v", rows[i][1]) != w {
				t.Errorf("id %d: want tier %s, got %v", i+1, w, rows[i][1])
			}
		}
	})
}

// TestFDB_UpdateMultipleColumns — UPDATE SET on multiple columns at once
func TestFDB_UpdateMultipleColumns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "umc",
		"CREATE TABLE umc_t(id BIGINT, name STRING, score BIGINT, active BOOLEAN, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO umc_t VALUES (1,'alice',80,true),(2,'bob',90,true),(3,'carol',70,false)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_score_and_active", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE umc_t SET score = score + 5, active = false WHERE id = 1")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1 affected, got %d", n)
		}

		rows := testkit.CollectRows(t, db, "SELECT score, active FROM umc_t WHERE id = 1")
		if toInt64(rows[0][0]) != 85 {
			t.Errorf("score: want 85, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[0][1]) != "false" {
			t.Errorf("active: want false, got %v", rows[0][1])
		}
	})

	t.Run("others_unchanged", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, score FROM umc_t WHERE id > 1 ORDER BY id")
		if toInt64(rows[0][1]) != 90 || toInt64(rows[1][1]) != 70 {
			t.Errorf("other rows changed: got [%v %v]", rows[0][1], rows[1][1])
		}
	})
}

// TestFDB_UpdateWhereArithmetic — UPDATE with arithmetic condition in WHERE
func TestFDB_UpdateWhereArithmetic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uwa",
		"CREATE TABLE uwa_t(id BIGINT, qty BIGINT, price BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO uwa_t VALUES (1,5,20),(2,10,5),(3,3,100),(4,8,15)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_high_value_items", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE uwa_t SET price = price + 10 WHERE qty * price > 100")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		// qty*price: 100, 50, 300, 120 → >100: id=3(300), id=4(120)
		if n != 2 {
			t.Errorf("want 2 affected, got %d", n)
		}
	})

	t.Run("verify_prices", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, price FROM uwa_t ORDER BY id")
		wantPrice := []int64{20, 5, 110, 25}
		for i, w := range wantPrice {
			if toInt64(rows[i][1]) != w {
				t.Errorf("id %d: want price %d, got %v", i+1, w, rows[i][1])
			}
		}
	})
}

// TestFDB_UpdateSetToNull — UPDATE setting column to NULL
func TestFDB_UpdateSetToNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ustn",
		"CREATE TABLE ustn_t(id BIGINT, val BIGINT, label STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ustn_t VALUES (1,10,'x'),(2,20,'y'),(3,30,'z')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("set_val_null", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE ustn_t SET val = null WHERE id = 2")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM ustn_t WHERE id = 2")
		if rows[0][0] != nil {
			t.Errorf("want NULL, got %v", rows[0][0])
		}
	})

	t.Run("count_non_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(val) FROM ustn_t")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 non-null, got %v", rows[0][0])
		}
	})
}

// TestFDB_UpdateArithmeticAllRows — UPDATE all rows with arithmetic
func TestFDB_UpdateArithmeticAllRows(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uaar",
		"CREATE TABLE uaar_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO uaar_t VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("double_all", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE uaar_t SET val = val * 2")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 affected, got %d", n)
		}
	})

	t.Run("verify_doubled", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM uaar_t")
		// original sum=60, doubled=120
		if toInt64(rows[0][0]) != 120 {
			t.Errorf("want 120, got %v", rows[0][0])
		}
	})

	t.Run("add_5_to_all", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE uaar_t SET val = val + 5")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM uaar_t ORDER BY id")
		// 20+5=25, 40+5=45, 60+5=65
		want := []int64{25, 45, 65}
		for i, w := range want {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}
