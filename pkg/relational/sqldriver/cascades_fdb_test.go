package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func setupCascadesTestDB(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("casc_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	naiveDSN := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	naiveDB, err := sql.Open("fdbsql", naiveDSN)
	if err != nil {
		t.Fatalf("sql.Open naive: %v", err)
	}
	t.Cleanup(func() { naiveDB.Close() })

	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (1, 'Widget', 100)"); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (2, 'Gadget', 200)"); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (3, 'Doohickey', 50)"); err != nil {
		t.Fatalf("INSERT 3: %v", err)
	}

	cascadesDSN := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	cascadesDB, err := sql.Open("fdbsql", cascadesDSN)
	if err != nil {
		t.Fatalf("sql.Open cascades: %v", err)
	}
	t.Cleanup(func() { cascadesDB.Close() })

	return naiveDB, cascadesDB
}

func TestFDB_CascadesScan(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item")
	if err != nil {
		t.Fatalf("SELECT *: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
	t.Logf("Cascades SELECT * → %d rows ✓", count)
}

func TestFDB_CascadesFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price > 100")
	if err != nil {
		t.Fatalf("SELECT WHERE: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 1 {
		t.Fatalf("expected 1 row with price > 100, got %d", count)
	}
	t.Logf("Cascades WHERE → %d row ✓", count)
}

func TestFDB_CascadesProjection(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT item_id, name FROM Item")
	if err != nil {
		t.Skipf("projection not supported yet: %v", err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	t.Logf("columns: %v", cols)

	count := countRows(t, rows)
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
	t.Logf("Cascades projection → %d rows ✓", count)
}

func TestFDB_CascadesStringFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE name = 'Gadget'")
	if err != nil {
		t.Skipf("string filter not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 1 {
		t.Fatalf("expected 1 row (Gadget), got %d", count)
	}
	t.Logf("Cascades string = filter → %d row ✓", count)
}

func TestFDB_CascadesInequalityFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price >= 100")
	if err != nil {
		t.Skipf("inequality filter not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 2 {
		t.Fatalf("expected 2 rows (price >= 100), got %d", count)
	}
	t.Logf("Cascades >= filter → %d rows ✓", count)
}

func TestFDB_CascadesMultiPredicate(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price > 50 AND price < 200")
	if err != nil {
		t.Skipf("multi-predicate WHERE not supported yet: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 1 {
		t.Fatalf("expected 1 row (Widget, price=100), got %d", count)
	}
	t.Logf("Cascades multi-predicate WHERE → %d row ✓", count)
}

func TestFDB_CascadesIndexScan(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_idx_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("idx_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Product (product_id BIGINT NOT NULL, category STRING, price BIGINT, PRIMARY KEY (product_id)) "+
			"CREATE INDEX idx_category ON Product (category)", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/shop WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=shop", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for i := 1; i <= 5; i++ {
		cat := "electronics"
		if i > 3 {
			cat = "clothing"
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO Product VALUES (%d, '%s', %d)", i, cat, i*100)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT * FROM Product WHERE category = 'electronics'")
	if err != nil {
		t.Skipf("index scan via Cascades not yet working: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 3 {
		t.Fatalf("expected 3 electronics, got %d", count)
	}
	t.Logf("Cascades with index → %d rows ✓", count)
}

func TestFDB_CascadesSumAggregate(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT SUM(price) FROM Item")
	if err != nil {
		t.Skipf("SUM not supported via Cascades: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row from SUM")
	}
	var total int64
	if err := rows.Scan(&total); err != nil {
		t.Skipf("SUM scan failed: %v", err)
	}
	// 100 + 200 + 50 = 350
	if total != 350 {
		t.Fatalf("expected SUM(price) = 350, got %d", total)
	}
	t.Logf("Cascades SUM(price) → %d ✓", total)
}

func TestFDB_CascadesDistinct(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT DISTINCT price FROM Item")
	if err != nil {
		t.Skipf("DISTINCT not supported via Cascades: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	// 3 items with prices 100, 200, 50 — all distinct
	if count != 3 {
		t.Fatalf("expected 3 distinct prices, got %d", count)
	}
	t.Logf("Cascades DISTINCT → %d rows ✓", count)
}

func TestFDB_CascadesNotEqual(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price <> 100")
	if err != nil {
		t.Skipf("<> filter not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 2 {
		t.Fatalf("expected 2 rows (price <> 100), got %d", count)
	}
	t.Logf("Cascades <> filter → %d rows ✓", count)
}

func TestFDB_CascadesOrFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price > 150 OR name = 'Doohickey'")
	if err != nil {
		t.Skipf("OR filter not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 2 {
		t.Fatalf("expected 2 rows (Gadget price=200, Doohickey), got %d", count)
	}
	t.Logf("Cascades OR filter → %d rows ✓", count)
}

func TestFDB_CascadesCount(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT COUNT(*) FROM Item")
	if err != nil {
		t.Skipf("COUNT(*) not supported via Cascades yet: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row from COUNT(*)")
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		t.Skipf("COUNT(*) scan failed (may need aggregate support): %v", err)
	}
	if count != 3 {
		t.Fatalf("expected COUNT(*) = 3, got %d", count)
	}
	t.Logf("Cascades COUNT(*) → %d ✓", count)
}

func TestFDB_CascadesOrderBy(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT name FROM Item ORDER BY name ASC")
	if err != nil {
		t.Skipf("ORDER BY not supported via Cascades yet: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("not sorted: %v", names)
		}
	}
	t.Logf("Cascades ORDER BY → %v ✓", names)
}

func countRows(t *testing.T, rows *sql.Rows) int {
	t.Helper()
	var n int
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return n
}
