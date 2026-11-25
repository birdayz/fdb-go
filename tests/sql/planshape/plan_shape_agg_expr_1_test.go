package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_EmptyTableAggregates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "emptyagg",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	t.Run("sum_empty_is_null", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(col1) FROM t1")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("expected 1 row")
		}
		var v *int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if v != nil {
			t.Errorf("SUM on empty table: got %d, want NULL", *v)
		}
	})

	t.Run("count_star_empty_is_zero", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM t1")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("expected 1 row")
		}
		var cnt int64
		if err := rows.Scan(&cnt); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if cnt != 0 {
			t.Errorf("COUNT(*) on empty table: got %d, want 0", cnt)
		}
	})

	t.Run("sum_and_count_empty", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(col1) AS a, COUNT(*) AS b FROM t1")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("expected 1 row")
		}
		var sumVal *int64
		var cntVal int64
		if err := rows.Scan(&sumVal, &cntVal); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if sumVal != nil {
			t.Errorf("SUM: got %d, want NULL", *sumVal)
		}
		if cntVal != 0 {
			t.Errorf("COUNT: got %d, want 0", cntVal)
		}
	})

	t.Run("union_all_empty_tables", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1, col2 FROM t1 UNION ALL SELECT col1, col2 FROM t1")
		if len(rows) != 0 {
			t.Fatalf("want 0 rows, got %d", len(rows))
		}
	})

	_ = ctx
}

func TestFDB_CompositeAggregateExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "compagg",
		"CREATE TABLE t1 (id BIGINT, grp BIGINT, val BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id, grp, val int
	}{
		{1, 1, 10},
		{2, 1, 20},
		{3, 1, 30},
		{4, 2, 100},
		{5, 2, 200},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO t1 VALUES (%d, %d, %d)", r.id, r.grp, r.val,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("sum_and_count_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, SUM(val), COUNT(*) FROM t1 GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 60 || rows[0][2].(int64) != 3 {
			t.Errorf("grp 1: got sum=%v count=%v, want 60, 3", rows[0][1], rows[0][2])
		}
		if rows[1][1].(int64) != 300 || rows[1][2].(int64) != 2 {
			t.Errorf("grp 2: got sum=%v count=%v, want 300, 2", rows[1][1], rows[1][2])
		}
	})

	t.Run("avg_as_sum_div_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, SUM(val) / COUNT(*) FROM t1 GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 20 {
			t.Errorf("grp 1 avg: got %v, want 20", rows[0][1])
		}
		if rows[1][1].(int64) != 150 {
			t.Errorf("grp 2 avg: got %v, want 150", rows[1][1])
		}
	})

	t.Run("min_max_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, MIN(val), MAX(val) FROM t1 GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 10 || rows[0][2].(int64) != 30 {
			t.Errorf("grp 1: got min=%v max=%v, want 10, 30", rows[0][1], rows[0][2])
		}
		if rows[1][1].(int64) != 100 || rows[1][2].(int64) != 200 {
			t.Errorf("grp 2: got min=%v max=%v, want 100, 200", rows[1][1], rows[1][2])
		}
	})

	t.Run("having_with_sum_and_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, SUM(val) FROM t1 GROUP BY grp HAVING COUNT(*) > 2")
		if len(rows) != 1 {
			t.Fatalf("want 1 row (grp 1), got %d", len(rows))
		}
		if rows[0][0].(int64) != 1 || rows[0][1].(int64) != 60 {
			t.Errorf("got grp=%v sum=%v, want 1, 60", rows[0][0], rows[0][1])
		}
	})

	t.Run("duplicate_alias_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp AS g, grp AS g2, SUM(val) AS s1, SUM(val) AS s2 FROM t1 GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if rows[0][0] != rows[0][1] || rows[0][2] != rows[0][3] {
			t.Errorf("duplicate aliases should match: %v", rows[0])
		}
	})

	_ = ctx
}

func TestFDB_AggExprArgDirect(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "aggexpr",
		"CREATE TABLE items (id BIGINT, cat STRING, price BIGINT, qty BIGINT, PRIMARY KEY (id))")
	for _, r := range []struct {
		id   int
		c    string
		p, q int
	}{
		{1, "A", 10, 5}, {2, "A", 20, 3}, {3, "B", 100, 2},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, '%s', %d, %d)", r.id, r.c, r.p, r.q))
	}

	t.Run("expr_only", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, price * qty FROM items ORDER BY id")
		t.Logf("price*qty: %v", rows)
	})

	t.Run("sum_bare", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, SUM(price) FROM items GROUP BY cat ORDER BY cat")
		t.Logf("sum(price): %v", rows)
	})

	t.Run("sum_col_times_col", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT cat, SUM(price * qty) AS tv FROM items GROUP BY cat ORDER BY cat")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1] == nil {
			t.Fatalf("SUM(price*qty) is NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 110 {
			t.Errorf("A: got %v, want 110 (10*5 + 20*3)", rows[0][1])
		}
		if rows[1][1].(int64) != 200 {
			t.Errorf("B: got %v, want 200 (100*2)", rows[1][1])
		}
	})

	_ = ctx
}

func TestFDB_AggregateExpressionVariants(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aggvar",
		"CREATE TABLE sales (id BIGINT, region STRING, units BIGINT, price BIGINT, discount BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id                     int
		region                 string
		units, price, discount int
	}{
		{1, "US", 10, 100, 5},
		{2, "US", 20, 50, 10},
		{3, "EU", 5, 200, 0},
		{4, "EU", 15, 80, 20},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO sales VALUES (%d, '%s', %d, %d, %d)",
			r.id, r.region, r.units, r.price, r.discount,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("sum_product", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT region, SUM(units * price) AS revenue FROM sales GROUP BY region ORDER BY region")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1] == nil {
			t.Fatalf("revenue NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 2200 {
			t.Errorf("EU: got %v, want 2200 (5*200+15*80)", rows[0][1])
		}
		if rows[1][1].(int64) != 2000 {
			t.Errorf("US: got %v, want 2000 (10*100+20*50)", rows[1][1])
		}
	})

	t.Run("sum_subtraction", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT region, SUM(price - discount) AS net FROM sales GROUP BY region ORDER BY region")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1] == nil {
			t.Fatalf("net NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 260 {
			t.Errorf("EU: got %v, want 260 (200-0+80-20)", rows[0][1])
		}
		if rows[1][1].(int64) != 135 {
			t.Errorf("US: got %v, want 135 (100-5+50-10)", rows[1][1])
		}
	})

	t.Run("count_with_min_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT region, COUNT(*), MIN(price), MAX(price) FROM sales GROUP BY region ORDER BY region")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1].(int64) != 2 || rows[0][2].(int64) != 80 || rows[0][3].(int64) != 200 {
			t.Errorf("EU: got count=%v min=%v max=%v, want 2,80,200", rows[0][1], rows[0][2], rows[0][3])
		}
	})

	_ = ctx
}

func TestFDB_MinMaxExpressionArg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "mmexpr",
		"CREATE TABLE items (id BIGINT, cat STRING, price BIGINT, qty BIGINT, PRIMARY KEY (id))")
	for _, r := range []struct {
		id   int
		c    string
		p, q int
	}{
		{1, "A", 10, 5}, {2, "A", 20, 3}, {3, "B", 100, 2}, {4, "B", 50, 4},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, '%s', %d, %d)", r.id, r.c, r.p, r.q))
	}

	t.Run("min_expr", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT cat, MIN(price * qty) FROM items GROUP BY cat ORDER BY cat")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1] == nil {
			t.Fatalf("MIN(price*qty) NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 50 {
			t.Errorf("A min: got %v, want 50 (min(10*5,20*3))", rows[0][1])
		}
		if rows[1][1].(int64) != 200 {
			t.Errorf("B min: got %v, want 200 (min(100*2,50*4))", rows[1][1])
		}
	})

	t.Run("max_expr", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT cat, MAX(price * qty) FROM items GROUP BY cat ORDER BY cat")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if rows[0][1] == nil {
			t.Fatalf("MAX(price*qty) NULL for %v", rows[0][0])
		}
		if rows[0][1].(int64) != 60 {
			t.Errorf("A max: got %v, want 60 (max(50,60))", rows[0][1])
		}
		if rows[1][1].(int64) != 200 {
			t.Errorf("B max: got %v, want 200 (max(200,200))", rows[1][1])
		}
	})

	_ = ctx
}

func TestFDB_AggExprWithNulls(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "aggnull",
		"CREATE TABLE items (id BIGINT, grp STRING, price BIGINT, qty BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id   int
		g    string
		p, q *int64
	}{
		{1, "A", ptr(int64(10)), ptr(int64(5))},
		{2, "A", ptr(int64(20)), nil},
		{3, "B", nil, ptr(int64(3))},
		{4, "B", ptr(int64(50)), ptr(int64(2))},
	} {
		if r.p == nil && r.q == nil {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items (id, grp) VALUES (%d, '%s')", r.id, r.g))
		} else if r.p == nil {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items (id, grp, qty) VALUES (%d, '%s', %d)", r.id, r.g, *r.q))
		} else if r.q == nil {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items (id, grp, price) VALUES (%d, '%s', %d)", r.id, r.g, *r.p))
		} else {
			db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, '%s', %d, %d)", r.id, r.g, *r.p, *r.q))
		}
	}

	t.Run("sum_expr_skips_null_operand", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, SUM(price * qty) FROM items GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		// A: id=1 has 10*5=50, id=2 has 20*NULL=NULL (skipped). SUM=50.
		// B: id=3 has NULL*3=NULL (skipped), id=4 has 50*2=100. SUM=100.
		if rows[0][1] == nil {
			t.Errorf("A: SUM is NULL, want 50")
		} else if rows[0][1].(int64) != 50 {
			t.Errorf("A: got %v, want 50", rows[0][1])
		}
		if rows[1][1] == nil {
			t.Errorf("B: SUM is NULL, want 100")
		} else if rows[1][1].(int64) != 100 {
			t.Errorf("B: got %v, want 100", rows[1][1])
		}
	})

	t.Run("count_with_null_expr", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT grp, COUNT(price * qty) FROM items GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		// COUNT(expr) skips NULLs: A has 1 non-null, B has 1 non-null.
		if rows[0][1].(int64) != 1 {
			t.Errorf("A count: got %v, want 1", rows[0][1])
		}
		if rows[1][1].(int64) != 1 {
			t.Errorf("B count: got %v, want 1", rows[1][1])
		}
	})

	_ = ctx
}

func TestFDB_MultipleAggExprsInOneQuery(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "multiaggx",
		"CREATE TABLE sales (id BIGINT, region STRING, qty BIGINT, price BIGINT, cost BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id               int
		region           string
		qty, price, cost int
	}{
		{1, "US", 10, 100, 50},
		{2, "US", 20, 50, 30},
		{3, "EU", 5, 200, 80},
	} {
		db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO sales VALUES (%d, '%s', %d, %d, %d)",
			r.id, r.region, r.qty, r.price, r.cost,
		))
	}

	t.Run("revenue_and_cost_per_region", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT region,
			        SUM(qty * price) AS revenue,
			        SUM(qty * cost) AS total_cost
			 FROM sales GROUP BY region ORDER BY region`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		// EU: 5*200=1000 revenue, 5*80=400 cost
		if rows[0][1] == nil || rows[0][2] == nil {
			t.Fatalf("EU has NULL: revenue=%v cost=%v", rows[0][1], rows[0][2])
		}
		if rows[0][1].(int64) != 1000 {
			t.Errorf("EU revenue: got %v, want 1000", rows[0][1])
		}
		if rows[0][2].(int64) != 400 {
			t.Errorf("EU cost: got %v, want 400", rows[0][2])
		}
		// US: 10*100+20*50=2000 revenue, 10*50+20*30=1100 cost
		if rows[1][1].(int64) != 2000 {
			t.Errorf("US revenue: got %v, want 2000", rows[1][1])
		}
		if rows[1][2].(int64) != 1100 {
			t.Errorf("US cost: got %v, want 1100", rows[1][2])
		}
	})

	t.Run("profit_margin_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT s.region, s.revenue - s.total_cost AS profit
			 FROM (SELECT region,
			              SUM(qty * price) AS revenue,
			              SUM(qty * cost) AS total_cost
			       FROM sales GROUP BY region) s
			 ORDER BY profit DESC`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][1] == nil {
			t.Fatalf("profit NULL for %v", rows[0][0])
		}
		// US profit: 2000-1100=900, EU profit: 1000-400=600
		if rows[0][0].(string) != "US" || rows[0][1].(int64) != 900 {
			t.Errorf("row 0: got %v, want [US, 900]", rows[0])
		}
		if rows[1][0].(string) != "EU" || rows[1][1].(int64) != 600 {
			t.Errorf("row 1: got %v, want [EU, 600]", rows[1])
		}
	})

	_ = ctx
}
