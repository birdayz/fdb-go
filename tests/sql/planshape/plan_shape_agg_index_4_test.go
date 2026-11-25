package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_AggregateIndexOrderByDesc(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aggdesc",
		"CREATE TABLE orders (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX count_by_status AS SELECT COUNT(*) FROM orders GROUP BY status")

	for _, o := range []struct {
		id     int
		status string
		amount int
	}{
		{1, "pending", 100},
		{2, "pending", 200},
		{3, "shipped", 300},
		{4, "shipped", 400},
		{5, "shipped", 500},
		{6, "delivered", 600},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, '%s', %d)", o.id, o.status, o.amount,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("order_by_group_key_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT status, COUNT(*) FROM orders GROUP BY status ORDER BY status")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if rows[0][0].(string) != "delivered" {
			t.Errorf("first: got %v, want delivered", rows[0][0])
		}
		if rows[2][0].(string) != "shipped" {
			t.Errorf("last: got %v, want shipped", rows[2][0])
		}
	})

	t.Run("order_by_group_key_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT status, COUNT(*) FROM orders GROUP BY status ORDER BY status DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if rows[0][0].(string) != "shipped" {
			t.Errorf("first: got %v, want shipped", rows[0][0])
		}
		if rows[2][0].(string) != "delivered" {
			t.Errorf("last: got %v, want delivered", rows[2][0])
		}
	})

	t.Run("order_by_aggregate_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT status, COUNT(*) AS cnt FROM orders GROUP BY status ORDER BY cnt DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if rows[0][1].(int64) != 3 {
			t.Errorf("first count: got %v, want 3 (shipped)", rows[0][1])
		}
		if rows[2][1].(int64) != 1 {
			t.Errorf("last count: got %v, want 1 (delivered)", rows[2][1])
		}
	})

	_ = ctx
}

// TestFDB_AggregateIndexUsage — queries that should use aggregate indexes
func TestFDB_AggregateIndexUsage(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "agidx2",
		"CREATE TABLE aitems(id BIGINT, cat STRING, price BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX cnt_by_cat AS SELECT COUNT(*) FROM aitems GROUP BY cat "+
			"CREATE INDEX sum_price_by_cat AS SELECT SUM(price) FROM aitems GROUP BY cat")
	for i, item := range []struct {
		cat   string
		price int
	}{
		{"electronics", 100},
		{"electronics", 200},
		{"electronics", 300},
		{"books", 15},
		{"books", 25},
		{"food", 5},
		{"food", 10},
		{"food", 8},
		{"food", 12},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO aitems VALUES (%d, '%s', %d)", i+1, item.cat, item.price)); err != nil {
			t.Fatalf("INSERT %d: %v", i+1, err)
		}
	}

	t.Run("count_by_category", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, COUNT(*) FROM aitems GROUP BY cat ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3 categories, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "books" || toInt64(rows[0][1]) != 2 {
			t.Errorf("books: want 2, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "electronics" || toInt64(rows[1][1]) != 3 {
			t.Errorf("electronics: want 3, got %v %v", rows[1][0], rows[1][1])
		}
		if fmt.Sprintf("%v", rows[2][0]) != "food" || toInt64(rows[2][1]) != 4 {
			t.Errorf("food: want 4, got %v %v", rows[2][0], rows[2][1])
		}
	})

	t.Run("sum_by_category", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, SUM(price) FROM aitems GROUP BY cat ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 40 {
			t.Errorf("books sum = 15+25 = 40, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 600 {
			t.Errorf("electronics sum = 100+200+300 = 600, got %v", rows[1][1])
		}
		if toInt64(rows[2][1]) != 35 {
			t.Errorf("food sum = 5+10+8+12 = 35, got %v", rows[2][1])
		}
	})

	t.Run("global_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM aitems")
		if toInt64(rows[0][0]) != 9 {
			t.Errorf("total count should be 9, got %v", rows[0][0])
		}
	})

	t.Run("global_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(price) FROM aitems")
		if toInt64(rows[0][0]) != 675 {
			t.Errorf("total sum should be 675, got %v", rows[0][0])
		}
	})

	t.Run("count_with_eq_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM aitems WHERE cat = 'food'")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("food count should be 4, got %v", rows[0][0])
		}
	})

	t.Run("sum_with_eq_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(price) FROM aitems WHERE cat = 'electronics'")
		if toInt64(rows[0][0]) != 600 {
			t.Errorf("electronics sum should be 600, got %v", rows[0][0])
		}
	})

	t.Run("having_on_aggregate_index", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, COUNT(*) FROM aitems GROUP BY cat HAVING COUNT(*) > 2 ORDER BY cat")
		if len(rows) != 2 {
			t.Fatalf("want 2 (electronics=3, food=4), got %d: %v", len(rows), rows)
		}
	})

	t.Run("insert_then_verify_aggregate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO aitems VALUES (10, 'books', 35)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT cat, COUNT(*), SUM(price) FROM aitems WHERE cat = 'books' GROUP BY cat")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("books count after insert should be 3, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 75 {
			t.Errorf("books sum after insert should be 75, got %v", rows[0][2])
		}
	})

	t.Run("delete_then_verify_aggregate", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM aitems WHERE id = 10"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM aitems WHERE cat = 'books'")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("books count after delete should be 2, got %v", rows[0][0])
		}
	})
}

// TestFDB_MultiAggregateIntersection_Filtered pins the multi-aggregate
// intersection plan (one aggregate index per aggregate, intersected on the
// group key) under a WHERE-equality filter on the group key. This is the
// MultiIntersectionOnValues path built by tryMultiAggregateIntersection.
//
// Regression: the intersection's child plans were bare RecordQueryIndexPlans
// over aggregate indexes — a plain index scan tries to FETCH a base record
// that aggregate entries don't point at, so it yielded zero rows; and the
// merge cursor returned only the first child (dropping every other
// aggregate) without ever evaluating the plan's result value. With a
// WHERE cat='X' prefix each per-aggregate stream emitted exactly one group,
// and the broken merge produced zero rows. The fix routes children through
// the aggregate-index executor, intersects with IntersectionMulti (keeping
// every child's row), merges them, and evaluates the result value.
func TestFDB_MultiAggregateIntersection_Filtered(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "magi",
		"CREATE TABLE mitems(id BIGINT, cat STRING, price BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX m_cnt_by_cat AS SELECT COUNT(*) FROM mitems GROUP BY cat "+
			"CREATE INDEX m_sum_price_by_cat AS SELECT SUM(price) FROM mitems GROUP BY cat "+
			"CREATE INDEX m_sum_price_by_cat_nn AS SELECT COUNT(price) FROM mitems GROUP BY cat")
	for i, item := range []struct {
		cat   string
		price int
	}{
		{"books", 15},
		{"books", 25},
		{"books", 35},
		{"food", 5},
		{"food", 10},
		{"electronics", 100},
		{"electronics", 200},
		{"electronics", 300},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO mitems VALUES (%d, '%s', %d)", i+1, item.cat, item.price)); err != nil {
			t.Fatalf("INSERT %d: %v", i+1, err)
		}
	}

	const q = "SELECT cat, COUNT(*), SUM(price) FROM mitems WHERE cat = 'books' GROUP BY cat"

	// Prove the multi-aggregate intersection plan actually fires — otherwise
	// the correct counts below could be coming from a streaming-agg fallback.
	plan := testkit.ExplainVia(t, ctx, db, q)
	if !strings.Contains(plan, "Intersection(") {
		t.Fatalf("expected the multi-aggregate merge plan, got: %s", plan)
	}

	rows := testkit.CollectRows(t, db, q)
	if len(rows) != 1 {
		t.Fatalf("filtered multi-aggregate: want 1 row, got %d: %v", len(rows), rows)
	}
	if fmt.Sprintf("%v", rows[0][0]) != "books" {
		t.Errorf("group key: want books, got %v", rows[0][0])
	}
	if toInt64(rows[0][1]) != 3 {
		t.Errorf("COUNT(*): want 3, got %v", rows[0][1])
	}
	if toInt64(rows[0][2]) != 75 {
		t.Errorf("SUM(price): want 75 (15+25+35), got %v", rows[0][2])
	}

	// Unfiltered multi-aggregate over every group must also be correct.
	// (The cost model may pick a streaming aggregation here rather than the
	// intersection — the selective WHERE filter is what tips the cost toward
	// the intersection above. We only pin correctness, not plan shape.)
	const qAll = "SELECT cat, COUNT(*), SUM(price) FROM mitems GROUP BY cat ORDER BY cat"
	allRows := testkit.CollectRows(t, db, qAll)
	if len(allRows) != 3 {
		t.Fatalf("unfiltered multi-aggregate: want 3 rows, got %d: %v", len(allRows), allRows)
	}
	want := []struct {
		cat   string
		count int64
		sum   int64
	}{
		{"books", 3, 75},
		{"electronics", 3, 600},
		{"food", 2, 15},
	}
	for i, w := range want {
		if fmt.Sprintf("%v", allRows[i][0]) != w.cat {
			t.Errorf("row %d cat: want %s, got %v", i, w.cat, allRows[i][0])
		}
		if toInt64(allRows[i][1]) != w.count {
			t.Errorf("row %d count: want %d, got %v", i, w.count, allRows[i][1])
		}
		if toInt64(allRows[i][2]) != w.sum {
			t.Errorf("row %d sum: want %d, got %v", i, w.sum, allRows[i][2])
		}
	}
}

// TestFDB_AggregateIndexWithUpdate — aggregate index correctness after updates
func TestFDB_AggregateIndexWithUpdate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aiupd",
		"CREATE TABLE ai_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX cnt_grp AS SELECT COUNT(*) FROM ai_t GROUP BY grp "+
			"CREATE INDEX sum_grp AS SELECT SUM(val) FROM ai_t GROUP BY grp")
	if _, err := db.ExecContext(ctx, `INSERT INTO ai_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'B', 30)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("initial_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*), SUM(val) FROM ai_t GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 2 || toInt64(rows[0][2]) != 30 {
			t.Errorf("A: want cnt=2,sum=30, got %v,%v", rows[0][1], rows[0][2])
		}
	})

	t.Run("after_update_value", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE ai_t SET val = 100 WHERE id = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT grp, SUM(val) FROM ai_t WHERE grp = 'A' GROUP BY grp")
		if toInt64(rows[0][1]) != 120 {
			t.Errorf("A sum after update: 100+20=120, got %v", rows[0][1])
		}
	})

	t.Run("after_delete", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM ai_t WHERE id = 2"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*) FROM ai_t WHERE grp = 'A' GROUP BY grp")
		if toInt64(rows[0][1]) != 1 {
			t.Errorf("A count after delete: want 1, got %v", rows[0][1])
		}
	})

	t.Run("after_insert_new_group", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO ai_t VALUES (4, 'C', 50)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*), SUM(val) FROM ai_t GROUP BY grp ORDER BY grp")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups now, got %d", len(rows))
		}
	})
}

// TestFDB_DeleteAndReverifyAggregateIndex — delete rows and verify aggregate index stays correct
func TestFDB_DeleteAndReverifyAggregateIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "drai",
		"CREATE TABLE drai_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX drai_cnt AS SELECT COUNT(*) FROM drai_t GROUP BY grp "+
			"CREATE INDEX drai_sum AS SELECT SUM(val) FROM drai_t GROUP BY grp")
	if _, err := db.ExecContext(ctx, `INSERT INTO drai_t VALUES
		(1, 'X', 10), (2, 'X', 20), (3, 'Y', 30), (4, 'Y', 40), (5, 'X', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("initial_state", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*), SUM(val) FROM drai_t GROUP BY grp ORDER BY grp")
		if toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 80 {
			t.Errorf("X: want cnt=3 sum=80, got %v %v", rows[0][1], rows[0][2])
		}
	})

	t.Run("delete_and_verify", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM drai_t WHERE id IN (1, 3)"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*), SUM(val) FROM drai_t GROUP BY grp ORDER BY grp")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 2 || toInt64(rows[0][2]) != 70 {
			t.Errorf("X after delete: want cnt=2 sum=70 (20+50), got %v %v", rows[0][1], rows[0][2])
		}
		if toInt64(rows[1][1]) != 1 || toInt64(rows[1][2]) != 40 {
			t.Errorf("Y after delete: want cnt=1 sum=40, got %v %v", rows[1][1], rows[1][2])
		}
	})
}
