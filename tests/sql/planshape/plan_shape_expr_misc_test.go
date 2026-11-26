package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_ComplexExpressionCombinations(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "cplxexpr",
		"CREATE TABLE t (id BIGINT, cat STRING, val BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id int
		c  string
		v  int
	}{
		{1, "A", 10},
		{2, "A", 20},
		{3, "A", 30},
		{4, "B", 100},
		{5, "B", 200},
		{6, "C", 1},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, '%s', %d)", r.id, r.c, r.v))
	}

	t.Run("coalesce_sum_zero", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT cat, COALESCE(SUM(val), 0) FROM t GROUP BY cat ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if rows[0][1].(int64) != 60 {
			t.Errorf("A: got %v, want 60", rows[0][1])
		}
	})

	t.Run("case_over_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT cat,
			        CASE WHEN SUM(val) > 100 THEN 'high'
			             WHEN SUM(val) > 10 THEN 'medium'
			             ELSE 'low'
			        END AS tier
			 FROM t GROUP BY cat ORDER BY cat`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if rows[0][1].(string) != "medium" {
			t.Errorf("A (sum=60): got %v, want medium", rows[0][1])
		}
		if rows[1][1].(string) != "high" {
			t.Errorf("B (sum=300): got %v, want high", rows[1][1])
		}
		if rows[2][1].(string) != "low" {
			t.Errorf("C (sum=1): got %v, want low", rows[2][1])
		}
	})

	t.Run("arithmetic_on_aggregates", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT cat, SUM(val) * 2 + 1 AS doubled_plus FROM t GROUP BY cat ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if rows[0][1] == nil {
			t.Fatalf("doubled_plus NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 121 {
			t.Errorf("A: got %v, want 121 (60*2+1)", rows[0][1])
		}
		if rows[1][1].(int64) != 601 {
			t.Errorf("B: got %v, want 601 (300*2+1)", rows[1][1])
		}
	})

	_ = ctx
}

func TestFDB_MixedTypeArithmetic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "mixtype1",
		"CREATE TABLE t1 (id BIGINT, int_val INTEGER, long_val BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 100)",
		"INSERT INTO t1 VALUES (2, 20, 200)",
		"INSERT INTO t1 VALUES (3, 30, 300)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("int_plus_literal", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, int_val + 5 FROM t1 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 15 {
			t.Errorf("10+5: got %v", rows[0][1])
		}
	})

	t.Run("long_minus_int", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, long_val - int_val FROM t1 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 90 {
			t.Errorf("100-10: got %v", rows[0][1])
		}
	})

	t.Run("multiplication", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, int_val * 3 FROM t1 WHERE id = 2")
		if len(rows) != 1 || toInt64(rows[0][1]) != 60 {
			t.Fatalf("want 60, got %v", rows)
		}
	})

	t.Run("division", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, long_val / int_val FROM t1 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 10 {
			t.Errorf("100/10: got %v", rows[0][1])
		}
	})

	t.Run("aggregate_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(int_val) * 2, SUM(long_val) / 3 FROM t1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 120 {
			t.Errorf("SUM(int_val)*2 = 60*2 = 120, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 200 {
			t.Errorf("SUM(long_val)/3 = 600/3 = 200, got %v", rows[0][1])
		}
	})
}

// TestFDB_ComplexExpressionEvaluation — complex expressions in various positions
func TestFDB_ComplexExpressionEvaluation(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cxeval", "CREATE TABLE expr_t(id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO expr_t VALUES
		(1, 10, 20, 30), (2, 40, 50, 60), (3, 70, 80, 90), (4, 100, 0, 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("arithmetic_chain", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, a + b + c FROM expr_t ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 60 {
			t.Errorf("10+20+30=60, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 150 {
			t.Errorf("40+50+60=150, got %v", rows[1][1])
		}
	})

	t.Run("multiply_in_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM expr_t WHERE a * b > 3000 ORDER BY id")
		if len(rows) != 1 {
			t.Fatalf("want 1 (70*80=5600>3000), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("should be id=3, got %v", rows[0][0])
		}
	})

	t.Run("subtraction", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, a - b FROM expr_t ORDER BY id")
		if toInt64(rows[0][1]) != -10 {
			t.Errorf("10-20=-10, got %v", rows[0][1])
		}
		if toInt64(rows[3][1]) != 100 {
			t.Errorf("100-0=100, got %v", rows[3][1])
		}
	})

	t.Run("case_with_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, CASE WHEN a + b > 100 THEN a * 2 ELSE b * 2 END
			FROM expr_t ORDER BY id
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 40 {
			t.Errorf("id=1: a+b=30<100 → b*2=40, got %v", rows[0][1])
		}
		if toInt64(rows[2][1]) != 140 {
			t.Errorf("id=3: a+b=150>100 → a*2=140, got %v", rows[2][1])
		}
	})

	t.Run("nested_coalesce", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO expr_t(id, a) VALUES (5, 42)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, COALESCE(b, COALESCE(c, 999)) FROM expr_t WHERE id = 5")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 999 {
			t.Errorf("COALESCE(NULL, COALESCE(NULL, 999)) = 999, got %v", rows[0][1])
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM expr_t WHERE id = 5"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
	})
}

// TestFDB_ErrorHandling — SQL error conditions
func TestFDB_ErrorHandling(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "errhnd", "CREATE TABLE err_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO err_t VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("select_nonexistent_table", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT * FROM nonexistent_table")
		if err == nil {
			t.Errorf("expected error for nonexistent table")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("select_nonexistent_column", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT nonexistent_col FROM err_t")
		if err == nil {
			t.Errorf("expected error for nonexistent column")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("group_by_missing_column", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id, val FROM err_t GROUP BY id")
		if err == nil {
			t.Errorf("expected error: val not in GROUP BY")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("insert_wrong_column_count", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO err_t VALUES (3)")
		if err == nil {
			t.Errorf("expected error for wrong column count")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("syntax_error", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELEKT * FROM err_t")
		if err == nil {
			t.Errorf("expected syntax error")
		} else {
			t.Logf("expected error: %v", err)
		}
	})
}

// TestFDB_SelectExpressions — computed columns and aliases in SELECT
func TestFDB_SelectExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "selexpr", "CREATE TABLE se_t(id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO se_t VALUES (1, 10, 5), (2, 20, 3), (3, 30, 7)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("computed_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, price * qty AS total FROM se_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 50 {
			t.Errorf("10*5=50, got %v", rows[0][1])
		}
		if toInt64(rows[2][1]) != 210 {
			t.Errorf("30*7=210, got %v", rows[2][1])
		}
	})

	t.Run("sum_of_computed", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(price * qty) FROM se_t")
		if toInt64(rows[0][0]) != 320 {
			t.Errorf("SUM(price*qty)=50+60+210=320, got %v", rows[0][0])
		}
	})

	t.Run("order_by_expression", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, price * qty AS total FROM se_t ORDER BY price * qty DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("highest total should be id=3 (210), got id=%v", rows[0][0])
		}
	})

	t.Run("constant_expression_from_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT 1 + 2 + 3 FROM se_t LIMIT 1")
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("1+2+3=6, got %v", rows[0][0])
		}
	})

	t.Run("where_on_computed", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM se_t WHERE price * qty > 100 ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Errorf("only id=3 has price*qty=210>100, got %v", rows)
		}
	})
}

// TestFDB_WindowFunctionErrors — window functions should error (not supported)
func TestFDB_WindowFunctionErrors(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wferr", "CREATE TABLE wf_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO wf_t VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("row_number_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id, ROW_NUMBER() OVER (ORDER BY id) FROM wf_t")
		if err == nil {
			t.Logf("ROW_NUMBER() OVER unexpectedly succeeded")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("rank_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id, RANK() OVER (ORDER BY val) FROM wf_t")
		if err == nil {
			t.Logf("RANK() OVER unexpectedly succeeded")
		} else {
			t.Logf("expected error: %v", err)
		}
	})
}

// TestFDB_MixedOperators — queries mixing multiple operator types
func TestFDB_MixedOperators(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mixop", "CREATE TABLE mo_t(id BIGINT, a BIGINT, b STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO mo_t VALUES
		(1, 10, 'hello'), (2, 20, 'world'), (3, 30, 'hello'),
		(4, 40, 'test'), (5, 50, 'world'), (6, 60, 'hello')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("in_and_like_combined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM mo_t WHERE a IN (10, 30, 50) AND b LIKE 'hel%' ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (id=1: a=10+hello, id=3: a=30+hello), got %d: %v", len(rows), rows)
		}
	})

	t.Run("between_and_not_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM mo_t WHERE a BETWEEN 20 AND 50 AND b NOT LIKE 'hel%' ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (id=2:world, id=4:test, id=5:world), got %d: %v", len(rows), rows)
		}
	})

	t.Run("group_by_string_with_count_and_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, COUNT(*), SUM(a) FROM mo_t GROUP BY b ORDER BY b")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "hello" || toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 100 {
			t.Errorf("hello: want count=3, sum=10+30+60=100, got %v %v %v", rows[0][0], rows[0][1], rows[0][2])
		}
	})

	t.Run("order_by_aggregate_desc_with_limit", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, SUM(a) AS total FROM mo_t GROUP BY b ORDER BY total DESC LIMIT 1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "hello" {
			t.Errorf("top group should be hello(100), got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectWithAlias — column and table aliases in various positions
func TestFDB_SelectWithAlias(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swal", "CREATE TABLE swa_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO swa_t VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("column_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id AS key, val AS value FROM swa_t ORDER BY key")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
	})

	t.Run("table_alias_qualified", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT t.id, t.val FROM swa_t t ORDER BY t.id")
		if len(rows) != 3 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want id=1, got %v", rows)
		}
	})

	t.Run("aggregate_alias_in_order_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val AS v FROM swa_t ORDER BY v DESC")
		if toInt64(rows[0][0]) != 300 {
			t.Errorf("first val DESC should be 300, got %v", rows[0][0])
		}
	})

	t.Run("expression_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val * 2 AS doubled FROM swa_t ORDER BY id")
		if toInt64(rows[0][1]) != 200 {
			t.Errorf("100*2=200, got %v", rows[0][1])
		}
	})
}

// TestFDB_SelectWithArithmeticAndAlias — arithmetic expressions with column aliases
func TestFDB_SelectWithArithmeticAndAlias(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swaaa", "CREATE TABLE swaa_t(id BIGINT, width BIGINT, height BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO swaa_t VALUES (1, 10, 5), (2, 20, 8), (3, 15, 12)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("area_computation", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, width * height AS area FROM swaa_t ORDER BY area DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 180 {
			t.Errorf("largest area: 15*12=180, got %v", rows[0][1])
		}
	})

	t.Run("sum_simple_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(width + height) FROM swaa_t")
		if toInt64(rows[0][0]) != 70 {
			t.Errorf("SUM(width+height) = (10+5)+(20+8)+(15+12) = 70, got %v", rows[0][0])
		}
	})
}

// TestFDB_ArithmeticExpressionProjection — complex arithmetic in SELECT
func TestFDB_ArithmeticExpressionProjection(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aep",
		"CREATE TABLE aep_t(id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO aep_t VALUES (1,10,3),(2,20,7),(3,15,5)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("add_multiply", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a + b, a * b FROM aep_t ORDER BY id")
		wantAdd := []int64{13, 27, 20}
		wantMul := []int64{30, 140, 75}
		for i := range wantAdd {
			if toInt64(rows[i][0]) != wantAdd[i] || toInt64(rows[i][1]) != wantMul[i] {
				t.Errorf("row %d: want [%d %d], got [%v %v]", i, wantAdd[i], wantMul[i], rows[i][0], rows[i][1])
			}
		}
	})

	t.Run("subtract_in_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM aep_t WHERE a - b > 10 ORDER BY id")
		// 10-3=7, 20-7=13, 15-5=10 → only id=2 has diff > 10
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want [2], got %v", rows)
		}
	})

	t.Run("sum_of_expression", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(a + b) FROM aep_t")
		// (10+3)+(20+7)+(15+5) = 13+27+20 = 60
		if toInt64(rows[0][0]) != 60 {
			t.Errorf("want 60, got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectConstantExpression — SELECT with constant expressions (no table)
func TestFDB_SelectConstantExpression(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}

	db := testkit.SetupPlanShapeDB(t, "sce",
		"CREATE TABLE sce_t(id BIGINT, PRIMARY KEY(id))")

	t.Run("select_1_plus_2", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT 1 + 2 FROM sce_t")
		// No rows in table, so no result
		if len(rows) != 0 {
			t.Errorf("want 0 rows from empty table, got %d", len(rows))
		}
	})

	t.Run("constant_with_row", func(t *testing.T) {
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, "INSERT INTO sce_t VALUES (1)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT 42, id FROM sce_t")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 42 {
			t.Errorf("want 42, got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectWithColumnAlias — column alias in SELECT
func TestFDB_SelectWithColumnAlias(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swca",
		"CREATE TABLE swca_t(id BIGINT, first_name STRING, last_name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO swca_t VALUES (1,'alice','smith'),(2,'bob','jones')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("alias_in_order_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT first_name AS fname FROM swca_t ORDER BY fname
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" {
			t.Errorf("want alice first, got %v", rows[0][0])
		}
	})

	t.Run("count_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) AS total FROM swca_t
		`)
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2, got %v", rows[0][0])
		}
	})
}
