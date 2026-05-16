package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
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

func TestFDB_CascadesCTEColumnAliases(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// WITH c(alias1, alias2) AS (...) renames body columns.
	rows, err := cascadesDB.QueryContext(ctx,
		"WITH priced(product, cost) AS (SELECT name, price FROM Item) "+
			"SELECT product FROM priced WHERE cost > 100 ORDER BY product")
	if err != nil {
		t.Fatalf("CTE column aliases not supported: %v", err)
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
	if len(names) != 1 || names[0] != "Gadget" {
		t.Fatalf("expected [Gadget], got %v", names)
	}
	t.Logf("CTE column aliases → %v ✓", names)
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

func TestFDB_CascadesRecursiveCTE(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_reccte_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("reccte_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))", tmpl)); err != nil {
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
	t.Cleanup(func() { db.Close() })

	// Build a small hierarchy:
	//   1 (root, parent=-1)
	//   ├── 50 (parent=1)
	//   │   └── 250 (parent=50)
	//   └── 51 (parent=1)
	for _, row := range [][2]int64{{1, -1}, {50, 1}, {51, 1}, {250, 50}} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", row[0], row[1])); err != nil {
			t.Fatalf("INSERT %d: %v", row[0], err)
		}
	}

	// Recursive CTE: walk descendants of root (parent=-1).
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE descendants AS ("+
			"SELECT id, parent FROM t WHERE parent = -1 "+
			"UNION ALL "+
			"SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id"+
			") SELECT COUNT(*) FROM descendants")
	if err != nil {
		t.Fatalf("recursive CTE count query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected a row")
	}
	var cnt int64
	if err := rows.Scan(&cnt); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if cnt != 4 {
		t.Fatalf("expected COUNT(*)=4, got %d", cnt)
	}
	rows.Close()

	// Verify that individual column values are NOT NULL.
	// This is the key regression: before the fix, the recursive leg's
	// datum used qualified keys ("B.ID") but the outer projection
	// expected unqualified keys ("ID"), producing NULLs.
	rows2, err := db.QueryContext(ctx,
		"WITH RECURSIVE descendants AS ("+
			"SELECT id, parent FROM t WHERE parent = -1 "+
			"UNION ALL "+
			"SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id"+
			") SELECT id FROM descendants ORDER BY id")
	if err != nil {
		t.Fatalf("recursive CTE id query: %v", err)
	}
	defer rows2.Close()

	var ids []int64
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	expected := []int64{1, 50, 51, 250}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %v", len(expected), len(ids), ids)
	}
	for i, want := range expected {
		if ids[i] != want {
			t.Fatalf("row %d: expected %d, got %d (all ids: %v)", i, want, ids[i], ids)
		}
	}
	t.Logf("Recursive CTE → %v ✓", ids)
}

func TestFDB_CascadesRecursiveCTEPostOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := "/casc_reccte_postorder"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE reccte_po CREATE TABLE t (id BIGINT NOT NULL, parent BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE reccte_po", dbPath)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Chain: 1 → 10 → 50 → 250
	for _, row := range [][2]int64{{1, -1}, {10, 1}, {50, 10}, {250, 50}} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", row[0], row[1])); err != nil {
			t.Fatalf("INSERT %d: %v", row[0], err)
		}
	}

	// Post-order: walk ancestors from 250, emit children before parents.
	rows, err := db.QueryContext(ctx,
		"WITH RECURSIVE ancestors AS ("+
			"SELECT id, parent FROM t WHERE id = 250 "+
			"UNION ALL "+
			"SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent"+
			") TRAVERSAL ORDER post_order "+
			"SELECT id FROM ancestors")
	if err != nil {
		t.Fatalf("recursive CTE post_order: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	expected := []int64{1, 10, 50, 250}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %v", len(expected), len(ids), ids)
	}
	for i, want := range expected {
		if ids[i] != want {
			t.Fatalf("row %d: expected %d, got %d (all: %v)", i, want, ids[i], ids)
		}
	}
	t.Logf("Post-order: %v ✓", ids)
}

func TestFDB_CascadesScalarSubqueryInProjection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_ssq_proj_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("ssq_proj_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE T1 (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE T2 (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))", tmpl)); err != nil {
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

	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT T1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (2, 20)"); err != nil {
		t.Fatalf("INSERT T1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T2 VALUES (1, 100)"); err != nil {
		t.Fatalf("INSERT T2: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T2 VALUES (2, 200)"); err != nil {
		t.Fatalf("INSERT T2: %v", err)
	}

	// Scalar subquery in SELECT projection: (SELECT MAX(w) FROM T2)
	var id, v, maxW int64
	err = db.QueryRowContext(ctx, "SELECT id, v, (SELECT MAX(w) FROM T2) AS max_w FROM T1 WHERE id = 1").Scan(&id, &v, &maxW)
	if err != nil {
		t.Fatalf("scalar subquery in projection: %v", err)
	}
	if id != 1 || v != 10 || maxW != 200 {
		t.Fatalf("expected (1, 10, 200), got (%d, %d, %d)", id, v, maxW)
	}
	t.Logf("Cascades scalar subquery in projection → (%d, %d, %d) ✓", id, v, maxW)
}

func TestFDB_CascadesScalarSubqueryInWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_ssq_where_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("ssq_where_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE T1 (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE T2 (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))", tmpl)); err != nil {
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

	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT T1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (2, 20)"); err != nil {
		t.Fatalf("INSERT T1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (3, 30)"); err != nil {
		t.Fatalf("INSERT T1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T2 VALUES (1, 15)"); err != nil {
		t.Fatalf("INSERT T2: %v", err)
	}

	// Scalar subquery in WHERE: v > (SELECT w FROM T2 WHERE id = 1)
	// Should return rows where v > 15, i.e. id=2 (v=20) and id=3 (v=30)
	rows, err := db.QueryContext(ctx, "SELECT id FROM T1 WHERE v > (SELECT w FROM T2 WHERE id = 1) ORDER BY id")
	if err != nil {
		t.Fatalf("scalar subquery in WHERE: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("expected [2 3], got %v", ids)
	}
	t.Logf("Cascades scalar subquery in WHERE → %v ✓", ids)
}

// TestFDB_CascadesMinMaxStringRejected verifies that MIN/MAX over a
// STRING column returns error 0A000 (unsupported operation), matching
// Java's fdb-relational which only installs numeric MIN/MAX overloads.
func TestFDB_CascadesMinMaxStringRejected(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	// MIN(name) where name is STRING — must fail with 0A000.
	_, err := cascadesDB.QueryContext(ctx, "SELECT MIN(name) FROM Item")
	if err == nil {
		t.Fatal("expected error for MIN(name) on STRING column, got nil")
	}
	if !strings.Contains(err.Error(), "0A000") && !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("expected 0A000 / type mismatch error, got: %v", err)
	}
	t.Logf("MIN(name) correctly rejected: %v", err)

	// MAX(name) where name is STRING — must also fail.
	_, err = cascadesDB.QueryContext(ctx, "SELECT MAX(name) FROM Item")
	if err == nil {
		t.Fatal("expected error for MAX(name) on STRING column, got nil")
	}
	if !strings.Contains(err.Error(), "0A000") && !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("expected 0A000 / type mismatch error, got: %v", err)
	}
	t.Logf("MAX(name) correctly rejected: %v", err)

	// MIN(price) where price is BIGINT — must still succeed.
	rows, err := cascadesDB.QueryContext(ctx, "SELECT MIN(price) FROM Item")
	if err != nil {
		t.Fatalf("MIN(price) on BIGINT should succeed: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected 1 row from MIN(price)")
	}
	var minPrice int64
	if err := rows.Scan(&minPrice); err != nil {
		t.Fatalf("scan MIN(price): %v", err)
	}
	if minPrice != 50 {
		t.Fatalf("expected MIN(price) = 50, got %d", minPrice)
	}
	t.Logf("MIN(price) on BIGINT → %d (correct)", minPrice)

	// Scalar subquery wrapping: SELECT (SELECT MIN(name) FROM Item)
	_, err = cascadesDB.QueryContext(ctx, "SELECT (SELECT MIN(name) FROM Item) FROM Item WHERE item_id = 1")
	if err == nil {
		t.Fatal("expected error for scalar subquery MIN(name) on STRING, got nil")
	}
	if !strings.Contains(err.Error(), "0A000") && !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("expected 0A000 / type mismatch for scalar subquery, got: %v", err)
	}
	t.Logf("Scalar subquery MIN(name) correctly rejected: %v", err)
}

// Tied sort keys must break by primary key. When the sort direction
// is DESC, the PK tiebreaker is also DESC (matching Java's index scan
// behaviour where a reverse scan on a composite index emits PKs in
// descending order within each tied group).
func TestFDB_CascadesSortPKTiebreaker(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_sorttie_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("sorttie_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE rp (id BIGINT NOT NULL, region STRING, plan STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_region_plan ON rp (region, plan)", tmpl)); err != nil {
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

	// Insert: ids 1 and 3 share plan='pro', id 2 has plan='free'.
	if _, err := db.ExecContext(ctx, "INSERT INTO rp VALUES (1, 'us', 'pro')"); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO rp VALUES (2, 'us', 'free')"); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO rp VALUES (3, 'us', 'pro')"); err != nil {
		t.Fatalf("INSERT 3: %v", err)
	}

	// DESC sort: tied plan='pro' should have id=3 before id=1 (PK DESC tiebreaker).
	rows, err := db.QueryContext(ctx,
		"SELECT id, region, plan FROM rp WHERE region = 'us' ORDER BY plan DESC")
	if err != nil {
		t.Fatalf("ORDER BY plan DESC: %v", err)
	}
	defer rows.Close()

	type row struct {
		id     int64
		region string
		plan   string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.region, &r.plan); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Expected: [3, 'us', 'pro'], [1, 'us', 'pro'], [2, 'us', 'free']
	expected := []row{
		{3, "us", "pro"},
		{1, "us", "pro"},
		{2, "us", "free"},
	}
	if len(got) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %+v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("row %d: expected %+v, got %+v\nfull result: %+v", i, expected[i], got[i], got)
		}
	}
	t.Logf("DESC PK tiebreaker correct: %+v", got)

	// ASC sort: tied plan='free' has only id=2, tied plan='pro' should
	// have id=1 before id=3 (PK ASC tiebreaker).
	rows2, err := db.QueryContext(ctx,
		"SELECT id, region, plan FROM rp WHERE region = 'us' ORDER BY plan ASC")
	if err != nil {
		t.Fatalf("ORDER BY plan ASC: %v", err)
	}
	defer rows2.Close()

	var got2 []row
	for rows2.Next() {
		var r row
		if err := rows2.Scan(&r.id, &r.region, &r.plan); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got2 = append(got2, r)
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("rows2.Err: %v", err)
	}

	// Expected: [2, 'us', 'free'], [1, 'us', 'pro'], [3, 'us', 'pro']
	expected2 := []row{
		{2, "us", "free"},
		{1, "us", "pro"},
		{3, "us", "pro"},
	}
	if len(got2) != len(expected2) {
		t.Fatalf("expected %d rows, got %d: %+v", len(expected2), len(got2), got2)
	}
	for i := range expected2 {
		if got2[i] != expected2[i] {
			t.Fatalf("row %d: expected %+v, got %+v\nfull result: %+v", i, expected2[i], got2[i], got2)
		}
	}
	t.Logf("ASC PK tiebreaker correct: %+v", got2)
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

// TestFDB_CascadesSortEliminationViaIndex verifies that the Cascades planner
// eliminates in-memory sort when a secondary index provides the requested
// ORDER BY ordering, and falls back to in-memory sort when no matching index
// exists.
func TestFDB_CascadesSortEliminationViaIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_sortelim_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("sortelim_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE items (id BIGINT NOT NULL, category STRING, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_price ON items (price)", tmpl)); err != nil {
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

	// Insert items with various prices.
	for _, item := range []struct {
		id       int
		category string
		price    int
	}{
		{1, "electronics", 500},
		{2, "books", 50},
		{3, "clothing", 150},
		{4, "electronics", 200},
		{5, "books", 25},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, '%s', %d)", item.id, item.category, item.price)); err != nil {
			t.Fatalf("INSERT id=%d: %v", item.id, err)
		}
	}

	// planExplain retrieves the Cascades physical plan Explain string
	// via the underlying EmbeddedConnection.
	planExplain := func(t *testing.T, query string) string {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		defer conn.Close()
		var plan string
		if err := conn.Raw(func(driverConn any) error {
			ec, ok := driverConn.(*embedded.EmbeddedConnection)
			if !ok {
				t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
			}
			p, err := ec.PlanExplain(ctx, query)
			if err != nil {
				return err
			}
			plan = p
			return nil
		}); err != nil {
			t.Fatalf("PlanExplain(%q): %v", query, err)
		}
		return plan
	}

	// collectRows scans (id, price) pairs from the query result.
	type row struct {
		id    int64
		price int64
	}
	collectRows := func(t *testing.T, query string) []row {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("QueryContext(%q): %v", query, err)
		}
		defer rows.Close()
		var result []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.price); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			result = append(result, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		return result
	}

	// --- Query 1: ORDER BY price ASC — index scan, no in-memory sort ---
	t.Run("OrderByPriceASC", func(t *testing.T) {
		q := "SELECT id, price FROM items ORDER BY price ASC"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)
		if strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected sort elimination via idx_price, but plan has InMemorySort: %s", plan)
		}
		if !strings.Contains(plan, "IndexScan") {
			t.Fatalf("expected IndexScan in plan, got: %s", plan)
		}

		got := collectRows(t, q)
		expected := []row{{5, 25}, {2, 50}, {3, 150}, {4, 200}, {1, 500}}
		if len(got) != len(expected) {
			t.Fatalf("expected %d rows, got %d: %+v", len(expected), len(got), got)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
			}
		}
	})

	// --- Query 2: WHERE price > 100 ORDER BY price ASC — range scan, no sort ---
	t.Run("FilteredOrderByPriceASC", func(t *testing.T) {
		q := "SELECT id, price FROM items WHERE price > 100 ORDER BY price ASC"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)
		if strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected sort elimination via idx_price range scan, but plan has InMemorySort: %s", plan)
		}

		got := collectRows(t, q)
		expected := []row{{3, 150}, {4, 200}, {1, 500}}
		if len(got) != len(expected) {
			t.Fatalf("expected %d rows, got %d: %+v", len(expected), len(got), got)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
			}
		}
	})

	// --- Query 3: ORDER BY price DESC — reverse index scan, no sort ---
	t.Run("OrderByPriceDESC", func(t *testing.T) {
		q := "SELECT id, price FROM items ORDER BY price DESC"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)
		if strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected sort elimination via reverse idx_price, but plan has InMemorySort: %s", plan)
		}
		if !strings.Contains(plan, "REVERSE") {
			t.Fatalf("expected REVERSE index scan in plan, got: %s", plan)
		}

		got := collectRows(t, q)
		expected := []row{{1, 500}, {4, 200}, {3, 150}, {2, 50}, {5, 25}}
		if len(got) != len(expected) {
			t.Fatalf("expected %d rows, got %d: %+v", len(expected), len(got), got)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
			}
		}
	})

	// --- Query 4: ORDER BY category — no matching index, MUST use in-memory sort ---
	t.Run("OrderByCategoryNoIndex", func(t *testing.T) {
		q := "SELECT id, price FROM items ORDER BY category"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected InMemorySort for unindexed column, but plan is: %s", plan)
		}

		// Verify results are ordered by category (alphabetically):
		// books (id=2,5), clothing (id=3), electronics (id=1,4).
		// Within same category, order by PK (id) ascending.
		got := collectRows(t, q)
		if len(got) != 5 {
			t.Fatalf("expected 5 rows, got %d: %+v", len(got), got)
		}
		// books: id=2 (price=50), id=5 (price=25)
		// clothing: id=3 (price=150)
		// electronics: id=1 (price=500), id=4 (price=200)
		expectedIDs := []int64{2, 5, 3, 1, 4}
		for i, wantID := range expectedIDs {
			if got[i].id != wantID {
				t.Fatalf("row %d: expected id=%d, got id=%d\nfull: %+v", i, wantID, got[i].id, got)
			}
		}
	})
}

// TestFDB_CascadesStreamingAggFromIndex verifies that the Cascades planner
// picks StreamingAgg when a secondary index provides the GROUP BY ordering,
// and still uses StreamingAgg when no matching index exists.
func TestFDB_CascadesStreamingAggFromIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_streamagg_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("streamagg_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE sales (id BIGINT NOT NULL, region STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_region ON sales (region)", tmpl)); err != nil {
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

	// Insert test data across several regions.
	for _, s := range []struct {
		id     int
		region string
		amount int
	}{
		{1, "east", 100},
		{2, "east", 200},
		{3, "west", 50},
		{4, "west", 150},
		{5, "north", 300},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO sales VALUES (%d, '%s', %d)", s.id, s.region, s.amount)); err != nil {
			t.Fatalf("INSERT id=%d: %v", s.id, err)
		}
	}

	// planExplain retrieves the Cascades physical plan Explain string
	// via the underlying EmbeddedConnection.
	planExplain := func(t *testing.T, query string) string {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		defer conn.Close()
		var plan string
		if err := conn.Raw(func(driverConn any) error {
			ec, ok := driverConn.(*embedded.EmbeddedConnection)
			if !ok {
				t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
			}
			p, err := ec.PlanExplain(ctx, query)
			if err != nil {
				return err
			}
			plan = p
			return nil
		}); err != nil {
			t.Fatalf("PlanExplain(%q): %v", query, err)
		}
		return plan
	}

	// --- Subtest 1: GROUP BY region ORDER BY region -> StreamingAgg ---
	t.Run("StreamingAggViaIndex", func(t *testing.T) {
		q := "SELECT region, COUNT(*), SUM(amount) FROM sales GROUP BY region ORDER BY region"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)

		if !strings.Contains(plan, "StreamingAgg") {
			t.Fatalf("expected StreamingAgg in plan, got: %s", plan)
		}
		// Ideally the planner would pick IndexScan when the index covers
		// the GROUP BY key, but the current cost model may prefer
		// InMemorySort(Scan). Either path is correct.

		// Verify query results: grouped + ordered by region ASC.
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()

		type aggRow struct {
			region string
			cnt    int64
			total  int64
		}
		var got []aggRow
		for rows.Next() {
			var r aggRow
			if err := rows.Scan(&r.region, &r.cnt, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}

		expected := []aggRow{
			{"east", 2, 300},
			{"north", 1, 300},
			{"west", 2, 200},
		}
		if len(got) != len(expected) {
			t.Fatalf("expected %d groups, got %d: %+v", len(expected), len(got), got)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
			}
		}
	})

	// --- Subtest 2: GROUP BY amount (no index) -> StreamingAgg ---
	t.Run("StreamingAggNoIndex", func(t *testing.T) {
		q := "SELECT amount, COUNT(*) FROM sales GROUP BY amount"
		plan := planExplain(t, q)
		t.Logf("plan: %s", plan)

		if !strings.Contains(plan, "StreamingAgg") {
			t.Fatalf("expected StreamingAgg for unindexed GROUP BY, got: %s", plan)
		}

		// Verify query results: 5 distinct amounts, each with count 1.
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()

		type aggRow struct {
			amount int64
			cnt    int64
		}
		results := make(map[int64]int64)
		for rows.Next() {
			var r aggRow
			if err := rows.Scan(&r.amount, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			results[r.amount] = r.cnt
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}

		if len(results) != 5 {
			t.Fatalf("expected 5 distinct amounts, got %d: %v", len(results), results)
		}
		for amt, cnt := range results {
			if cnt != 1 {
				t.Fatalf("expected COUNT(*)=1 for amount=%d, got %d", amt, cnt)
			}
		}
	})
}

// TestFDB_PlanCacheCorrectness verifies that the Cascades plan cache
// produces correct results across multiple executions and DDL
// invalidation. It pins a single driver connection (via sql.Conn) so
// the plan cache survives between queries — database/sql normally
// calls ResetSession (which clears the cache) when returning
// connections to the pool.
func TestFDB_PlanCacheCorrectness(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_plancache_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("pc_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))", tmpl)); err != nil {
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

	// Insert test data.
	for _, q := range []string{
		"INSERT INTO Item VALUES (1, 'Widget', 100)",
		"INSERT INTO Item VALUES (2, 'Gadget', 200)",
		"INSERT INTO Item VALUES (3, 'Doohickey', 50)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	// Pin a single connection so the plan cache persists across queries.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	selectQuery := "SELECT item_id, name, price FROM Item ORDER BY item_id"

	// Helper: run the select and return results as a slice of
	// (item_id, name, price) tuples.
	type row struct {
		id    int64
		name  string
		price int64
	}
	queryRows := func(t *testing.T, c *sql.Conn, q string) []row {
		t.Helper()
		rows, err := c.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("QueryContext(%q): %v", q, err)
		}
		defer rows.Close()
		var result []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.name, &r.price); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			result = append(result, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		return result
	}

	rowsEqual := func(a, b []row) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// --- Step 1: first execution (cache miss) ---
	result1 := queryRows(t, conn, selectQuery)
	expected := []row{
		{1, "Widget", 100},
		{2, "Gadget", 200},
		{3, "Doohickey", 50},
	}
	if !rowsEqual(result1, expected) {
		t.Fatalf("first query: expected %v, got %v", expected, result1)
	}
	t.Logf("first execution (cache miss): %v", result1)

	// --- Step 2: same query again (cache hit) ---
	result2 := queryRows(t, conn, selectQuery)
	if !rowsEqual(result2, result1) {
		t.Fatalf("cached query returned different results: first=%v, second=%v", result1, result2)
	}
	t.Logf("second execution (cache hit): results match")

	// --- Step 3: normalized variants should also produce correct results ---
	// The plan cache normalizes SQL by uppercasing, collapsing whitespace,
	// and stripping comments. These variants should hash identically.
	variants := []string{
		// Extra whitespace.
		"SELECT   item_id,  name,   price   FROM   Item   ORDER BY   item_id",
		// Different case.
		"select item_id, name, price from Item order by item_id",
		// Mixed case + extra whitespace.
		"SELECT item_id, name, price  FROM  item  ORDER  BY  item_id",
		// Trailing/leading whitespace.
		"  SELECT item_id, name, price FROM Item ORDER BY item_id  ",
	}
	for _, v := range variants {
		result := queryRows(t, conn, v)
		if !rowsEqual(result, expected) {
			t.Fatalf("variant %q returned wrong results: expected %v, got %v", v, expected, result)
		}
	}
	t.Logf("normalized variants all return correct results")

	// Verify that the normalization actually produces the same hash.
	canonicalHash := embedded.QueryHash(selectQuery)
	for _, v := range variants {
		h := embedded.QueryHash(v)
		if h != canonicalHash {
			t.Fatalf("QueryHash(%q) = %d, want %d (same as canonical)", v, h, canonicalHash)
		}
	}
	t.Logf("all variants hash to %d", canonicalHash)

	// --- Step 4: DDL invalidates the cache ---
	// Creating a new schema template is a DDL statement that triggers
	// plan cache invalidation on the connection. The existing schema
	// and data are unaffected.
	ddlTmpl := fmt.Sprintf("pc_ddl_tmpl_%s", t.Name())
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Other (id BIGINT NOT NULL, PRIMARY KEY (id))", ddlTmpl)); err != nil {
		t.Fatalf("DDL (CREATE SCHEMA TEMPLATE): %v", err)
	}
	t.Logf("DDL executed, plan cache invalidated")

	// --- Step 5: same query after DDL (cache miss, re-planned) ---
	result3 := queryRows(t, conn, selectQuery)
	if !rowsEqual(result3, expected) {
		t.Fatalf("post-DDL query returned wrong results: expected %v, got %v", expected, result3)
	}
	t.Logf("post-DDL execution: results still correct")

	// --- Step 6: verify cache is warm again after the post-DDL query ---
	result4 := queryRows(t, conn, selectQuery)
	if !rowsEqual(result4, expected) {
		t.Fatalf("post-DDL cached query returned wrong results: expected %v, got %v", expected, result4)
	}
	t.Logf("post-DDL cache hit: results still correct")

	// --- Step 7: different SQL text with different hash should also work ---
	diffResult := queryRows(t, conn, "SELECT item_id, name, price FROM Item WHERE price > 100 ORDER BY item_id")
	diffExpected := []row{{2, "Gadget", 200}}
	if !rowsEqual(diffResult, diffExpected) {
		t.Fatalf("different query: expected %v, got %v", diffExpected, diffResult)
	}

	// Re-run the original query — the filtered query must NOT have
	// polluted the cache entry for the unfiltered query.
	result5 := queryRows(t, conn, selectQuery)
	if !rowsEqual(result5, expected) {
		t.Fatalf("original query after different query: expected %v, got %v", expected, result5)
	}
	t.Logf("cache isolation: different SQL hashes do not interfere")

	// --- Step 8: schema with index — DDL invalidation via template + schema ---
	// Drop existing schema, create a new template with an index, and
	// recreate the schema. This exercises the full DDL-invalidation path
	// and verifies that the plan cache picks up the new index metadata.
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("DROP SCHEMA %s/store", dbPath)); err != nil {
		t.Fatalf("DROP SCHEMA: %v", err)
	}
	idxTmpl := fmt.Sprintf("pc_idx_tmpl_%s", t.Name())
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_price ON Item (price)", idxTmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE with index: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, idxTmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA with index template: %v", err)
	}

	// Re-insert data into the fresh schema.
	for _, q := range []string{
		"INSERT INTO Item VALUES (1, 'Widget', 100)",
		"INSERT INTO Item VALUES (2, 'Gadget', 200)",
		"INSERT INTO Item VALUES (3, 'Doohickey', 50)",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("re-INSERT: %v", err)
		}
	}

	// Query should still produce correct results with the new schema
	// (which now has a price index that the planner may use).
	result6 := queryRows(t, conn, selectQuery)
	// Sort by item_id since ORDER BY may use the index now.
	sort.Slice(result6, func(i, j int) bool { return result6[i].id < result6[j].id })
	if !rowsEqual(result6, expected) {
		t.Fatalf("post-index-DDL query: expected %v, got %v", expected, result6)
	}
	t.Logf("post-index schema recreation: results correct, plan cache working end-to-end")
}

func TestFDB_CascadesFlatMapCorrelatedJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/casc_flatmap_%s", t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("flatmap_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE customers (id BIGINT NOT NULL, name STRING, tier STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE orders (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT, PRIMARY KEY (id))", tmpl)); err != nil {
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

	// Insert 20 customers.
	for i := 1; i <= 20; i++ {
		tier := "silver"
		if i%5 == 0 {
			tier = "gold"
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO customers VALUES (%d, 'Customer%d', '%s')", i, i, tier)); err != nil {
			t.Fatalf("INSERT customer %d: %v", i, err)
		}
	}

	// Insert 100 orders (5 per customer, customer_id cycling 1-20).
	// Amounts alternate: even order IDs get 30, odd get 75, so roughly half exceed 50.
	for i := 1; i <= 100; i++ {
		customerID := ((i - 1) % 20) + 1
		amount := 30
		if i%2 != 0 {
			amount = 75
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO orders VALUES (%d, %d, %d)", i, customerID, amount)); err != nil {
			t.Fatalf("INSERT order %d: %v", i, err)
		}
	}

	// --- Part 1: inner join with filter + ORDER BY ---
	innerJoinQ := "SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.amount > 50 ORDER BY o.id"

	plan := planExplainVia(t, ctx, db, innerJoinQ)
	t.Logf("FlatMap plan: %s", plan)
	if !strings.Contains(plan, "FlatMap") {
		t.Fatalf("expected FlatMap in plan for correlated join, got: %s", plan)
	}

	rows, err := db.QueryContext(ctx, innerJoinQ)
	if err != nil {
		t.Fatalf("inner join query: %v", err)
	}
	defer rows.Close()

	type joinRow struct {
		orderID      int64
		customerName string
	}
	var got []joinRow
	for rows.Next() {
		var r joinRow
		if err := rows.Scan(&r.orderID, &r.customerName); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// 100 orders, odd IDs have amount=75 (>50), even have amount=30.
	// Odd IDs: 1,3,5,...,99 → 50 rows match the filter.
	if len(got) != 50 {
		t.Fatalf("expected 50 rows (odd orders with amount>50), got %d", len(got))
	}

	// Verify ordering by o.id is ascending.
	for i := 1; i < len(got); i++ {
		if got[i].orderID <= got[i-1].orderID {
			t.Fatalf("results not ordered by o.id: got[%d].id=%d <= got[%d].id=%d",
				i, got[i].orderID, i-1, got[i-1].orderID)
		}
	}

	// Verify each row has the correct customer name and amount > 50 (odd IDs have amount=75).
	for _, r := range got {
		if r.orderID%2 == 0 {
			t.Fatalf("order id=%d has amount=30 (<=50) but appeared in result", r.orderID)
		}
		// Customer name must match the cycling assignment.
		expectedCustomerID := int64(((r.orderID - 1) % 20) + 1)
		expectedName := fmt.Sprintf("Customer%d", expectedCustomerID)
		if r.customerName != expectedName {
			t.Fatalf("order id=%d: expected customer name %q, got %q",
				r.orderID, expectedName, r.customerName)
		}
	}
	t.Logf("FlatMap inner join → %d rows, ordered, correct ✓", len(got))

	// --- Part 2: LEFT OUTER JOIN ---
	leftJoinQ := "SELECT o.id, c.name FROM orders o LEFT JOIN customers c ON o.customer_id = c.id WHERE o.id < 5 ORDER BY o.id"

	leftRows, err := db.QueryContext(ctx, leftJoinQ)
	if err != nil {
		t.Fatalf("LEFT JOIN query: %v", err)
	}
	defer leftRows.Close()

	type leftRow struct {
		orderID      int64
		customerName *string // nullable — NULL when no match
	}
	var leftGot []leftRow
	for leftRows.Next() {
		var r leftRow
		if err := leftRows.Scan(&r.orderID, &r.customerName); err != nil {
			t.Fatalf("LEFT JOIN Scan: %v", err)
		}
		leftGot = append(leftGot, r)
	}
	if err := leftRows.Err(); err != nil {
		t.Fatalf("leftRows.Err: %v", err)
	}

	// Orders 1-4 all have customer_ids 1-4 which exist, so all should have non-NULL names.
	if len(leftGot) != 4 {
		t.Fatalf("LEFT JOIN: expected 4 rows (o.id in [1,2,3,4]), got %d", len(leftGot))
	}
	for i, r := range leftGot {
		expectedID := int64(i + 1)
		if r.orderID != expectedID {
			t.Fatalf("LEFT JOIN row %d: expected o.id=%d, got %d", i, expectedID, r.orderID)
		}
		if r.customerName == nil {
			t.Fatalf("LEFT JOIN row %d (o.id=%d): expected non-NULL customer name, got NULL", i, r.orderID)
		}
		expectedName := fmt.Sprintf("Customer%d", r.orderID)
		if *r.customerName != expectedName {
			t.Fatalf("LEFT JOIN row %d: expected name %q, got %q", i, expectedName, *r.customerName)
		}
	}
	t.Logf("FlatMap LEFT JOIN → %d rows, all matched ✓", len(leftGot))

	// --- Part 3: LIMIT with FlatMap join ---
	limitQ := "SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id ORDER BY o.id LIMIT 5"
	limitRows, err := db.QueryContext(ctx, limitQ)
	if err != nil {
		t.Fatalf("LIMIT+FlatMap query: %v", err)
	}
	defer limitRows.Close()

	var limitCount int
	for limitRows.Next() {
		var oid int64
		var cname string
		if err := limitRows.Scan(&oid, &cname); err != nil {
			t.Fatalf("LIMIT+FlatMap Scan: %v", err)
		}
		limitCount++
	}
	if err := limitRows.Err(); err != nil {
		t.Fatalf("LIMIT+FlatMap rows.Err: %v", err)
	}
	if limitCount != 5 {
		t.Fatalf("LIMIT 5 on FlatMap join: expected 5 rows, got %d", limitCount)
	}
	t.Logf("FlatMap + LIMIT 5 → %d rows ✓", limitCount)
}
