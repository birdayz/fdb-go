package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_RecursiveCTECrossJoin reproduces recursive_cte.yaml test 20:
// a recursive CTE joined with the base table in the outer query via a
// comma-join (cross-join with WHERE filter). Both `t` and `descendants`
// have an `id` column; the outer WHERE uses qualified refs `t.id` and
// `descendants.id`.
func TestFDB_RecursiveCTECrossJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := "/rcte_crossjoin"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcte_cj_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE rcte_cj_tmpl", dbPath)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Insert hierarchy from recursive_cte.yaml:
	//   1 (root, parent: -1)
	//   ├── 10
	//   │   ├── 40
	//   │   ├── 50
	//   │   │   └── 250
	//   │   └── 70
	//   └── 20
	//       ├── 100
	//       └── 210
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10), (70, 10), (100, 20), (210, 20), (250, 50)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Test 20: recursive CTE cross-joined with base table.
	query := `WITH RECURSIVE descendants AS (
		SELECT id, parent FROM t WHERE id = 10
		UNION ALL
		SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id
	)
	SELECT t.id FROM t, descendants WHERE t.id = descendants.id ORDER BY t.id`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query error: %v", err)
	}
	defer rows.Close()

	var results []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// All descendants of id=10: 10, 40, 50, 70, 250
	expected := []int64{10, 40, 50, 70, 250}
	if len(results) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %v", len(expected), len(results), results)
	}
	for i, want := range expected {
		if results[i] != want {
			t.Fatalf("row %d: expected %d, got %d (all: %v)", i, want, results[i], results)
		}
	}
	t.Logf("Recursive CTE cross-join -> %v", results)

	// Test UNION DISTINCT in recursive CTE for cycle detection.
	t.Run("union_distinct_cycle", func(t *testing.T) {
		// Create edge table for cycle test.
		if _, err := setup.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE rcte_edge_tmpl "+
				"CREATE TABLE edge (src BIGINT NOT NULL, dst BIGINT NOT NULL, PRIMARY KEY (src, dst))"); err != nil {
			t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
		}
		if _, err := setup.ExecContext(ctx,
			fmt.Sprintf("CREATE SCHEMA %s/e WITH TEMPLATE rcte_edge_tmpl", dbPath)); err != nil {
			t.Fatalf("CREATE SCHEMA: %v", err)
		}
		edgeDSN := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=e", dbPath, clusterFilePath)
		edb, err := sql.Open("fdbsql", edgeDSN)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer edb.Close()

		// Cycle: 1 -> 2 -> 3 -> 1
		if _, err := edb.ExecContext(ctx,
			"INSERT INTO edge VALUES (1, 2), (2, 3), (3, 1)"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		query := `WITH RECURSIVE reach(n) AS (
			SELECT src FROM edge WHERE src = 1
			UNION
			SELECT e.dst FROM reach AS r, edge AS e WHERE e.src = r.n
		)
		SELECT n FROM reach ORDER BY n`

		rows, err := edb.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query error: %v", err)
		}
		defer rows.Close()

		var results []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}

		expected := []int64{1, 2, 3}
		if len(results) != len(expected) {
			t.Fatalf("expected %d rows, got %d: %v", len(expected), len(results), results)
		}
		for i, want := range expected {
			if results[i] != want {
				t.Fatalf("row %d: expected %d, got %d (all: %v)", i, want, results[i], results)
			}
		}
		t.Logf("UNION DISTINCT cycle detection -> %v", results)
	})
}
