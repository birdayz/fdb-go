package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_DeleteInsertCycles — transactional integrity of delete+insert patterns
func TestFDB_DeleteInsertCycles(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "delic", "CREATE TABLE cycle_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")

	t.Run("insert_verify_delete_verify", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO cycle_t VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM cycle_t")
		if toInt64(rows[0][0]) != 3 {
			t.Fatalf("after insert: want 3, got %v", rows[0][0])
		}

		if _, err := db.ExecContext(ctx, "DELETE FROM cycle_t WHERE val > 150"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows = testkit.CollectRows(t, db, "SELECT COUNT(*) FROM cycle_t")
		if toInt64(rows[0][0]) != 1 {
			t.Fatalf("after delete: want 1, got %v", rows[0][0])
		}

		if _, err := db.ExecContext(ctx, "INSERT INTO cycle_t VALUES (4, 400), (5, 500)"); err != nil {
			t.Fatalf("INSERT round 2: %v", err)
		}
		rows = testkit.CollectRows(t, db, "SELECT id, val FROM cycle_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (1,4,5), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("first should be id=1, got %v", rows[0][0])
		}
		if toInt64(rows[1][0]) != 4 {
			t.Errorf("second should be id=4, got %v", rows[1][0])
		}
	})

	t.Run("update_then_aggregate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE cycle_t SET val = val * 2"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT SUM(val), MIN(val), MAX(val) FROM cycle_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 200+800+1000 {
			t.Errorf("SUM should be 200+800+1000=2000, got %v", rows[0][0])
		}
	})

	t.Run("delete_all_then_aggregate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM cycle_t WHERE id >= 0"); err != nil {
			t.Fatalf("DELETE ALL: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM cycle_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("after delete all: COUNT should be 0, got %v", rows[0][0])
		}
		rows = testkit.CollectRows(t, db, "SELECT SUM(val) FROM cycle_t")
		if rows[0][0] != nil {
			t.Errorf("SUM on empty should be NULL, got %v", rows[0][0])
		}
	})
}

// TestFDB_DeleteAllThenInsert — full table delete then repopulate
func TestFDB_DeleteAllThenInsert(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dati", "CREATE TABLE dati_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dati_t VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_all", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM dati_t WHERE id > 0")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 deleted, got %d", n)
		}
	})

	t.Run("empty_aggregates", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(val) FROM dati_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("COUNT should be 0, got %v", rows[0][0])
		}
		if rows[0][1] != nil {
			t.Errorf("SUM should be NULL, got %v", rows[0][1])
		}
	})

	t.Run("repopulate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO dati_t VALUES (10, 1000), (20, 2000)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(val) FROM dati_t")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("COUNT should be 2, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3000 {
			t.Errorf("SUM should be 3000, got %v", rows[0][1])
		}
	})
}

// TestFDB_CombinedDMLWorkflow — INSERT + SELECT + UPDATE + DELETE + verify workflow
func TestFDB_CombinedDMLWorkflow(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cdml", "CREATE TABLE cdml_t(id BIGINT, name STRING, active BIGINT, PRIMARY KEY(id))")

	t.Run("full_lifecycle", func(t *testing.T) {
		// INSERT
		if _, err := db.ExecContext(ctx, "INSERT INTO cdml_t VALUES (1, 'alice', 1), (2, 'bob', 1), (3, 'charlie', 0)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		// SELECT verify
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM cdml_t")
		if toInt64(rows[0][0]) != 3 {
			t.Fatalf("after INSERT: want 3, got %v", rows[0][0])
		}

		// UPDATE
		if _, err := db.ExecContext(ctx, "UPDATE cdml_t SET active = 0 WHERE name = 'bob'"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows = testkit.CollectRows(t, db, "SELECT COUNT(*) FROM cdml_t WHERE active = 1")
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("after UPDATE: want 1 active, got %v", rows[0][0])
		}

		// DELETE
		res, err := db.ExecContext(ctx, "DELETE FROM cdml_t WHERE active = 0")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 deleted (bob+charlie inactive), got %d", n)
		}

		// Final verify
		rows = testkit.CollectRows(t, db, "SELECT name FROM cdml_t")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "alice" {
			t.Errorf("only alice should remain, got %v", rows)
		}
	})
}

// TestFDB_EndToEndWorkflow — complete e2e: CREATE + INSERT + SELECT + UPDATE + DELETE + aggregate verify
func TestFDB_EndToEndWorkflow(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "e2ew", "CREATE TABLE e2e_t(id BIGINT, name STRING, score BIGINT, PRIMARY KEY(id))")

	t.Run("full_e2e", func(t *testing.T) {
		// INSERT
		if _, err := db.ExecContext(ctx, `INSERT INTO e2e_t VALUES
			(1, 'alice', 90), (2, 'bob', 80), (3, 'charlie', 70),
			(4, 'david', 60), (5, 'eve', 95)
		`); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		// SELECT with ORDER BY
		rows := testkit.CollectRows(t, db, "SELECT name, score FROM e2e_t ORDER BY score DESC LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eve" {
			t.Errorf("top scorer should be eve, got %v", rows[0][0])
		}

		// UPDATE
		if _, err := db.ExecContext(ctx, "UPDATE e2e_t SET score = score + 10 WHERE score < 80"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}

		// DELETE
		res, err := db.ExecContext(ctx, "DELETE FROM e2e_t WHERE name = 'david'")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1 deleted, got %d", n)
		}

		// Aggregate verify
		rows = testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(score), MIN(score), MAX(score) FROM e2e_t")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("COUNT should be 4, got %v", rows[0][0])
		}
		if toInt64(rows[0][3]) != 95 {
			t.Errorf("MAX should still be 95 (eve), got %v", rows[0][3])
		}
	})
}

// TestFDB_DeleteAllAndRepopulate — DELETE all rows, repopulate, verify
func TestFDB_DeleteAllAndRepopulate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "darep", "CREATE TABLE darep_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO darep_t VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_all_verify_empty", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM darep_t WHERE id > 0"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM darep_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("want 0 after delete all, got %v", rows[0][0])
		}
	})

	t.Run("repopulate_verify", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO darep_t VALUES (10, 100), (20, 200)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*), SUM(val) FROM darep_t")
		if toInt64(rows[0][0]) != 2 || toInt64(rows[0][1]) != 300 {
			t.Errorf("want count=2 sum=300, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_InsertMultipleBatchesThenDelete — multi-batch insert then selective delete
func TestFDB_InsertMultipleBatchesThenDelete(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "imbd",
		"CREATE TABLE imbd_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO imbd_t VALUES (1,10),(2,20),(3,30),(4,40),(5,50)"); err != nil {
		t.Fatalf("INSERT batch 1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO imbd_t VALUES (6,60),(7,70),(8,80),(9,90),(10,100)"); err != nil {
		t.Fatalf("INSERT batch 2: %v", err)
	}

	t.Run("delete_even_ids", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM imbd_t WHERE id IN (2,4,6,8,10)")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 5 {
			t.Errorf("want 5 deleted, got %d", n)
		}
	})

	t.Run("remaining_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM imbd_t")
		// remaining: 10+30+50+70+90 = 250
		if toInt64(rows[0][0]) != 250 {
			t.Errorf("want 250, got %v", rows[0][0])
		}
	})

	t.Run("remaining_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM imbd_t")
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5, got %v", rows[0][0])
		}
	})
}

// TestFDB_CRUDCycle — full CRUD cycle (insert, update, verify)
func TestFDB_CRUDCycle(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "crud",
		"CREATE TABLE crud_t(id BIGINT, name STRING, val BIGINT, PRIMARY KEY(id))")

	t.Run("create", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO crud_t VALUES (1,'item1',100),(2,'item2',200),(3,'item3',300)")
		if err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 inserted, got %d", n)
		}
	})

	t.Run("read", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name, val FROM crud_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "item1" || toInt64(rows[0][1]) != 100 {
			t.Errorf("row 0: want [item1 100], got %v", rows[0])
		}
	})

	t.Run("update", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE crud_t SET val = val + 50 WHERE id = 2")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM crud_t WHERE id = 2")
		if toInt64(rows[0][0]) != 250 {
			t.Errorf("want 250, got %v", rows[0][0])
		}
	})

	t.Run("delete", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM crud_t WHERE id = 3")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1, got %d", n)
		}
	})

	t.Run("final_state", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM crud_t ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 remaining, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[1][0]) != 2 {
			t.Errorf("want ids [1 2], got [%v %v]", rows[0][0], rows[1][0])
		}
	})
}

// TestFDB_DeleteThenInsertSameKey — delete row then re-insert same PK
func TestFDB_DeleteThenInsertSameKey(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtis",
		"CREATE TABLE dtis_t(id BIGINT, val STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dtis_t VALUES (1,'original')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("delete_and_reinsert", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM dtis_t WHERE id = 1"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO dtis_t VALUES (1,'replacement')"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM dtis_t WHERE id = 1")
		if fmt.Sprintf("%v", rows[0][0]) != "replacement" {
			t.Errorf("want 'replacement', got %v", rows[0][0])
		}
	})

	t.Run("count_still_one", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM dtis_t")
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("want 1, got %v", rows[0][0])
		}
	})
}
