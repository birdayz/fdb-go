package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_SelectWithMultipleAggregatesAndWhere — multiple aggregates with WHERE
func TestFDB_SelectWithMultipleAggregatesAndWhere(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "smaw", "CREATE TABLE smaw_t(id BIGINT, status STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO smaw_t VALUES
		(1, 'active', 100), (2, 'active', 200), (3, 'inactive', 50),
		(4, 'active', 150), (5, 'inactive', 75)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("all_aggregates_filtered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount)
			FROM smaw_t WHERE status = 'active'
		`)
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("COUNT active = 3, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 450 {
			t.Errorf("SUM active = 100+200+150 = 450, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 100 {
			t.Errorf("MIN active = 100, got %v", rows[0][2])
		}
		if toInt64(rows[0][3]) != 200 {
			t.Errorf("MAX active = 200, got %v", rows[0][3])
		}
	})
}

// TestFDB_GroupByCountWithFilter — GROUP BY + COUNT + WHERE + ORDER BY
func TestFDB_GroupByCountWithFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbcwf", "CREATE TABLE gbcwf_t(id BIGINT, cat STRING, active BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbcwf_t VALUES
		(1, 'A', 1), (2, 'A', 0), (3, 'B', 1), (4, 'B', 1),
		(5, 'C', 0), (6, 'C', 1), (7, 'A', 1)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_active_per_category", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT cat, COUNT(*) FROM gbcwf_t
			WHERE active = 1
			GROUP BY cat ORDER BY COUNT(*) DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" || toInt64(rows[0][1]) != 2 {
			t.Errorf("first: A with 2 active, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_GroupByWithMaxAndOrderBy — GROUP BY + MAX + ORDER BY MAX
func TestFDB_GroupByWithMaxAndOrderBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbmob", "CREATE TABLE gbmob_t(id BIGINT, team STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbmob_t VALUES
		(1, 'red', 80), (2, 'red', 95), (3, 'blue', 70), (4, 'blue', 85), (5, 'green', 90)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("max_score_by_team", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT team, MAX(score) FROM gbmob_t GROUP BY team ORDER BY MAX(score) DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "red" || toInt64(rows[0][1]) != 95 {
			t.Errorf("top: red 95, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_SelectWithCaseAndAggregate — CASE in SELECT with aggregate
func TestFDB_SelectWithCaseAndAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "scaga", "CREATE TABLE scaga_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO scaga_t VALUES (1, 10), (2, 50), (3, 30), (4, 80), (5, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("sum_case_when", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(CASE WHEN val > 25 THEN val ELSE 0 END) FROM scaga_t
		`)
		if toInt64(rows[0][0]) != 160 {
			t.Errorf("SUM(CASE WHEN val>25) = 50+30+80 = 160, got %v", rows[0][0])
		}
	})

	t.Run("count_case_categories", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT
				CASE WHEN val >= 50 THEN 'high' ELSE 'low' END AS tier,
				COUNT(*)
			FROM scaga_t
			GROUP BY CASE WHEN val >= 50 THEN 'high' ELSE 'low' END
		`)
		t.Logf("CASE tier GROUP BY: %v", rows)
		if len(rows) < 2 {
			t.Fatalf("want at least 2 tiers, got %d", len(rows))
		}
	})

	t.Run("max_of_case", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT MAX(CASE WHEN val < 50 THEN val ELSE 0 END) FROM scaga_t
		`)
		if toInt64(rows[0][0]) != 30 {
			t.Errorf("MAX(CASE val<50 THEN val) = 30, got %v", rows[0][0])
		}
	})
}

// TestFDB_CombinedWhereAndGroupBy — WHERE filter before GROUP BY
func TestFDB_CombinedWhereAndGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwagb", "CREATE TABLE cwagb_t(id BIGINT, region STRING, status STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cwagb_t VALUES
		(1, 'east', 'active', 100), (2, 'east', 'inactive', 50),
		(3, 'west', 'active', 200), (4, 'west', 'active', 150),
		(5, 'east', 'active', 75)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("where_active_group_by_region", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT region, SUM(amount) AS total
			FROM cwagb_t WHERE status = 'active'
			GROUP BY region ORDER BY total DESC
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "west" || toInt64(rows[0][1]) != 350 {
			t.Errorf("west active: 200+150=350, got %v %v", rows[0][0], rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "east" || toInt64(rows[1][1]) != 175 {
			t.Errorf("east active: 100+75=175, got %v %v", rows[1][0], rows[1][1])
		}
	})
}

// TestFDB_AggregateMultipleGroupKeys — GROUP BY two columns
func TestFDB_AggregateMultipleGroupKeys(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "amgk",
		"CREATE TABLE amgk_t(id BIGINT, dept STRING, role STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO amgk_t VALUES (1,'eng','dev',100),(2,'eng','dev',120),(3,'eng','qa',90),(4,'sales','rep',80),(5,'sales','rep',85),(6,'sales','mgr',150)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_per_dept_role", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT dept, role, COUNT(*) FROM amgk_t GROUP BY dept, role ORDER BY dept, role")
		if len(rows) != 4 {
			t.Fatalf("want 4 groups, got %d", len(rows))
		}
		type grp struct {
			dept, role string
			cnt        int64
		}
		want := []grp{
			{"eng", "dev", 2}, {"eng", "qa", 1}, {"sales", "mgr", 1}, {"sales", "rep", 2},
		}
		for i, w := range want {
			d := fmt.Sprintf("%v", rows[i][0])
			r := fmt.Sprintf("%v", rows[i][1])
			c := toInt64(rows[i][2])
			if d != w.dept || r != w.role || c != w.cnt {
				t.Errorf("row %d: want %v, got [%s %s %d]", i, w, d, r, c)
			}
		}
	})

	t.Run("sum_per_dept_role", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT dept, role, SUM(salary) FROM amgk_t GROUP BY dept, role ORDER BY dept, role")
		wantSum := []int64{220, 90, 150, 165}
		for i, w := range wantSum {
			if toInt64(rows[i][2]) != w {
				t.Errorf("row %d: want sum %d, got %v", i, w, rows[i][2])
			}
		}
	})
}

// TestFDB_LargerDatasetAggregate — 50-row dataset with aggregate queries
func TestFDB_LargerDatasetAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "lda",
		"CREATE TABLE lda_t(id BIGINT, bucket BIGINT, val BIGINT, PRIMARY KEY(id))")

	var vals strings.Builder
	for i := 1; i <= 50; i++ {
		if i > 1 {
			vals.WriteString(",")
		}
		bucket := (i-1)%5 + 1
		fmt.Fprintf(&vals, "(%d,%d,%d)", i, bucket, i*10)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO lda_t VALUES "+vals.String()); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("total_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM lda_t")
		if toInt64(rows[0][0]) != 50 {
			t.Errorf("want 50, got %v", rows[0][0])
		}
	})

	t.Run("sum_all", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM lda_t")
		// sum of 10+20+...+500 = 10*(1+2+...+50) = 10*1275 = 12750
		if toInt64(rows[0][0]) != 12750 {
			t.Errorf("want 12750, got %v", rows[0][0])
		}
	})

	t.Run("count_per_bucket", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT bucket, COUNT(*) FROM lda_t GROUP BY bucket ORDER BY bucket")
		if len(rows) != 5 {
			t.Fatalf("want 5 buckets, got %d", len(rows))
		}
		for _, r := range rows {
			if toInt64(r[1]) != 10 {
				t.Errorf("bucket %v: want count 10, got %v", r[0], r[1])
			}
		}
	})

	t.Run("min_max_per_bucket", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT bucket, MIN(val), MAX(val) FROM lda_t GROUP BY bucket ORDER BY bucket")
		if len(rows) != 5 {
			t.Fatalf("want 5 buckets, got %d", len(rows))
		}
		// bucket 1: ids 1,6,11,16,21,26,31,36,41,46 → vals 10,60,110,...,460
		if toInt64(rows[0][1]) != 10 || toInt64(rows[0][2]) != 460 {
			t.Errorf("bucket 1: want min=10 max=460, got min=%v max=%v", rows[0][1], rows[0][2])
		}
	})

	t.Run("top_5_by_val", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM lda_t ORDER BY val DESC LIMIT 5")
		if len(rows) != 5 {
			t.Fatalf("want 5 rows, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 500 {
			t.Errorf("top val: want 500, got %v", rows[0][1])
		}
		if toInt64(rows[4][1]) != 460 {
			t.Errorf("5th val: want 460, got %v", rows[4][1])
		}
	})

	t.Run("where_range_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM lda_t WHERE val >= 200 AND val <= 300")
		// vals 200,210,...,300 → 11 values
		if toInt64(rows[0][0]) != 11 {
			t.Errorf("want 11 rows in [200,300], got %v", rows[0][0])
		}
	})
}

// TestFDB_GroupByCoalesceCaseBucket — GROUP BY COALESCE for NULL regions + CASE bucketing
func TestFDB_GroupByCoalesceCaseBucket(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbcc",
		"CREATE TABLE gbcc_t(id BIGINT, region STRING, amount BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbcc_t VALUES
		(1,'east',100),(2,'west',200),(3,'east',150),(4,null,50),(5,null,75)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("coalesce_null_region", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COALESCE(region, 'unknown'), SUM(amount)
			FROM gbcc_t GROUP BY COALESCE(region, 'unknown')
			ORDER BY COALESCE(region, 'unknown')
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 groups, got %d: %v", len(rows), rows)
		}
		// east=250, unknown=125, west=200
		if fmt.Sprintf("%v", rows[0][0]) != "east" || toInt64(rows[0][1]) != 250 {
			t.Errorf("row 0: want [east 250], got %v", rows[0])
		}
	})

	t.Run("case_bucket", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT CASE WHEN amount >= 150 THEN 'high' ELSE 'low' END AS bucket, COUNT(*)
			FROM gbcc_t
			GROUP BY CASE WHEN amount >= 150 THEN 'high' ELSE 'low' END
			ORDER BY bucket
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 buckets, got %d: %v", len(rows), rows)
		}
		// high: 200,150 = 2; low: 100,50,75 = 3
		if fmt.Sprintf("%v", rows[0][0]) != "high" || toInt64(rows[0][1]) != 2 {
			t.Errorf("high: want 2, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "low" || toInt64(rows[1][1]) != 3 {
			t.Errorf("low: want 3, got %v", rows[1])
		}
	})
}

// TestFDB_GroupByWithOrderByNonAggColumn — ORDER BY group key column
func TestFDB_GroupByWithOrderByNonAggColumn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbon",
		"CREATE TABLE gbon_t(id BIGINT, city STRING, revenue BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbon_t VALUES
		(1,'berlin',100),(2,'munich',200),(3,'berlin',150),(4,'hamburg',300),(5,'munich',50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_city_asc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT city, SUM(revenue) FROM gbon_t GROUP BY city ORDER BY city ASC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "berlin" || toInt64(rows[0][1]) != 250 {
			t.Errorf("berlin: want 250, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "hamburg" || toInt64(rows[1][1]) != 300 {
			t.Errorf("hamburg: want 300, got %v", rows[1])
		}
		if fmt.Sprintf("%v", rows[2][0]) != "munich" || toInt64(rows[2][1]) != 250 {
			t.Errorf("munich: want 250, got %v", rows[2])
		}
	})

	t.Run("order_by_sum_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT city, SUM(revenue) FROM gbon_t GROUP BY city ORDER BY SUM(revenue) DESC
		`)
		// hamburg=300, berlin=250, munich=250
		if toInt64(rows[0][1]) != 300 {
			t.Errorf("top: want 300, got %v", rows[0][1])
		}
	})
}

// TestFDB_GroupByMultipleAggregatesAll — COUNT+SUM+MIN+MAX in single query
func TestFDB_GroupByMultipleAggregatesAll(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbma2",
		"CREATE TABLE gbma2_t(id BIGINT, grp STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbma2_t VALUES
		(1,'a',10),(2,'a',20),(3,'a',30),
		(4,'b',100),(5,'b',200)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("all_aggs_per_group", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, COUNT(*), SUM(val), MIN(val), MAX(val) FROM gbma2_t
			GROUP BY grp ORDER BY grp
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d", len(rows))
		}
		// a: cnt=3, sum=60, min=10, max=30
		if toInt64(rows[0][1]) != 3 || toInt64(rows[0][2]) != 60 || toInt64(rows[0][3]) != 10 || toInt64(rows[0][4]) != 30 {
			t.Errorf("a: want [3 60 10 30], got [%v %v %v %v]", rows[0][1], rows[0][2], rows[0][3], rows[0][4])
		}
		// b: cnt=2, sum=300, min=100, max=200
		if toInt64(rows[1][1]) != 2 || toInt64(rows[1][2]) != 300 || toInt64(rows[1][3]) != 100 || toInt64(rows[1][4]) != 200 {
			t.Errorf("b: want [2 300 100 200], got [%v %v %v %v]", rows[1][1], rows[1][2], rows[1][3], rows[1][4])
		}
	})
}

// TestFDB_GroupByBooleanColumnAgg — GROUP BY on boolean values
func TestFDB_GroupByBooleanColumnAgg(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gbba",
		"CREATE TABLE gbba_t(id BIGINT, active BOOLEAN, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gbba_t VALUES
		(1,true,10),(2,false,20),(3,true,30),(4,false,40),(5,true,50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("count_per_boolean", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT active, COUNT(*) FROM gbba_t GROUP BY active ORDER BY active")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d", len(rows))
		}
		// false=2, true=3
		if toInt64(rows[0][1]) != 2 {
			t.Errorf("false count: want 2, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 3 {
			t.Errorf("true count: want 3, got %v", rows[1][1])
		}
	})

	t.Run("sum_per_boolean", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT active, SUM(val) FROM gbba_t GROUP BY active ORDER BY active")
		// false: 20+40=60, true: 10+30+50=90
		if toInt64(rows[0][1]) != 60 || toInt64(rows[1][1]) != 90 {
			t.Errorf("want [60 90], got [%v %v]", rows[0][1], rows[1][1])
		}
	})
}

// TestFDB_GroupByWithLimitOnResult — GROUP BY then LIMIT on grouped results
func TestFDB_GroupByWithLimitOnResult(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gblr",
		"CREATE TABLE gblr_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gblr_t VALUES
		(1,'a',10),(2,'b',20),(3,'c',30),(4,'d',40),(5,'e',50),
		(6,'a',15),(7,'b',25),(8,'c',35)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("top_3_groups_by_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT cat, SUM(val) FROM gblr_t GROUP BY cat ORDER BY SUM(val) DESC LIMIT 3
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		// c:65, e:50, b:45, d:40, a:25 → top 3: c, e, b
		if toInt64(rows[0][1]) != 65 {
			t.Errorf("top sum: want 65, got %v", rows[0][1])
		}
	})

	t.Run("bottom_2_groups", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT cat, SUM(val) FROM gblr_t GROUP BY cat ORDER BY SUM(val) ASC LIMIT 2
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		// a:25, d:40 → bottom 2
		if toInt64(rows[0][1]) != 25 {
			t.Errorf("bottom sum: want 25, got %v", rows[0][1])
		}
	})
}

// TestFDB_GroupByOrderByCountDescLimit — GROUP BY with ORDER BY COUNT DESC
func TestFDB_GroupByOrderByCountDescLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "gocdl",
		"CREATE TABLE gocdl_t(id BIGINT, word STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO gocdl_t VALUES
		(1,'the'),(2,'a'),(3,'the'),(4,'is'),(5,'the'),(6,'a'),(7,'was'),(8,'the')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("most_frequent_word", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT word, COUNT(*) FROM gocdl_t GROUP BY word ORDER BY COUNT(*) DESC LIMIT 1
		`)
		if fmt.Sprintf("%v", rows[0][0]) != "the" || toInt64(rows[0][1]) != 4 {
			t.Errorf("want [the 4], got %v", rows[0])
		}
	})

	t.Run("all_words_ordered", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT word, COUNT(*) FROM gocdl_t GROUP BY word ORDER BY COUNT(*) DESC
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 distinct words, got %d", len(rows))
		}
		// the=4, a=2, is=1, was=1
		if toInt64(rows[0][1]) != 4 || toInt64(rows[1][1]) != 2 {
			t.Errorf("want counts [4 2 ...], got [%v %v ...]", rows[0][1], rows[1][1])
		}
	})
}
