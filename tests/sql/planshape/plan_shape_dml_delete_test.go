package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_UpdateDeleteWithExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "upddel",
		"CREATE TABLE inventory (id BIGINT, name STRING, qty BIGINT, price BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id         int
		name       string
		qty, price int
	}{
		{1, "Widget", 100, 10},
		{2, "Gadget", 50, 20},
		{3, "Doohickey", 200, 5},
		{4, "Thingamajig", 10, 100},
	} {
		db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO inventory VALUES (%d, '%s', %d, %d)", r.id, r.name, r.qty, r.price,
		))
	}

	t.Run("update_with_arithmetic", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE inventory SET qty = qty + 10 WHERE price > 15")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("rows affected: got %d, want 2 (Gadget, Thingamajig)", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT name, qty FROM inventory WHERE price > 15 ORDER BY name")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1].(int64) != 60 {
			t.Errorf("Gadget qty: got %v, want 60 (50+10)", rows[0][1])
		}
		if rows[1][1].(int64) != 20 {
			t.Errorf("Thingamajig qty: got %v, want 20 (10+10)", rows[1][1])
		}
	})

	t.Run("delete_with_between", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM inventory WHERE price BETWEEN 5 AND 15")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("rows affected: got %d, want 2 (Widget=10, Doohickey=5)", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM inventory")
		if rows[0][0].(int64) != 2 {
			t.Errorf("remaining: got %v, want 2", rows[0][0])
		}
	})

	_ = ctx
}

func TestFDB_MultiTableDeleteUpdate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "mtdel1",
		"CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, status STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO orders VALUES (1, 100, 50, 'pending')",
		"INSERT INTO orders VALUES (2, 100, 75, 'shipped')",
		"INSERT INTO orders VALUES (3, 200, 100, 'pending')",
		"INSERT INTO orders VALUES (4, 200, 200, 'delivered')",
		"INSERT INTO orders VALUES (5, 300, 150, 'pending')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("delete_with_in_list", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM orders WHERE id IN (1, 3)")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2, got %d", n)
		}
	})

	t.Run("verify_after_delete", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM orders ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 remaining, got %d", len(rows))
		}
		wantIDs := []int64{2, 4, 5}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})

	t.Run("update_with_case", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`UPDATE orders SET status = CASE
				WHEN amount > 100 THEN 'high'
				ELSE 'low'
			 END
			 WHERE status = 'pending'`)
		if err != nil {
			t.Fatalf("UPDATE CASE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, status FROM orders WHERE id = 5")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if rows[0][1].(string) != "high" {
			t.Errorf("want 'high', got %v", rows[0][1])
		}
	})

	t.Run("delete_between", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM orders WHERE amount BETWEEN 100 AND 200")
		if err != nil {
			t.Fatalf("DELETE BETWEEN: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 (ids 4,5), got %d", n)
		}
	})

	t.Run("verify_final_state", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, amount FROM orders ORDER BY id")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 2 || toInt64(rows[0][1]) != 75 {
			t.Errorf("want (2, 75), got (%v, %v)", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_UpdateDeleteReturnCount — UPDATE/DELETE affected row counts
func TestFDB_UpdateDeleteReturnCount(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "udrc", "CREATE TABLE cnt_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO cnt_t VALUES (1,10),(2,20),(3,30),(4,40),(5,50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_returns_count", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE cnt_t SET val = val + 1 WHERE val > 30")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 2 {
			t.Errorf("want 2 updated (40,50), got %d", n)
		}
	})

	t.Run("delete_returns_count", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM cnt_t WHERE val <= 20")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 2 {
			t.Errorf("want 2 deleted (10,20), got %d", n)
		}
	})

	t.Run("verify_remaining", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM cnt_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 remaining, got %d: %v", len(rows), rows)
		}
	})

	t.Run("update_no_match", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE cnt_t SET val = 999 WHERE id = 999")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 0 {
			t.Errorf("want 0 updated, got %d", n)
		}
	})

	t.Run("delete_no_match", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM cnt_t WHERE id = 999")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 0 {
			t.Errorf("want 0 deleted, got %d", n)
		}
	})
}

// TestFDB_DeleteWithComplexWhere — DELETE with various WHERE patterns
func TestFDB_DeleteWithComplexWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dcw", "CREATE TABLE dcw_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO dcw_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 30), (4, 'C', 40), (5, 'B', 50),
		(6, 'A', 60), (7, 'C', 70), (8, 'B', 80)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_with_in", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dcw_t WHERE cat IN ('C')")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 deleted (cat=C), got %d", n)
		}
	})

	t.Run("delete_with_between", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dcw_t WHERE val BETWEEN 25 AND 55")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 deleted (val 30,50), got %d", n)
		}
	})

	t.Run("verify_remaining", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, cat, val FROM dcw_t ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4 remaining, got %d: %v", len(rows), rows)
		}
	})

	t.Run("delete_with_and_or", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dcw_t WHERE cat = 'A' AND val > 50")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1 deleted (id=6, A, 60), got %d", n)
		}
	})
}

// TestFDB_DeleteWithMultipleConditions — DELETE with AND/OR/IN conditions
func TestFDB_DeleteWithMultipleConditions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dwmc2", "CREATE TABLE dwmc_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO dwmc_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 30), (4, 'C', 40),
		(5, 'B', 50), (6, 'A', 60), (7, 'C', 70)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_with_and", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dwmc_t WHERE cat = 'A' AND val < 35")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 (id=1:10, id=3:30), got %d", n)
		}
	})

	t.Run("verify_remaining", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM dwmc_t")
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5 remaining, got %v", rows[0][0])
		}
	})

	t.Run("delete_with_in", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dwmc_t WHERE cat IN ('B', 'C')")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 4 {
			t.Errorf("want 4 (B:2 + C:2), got %d", n)
		}
	})

	t.Run("only_a_remains", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM dwmc_t ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 6 {
			t.Errorf("only id=6 (A,60) should remain, got %v", rows)
		}
	})
}

// TestFDB_UpdateAndDeleteWithAggregate — UPDATE + DELETE then verify aggregates
func TestFDB_UpdateAndDeleteWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "udwa", "CREATE TABLE udwa_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO udwa_t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("update_then_sum", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE udwa_t SET val = val * 2 WHERE id <= 3"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM udwa_t")
		if toInt64(rows[0][0]) != 210 {
			t.Errorf("SUM after *2 for ids 1-3: 20+40+60+40+50 = 210, got %v", rows[0][0])
		}
	})

	t.Run("delete_then_count", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM udwa_t WHERE val > 45"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(val) FROM udwa_t")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT after delete val>45: want 3, got %v", rows[0][0])
		}
	})
}

// TestFDB_DeleteAllThenCount — DELETE all rows then verify COUNT=0
func TestFDB_DeleteAllThenCount(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "datc",
		"CREATE TABLE datc_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO datc_t VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_all", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM datc_t WHERE id >= 1")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 deleted, got %d", n)
		}
	})

	t.Run("count_is_zero", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM datc_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("want 0 after delete all, got %v", rows[0][0])
		}
	})

	t.Run("select_returns_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM datc_t")
		if len(rows) != 0 {
			t.Errorf("want 0 rows, got %d", len(rows))
		}
	})
}

// TestFDB_DeleteOldAndLowValue — DELETE with AND condition on status+val
func TestFDB_DeleteOldAndLowValue(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dolv",
		"CREATE TABLE dolv_t(id BIGINT, status STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO dolv_t VALUES
		(1,'old',10),(2,'new',20),(3,'old',30),(4,'new',40),(5,'old',5)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_old_and_low", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dolv_t WHERE status = 'old' AND val < 15")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		// old AND val<15: id=1(old,10), id=5(old,5) → 2 deleted
		if n != 2 {
			t.Errorf("want 2 deleted, got %d", n)
		}
	})

	t.Run("remaining_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM dolv_t")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 remaining, got %v", rows[0][0])
		}
	})

	t.Run("remaining_ids", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM dolv_t ORDER BY id")
		wantIds := []int64{2, 3, 4}
		for i, w := range wantIds {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}
