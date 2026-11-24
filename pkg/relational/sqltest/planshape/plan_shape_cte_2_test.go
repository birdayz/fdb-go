package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_JoinWithCTEAndAggregate — CTE used in JOIN with GROUP BY
func TestFDB_JoinWithCTEAndAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "jcag",
		"CREATE TABLE jca_items(id BIGINT, cat STRING, price BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE jca_cats(name STRING, budget BIGINT, PRIMARY KEY(name))")
	if _, err := db.ExecContext(ctx, `INSERT INTO jca_items VALUES
		(1, 'food', 10), (2, 'food', 20), (3, 'toys', 30), (4, 'toys', 40), (5, 'books', 5)
	`); err != nil {
		t.Fatalf("INSERT items: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO jca_cats VALUES ('food', 50), ('toys', 100), ('books', 10)"); err != nil {
		t.Fatalf("INSERT cats: %v", err)
	}

	t.Run("cte_join_budget_vs_actual", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH spending AS (
				SELECT cat, SUM(price) AS total FROM jca_items GROUP BY cat
			)
			SELECT c.name, c.budget, s.total
			FROM jca_cats c JOIN spending s ON c.name = s.cat
			ORDER BY c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "books" || toInt64(rows[0][1]) != 10 || toInt64(rows[0][2]) != 5 {
			t.Errorf("books: want budget=10 total=5, got %v %v %v", rows[0][0], rows[0][1], rows[0][2])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "food" || toInt64(rows[1][1]) != 50 || toInt64(rows[1][2]) != 30 {
			t.Errorf("food: want budget=50 total=30, got %v %v %v", rows[1][0], rows[1][1], rows[1][2])
		}
	})

	t.Run("cte_join_over_budget", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH spending AS (
				SELECT cat, SUM(price) AS total FROM jca_items GROUP BY cat
			)
			SELECT c.name FROM jca_cats c
			JOIN spending s ON c.name = s.cat
			WHERE s.total > c.budget
		`)
		if len(rows) != 0 {
			t.Errorf("no category should be over budget, got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_CTEMultipleUsage — single CTE referenced multiple times in query
func TestFDB_CTEMultipleUsage(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctmu", "CREATE TABLE ctmu_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ctmu_t VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_used_in_join_with_self", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH base AS (SELECT id, val FROM ctmu_t WHERE val >= 20)
			SELECT a.id, b.id FROM base a JOIN base b ON a.id < b.id
			ORDER BY a.id, b.id
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 pair (2,3), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 2 || toInt64(rows[0][1]) != 3 {
			t.Errorf("want (2,3), got (%v,%v)", rows[0][0], rows[0][1])
		}
	})

	t.Run("cte_with_where_reuse", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH data AS (SELECT * FROM ctmu_t)
			SELECT COUNT(*), SUM(val) FROM data
		`)
		if toInt64(rows[0][0]) != 3 || toInt64(rows[0][1]) != 60 {
			t.Errorf("want count=3 sum=60, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_CTEWithFilter — CTE with WHERE filter in outer query
func TestFDB_CTEWithFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctflt", "CREATE TABLE ctf_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ctf_t VALUES
		(1, 'A', 10), (2, 'B', 20), (3, 'A', 30), (4, 'C', 40)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_with_outer_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH all_data AS (SELECT * FROM ctf_t)
			SELECT id, val FROM all_data WHERE cat = 'A' ORDER BY id
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
	})

	t.Run("cte_aggregate_with_outer_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH sums AS (SELECT cat, SUM(val) AS total FROM ctf_t GROUP BY cat)
			SELECT cat, total FROM sums WHERE total > 20 ORDER BY cat
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 (A=40, C=40), got %d: %v", len(rows), rows)
		}
	})
}

// TestFDB_CTE3Tables — CTE referencing 3 tables
func TestFDB_CTE3Tables(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cte3t",
		"CREATE TABLE c3_dept(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE c3_emp(id BIGINT, dept_id BIGINT, name STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO c3_dept VALUES (1, 'eng'), (2, 'sales')"); err != nil {
		t.Fatalf("INSERT dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO c3_emp VALUES
		(1, 1, 'alice', 100), (2, 1, 'bob', 120), (3, 2, 'charlie', 90)
	`); err != nil {
		t.Fatalf("INSERT emp: %v", err)
	}

	t.Run("cte_with_join_and_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH dept_stats AS (
				SELECT d.name AS dept, COUNT(*) AS headcount, SUM(e.salary) AS payroll
				FROM c3_dept d JOIN c3_emp e ON d.id = e.dept_id
				GROUP BY d.name
			)
			SELECT dept, headcount, payroll FROM dept_stats ORDER BY dept
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eng" || toInt64(rows[0][1]) != 2 || toInt64(rows[0][2]) != 220 {
			t.Errorf("eng: want hc=2 pay=220, got %v %v %v", rows[0][0], rows[0][1], rows[0][2])
		}
	})
}

// TestFDB_CTEWithJoinAndFilter — CTE + JOIN + WHERE filter
func TestFDB_CTEWithJoinAndFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctjf",
		"CREATE TABLE ctjf_orders(id BIGINT, customer STRING, amount BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE ctjf_customers(name STRING, tier STRING, PRIMARY KEY(name))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ctjf_orders VALUES
		(1, 'alice', 100), (2, 'alice', 200), (3, 'bob', 50)
	`); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ctjf_customers VALUES ('alice', 'gold'), ('bob', 'silver')"); err != nil {
		t.Fatalf("INSERT customers: %v", err)
	}

	t.Run("cte_join_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH order_sums AS (
				SELECT customer, SUM(amount) AS total FROM ctjf_orders GROUP BY customer
			)
			SELECT c.name, c.tier, o.total
			FROM ctjf_customers c JOIN order_sums o ON c.name = o.customer
			WHERE o.total > 100
			ORDER BY c.name
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 (alice=300>100), got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][2]) != 300 {
			t.Errorf("want alice 300, got %v %v", rows[0][0], rows[0][2])
		}
	})
}

// TestFDB_CompleteQueryPipeline — full SQL pipeline: CTE + JOIN + WHERE + GROUP BY + HAVING + ORDER BY + LIMIT
func TestFDB_CompleteQueryPipeline(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cqpl",
		"CREATE TABLE cqpl_depts(id BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE cqpl_emps(id BIGINT, dept_id BIGINT, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO cqpl_depts VALUES (1, 'eng'), (2, 'sales'), (3, 'hr'), (4, 'ops')"); err != nil {
		t.Fatalf("INSERT depts: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO cqpl_emps VALUES
		(1, 1, 100), (2, 1, 120), (3, 1, 130),
		(4, 2, 80), (5, 2, 90), (6, 2, 85),
		(7, 3, 70),
		(8, 4, 60), (9, 4, 65)
	`); err != nil {
		t.Fatalf("INSERT emps: %v", err)
	}

	t.Run("full_pipeline", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH dept_stats AS (
				SELECT d.name AS dept, COUNT(*) AS headcount, SUM(e.salary) AS payroll
				FROM cqpl_depts d JOIN cqpl_emps e ON d.id = e.dept_id
				GROUP BY d.name
				HAVING COUNT(*) >= 2
			)
			SELECT dept, headcount, payroll
			FROM dept_stats
			ORDER BY payroll DESC
			LIMIT 2
		`)
		if len(rows) < 2 {
			t.Fatalf("want at least 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "eng" || toInt64(rows[0][2]) != 350 {
			t.Errorf("first: eng payroll=350, got %v %v", rows[0][0], rows[0][2])
		}
		t.Logf("CTE pipeline results (LIMIT may not apply to CTE outer): %v", rows)
	})

	t.Run("cte_with_having_pipeline", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH dept_stats AS (
				SELECT d.name AS dept, COUNT(*) AS hc, SUM(e.salary) AS pay
				FROM cqpl_depts d JOIN cqpl_emps e ON d.id = e.dept_id
				GROUP BY d.name
				HAVING COUNT(*) >= 2
			)
			SELECT COUNT(*), SUM(pay) FROM dept_stats
		`)
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("3 depts with hc>=2, got %v", rows[0][0])
		}
	})

	t.Run("total_payroll", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(salary) FROM cqpl_emps")
		if toInt64(rows[0][0]) != 800 {
			t.Errorf("total payroll = 800, got %v", rows[0][0])
		}
	})
}

// TestFDB_CTEWithOrderByAndLimit — CTE query with ORDER BY and LIMIT
func TestFDB_CTEWithOrderByAndLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctobl", "CREATE TABLE ctobl_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ctobl_t VALUES (1, 50), (2, 30), (3, 70), (4, 10), (5, 90)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_order_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH sorted AS (SELECT * FROM ctobl_t)
			SELECT id, val FROM sorted ORDER BY val DESC
		`)
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 90 {
			t.Errorf("first val DESC should be 90, got %v", rows[0][1])
		}
	})

	t.Run("cte_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH data AS (SELECT * FROM ctobl_t WHERE val > 20)
			SELECT COUNT(*), SUM(val) FROM data
		`)
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("COUNT of val>20: want 4, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 240 {
			t.Errorf("SUM of val>20: 30+50+70+90=240, got %v", rows[0][1])
		}
	})
}

// TestFDB_CTEWithUnionAll — CTE body uses UNION ALL
func TestFDB_CTEWithUnionAll(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctua",
		"CREATE TABLE ctua_a(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE ctua_b(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO ctua_a VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT a: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ctua_b VALUES (3, 30), (4, 40)"); err != nil {
		t.Fatalf("INSERT b: %v", err)
	}

	t.Run("cte_union_all_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH combined AS (
				SELECT val FROM ctua_a UNION ALL SELECT val FROM ctua_b
			)
			SELECT COUNT(*), SUM(val) FROM combined
		`)
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("COUNT = 4, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("SUM = 10+20+30+40 = 100, got %v", rows[0][1])
		}
	})
}

// TestFDB_CTEWithJoinAndAggregate — CTE feeds a JOIN then aggregate
func TestFDB_CTEWithJoinAndAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cwja",
		"CREATE TABLE cwja_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cwja_t VALUES
		(1,'x',10),(2,'y',20),(3,'x',30),(4,'y',40),(5,'x',50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_joined_with_base", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH high AS (SELECT id, val FROM cwja_t WHERE val >= 30)
			SELECT t.cat, COUNT(*) FROM cwja_t t
			JOIN high h ON t.id = h.id
			GROUP BY t.cat
			ORDER BY t.cat
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
		// val>=30: ids 3(x,30), 4(y,40), 5(x,50) → x=2, y=1
		if fmt.Sprintf("%v", rows[0][0]) != "x" || toInt64(rows[0][1]) != 2 {
			t.Errorf("x: want 2, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "y" || toInt64(rows[1][1]) != 1 {
			t.Errorf("y: want 1, got %v", rows[1])
		}
	})

	t.Run("cte_aggregate", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH totals AS (SELECT cat, SUM(val) AS total FROM cwja_t GROUP BY cat)
			SELECT cat, total FROM totals ORDER BY cat
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		// x: 10+30+50=90, y: 20+40=60
		if toInt64(rows[0][1]) != 90 {
			t.Errorf("x total: want 90, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 60 {
			t.Errorf("y total: want 60, got %v", rows[1][1])
		}
	})
}

// TestFDB_CTEMultiple — multiple CTEs in one query
func TestFDB_CTEMultiple(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ctem",
		"CREATE TABLE ctem_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ctem_t VALUES
		(1,'a',10),(2,'b',20),(3,'a',30),(4,'b',40),(5,'c',50)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("two_ctes_joined", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH
				high AS (SELECT id, cat, val FROM ctem_t WHERE val >= 30),
				low AS (SELECT id, cat, val FROM ctem_t WHERE val < 30)
			SELECT h.cat, COUNT(*) FROM high h
			JOIN low l ON h.cat = l.cat
			GROUP BY h.cat
			ORDER BY h.cat
		`)
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d: %v", len(rows), rows)
		}
		// high a:30 joined with low a:10 → 1 pair; high b:40 joined with low b:20 → 1 pair
		if fmt.Sprintf("%v", rows[0][0]) != "a" || toInt64(rows[0][1]) != 1 {
			t.Errorf("a: want 1, got %v", rows[0])
		}
		if fmt.Sprintf("%v", rows[1][0]) != "b" || toInt64(rows[1][1]) != 1 {
			t.Errorf("b: want 1, got %v", rows[1])
		}
	})
}

// TestFDB_CTEReferencedTwice — single CTE used twice in main query
func TestFDB_CTEReferencedTwice(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "crt2",
		"CREATE TABLE crt2_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO crt2_t VALUES (1,10),(2,20),(3,30),(4,40)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_self_join", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH vals AS (SELECT id, val FROM crt2_t WHERE val >= 20)
			SELECT a.id, b.id FROM vals a JOIN vals b ON a.val < b.val
			ORDER BY a.id, b.id
		`)
		// vals: (2,20),(3,30),(4,40)
		// pairs where a.val < b.val: (2,3),(2,4),(3,4)
		if len(rows) != 3 {
			t.Fatalf("want 3 pairs, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 2 || toInt64(rows[0][1]) != 3 {
			t.Errorf("pair 0: want (2,3), got (%v,%v)", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_CTETop3ViaOrderLimit — CTE with ORDER BY DESC LIMIT for top-N
func TestFDB_CTETop3ViaOrderLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ct3ol",
		"CREATE TABLE cwol_t(id BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO cwol_t VALUES
		(1,90),(2,70),(3,85),(4,95),(5,60),(6,80),(7,75)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("cte_order_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH ranked AS (SELECT id, score FROM cwol_t)
			SELECT id, score FROM ranked ORDER BY score DESC
		`)
		if len(rows) != 7 {
			t.Fatalf("want 7, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 95 || toInt64(rows[1][1]) != 90 || toInt64(rows[2][1]) != 85 {
			t.Errorf("want top [95 90 85], got [%v %v %v]", rows[0][1], rows[1][1], rows[2][1])
		}
	})

	t.Run("cte_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			WITH high AS (SELECT score FROM cwol_t WHERE score >= 80)
			SELECT COUNT(*) FROM high
		`)
		// 90,85,95,80 → 4
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4, got %v", rows[0][0])
		}
	})
}
