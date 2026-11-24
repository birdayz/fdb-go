package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_IndexScanPatterns — queries that should use index scans
func TestFDB_IndexScanPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ixsc",
		"CREATE TABLE idx_t(id BIGINT, status STRING, score BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX status_idx ON idx_t (status)")
	for i := 1; i <= 20; i++ {
		status := "active"
		if i%3 == 0 {
			status = "inactive"
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO idx_t VALUES (%d, '%s', %d)", i, status, i*5)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	t.Run("index_eq_scan", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, score FROM idx_t WHERE status = 'inactive' ORDER BY id")
		if len(rows) != 6 {
			t.Fatalf("want 6 inactive (3,6,9,12,15,18), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("first inactive should be id=3, got %v", rows[0][0])
		}
	})

	t.Run("count_via_index", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM idx_t WHERE status = 'active'")
		if toInt64(rows[0][0]) != 14 {
			t.Errorf("active count should be 14, got %v", rows[0][0])
		}
	})

	t.Run("aggregate_with_index_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT status, SUM(score), MIN(score), MAX(score) FROM idx_t GROUP BY status ORDER BY status")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d", len(rows))
		}
	})

	t.Run("update_indexed_column", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE idx_t SET status = 'archived' WHERE id = 3"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM idx_t WHERE status = 'inactive'")
		if toInt64(rows[0][0]) != 5 {
			t.Errorf("after archiving id=3: inactive should be 5, got %v", rows[0][0])
		}
		rows = testkit.CollectRows(t, db, "SELECT COUNT(*) FROM idx_t WHERE status = 'archived'")
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("archived should be 1, got %v", rows[0][0])
		}
	})
}

// TestFDB_MultiColumnIndex — queries using multi-column indexes
func TestFDB_MultiColumnIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mcidx",
		"CREATE TABLE mci_t(id BIGINT, a STRING, b BIGINT, c STRING, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_ab ON mci_t (a, b)")
	if _, err := db.ExecContext(ctx, `INSERT INTO mci_t VALUES
		(1, 'x', 10, 'p'), (2, 'x', 20, 'q'), (3, 'x', 30, 'r'),
		(4, 'y', 10, 's'), (5, 'y', 20, 't'),
		(6, 'z', 50, 'u')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("prefix_eq_scan", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, b FROM mci_t WHERE a = 'x' ORDER BY b")
		if len(rows) != 3 {
			t.Fatalf("want 3 for a='x', got %d", len(rows))
		}
	})

	t.Run("full_eq_scan", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM mci_t WHERE a = 'y' AND b = 20")
		if len(rows) != 1 || toInt64(rows[0][0]) != 5 {
			t.Errorf("want id=5, got %v", rows)
		}
	})

	t.Run("prefix_eq_with_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT a, COUNT(*), SUM(b) FROM mci_t GROUP BY a ORDER BY a")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d", len(rows))
		}
		if toInt64(rows[0][2]) != 60 {
			t.Errorf("x sum = 10+20+30 = 60, got %v", rows[0][2])
		}
	})

	t.Run("range_on_second_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM mci_t WHERE a = 'x' AND b > 15 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (b=20,30), got %d", len(rows))
		}
	})
}

// TestFDB_DistinctPatterns — DISTINCT queries from Java patterns
func TestFDB_DistinctPatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sdist", "CREATE TABLE sd_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sd_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 10), (4, 'C', 30), (5, 'B', 20), (6, 'A', 40)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("distinct_single_column", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT DISTINCT cat FROM sd_t ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3 distinct cats (A,B,C), got %d: %v", len(rows), rows)
		}
	})

	t.Run("distinct_two_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT DISTINCT cat, val FROM sd_t ORDER BY cat, val")
		if len(rows) != 4 {
			t.Fatalf("want 4 distinct (A,10),(A,40),(B,20),(C,30), got %d: %v", len(rows), rows)
		}
	})

	t.Run("count_distinct_via_derived_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM (SELECT DISTINCT cat FROM sd_t) AS d")
		if err != nil {
			t.Logf("COUNT over DISTINCT derived: %v", err)
		}
	})
}

// TestFDB_SelectWithAllColumnsAndFilter — SELECT * with WHERE
func TestFDB_SelectWithAllColumnsAndFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "sacf", "CREATE TABLE sacf_t(id BIGINT, name STRING, age BIGINT, city STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sacf_t VALUES
		(1, 'alice', 30, 'nyc'), (2, 'bob', 25, 'la'),
		(3, 'charlie', 35, 'nyc'), (4, 'david', 28, 'sf')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("select_star_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM sacf_t WHERE city = 'nyc' ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 NYC residents, got %d", len(rows))
		}
		if len(rows[0]) != 4 {
			t.Errorf("SELECT * should return 4 columns, got %d", len(rows[0]))
		}
	})

	t.Run("select_star_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM sacf_t WHERE age > 27")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3 (30, 35, 28), got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectCountDistinctViaGroupBy — count distinct via GROUP BY
func TestFDB_SelectCountDistinctViaGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scdvg", "CREATE TABLE scdvg_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO scdvg_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 30), (4, 'C', 40), (5, 'B', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT cat, COUNT(*) FROM scdvg_t GROUP BY cat ORDER BY cat")
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" || toInt64(rows[0][1]) != 2 {
			t.Errorf("A: want 2, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_100RowDataset — larger dataset to stress aggregation
func TestFDB_100RowDataset(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "r100",
		"CREATE TABLE r100_t(id BIGINT, grp BIGINT, val BIGINT, PRIMARY KEY(id))")

	var b strings.Builder
	for i := 1; i <= 100; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d,%d,%d)", i, (i-1)%10+1, i)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO r100_t VALUES "+b.String()); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("total_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM r100_t")
		if toInt64(rows[0][0]) != 100 {
			t.Errorf("want 100, got %v", rows[0][0])
		}
	})

	t.Run("total_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM r100_t")
		// 1+2+...+100 = 5050
		if toInt64(rows[0][0]) != 5050 {
			t.Errorf("want 5050, got %v", rows[0][0])
		}
	})

	t.Run("group_count_10_each", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*) FROM r100_t GROUP BY grp ORDER BY grp")
		if len(rows) != 10 {
			t.Fatalf("want 10 groups, got %d", len(rows))
		}
		for _, r := range rows {
			if toInt64(r[1]) != 10 {
				t.Errorf("grp %v: want 10, got %v", r[0], r[1])
			}
		}
	})

	t.Run("top_10_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val FROM r100_t ORDER BY val DESC LIMIT 10")
		for i, w := range []int64{100, 99, 98, 97, 96, 95, 94, 93, 92, 91} {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: want %d, got %v", i, w, rows[i][0])
			}
		}
	})
}

// TestFDB_SelectStarFromTable — SELECT * returns all columns
func TestFDB_SelectStarFromTable(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ssft",
		"CREATE TABLE ssft_t(id BIGINT, name STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO ssft_t VALUES (1,'alice',90),(2,'bob',80)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("star_returns_all_cols", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM ssft_t ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		if len(rows[0]) < 3 {
			t.Fatalf("want >=3 columns, got %d", len(rows[0]))
		}
		if toInt64(rows[0][0]) != 1 || fmt.Sprintf("%v", rows[0][1]) != "alice" || toInt64(rows[0][2]) != 90 {
			t.Errorf("row 0: want [1 alice 90], got %v", rows[0])
		}
	})

	t.Run("star_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM ssft_t WHERE score > 85")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
	})
}

// TestFDB_200RowDataset — stress test with 200 rows
func TestFDB_200RowDataset(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "r200",
		"CREATE TABLE r200_t(id BIGINT, grp BIGINT, val BIGINT, PRIMARY KEY(id))")

	for batch := 0; batch < 4; batch++ {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			id := batch*50 + i + 1
			fmt.Fprintf(&b, "(%d,%d,%d)", id, (id-1)%20+1, id)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO r200_t VALUES "+b.String()); err != nil {
			t.Fatalf("INSERT batch %d: %v", batch, err)
		}
	}

	t.Run("count_200", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM r200_t")
		if toInt64(rows[0][0]) != 200 {
			t.Errorf("want 200, got %v", rows[0][0])
		}
	})

	t.Run("sum_200", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM r200_t")
		// 1+2+...+200 = 20100
		if toInt64(rows[0][0]) != 20100 {
			t.Errorf("want 20100, got %v", rows[0][0])
		}
	})

	t.Run("20_groups_10_each", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT grp, COUNT(*) FROM r200_t GROUP BY grp ORDER BY grp")
		if len(rows) != 20 {
			t.Fatalf("want 20 groups, got %d", len(rows))
		}
		for _, r := range rows {
			if toInt64(r[1]) != 10 {
				t.Errorf("grp %v: want 10, got %v", r[0], r[1])
			}
		}
	})

	t.Run("top_1", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT val FROM r200_t ORDER BY val DESC LIMIT 1")
		if toInt64(rows[0][0]) != 200 {
			t.Errorf("want 200, got %v", rows[0][0])
		}
	})
}

// TestFDB_EmptyTableOps — operations on empty table
func TestFDB_EmptyTableOps(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "eto",
		"CREATE TABLE eto_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")

	t.Run("select_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM eto_t")
		if len(rows) != 0 {
			t.Errorf("want 0 rows, got %d", len(rows))
		}
	})

	t.Run("count_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM eto_t")
		if toInt64(rows[0][0]) != 0 {
			t.Errorf("want 0, got %v", rows[0][0])
		}
	})

	t.Run("delete_empty", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM eto_t WHERE id = 1")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("want 0 deleted, got %d", n)
		}
	})

	t.Run("update_empty", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "UPDATE eto_t SET val = 999")
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("want 0 updated, got %d", n)
		}
	})
}
