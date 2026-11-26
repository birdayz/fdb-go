package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

// TestFDB_SelectStarDerived — derived table SELECT * patterns
func TestFDB_SelectStarDerived(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "stard", "CREATE TABLE star_t(id BIGINT, name STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO star_t VALUES (1, 'alice', 10), (2, 'bob', 20), (3, 'charlie', 30)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("select_star_basic", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM star_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][1]) != "alice" {
			t.Errorf("row 0 name: want alice, got %v", rows[0][1])
		}
	})

	t.Run("select_star_from_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM (SELECT id, name FROM star_t) AS d ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		if len(rows[0]) != 2 {
			t.Errorf("derived should have 2 columns, got %d", len(rows[0]))
		}
	})

	t.Run("select_star_from_nested_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM (SELECT * FROM (SELECT id, val FROM star_t) AS inner_d) AS outer_d ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
	})

	t.Run("select_star_derived_with_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM (SELECT * FROM star_t WHERE val > 15) AS d ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows (val>15), got %d: %v", len(rows), rows)
		}
	})

	t.Run("count_from_derived_star", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM (SELECT * FROM star_t) AS d")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Errorf("want COUNT(*)=3, got %v", rows)
		}
	})
}

// TestFDB_DerivedTableAggregateJoin — derived tables with aggregates and joins
func TestFDB_DerivedTableAggregateJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtaj",
		"CREATE TABLE orders(id BIGINT, customer STRING, amount BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE customers(id BIGINT, name STRING, region STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO orders VALUES
		(1, 'alice', 100), (2, 'alice', 200), (3, 'bob', 150),
		(4, 'bob', 250), (5, 'charlie', 300)
	`); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO customers VALUES
		(1, 'alice', 'east'), (2, 'bob', 'west'), (3, 'charlie', 'east')
	`); err != nil {
		t.Fatalf("INSERT customers: %v", err)
	}

	t.Run("derived_aggregate_in_from", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.customer, d.total
			FROM (SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer) AS d
			ORDER BY d.total DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 400 {
			t.Errorf("bob total should be 400, got %v", rows[0][1])
		}
	})

	t.Run("derived_with_having", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT customer, SUM(amount) AS total
			FROM orders
			GROUP BY customer
			HAVING SUM(amount) > 200
			ORDER BY total
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 groups with SUM>200 (alice=300,charlie=300,bob=400), got %d: %v", len(rows), rows)
		}
	})

	t.Run("join_with_derived_subquery", func(t *testing.T) {
		// RFC-144 TASK A: a derived table (subquery) on the right of an explicit
		// JOIN is now supported (Java-aligned). Each customer matches its
		// per-customer SUM(amount): alice=300, bob=400, charlie=300.
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, c.region, o.total
			FROM customers c
			JOIN (SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer) AS o
			ON c.name = o.customer
			ORDER BY o.total DESC, c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		// bob=400 first, then alice=300, charlie=300 (name tie-break).
		if rows[0][0] != "bob" || toInt64(rows[0][2]) != 400 {
			t.Errorf("row0 want [bob,*,400], got %v", rows[0])
		}
		if rows[1][0] != "alice" || toInt64(rows[1][2]) != 300 {
			t.Errorf("row1 want [alice,*,300], got %v", rows[1])
		}
		if rows[2][0] != "charlie" || toInt64(rows[2][2]) != 300 {
			t.Errorf("row2 want [charlie,*,300], got %v", rows[2])
		}
	})

	t.Run("count_distinct_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT COUNT(DISTINCT customer) FROM orders")
		if err == nil {
			t.Errorf("expected error for COUNT(DISTINCT), got nil")
		} else {
			t.Logf("expected error: %v", err)
		}
	})

	t.Run("sum_with_case_when", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(CASE WHEN amount > 200 THEN amount ELSE 0 END) FROM orders
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 550 {
			t.Errorf("SUM(CASE) should be 250+300=550, got %v", rows[0][0])
		}
	})
}

// TestFDB_NestedDerivedTableQueries — deeply nested derived tables
func TestFDB_NestedDerivedTableQueries(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ndtq", "CREATE TABLE nest_t(id BIGINT, val BIGINT, grp STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO nest_t VALUES
		(1, 10, 'A'), (2, 20, 'A'), (3, 30, 'B'), (4, 40, 'B'), (5, 50, 'C')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("three_level_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT * FROM (
				SELECT * FROM (
					SELECT * FROM nest_t WHERE val > 15
				) AS inner_d
			) AS outer_d
			ORDER BY id
		`)
		if len(rows) != 4 {
			t.Fatalf("want 4 (val>15), got %d: %v", len(rows), rows)
		}
	})

	t.Run("derived_with_rename_and_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT d.x, d.y FROM (
				SELECT id AS x, val AS y FROM nest_t
			) AS d
			WHERE d.y >= 30
			ORDER BY d.x
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (val>=30), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("first x should be 3, got %v", rows[0][0])
		}
	})

	t.Run("aggregate_over_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*), SUM(val) FROM (
				SELECT * FROM nest_t WHERE grp = 'A'
			) AS d
		`)
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("COUNT should be 2, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 30 {
			t.Errorf("SUM should be 10+20=30, got %v", rows[0][1])
		}
	})

	t.Run("group_by_over_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT grp, SUM(val) FROM (
				SELECT * FROM nest_t WHERE val > 10
			) AS d
			GROUP BY grp
			ORDER BY grp
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][0]) != "A" || toInt64(rows[0][1]) != 20 {
			t.Errorf("A: want SUM=20, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_DerivedTableWithJoinAndAggregate — derived table in JOIN with aggregate
func TestFDB_DerivedTableWithJoinAndAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtja", "CREATE TABLE dtja_t(id BIGINT, cat STRING, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO dtja_t VALUES
		(1, 'A', 10), (2, 'A', 20), (3, 'B', 30), (4, 'B', 40), (5, 'C', 50)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("select_from_aggregate_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT cat, total FROM (
				SELECT cat, SUM(val) AS total FROM dtja_t GROUP BY cat
			) AS summary
			WHERE total > 25
			ORDER BY total DESC
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (A=30, B=70, C=50 all >25), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][1]) != 70 {
			t.Errorf("first should be B(70), got %v", rows[0][1])
		}
	})

	t.Run("count_over_aggregate_derived", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM (
				SELECT cat, SUM(val) AS total FROM dtja_t GROUP BY cat HAVING SUM(val) > 40
			) AS big_cats
		`)
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 cats with SUM>40 (B=70, C=50), got %v", rows[0][0])
		}
	})
}

// TestFDB_NestedAggregateInDerived — aggregate over aggregate via derived table
func TestFDB_NestedAggregateInDerived(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nagd", "CREATE TABLE nagd_t(id BIGINT, dept STRING, salary BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO nagd_t VALUES
		(1, 'eng', 100), (2, 'eng', 120), (3, 'eng', 80),
		(4, 'sales', 90), (5, 'sales', 110),
		(6, 'hr', 70)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("max_of_group_sums", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT MAX(total) FROM (
				SELECT dept, SUM(salary) AS total FROM nagd_t GROUP BY dept
			) AS dept_totals
		`)
		if toInt64(rows[0][0]) != 300 {
			t.Errorf("MAX of group sums: eng=300,sales=200,hr=70 → 300, got %v", rows[0][0])
		}
	})

	t.Run("min_of_group_counts", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT MIN(cnt) FROM (
				SELECT dept, COUNT(*) AS cnt FROM nagd_t GROUP BY dept
			) AS dept_counts
		`)
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("MIN of group counts: eng=3,sales=2,hr=1 → 1, got %v", rows[0][0])
		}
	})

	t.Run("sum_of_group_sums", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(total) FROM (
				SELECT dept, SUM(salary) AS total FROM nagd_t GROUP BY dept
			) AS dept_totals
		`)
		if toInt64(rows[0][0]) != 570 {
			t.Errorf("SUM of group sums: 300+200+70 = 570, got %v", rows[0][0])
		}
	})
}

// TestFDB_DerivedTableWithLimit — derived table containing LIMIT
func TestFDB_DerivedTableWithLimit(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtlim", "CREATE TABLE dtl_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dtl_t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("limit_in_derived_applied", func(t *testing.T) {
		// RFC-128: the LIMIT inside a derived table is now applied at its
		// pipeline position (no longer dropped/hoisted), so COUNT over a
		// LIMIT-3 derived table is exactly 3 — not 5 (the old wrong behavior).
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM (SELECT * FROM dtl_t LIMIT 3) AS d")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("LIMIT in derived table must cap the inner to 3 rows: COUNT=%v, want 3", rows[0][0])
		}
	})

	t.Run("outer_limit_works", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT * FROM (SELECT * FROM dtl_t) AS d ORDER BY id LIMIT 3")
		if len(rows) != 3 {
			t.Fatalf("outer LIMIT should work: want 3, got %d", len(rows))
		}
	})

	t.Run("aggregate_over_derived_no_limit", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM (SELECT * FROM dtl_t WHERE val <= 30) AS d")
		if toInt64(rows[0][0]) != 60 {
			t.Errorf("SUM of val<=30 (10+20+30=60), got %v", rows[0][0])
		}
	})
}

// TestFDB_NestedDerivedThreeLevels — 3 levels of derived table nesting
func TestFDB_NestedDerivedThreeLevels(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nd3",
		"CREATE TABLE nd3_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO nd3_t VALUES (1,10),(2,20),(3,30),(4,40),(5,50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("triple_nest_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(val) FROM (
				SELECT val FROM (
					SELECT val FROM nd3_t
				) l2
			) l3
		`)
		if toInt64(rows[0][0]) != 150 {
			t.Errorf("want 150, got %v", rows[0][0])
		}
	})

	t.Run("triple_nest_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT COUNT(*) FROM (
				SELECT val FROM (
					SELECT val FROM nd3_t WHERE val > 20
				) l2
			) l3
		`)
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("want 3, got %v", rows[0][0])
		}
	})

	t.Run("expression_alias_propagates_through_3_levels", func(t *testing.T) {
		// An aliased computed expression resolves through nested
		// derived tables by its OUTPUT name `doubled`, not a source-name
		// reverse-map. doubled = val+val over {10,20,30,40,50} → {20,40,60,80,100},
		// SUM = 300.
		rows := testkit.CollectRows(t, db, `
			SELECT SUM(doubled) FROM (
				SELECT doubled FROM (
					SELECT val + val AS doubled FROM nd3_t
				) l2
			) l3
		`)
		if toInt64(rows[0][0]) != 300 {
			t.Errorf("want 300, got %v", rows[0][0])
		}
	})
}

// TestFDB_DerivedTableWithJoin — derived table (subquery in FROM) joined with base table
func TestFDB_DerivedTableWithJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "dtwj",
		"CREATE TABLE dtwj_orders(id BIGINT, cust_id BIGINT, total BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE dtwj_cust(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dtwj_cust VALUES (1,'alice'),(2,'bob'),(3,'carol')"); err != nil {
		t.Fatalf("INSERT cust: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dtwj_orders VALUES (10,1,100),(11,1,200),(12,2,150),(13,3,50)"); err != nil {
		t.Fatalf("INSERT orders: %v", err)
	}

	t.Run("derived_as_join_source", func(t *testing.T) {
		// RFC-144 TASK A: a derived table on the right of an explicit JOIN is now
		// supported (Java-aligned). Per-customer SUM(total): alice=300, bob=150,
		// carol=50. ORDER BY c.name → alice, bob, carol.
		rows := testkit.CollectRows(t, db, `
			SELECT c.name, d.order_total FROM dtwj_cust c
			JOIN (SELECT cust_id, SUM(total) AS order_total FROM dtwj_orders GROUP BY cust_id) d
			ON c.id = d.cust_id
			ORDER BY c.name
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d: %v", len(rows), rows)
		}
		want := []struct {
			name  string
			total int64
		}{{"alice", 300}, {"bob", 150}, {"carol", 50}}
		for i, w := range want {
			if rows[i][0] != w.name || toInt64(rows[i][1]) != w.total {
				t.Errorf("row%d want [%s,%d], got %v", i, w.name, w.total, rows[i])
			}
		}
	})
}
