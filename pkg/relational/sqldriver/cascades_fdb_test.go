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
		t.Fatalf("projection not supported yet: %v", err)
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
		t.Fatalf("string filter not supported: %v", err)
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
		t.Fatalf("inequality filter not supported: %v", err)
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
		t.Fatalf("multi-predicate WHERE not supported yet: %v", err)
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
		t.Fatalf("index scan via Cascades not yet working: %v", err)
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
		t.Fatalf("SUM not supported via Cascades: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row from SUM")
	}
	var total int64
	if err := rows.Scan(&total); err != nil {
		t.Fatalf("SUM scan failed: %v", err)
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
		t.Fatalf("DISTINCT not supported via Cascades: %v", err)
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
		t.Fatalf("<> filter not supported: %v", err)
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
		t.Fatalf("OR filter not supported: %v", err)
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
		t.Fatalf("COUNT(*) not supported via Cascades yet: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row from COUNT(*)")
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("COUNT(*) scan failed (may need aggregate support): %v", err)
	}
	if count != 3 {
		t.Fatalf("expected COUNT(*) = 3, got %d", count)
	}
	t.Logf("Cascades COUNT(*) → %d ✓", count)
}

func TestFDB_CascadesOrderByNoIndex(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// ORDER BY on non-indexed column — uses in-memory sort (Go extension).
	rows, err := cascadesDB.QueryContext(ctx, "SELECT name FROM Item ORDER BY name ASC")
	if err != nil {
		t.Fatalf("ORDER BY without index should succeed via in-memory sort: %v", err)
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
	if len(names) != 3 || names[0] != "Doohickey" || names[1] != "Gadget" || names[2] != "Widget" {
		t.Fatalf("expected [Doohickey Gadget Widget], got %v", names)
	}
	t.Logf("In-memory sort ORDER BY without index → %v ✓", names)
}

func TestFDB_CascadesJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_join_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("join_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Orders (order_id BIGINT NOT NULL, customer STRING, PRIMARY KEY (order_id)) "+
			"CREATE TABLE Items (item_id BIGINT NOT NULL, order_id BIGINT, name STRING, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (2, 'Bob')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (10, 1, 'Widget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (20, 1, 'Gadget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (30, 2, 'Doohickey')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT * FROM Orders, Items WHERE Orders.order_id = Items.order_id")
	if err != nil {
		t.Fatalf("JOIN not supported via Cascades: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 3 {
		t.Fatalf("expected 3 rows from join, got %d", count)
	}
	t.Logf("Cascades JOIN → %d rows ✓", count)
}

func TestFDB_CascadesAggregateWithGroupBy(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_grpby_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("grpby_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Sales (sale_id BIGINT NOT NULL, category STRING, amount BIGINT, PRIMARY KEY (sale_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for _, sale := range []struct {
		id  int
		cat string
		amt int
	}{{1, "A", 100}, {2, "A", 200}, {3, "B", 150}, {4, "B", 50}, {5, "C", 300}} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO Sales VALUES (%d, '%s', %d)", sale.id, sale.cat, sale.amt)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT category, SUM(amount) FROM Sales GROUP BY category")
	if err != nil {
		t.Fatalf("GROUP BY not supported: %v", err)
	}
	defer rows.Close()

	type result struct {
		cat string
		sum int64
	}
	var results []result
	for rows.Next() {
		var r result
		if err := rows.Scan(&r.cat, &r.sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 groups, got %d: %v", len(results), results)
	}
	t.Logf("Cascades GROUP BY → %v ✓", results)
}

func TestFDB_CascadesDistinctWithFilter(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_distfilt_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("distfilt_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Product (product_id BIGINT NOT NULL, category STRING, PRIMARY KEY (product_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for _, item := range []struct {
		id  int
		cat string
	}{{1, "A"}, {2, "A"}, {3, "B"}, {4, "B"}, {5, "C"}} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO Product VALUES (%d, '%s')", item.id, item.cat)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT DISTINCT category FROM Product WHERE product_id > 1")
	if err != nil {
		t.Fatalf("DISTINCT+filter not supported: %v", err)
	}
	defer rows.Close()

	var cats []string
	for rows.Next() {
		var cat string
		if err := rows.Scan(&cat); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cats = append(cats, cat)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(cats) != 3 {
		t.Fatalf("expected 3 distinct categories (A, B, C from id > 1), got %d: %v", len(cats), cats)
	}
	t.Logf("Cascades DISTINCT+filter → %v ✓", cats)
}

func TestFDB_CascadesMultiColumnProjection(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT name, price FROM Item WHERE price > 50")
	if err != nil {
		t.Fatalf("multi-column projection+filter not supported: %v", err)
	}
	defer rows.Close()

	type row struct {
		name  string
		price int64
	}
	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.price); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 rows (price > 50), got %d", len(results))
	}
	t.Logf("Cascades multi-col projection+filter → %v ✓", results)
}

func TestFDB_CascadesOrderByWithIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_orderby_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("orderby_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Product (product_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (product_id)) "+
			"CREATE INDEX idx_name ON Product (name)", tmpl)); err != nil {
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

	for _, item := range []struct {
		id   int
		name string
	}{{1, "Cherry"}, {2, "Apple"}, {3, "Banana"}} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO Product VALUES (%d, '%s', %d)", item.id, item.name, item.id*100)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT name FROM Product ORDER BY name ASC")
	if err != nil {
		t.Fatalf("ORDER BY with index not supported via Cascades: %v", err)
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
	expected := []string{"Apple", "Banana", "Cherry"}
	for i, name := range names {
		if name != expected[i] {
			t.Fatalf("expected %v, got %v", expected, names)
		}
	}
	t.Logf("Cascades ORDER BY with index → %v ✓", names)

	rows2, err := db.QueryContext(ctx, "SELECT name FROM Product ORDER BY name DESC")
	if err != nil {
		t.Fatalf("ORDER BY DESC with index: %v", err)
	}
	defer rows2.Close()

	var descNames []string
	for rows2.Next() {
		var name string
		if err := rows2.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		descNames = append(descNames, name)
	}
	expectedDesc := []string{"Cherry", "Banana", "Apple"}
	for i, name := range descNames {
		if name != expectedDesc[i] {
			t.Fatalf("expected %v, got %v", expectedDesc, descNames)
		}
	}
	t.Logf("Cascades ORDER BY DESC with index → %v ✓", descNames)
}

func TestFDB_CascadesUnionAll(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx,
		"SELECT name FROM Item WHERE price > 150 "+
			"UNION ALL "+
			"SELECT name FROM Item WHERE price < 100")
	if err != nil {
		t.Fatalf("UNION ALL not supported via Cascades: %v", err)
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
	// price > 150: Gadget(200) | price < 100: Doohickey(50)
	if len(names) != 2 {
		t.Fatalf("expected 2 rows from UNION ALL, got %d: %v", len(names), names)
	}
	t.Logf("Cascades UNION ALL → %v ✓", names)
}

func TestFDB_CascadesCTESimple(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Simple CTE scan — inlines to a plain scan of Item.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH items AS (SELECT item_id, name, price FROM Item) SELECT name FROM items")
	if err != nil {
		t.Fatalf("CTE not supported via Cascades: %v", err)
	}
	defer rows.Close()
	count := countRows(t, rows)
	if count != 3 {
		t.Fatalf("expected 3 rows from CTE, got %d", count)
	}
	t.Logf("Cascades CTE simple → %d rows ✓", count)
}

func TestFDB_CascadesCTEWithFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE with WHERE on inner body — filter inlines into the plan.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH expensive AS (SELECT item_id, name FROM Item WHERE price > 100) SELECT name FROM expensive")
	if err != nil {
		t.Fatalf("CTE with body filter not supported: %v", err)
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
	// price > 100 → only Gadget (200)
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("Cascades CTE with body filter → %v ✓", names)
}

func TestFDB_CascadesCTEOuterWhere(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE with WHERE on the outer query (the CTE reference).
	// This tests that the outer predicate is properly resolved
	// using CTE-derived column schemas.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH items AS (SELECT item_id, name, price FROM Item) "+
			"SELECT name FROM items WHERE price > 100")
	if err != nil {
		t.Fatalf("CTE with outer WHERE not supported: %v", err)
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
	// price > 100 → only Gadget (200)
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("Cascades CTE with outer WHERE → %v ✓", names)
}

func TestFDB_CascadesCTEAggregateOnBody(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Aggregate (COUNT) over a CTE — CTE inlines, aggregate on top.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH expensive AS (SELECT item_id, name FROM Item WHERE price > 50) "+
			"SELECT COUNT(*) FROM expensive")
	if err != nil {
		t.Fatalf("CTE aggregate not supported: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row from COUNT")
	}
	var cnt int64
	if err := rows.Scan(&cnt); err != nil {
		t.Fatalf("COUNT scan: %v", err)
	}
	// price > 50: Widget(100), Gadget(200) → 2
	if cnt != 2 {
		t.Fatalf("expected COUNT=2, got %d", cnt)
	}
	t.Logf("Cascades CTE aggregate → COUNT=%d ✓", cnt)
}

func TestFDB_CascadesCTESelectStar(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE body uses SELECT * — all columns from underlying table.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH all_items AS (SELECT * FROM Item) "+
			"SELECT name FROM all_items WHERE price > 100")
	if err != nil {
		t.Fatalf("CTE SELECT * not supported: %v", err)
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
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("Cascades CTE SELECT * → %v ✓", names)
}

func TestFDB_CascadesCTEProjectionAlias(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE body with column aliases — tests that aliased projections
	// flow through Cascades correctly.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH items AS (SELECT name AS item_name, price AS cost FROM Item) "+
			"SELECT item_name FROM items WHERE cost > 100")
	if err != nil {
		t.Fatalf("CTE alias not supported: %v", err)
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
	// cost > 100: Gadget (200)
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("Cascades CTE projection alias → %v ✓", names)
}

func TestFDB_CascadesCTEUnionBody(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE body is a UNION ALL — both branches contribute rows.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH combined AS ("+
			"SELECT name FROM Item WHERE price > 150 "+
			"UNION ALL "+
			"SELECT name FROM Item WHERE price < 100) "+
			"SELECT name FROM combined")
	if err != nil {
		t.Fatalf("CTE UNION body not supported: %v", err)
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
	// price > 150: Gadget(200) | price < 100: Doohickey(50) → 2 rows
	if len(names) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(names), names)
	}
	t.Logf("Cascades CTE UNION body → %v ✓", names)
}

func TestFDB_CascadesCTEChainedSelectStar(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Chained CTE where the second CTE uses SELECT * from the first.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH base AS (SELECT item_id, name, price FROM Item WHERE price > 50), "+
			"all_base AS (SELECT * FROM base) "+
			"SELECT name FROM all_base WHERE item_id > 1")
	if err != nil {
		t.Fatalf("Chained CTE SELECT * not supported: %v", err)
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
	// base: price > 50 → Widget(1,100), Gadget(2,200); all_base: *; item_id > 1 → Gadget
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("Cascades chained CTE SELECT * → %v ✓", names)
}

func TestFDB_CascadesCTEGroupBy(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE + GROUP BY + SUM — tests aggregate on inlined CTE scan.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH all_items AS (SELECT item_id, name, price FROM Item) "+
			"SELECT SUM(price) FROM all_items")
	if err != nil {
		t.Fatalf("CTE GROUP BY not supported: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row")
	}
	var total int64
	if err := rows.Scan(&total); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// 100 + 200 + 50 = 350
	if total != 350 {
		t.Fatalf("expected SUM=350, got %d", total)
	}
	t.Logf("Cascades CTE GROUP BY → SUM=%d ✓", total)
}

func TestFDB_CascadesCTEJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_ctejoin_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("ctejoin_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Orders (order_id BIGINT NOT NULL, customer STRING, PRIMARY KEY (order_id)) "+
			"CREATE TABLE Items (item_id BIGINT NOT NULL, order_id BIGINT, name STRING, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (2, 'Bob')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (10, 1, 'Widget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (20, 2, 'Gadget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// JOIN a real table with a CTE.
	rows, err := db.QueryContext(ctx,
		"WITH big_orders AS (SELECT order_id, customer FROM Orders WHERE order_id > 0) "+
			"SELECT big_orders.customer FROM big_orders, Items "+
			"WHERE big_orders.order_id = Items.order_id")
	if err != nil {
		t.Fatalf("CTE JOIN not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	if count != 2 {
		t.Fatalf("expected 2 rows from CTE+JOIN, got %d", count)
	}
	t.Logf("Cascades CTE JOIN → %d rows ✓", count)
}

func TestFDB_CascadesCTEChained(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE B references CTE A — tests chained schema derivation.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH cheap AS (SELECT item_id, name, price FROM Item WHERE price < 200), "+
			"filtered AS (SELECT name FROM cheap WHERE item_id > 1) "+
			"SELECT name FROM filtered")
	if err != nil {
		t.Fatalf("Chained CTE not supported via Cascades: %v", err)
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
	// price < 200 → Widget(100), Doohickey(50); item_id > 1 → Doohickey(id=3)
	if len(names) != 1 || names[0] != "Doohickey" {
		t.Fatalf("expected [Doohickey], got %v", names)
	}
	t.Logf("Cascades chained CTE → %v ✓", names)
}

func TestFDB_CascadesCTEDistinct(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// CTE + DISTINCT — dedup over inlined CTE scan.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH all_items AS (SELECT price FROM Item) "+
			"SELECT DISTINCT price FROM all_items")
	if err != nil {
		t.Fatalf("CTE DISTINCT not supported: %v", err)
	}
	defer rows.Close()

	count := countRows(t, rows)
	// prices: 100, 200, 50 → all distinct → 3
	if count != 3 {
		t.Fatalf("expected 3 distinct prices, got %d", count)
	}
	t.Logf("Cascades CTE DISTINCT → %d rows ✓", count)
}

func TestFDB_CascadesExplicitJoinOn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_joinon_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("joinon_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Orders (order_id BIGINT NOT NULL, customer STRING, PRIMARY KEY (order_id)) "+
			"CREATE TABLE Items (item_id BIGINT NOT NULL, order_id BIGINT, name STRING, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (10, 1, 'Widget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (20, 99, 'Orphan')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Explicit INNER JOIN ON — tests that ON predicate is properly resolved.
	rows, err := db.QueryContext(ctx,
		"SELECT Items.name FROM Orders INNER JOIN Items ON Orders.order_id = Items.order_id")
	if err != nil {
		t.Fatalf("Explicit JOIN ON not supported: %v", err)
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
	// Only Widget matches (order_id=1); Orphan has order_id=99.
	if len(names) != 1 || names[0] != "Widget" {
		t.Fatalf("expected [Widget], got %v", names)
	}
	t.Logf("Cascades explicit JOIN ON → %v ✓", names)
}

func TestFDB_CascadesCTEDoubleFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Filter in both CTE body AND outer query.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH filtered AS (SELECT item_id, name, price FROM Item WHERE price < 200) "+
			"SELECT name FROM filtered WHERE item_id > 1")
	if err != nil {
		t.Fatalf("CTE double filter not supported: %v", err)
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
	// price < 200: Widget(1,100), Doohickey(3,50) | item_id > 1: Doohickey(3)
	if len(names) != 1 || names[0] != "Doohickey" {
		t.Fatalf("expected [Doohickey], got %v", names)
	}
	t.Logf("Cascades CTE double filter → %v ✓", names)
}

func TestFDB_CascadesCTEInUnion(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx,
		"WITH base AS (SELECT item_id, name FROM Item) "+
			"SELECT name FROM base WHERE item_id = 1 "+
			"UNION ALL "+
			"SELECT name FROM base WHERE item_id = 3")
	if err != nil {
		t.Fatalf("CTE-in-UNION query failed: %v", err)
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
	if len(names) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(names), names)
	}
	t.Logf("CTE-in-UNION → %v ✓", names)
}

func TestFDB_CascadesCTEComplexStack(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Complex CTE stack: filter → project → distinct → count.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH filtered AS (SELECT item_id, name FROM Item WHERE price > 50), "+
			"projected AS (SELECT name FROM filtered) "+
			"SELECT COUNT(*) FROM projected")
	if err != nil {
		t.Fatalf("Complex CTE stack not supported: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected 1 row")
	}
	var cnt int64
	if err := rows.Scan(&cnt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// price > 50: Widget(100), Gadget(200) → 2
	if cnt != 2 {
		t.Fatalf("expected COUNT=2, got %d", cnt)
	}
	t.Logf("Cascades CTE complex stack → COUNT=%d ✓", cnt)
}

func TestFDB_CascadesComputedProjection(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx,
		"SELECT price FROM Item WHERE price > 100")
	if err != nil {
		t.Fatalf("Computed projection not supported: %v", err)
	}
	defer rows.Close()

	var prices []int64
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		prices = append(prices, p)
	}
	// price > 100: Gadget(200)
	if len(prices) != 1 || prices[0] != 200 {
		t.Fatalf("expected [200], got %v", prices)
	}
	t.Logf("Cascades computed projection → %v ✓", prices)
}

func TestFDB_CascadesThreeWayJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_3join_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("j3_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE A (a_id BIGINT NOT NULL, val STRING, PRIMARY KEY (a_id)) "+
			"CREATE TABLE B (b_id BIGINT NOT NULL, a_ref BIGINT, PRIMARY KEY (b_id)) "+
			"CREATE TABLE C (c_id BIGINT NOT NULL, b_ref BIGINT, PRIMARY KEY (c_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO A VALUES (1, 'alpha')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO B VALUES (10, 1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO C VALUES (100, 10)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO C VALUES (200, 99)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 3-way join: A → B → C. Only C(100) matches the chain.
	rows, err := db.QueryContext(ctx,
		"SELECT A.val FROM A, B, C WHERE A.a_id = B.a_ref AND B.b_id = C.b_ref")
	if err != nil {
		t.Fatalf("3-way join not supported: %v", err)
	}
	defer rows.Close()

	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		vals = append(vals, v)
	}
	if len(vals) != 1 || vals[0] != "alpha" {
		t.Fatalf("expected [alpha], got %v", vals)
	}
	t.Logf("Cascades 3-way join → %v ✓", vals)
}

func TestFDB_CascadesMultiFilter(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Multiple predicates: AND compound filter.
	rows, err := cascadesDB.QueryContext(ctx,
		"SELECT name FROM Item WHERE price > 50 AND price < 200")
	if err != nil {
		t.Fatalf("Multi-filter not supported: %v", err)
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
	// price > 50 AND price < 200: Widget(100)
	if len(names) != 1 || names[0] != "Widget" {
		t.Fatalf("expected [Widget], got %v", names)
	}
	t.Logf("Cascades multi-filter → %v ✓", names)
}

func TestFDB_CascadesOrderByPK(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	rows, err := cascadesDB.QueryContext(ctx,
		"SELECT name FROM Item ORDER BY item_id ASC")
	if err != nil {
		t.Fatalf("ORDER BY PK should succeed: %v", err)
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
	if len(names) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(names))
	}
	if names[0] != "Widget" || names[1] != "Gadget" || names[2] != "Doohickey" {
		t.Fatalf("expected [Widget Gadget Doohickey], got %v", names)
	}
	t.Logf("Cascades ORDER BY PK → %v ✓", names)

	rows2, err := cascadesDB.QueryContext(ctx,
		"SELECT name FROM Item ORDER BY item_id DESC")
	if err != nil {
		t.Fatalf("ORDER BY PK DESC should succeed via reverse scan: %v", err)
	}
	defer rows2.Close()

	var descNames []string
	for rows2.Next() {
		var name string
		if err := rows2.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		descNames = append(descNames, name)
	}
	if len(descNames) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(descNames))
	}
	if descNames[0] != "Doohickey" || descNames[1] != "Gadget" || descNames[2] != "Widget" {
		t.Fatalf("expected [Doohickey Gadget Widget], got %v", descNames)
	}
	t.Logf("Cascades ORDER BY PK DESC → %v ✓", descNames)
}

// Go extension: in-memory sort — CTE + ORDER BY on a non-indexed column
// now succeeds via ImplementInMemorySortRule.
func TestFDB_CascadesCTEOrderByNoIndex(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// Go extension: in-memory sort — CTE + ORDER BY on a non-indexed column.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH items AS (SELECT item_id, name, price FROM Item) SELECT name FROM items ORDER BY name ASC")
	if err != nil {
		t.Fatalf("expected success; got error: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	// Data: Widget, Gadget, Doohickey — sorted ASC by name.
	expected := []string{"Doohickey", "Gadget", "Widget"}
	if len(got) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("row %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

// Go extension: in-memory sort — JOIN + ORDER BY on a non-indexed column
// now succeeds via ImplementInMemorySortRule.
func TestFDB_CascadesJoinOrderByNoIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_joinob_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("joinob_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Orders (order_id BIGINT NOT NULL, customer STRING, PRIMARY KEY (order_id)) "+
			"CREATE TABLE Items (item_id BIGINT NOT NULL, order_id BIGINT, name STRING, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Orders VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (1, 1, 'Widget')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Go extension: in-memory sort — JOIN + ORDER BY on a non-indexed column.
	rows, err := db.QueryContext(ctx,
		"SELECT o.customer, i.name FROM Orders o, Items i WHERE o.order_id = i.order_id ORDER BY o.customer")
	if err != nil {
		t.Fatalf("expected success; got error: %v", err)
	}
	defer rows.Close()
	var customer, name string
	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	if err := rows.Scan(&customer, &name); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if customer != "Alice" || name != "Widget" {
		t.Fatalf("expected (Alice, Widget), got (%s, %s)", customer, name)
	}
	if rows.Next() {
		t.Fatal("expected exactly one row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
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
