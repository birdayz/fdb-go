package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_UnionWithAggExpr(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "unagg",
		"CREATE TABLE t1 (id BIGINT, grp STRING, val BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, grp STRING, val BIGINT, PRIMARY KEY (id))")

	for _, r := range []struct {
		id int
		g  string
		v  int
	}{
		{1, "A", 10}, {2, "A", 20}, {3, "B", 30},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t1 VALUES (%d, '%s', %d)", r.id, r.g, r.v))
	}
	for _, r := range []struct {
		id int
		g  string
		v  int
	}{
		{1, "A", 100}, {2, "B", 200},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, '%s', %d)", r.id, r.g, r.v))
	}

	t.Run("union_all_then_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT grp, SUM(val) AS total
			 FROM (SELECT grp, val FROM t1 UNION ALL SELECT grp, val FROM t2) combined
			 GROUP BY grp
			 ORDER BY grp`)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %v", len(rows), rows)
		}
		if rows[0][0].(string) != "A" || rows[0][1].(int64) != 130 {
			t.Errorf("A: got %v, want [A, 130] (10+20+100)", rows[0])
		}
		if rows[1][0].(string) != "B" || rows[1][1].(int64) != 230 {
			t.Errorf("B: got %v, want [B, 230] (30+200)", rows[1])
		}
	})

	t.Run("separate_aggs_union", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT grp, SUM(val) FROM t1 GROUP BY grp
			 UNION ALL
			 SELECT grp, SUM(val) FROM t2 GROUP BY grp
			 ORDER BY grp`)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d: %v", len(rows), rows)
		}
	})

	_ = ctx
}

func TestFDB_UnionAllJavaPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "unionjava1",
		"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 10, 1)",
		"INSERT INTO t1 VALUES (2, 10, 2)",
		"INSERT INTO t1 VALUES (6, 20, 6)",
		"INSERT INTO t1 VALUES (7, 20, 7)",
		"INSERT INTO t2 VALUES (10, 100, 10)",
		"INSERT INTO t2 VALUES (20, 200, 20)",
		"INSERT INTO t2 VALUES (30, 300, 30)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("union_all_same_table", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1, col2 FROM t1 UNION ALL SELECT col1, col2 FROM t1 ORDER BY col2")
		if len(rows) != 8 {
			t.Fatalf("want 8, got %d", len(rows))
		}
	})

	t.Run("union_all_different_tables", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT col1 FROM t1 UNION ALL SELECT col1 FROM t2 ORDER BY col1")
		if len(rows) != 7 {
			t.Fatalf("want 7 (4+3), got %d", len(rows))
		}
	})

	t.Run("union_all_aggregate_over", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT SUM(a) AS a, SUM(b) AS b FROM (
				SELECT SUM(col1) AS a, COUNT(*) AS b FROM t1
				UNION ALL
				SELECT SUM(col1) AS a, COUNT(*) AS b FROM t2
			) AS x`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		wantA := int64(10 + 10 + 20 + 20 + 100 + 200 + 300)
		wantB := int64(4 + 3)
		if toInt64(rows[0][0]) != wantA {
			t.Errorf("SUM(a): want %d, got %v", wantA, rows[0][0])
		}
		if toInt64(rows[0][1]) != wantB {
			t.Errorf("SUM(b): want %d, got %v", wantB, rows[0][1])
		}
	})

	t.Run("union_all_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			`SELECT id, col1 FROM t1 WHERE col1 = 10
			 UNION ALL
			 SELECT id, col1 FROM t2 WHERE col1 = 200
			 ORDER BY id`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (2+1), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[1][0]) != 2 || toInt64(rows[2][0]) != 20 {
			t.Errorf("want ids (1,2,20), got (%v,%v,%v)", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	_ = ctx
}

// TestFDB_UnionAllEdgeCases — Java union.yamsql edge cases
func TestFDB_UnionAllEdgeCases(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uaec",
		"CREATE TABLE u1(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE u2(id BIGINT, col1 BIGINT, col2 BIGINT, col3 BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO u1 VALUES (1, 10, 1), (2, 10, 2), (6, 20, 6), (7, 20, 7)`); err != nil {
		t.Fatalf("INSERT u1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO u2 VALUES
		(1, 1, 1, 100), (2, 1, 1, 1), (3, 1, 2, 2), (4, 1, 2, 200),
		(5, 2, 1, 200), (6, 2, 1, 3), (7, 2, 1, 400), (8, 2, 1, 400), (9, 2, 1, 400)
	`); err != nil {
		t.Fatalf("INSERT u2: %v", err)
	}

	t.Run("union_all_self", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT col1, col2 FROM u1 UNION ALL SELECT col1, col2 FROM u1")
		if len(rows) != 8 {
			t.Fatalf("want 8 (4+4 dupes), got %d", len(rows))
		}
	})

	t.Run("union_all_star_self", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM u1 UNION ALL SELECT * FROM u1")
		if len(rows) != 8 {
			t.Fatalf("want 8 (4+4 dupes), got %d", len(rows))
		}
	})

	t.Run("union_all_with_alias", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id AS w, col1 AS x, col2 AS y FROM u1 UNION ALL SELECT * FROM u1")
		if len(rows) != 8 {
			t.Fatalf("want 8, got %d", len(rows))
		}
	})

	t.Run("union_all_column_count_mismatch", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT col1, col2 FROM u1 UNION ALL SELECT col1 FROM u1")
		if err == nil {
			t.Errorf("expected error for column count mismatch, got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("union_without_all_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT col1, col2 FROM u1 UNION SELECT col1, col2 FROM u1")
		if err == nil {
			t.Errorf("expected error for UNION (without ALL), got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("aggregate_over_union", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(a) AS total_a, SUM(b) AS total_b FROM (
				SELECT SUM(col1) AS a, COUNT(*) AS b FROM u1
				UNION ALL
				SELECT SUM(col1) AS a, COUNT(*) AS b FROM u2
			) AS x
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 13 {
			t.Errorf("total_b should be 4+9=13, got %v", rows[0][1])
		}
	})

	t.Run("union_all_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, col1 FROM u1 WHERE col1 = 10 UNION ALL SELECT id, col1 FROM u1 WHERE col1 = 20")
		if len(rows) != 4 {
			t.Fatalf("want 4 (2+2), got %d", len(rows))
		}
	})
}

// TestFDB_SetOperationErrors — INTERSECT/EXCEPT should error
func TestFDB_SetOperationErrors(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "setop", "CREATE TABLE sop(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO sop VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("intersect_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM sop INTERSECT SELECT id FROM sop")
		if err == nil {
			t.Errorf("expected error for INTERSECT, got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("except_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM sop EXCEPT SELECT id FROM sop")
		if err == nil {
			t.Errorf("expected error for EXCEPT, got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})
}

// TestFDB_UnionAllWithOrderBy — UNION ALL followed by ORDER BY
func TestFDB_UnionAllWithOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uaob",
		"CREATE TABLE ua1(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE ua2(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ua1 VALUES (1, 100), (2, 200)"); err != nil {
		t.Fatalf("INSERT ua1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ua2 VALUES (3, 50), (4, 150)"); err != nil {
		t.Fatalf("INSERT ua2: %v", err)
	}

	t.Run("union_all_order_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM ua1 UNION ALL SELECT id, val FROM ua2 ORDER BY val")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 50 {
			t.Errorf("first val should be 50, got %v", rows[0][1])
		}
		if toInt64(rows[3][1]) != 200 {
			t.Errorf("last val should be 200, got %v", rows[3][1])
		}
	})

	t.Run("union_all_order_by_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM ua1 UNION ALL SELECT id, val FROM ua2 ORDER BY val DESC")
		if len(rows) != 4 {
			t.Fatalf("want 4, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 200 {
			t.Errorf("first val DESC should be 200, got %v", rows[0][1])
		}
	})

	t.Run("union_all_with_limit", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM ua1 UNION ALL SELECT id, val FROM ua2 ORDER BY val LIMIT 2")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 50 || toInt64(rows[1][1]) != 100 {
			t.Errorf("want 50,100 got %v,%v", rows[0][1], rows[1][1])
		}
	})
}

// TestFDB_UnionAllThreeLeg — UNION ALL with three legs
func TestFDB_UnionAllThreeLeg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ua3l",
		"CREATE TABLE u3a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE u3b(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE u3c(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO u3a VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT u3a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO u3b VALUES (3, 30), (4, 40)"); err != nil {
		t.Fatalf("INSERT u3b: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO u3c VALUES (5, 50)"); err != nil {
		t.Fatalf("INSERT u3c: %v", err)
	}

	t.Run("three_way_union_all", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM u3a
			UNION ALL SELECT id, val FROM u3b
			UNION ALL SELECT id, val FROM u3c
			ORDER BY id
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5 (2+2+1), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 1 || toInt64(rows[4][0]) != 5 {
			t.Errorf("want ids 1..5, got first=%v last=%v", rows[0][0], rows[4][0])
		}
	})

	t.Run("aggregate_over_three_way", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(val), COUNT(*) FROM (
				SELECT val FROM u3a
				UNION ALL SELECT val FROM u3b
				UNION ALL SELECT val FROM u3c
			) AS combined
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 150 {
			t.Errorf("SUM=10+20+30+40+50=150, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 5 {
			t.Errorf("COUNT=5, got %v", rows[0][1])
		}
	})
}

// TestFDB_UnionAllWithDifferentWheres — UNION ALL where each leg has different WHERE
func TestFDB_UnionAllWithDifferentWheres(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uadw", "CREATE TABLE uadw_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO uadw_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 30), (4, 'C', 40), (5, 'B', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("union_different_filters", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM uadw_t WHERE cat = 'A'
			UNION ALL
			SELECT id, val FROM uadw_t WHERE val > 40
			ORDER BY id
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (A:1,3 + val>40:5), got %d: %v", len(rows), rows)
		}
	})

	t.Run("union_count_different_filters", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM (
				SELECT id FROM uadw_t WHERE cat = 'A'
				UNION ALL
				SELECT id FROM uadw_t WHERE cat = 'B'
			) AS combined
		`)
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("A(2) + B(2) = 4, got %v", rows[0][0])
		}
	})
}

// TestFDB_UnionAllWithAggregatePerLeg — each UNION ALL leg has its own aggregate
func TestFDB_UnionAllWithAggregatePerLeg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uaapl",
		"CREATE TABLE uaa_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE uaa_b(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO uaa_a VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO uaa_b VALUES (4, 40), (5, 50)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("aggregate_per_leg_no_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*), SUM(val) FROM uaa_a
			UNION ALL
			SELECT COUNT(*), SUM(val) FROM uaa_b
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		t.Logf("per-leg aggregates: %v", rows)
	})

	t.Run("sum_all_values_via_union", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(val), COUNT(*) FROM (
				SELECT val FROM uaa_a UNION ALL SELECT val FROM uaa_b
			) AS combined
		`)
		if toInt64(rows[0][0]) != 150 {
			t.Errorf("SUM all = 10+20+30+40+50 = 150, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 5 {
			t.Errorf("COUNT all = 5, got %v", rows[0][1])
		}
	})
}

// TestFDB_UnionAllThreeWayAggregate — 3-way UNION ALL with aggregates
func TestFDB_UnionAllThreeWayAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "u3wa",
		"CREATE TABLE u3wa_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE u3wa_b(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE u3wa_c(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO u3wa_a VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO u3wa_b VALUES (3, 30)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO u3wa_c VALUES (4, 40), (5, 50), (6, 60)"); err != nil {
		t.Fatalf("INSERT c: %v", err)
	}

	t.Run("three_way_count_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*), SUM(val) FROM (
				SELECT val FROM u3wa_a
				UNION ALL SELECT val FROM u3wa_b
				UNION ALL SELECT val FROM u3wa_c
			) AS all_data
		`)
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("COUNT = 2+1+3 = 6, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 210 {
			t.Errorf("SUM = 10+20+30+40+50+60 = 210, got %v", rows[0][1])
		}
	})
}

// TestFDB_UnionAllOrderByLimit — UNION ALL with ORDER BY and LIMIT
func TestFDB_UnionAllOrderByLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uaol",
		"CREATE TABLE uaol_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE uaol_b(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO uaol_a VALUES (1,10),(2,30),(3,50)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO uaol_b VALUES (4,20),(5,40),(6,60)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("union_order_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM uaol_a
			UNION ALL
			SELECT id, val FROM uaol_b
			ORDER BY val
		`)
		if len(rows) != 6 {
			t.Fatalf("want 6 rows, got %d", len(rows))
		}
		wantVals := []int64{10, 20, 30, 40, 50, 60}
		for i, w := range wantVals {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want val %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("union_order_limit_3", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM uaol_a
			UNION ALL
			SELECT id, val FROM uaol_b
			ORDER BY val DESC
			LIMIT 3
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		wantVals := []int64{60, 50, 40}
		for i, w := range wantVals {
			if toInt64(rows[i][1]) != w {
				t.Errorf("row %d: want val %d, got %v", i, w, rows[i][1])
			}
		}
	})

	t.Run("union_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM (
				SELECT val FROM uaol_a
				UNION ALL
				SELECT val FROM uaol_b
			) u
		`)
		if toInt64(rows[0][0]) != 6 {
			t.Errorf("want 6, got %v", rows[0][0])
		}
	})
}

// TestFDB_UnionAllDifferentFilters — UNION ALL with different WHERE on same table
func TestFDB_UnionAllDifferentFilters(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "uadf",
		"CREATE TABLE uadf_t(id BIGINT, status STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO uadf_t VALUES
		(1,'a',10),(2,'b',20),(3,'a',30),(4,'c',40),(5,'b',50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("union_a_and_b", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM uadf_t WHERE status = 'a'
			UNION ALL
			SELECT id, val FROM uadf_t WHERE status = 'b'
			ORDER BY id
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 rows, got %d", len(rows))
		}
		wantIds := []int64{1, 2, 3, 5}
		for i, w := range wantIds {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want id %d, got %v", i, w, rows[i][0])
			}
		}
	})

	t.Run("sum_over_union", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(val) FROM (
				SELECT val FROM uadf_t WHERE status = 'a'
				UNION ALL
				SELECT val FROM uadf_t WHERE status = 'b'
			) u
		`)
		// a: 10+30=40, b: 20+50=70 → total 110
		if toInt64(rows[0][0]) != 110 {
			t.Errorf("want 110, got %v", rows[0][0])
		}
	})
}

// TestFDB_UnionAllThreeBranches — UNION ALL with three SELECT branches
func TestFDB_UnionAllThreeBranches(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ua3b",
		"CREATE TABLE ua3b_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ua3b_t VALUES
		(1,'a',10),(2,'b',20),(3,'c',30),(4,'a',40),(5,'b',50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("three_branch_union", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT id, val FROM ua3b_t WHERE grp = 'a'
			UNION ALL
			SELECT id, val FROM ua3b_t WHERE grp = 'b'
			UNION ALL
			SELECT id, val FROM ua3b_t WHERE grp = 'c'
			ORDER BY id
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5 rows, got %d", len(rows))
		}
		for i := 0; i < 5; i++ {
			if toInt64(rows[i][0]) != int64(i+1) {
				t.Errorf("row %d: want id %d, got %v", i, i+1, rows[i][0])
			}
		}
	})

	t.Run("three_branch_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM (
				SELECT val FROM ua3b_t WHERE grp = 'a'
				UNION ALL
				SELECT val FROM ua3b_t WHERE grp = 'b'
				UNION ALL
				SELECT val FROM ua3b_t WHERE grp = 'c'
			) u
		`)
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("want 5, got %v", rows[0][0])
		}
	})
}
