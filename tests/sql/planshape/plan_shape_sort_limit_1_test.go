package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_LimitBasicPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "limoff1",
		"CREATE TABLE items (id BIGINT, name STRING, price BIGINT, PRIMARY KEY (id))")

	for i := int64(1); i <= 10; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, 'item%d', %d)", i, i, i*100,
		)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("limit_basic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM items ORDER BY id LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		for i := int64(0); i < 3; i++ {
			if toInt64(rows[i][0]) != i+1 {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], i+1)
			}
		}
	})

	t.Run("limit_exceeds_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM items ORDER BY id LIMIT 100")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
	})

	t.Run("limit_zero", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM items ORDER BY id LIMIT 0")
		if len(rows) != 0 {
			t.Fatalf("want 0, got %d", len(rows))
		}
	})

	t.Run("limit_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id, price FROM items ORDER BY price DESC LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("first should be id=10, got %v", rows[0][0])
		}
	})

	_ = ctx
}

func TestFDB_MultiColumnOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "mcord1",
		"CREATE TABLE items (id BIGINT, category STRING, name STRING, price BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO items VALUES (1, 'A', 'Widget', 100)",
		"INSERT INTO items VALUES (2, 'B', 'Gadget', 200)",
		"INSERT INTO items VALUES (3, 'A', 'Doohickey', 50)",
		"INSERT INTO items VALUES (4, 'B', 'Thingamajig', 300)",
		"INSERT INTO items VALUES (5, 'A', 'Whatchamacallit', 100)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("order_by_two_columns_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT category, name FROM items ORDER BY category, name")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if rows[0][0].(string) != "A" || rows[0][1].(string) != "Doohickey" {
			t.Errorf("first: got (%v, %v), want (A, Doohickey)", rows[0][0], rows[0][1])
		}
		if rows[4][0].(string) != "B" || rows[4][1].(string) != "Thingamajig" {
			t.Errorf("last: got (%v, %v), want (B, Thingamajig)", rows[4][0], rows[4][1])
		}
	})

	t.Run("order_by_asc_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT category, price FROM items ORDER BY category ASC, price DESC")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if rows[0][0].(string) != "A" || toInt64(rows[0][1]) != 100 {
			t.Errorf("first A row: got (%v, %v), want (A, 100)", rows[0][0], rows[0][1])
		}
		if rows[3][0].(string) != "B" || toInt64(rows[3][1]) != 300 {
			t.Errorf("first B row: got (%v, %v), want (B, 300)", rows[3][0], rows[3][1])
		}
	})

	t.Run("group_by_with_order_by_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT category, COUNT(*), SUM(price)
			 FROM items
			 GROUP BY category
			 ORDER BY SUM(price) DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][0].(string) != "B" || toInt64(rows[0][2]) != 500 {
			t.Errorf("first: got %v, want (B, 2, 500)", rows[0])
		}
		if rows[1][0].(string) != "A" || toInt64(rows[1][2]) != 250 {
			t.Errorf("second: got %v, want (A, 3, 250)", rows[1])
		}
	})

	_ = ctx
}

func TestFDB_NullOrderingAndArithmetic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "nullarith1",
		"CREATE TABLE t1 (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 20)",
		"INSERT INTO t1 VALUES (2, NULL, 30)",
		"INSERT INTO t1 VALUES (3, 40, NULL)",
		"INSERT INTO t1 VALUES (4, NULL, NULL)",
		"INSERT INTO t1 VALUES (5, 50, 60)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("null_arithmetic_propagates", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id, a + b FROM t1 ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 30 {
			t.Errorf("row 1: 10+20=30, got %v", rows[0][1])
		}
		if rows[1][1] != nil {
			t.Errorf("row 2: NULL+30 should be NULL, got %v", rows[1][1])
		}
		if rows[2][1] != nil {
			t.Errorf("row 3: 40+NULL should be NULL, got %v", rows[2][1])
		}
		if rows[3][1] != nil {
			t.Errorf("row 4: NULL+NULL should be NULL, got %v", rows[3][1])
		}
		if toInt64(rows[4][1]) != 110 {
			t.Errorf("row 5: 50+60=110, got %v", rows[4][1])
		}
	})

	t.Run("null_in_sum_skipped", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(a) FROM t1")
		if len(rows) != 1 || toInt64(rows[0][0]) != 100 {
			t.Fatalf("want 100 (10+40+50), got %v", rows)
		}
	})

	t.Run("count_col_skips_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(a), COUNT(b), COUNT(*) FROM t1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT(a): want 3, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("COUNT(b): want 3, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 5 {
			t.Errorf("COUNT(*): want 5, got %v", rows[0][2])
		}
	})

	t.Run("coalesce_with_arithmetic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT id, COALESCE(a, 0) + COALESCE(b, 0) FROM t1 ORDER BY id")
		wantSums := []int64{30, 30, 40, 0, 110}
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		for i, w := range wantSums {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][1], w)
			}
		}
	})

	t.Run("min_max_with_nulls", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT MIN(a), MAX(a), MIN(b), MAX(b) FROM t1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("MIN(a): want 10, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 50 {
			t.Errorf("MAX(a): want 50, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 20 {
			t.Errorf("MIN(b): want 20, got %v", rows[0][2])
		}
		if toInt64(rows[0][3]) != 60 {
			t.Errorf("MAX(b): want 60, got %v", rows[0][3])
		}
	})
}

// TestFDB_OrderByPatterns — Java orderby.yamsql patterns
func TestFDB_OrderByPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ordby", `
		CREATE TABLE obt(a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(a))
	`)
	if _, err := db.ExecContext(ctx, `INSERT INTO obt VALUES
		(1, 10, 5), (2, 9, 5), (3, 8, 5),
		(4, 7, 8), (5, 6, 8), (6, 5, 8),
		(7, 4, 1), (8, 3, 1),
		(9, 2, 0), (10, 1, 0)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_single_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, c FROM obt ORDER BY b")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("first b should be 1, got %v", rows[0][0])
		}
		if toInt64(rows[9][0]) != 10 {
			t.Errorf("last b should be 10, got %v", rows[9][0])
		}
	})

	t.Run("order_by_single_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, c FROM obt ORDER BY b DESC")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("first b should be 10, got %v", rows[0][0])
		}
		if toInt64(rows[9][0]) != 1 {
			t.Errorf("last b should be 1, got %v", rows[9][0])
		}
	})

	t.Run("order_by_with_range_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, c FROM obt WHERE b >= 5 ORDER BY b")
		if len(rows) != 6 {
			t.Fatalf("want 6 rows (b>=5), got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("first should be 5, got %v", rows[0][0])
		}
	})

	t.Run("order_by_with_filter_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, c FROM obt WHERE b >= 5 ORDER BY b DESC")
		if len(rows) != 6 {
			t.Fatalf("want 6 rows, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("first should be 10 (DESC), got %v", rows[0][0])
		}
	})

	t.Run("order_by_with_combined_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b, c FROM obt WHERE b >= 5 AND c = 5 ORDER BY b")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows (b>=5 AND c=5), got %d: %v", len(rows), rows)
		}
		wantB := []int64{8, 9, 10}
		for i, wb := range wantB {
			if toInt64(rows[i][0]) != wb {
				t.Errorf("row %d: want b=%d, got %v", i, wb, rows[i][0])
			}
		}
	})

	t.Run("order_by_repetitive_values", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c FROM obt ORDER BY c")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("first c should be 0, got %v", rows[0][0])
		}
		if toInt64(rows[9][0]) != 8 {
			t.Errorf("last c should be 8, got %v", rows[9][0])
		}
	})

	t.Run("order_by_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c, b FROM obt ORDER BY c, b")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 0 && toInt64(rows[0][1]) != 1 {
			t.Errorf("first should be c=0,b=1, got c=%v,b=%v", rows[0][0], rows[0][1])
		}
	})

	t.Run("order_by_two_columns_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c, b FROM obt ORDER BY c DESC, b DESC")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 8 {
			t.Errorf("first c should be 8 (DESC), got %v", rows[0][0])
		}
	})

	t.Run("order_by_with_limit", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT b FROM obt ORDER BY b LIMIT 4")
		if len(rows) != 4 {
			t.Fatalf("want 4 with LIMIT, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("first b=1, got %v", rows[0][0])
		}
		if toInt64(rows[3][0]) != 4 {
			t.Errorf("last b=4, got %v", rows[3][0])
		}
	})

	t.Run("order_by_non_projected", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c FROM obt ORDER BY b")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		wantC := []int64{0, 0, 1, 1, 8, 8, 8, 5, 5, 5}
		for i, wc := range wantC {
			if toInt64(rows[i][0]) != wc {
				t.Errorf("row %d: want c=%d, got %v", i, wc, rows[i][0])
			}
		}
	})

	t.Run("order_by_non_projected_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c FROM obt ORDER BY b DESC")
		if len(rows) != 10 {
			t.Fatalf("want 10, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("first c should be 5 (b=10 DESC), got %v", rows[0][0])
		}
	})

	t.Run("order_by_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT c, COUNT(*) FROM obt GROUP BY c ORDER BY COUNT(*)")
		if len(rows) < 3 {
			t.Fatalf("want at least 3 groups, got %d", len(rows))
		}
		t.Logf("ORDER BY aggregate: %v", rows)
	})
}

// TestFDB_OrderByDuplicate — ORDER BY with same column twice should error
func TestFDB_OrderByDuplicate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "obdup", "CREATE TABLE dup_t(a BIGINT, b BIGINT, PRIMARY KEY(a))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dup_t VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("duplicate_order_by_column", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT b FROM dup_t ORDER BY b, b")
		if err == nil {
			t.Errorf("expected error for duplicate ORDER BY column, got nil")
		} else {
			t.Logf("expected error: %v", err)
			if !strings.Contains(err.Error(), "42701") {
				t.Logf("error code may differ from Java 42701: %v", err)
			}
		}
	})
}

// TestFDB_LimitOffsetCombinations — LIMIT+OFFSET with ORDER BY
func TestFDB_LimitOffsetCombinations(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "limoff", "CREATE TABLE lim_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	for i := 1; i <= 10; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO lim_t VALUES (%d, %d)", i, i*10)); err != nil {
			t.Fatalf("INSERT id=%d: %v", i, err)
		}
	}

	t.Run("limit_3_order_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lim_t ORDER BY id LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[2][0]) != 3 {
			t.Errorf("want 1,2,3 got %v,%v,%v", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	t.Run("limit_exceeds_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lim_t ORDER BY id LIMIT 100")
		if len(rows) != 10 {
			t.Fatalf("want 10 (all), got %d", len(rows))
		}
	})

	t.Run("limit_0", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lim_t LIMIT 0")
		if len(rows) != 0 {
			t.Errorf("LIMIT 0 should return 0, got %d", len(rows))
		}
	})

	t.Run("limit_with_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lim_t ORDER BY id DESC LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 || toInt64(rows[2][0]) != 8 {
			t.Errorf("want 10,9,8 got %v,%v,%v", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	t.Run("limit_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lim_t WHERE val > 50 ORDER BY id LIMIT 2")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("first should be 6, got %v", rows[0][0])
		}
	})

	t.Run("limit_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM lim_t ORDER BY val DESC LIMIT 1")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("top val should be id=10, got %v", rows[0][0])
		}
	})
}
