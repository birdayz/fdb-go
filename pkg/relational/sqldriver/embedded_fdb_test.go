package sqldriver_test

// FDB integration tests for the embedded SQL connection. Tests spin up a real
// FoundationDB container and verify that DDL SQL (CREATE/DROP DATABASE/SCHEMA)
// round-trips through the full stack: sql.DB → driver.Conn → parser →
// MetadataOperationsFactory → FDB.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/onsi/gomega"

	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// clusterFilePath is written once in TestMain and shared across tests.
var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — run non-FDB tests only.
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}

	tmp, err := os.CreateTemp("", "fdb-sqldriver-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// openTestDB returns a *sql.DB wired to the test FDB container.
// Skips the test if Docker is not available.
func openTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_EmbeddedCreateDropDatabase(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_create_drop")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE /testdb_create_drop"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE /testdb_create_drop"); err != nil {
		t.Fatalf("DROP DATABASE: %v", err)
	}
}

func TestFDB_EmbeddedCreateDatabaseIdempotencyFails(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_dup")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE /testdb_dup"); err != nil {
		t.Fatalf("first CREATE DATABASE: %v", err)
	}
	// Second create must fail: database already exists.
	_, err := db.ExecContext(ctx, "CREATE DATABASE /testdb_dup")
	if err == nil {
		t.Fatal("expected error on duplicate CREATE DATABASE, got nil")
	}
}

func TestFDB_EmbeddedDropDatabaseIfExists(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_drop_noexist")
	ctx := context.Background()

	// Drop with IF EXISTS on non-existent database should succeed.
	if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS /testdb_drop_noexist"); err != nil {
		t.Fatalf("DROP DATABASE IF EXISTS: %v", err)
	}
}

func TestFDB_EmbeddedCreateDropSchemaTemplate(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_schema_tmpl")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE test_tmpl "+
			"CREATE TABLE RestaurantRecord (rest_no BIGINT NOT NULL, name STRING, PRIMARY KEY (rest_no))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}

	if _, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE test_tmpl"); err != nil {
		t.Fatalf("DROP SCHEMA TEMPLATE: %v", err)
	}
}

func TestFDB_EmbeddedCreateSchemaDuplicateTemplateFails(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_schema_tmpl_dup")
	ctx := context.Background()

	ddl := "CREATE SCHEMA TEMPLATE dup_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))"
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("first CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx, ddl); err == nil {
		t.Fatal("expected error on duplicate CREATE SCHEMA TEMPLATE, got nil")
	}
}

func TestFDB_EmbeddedCreateSchemaFullFlow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_full_flow")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE /testdb_full_flow"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE restaurant_tmpl "+
			"CREATE TABLE RestaurantRecord (rest_no BIGINT NOT NULL, name STRING, PRIMARY KEY (rest_no))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA /testdb_full_flow/restaurant WITH TEMPLATE restaurant_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP SCHEMA /testdb_full_flow/restaurant"); err != nil {
		t.Fatalf("DROP SCHEMA: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE restaurant_tmpl"); err != nil {
		t.Fatalf("DROP SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE /testdb_full_flow"); err != nil {
		t.Fatalf("DROP DATABASE: %v", err)
	}
}

func TestFDB_EmbeddedPingSucceeds(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_ping")
	ctx := context.Background()

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PingContext: %v", err)
	}
}

func TestFDB_EmbeddedDropSchemaTemplateIfExists(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_drop_tmpl")
	ctx := context.Background()

	// Drop a non-existent template with IF EXISTS must succeed.
	if _, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS nonexistent_tmpl"); err != nil {
		t.Fatalf("DROP SCHEMA TEMPLATE IF EXISTS: %v", err)
	}
}

func TestFDB_EmbeddedDropSchemaTemplateNotExistFails(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_drop_tmpl_fail")
	ctx := context.Background()

	// Drop a non-existent template without IF EXISTS must fail.
	_, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE missing_tmpl")
	if err == nil {
		t.Fatal("expected error dropping non-existent template, got nil")
	}
}

func TestFDB_EmbeddedSelectReturnsUnsupported(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_select")
	ctx := context.Background()

	_, err := db.ExecContext(ctx, "SELECT 1")
	if err == nil {
		t.Fatal("SELECT should return error (query planner not implemented)")
	}
}

func TestFDB_EmbeddedShowDatabases(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_show_db")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE /testdb_show_db"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		t.Fatalf("SHOW DATABASES: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var dbID string
		if err := rows.Scan(&dbID); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if dbID == "/testdb_show_db" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if !found {
		t.Error("SHOW DATABASES: did not find /testdb_show_db")
	}
}

func TestFDB_EmbeddedShowSchemaTemplates(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_show_tmpl")
	ctx := context.Background()

	const ddl = "CREATE SCHEMA TEMPLATE show_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))"
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SHOW SCHEMA TEMPLATES")
	if err != nil {
		t.Fatalf("SHOW SCHEMA TEMPLATES: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var name string
		var version int64
		if err := rows.Scan(&name, &version); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if name == "show_tmpl" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if !found {
		t.Error("SHOW SCHEMA TEMPLATES: did not find show_tmpl")
	}
}

func TestFDB_EmbeddedCreateSchemaTemplateWithIndex(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_tmpl_idx")
	ctx := context.Background()

	ddl := "CREATE SCHEMA TEMPLATE indexed_tmpl " +
		"CREATE TABLE Order (order_id BIGINT NOT NULL, customer_id BIGINT, total BIGINT, PRIMARY KEY (order_id)) " +
		"CREATE INDEX by_customer ON Order (customer_id)"
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE with index: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE indexed_tmpl"); err != nil {
		t.Fatalf("DROP SCHEMA TEMPLATE: %v", err)
	}
}

func TestFDB_EmbeddedCreateSchemaTemplateWithUniqueIndex(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, "/testdb_tmpl_uniq")
	ctx := context.Background()

	ddl := "CREATE SCHEMA TEMPLATE unique_tmpl " +
		"CREATE TABLE Employee (emp_id BIGINT NOT NULL, email STRING NOT NULL, PRIMARY KEY (emp_id)) " +
		"CREATE UNIQUE INDEX by_email ON Employee (email)"
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE with unique index: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE unique_tmpl"); err != nil {
		t.Fatalf("DROP SCHEMA TEMPLATE: %v", err)
	}
}

func TestFDB_EmbeddedInsert(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// Use a dedicated DB connection for setup DDL (no schema yet).
	setup := openTestDB(t, "/testdb_insert")

	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_insert"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE insert_tmpl "+
			"CREATE TABLE Employee (emp_id BIGINT NOT NULL, name STRING, PRIMARY KEY (emp_id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_insert/emp WITH TEMPLATE insert_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	// Open a new connection with the schema set via DSN.
	dsn := fmt.Sprintf("fdbsql:///testdb_insert?cluster_file=%s&schema=emp", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INSERT a row.
	res, err := db.ExecContext(ctx, "INSERT INTO Employee (emp_id, name) VALUES (1, 'Alice')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if rows != 1 {
		t.Errorf("RowsAffected = %d, want 1", rows)
	}
}

func TestFDB_EmbeddedInsertMultiRow(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_insert_multi")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_insert_multi"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE multi_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, label STRING, PRIMARY KEY (item_id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_insert_multi/items WITH TEMPLATE multi_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_insert_multi?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	res, err := db.ExecContext(ctx,
		"INSERT INTO Item (item_id, label) VALUES (1, 'first'), (2, 'second'), (3, 'third')")
	if err != nil {
		t.Fatalf("INSERT multi-row: %v", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if rows != 3 {
		t.Errorf("RowsAffected = %d, want 3", rows)
	}
}

func TestFDB_EmbeddedInsertNoSchemaFails(t *testing.T) {
	t.Parallel()
	// No schema= in DSN — INSERT should fail with "no schema selected".
	db := openTestDB(t, "/testdb_insert_noschema")
	ctx := context.Background()

	_, err := db.ExecContext(ctx, "INSERT INTO Employee (emp_id) VALUES (1)")
	if err == nil {
		t.Fatal("INSERT without schema should fail")
	}
}

func TestFDB_EmbeddedSelectAfterInsert(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_select_insert")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_select_insert"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sel_tmpl "+
			"CREATE TABLE Person (person_id BIGINT NOT NULL, name STRING, PRIMARY KEY (person_id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_select_insert/people WITH TEMPLATE sel_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_select_insert?cluster_file=%s&schema=people", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Insert two rows.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO Person (person_id, name) VALUES (1, 'Alice'), (2, 'Bob')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// SELECT * should return both rows.
	rows, err := db.QueryContext(ctx, "SELECT * FROM Person")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("expected columns, got none")
	}

	var count int
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if count != 2 {
		t.Errorf("row count = %d, want 2", count)
	}
}

func TestFDB_EmbeddedDeleteByPK(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_delete_pk")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_delete_pk")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE del_tmpl "+
			"CREATE TABLE Widget (widget_id BIGINT NOT NULL, label STRING, PRIMARY KEY (widget_id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_delete_pk/widgets WITH TEMPLATE del_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_delete_pk?cluster_file=%s&schema=widgets", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		"INSERT INTO Widget (widget_id, label) VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma')")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	res, err := db.ExecContext(ctx, "DELETE FROM Widget WHERE widget_id = 2")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	affected, err := res.RowsAffected()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(affected).To(gomega.Equal(int64(1)))

	rows, err := db.QueryContext(ctx, "SELECT * FROM Widget")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var count int
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		count++
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(count).To(gomega.Equal(2))
}

func TestFDB_EmbeddedUpdateWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_update_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_update_where")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE upd_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, PRIMARY KEY (item_id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_update_where/items WITH TEMPLATE upd_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_update_where?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		"INSERT INTO Item (item_id, name) VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma')")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	res, err := db.ExecContext(ctx, "UPDATE Item SET name = 'updated' WHERE item_id = 2")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	affected, err := res.RowsAffected()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(affected).To(gomega.Equal(int64(1)))

	// Verify via SELECT * that only row 2 changed.
	rows, err := db.QueryContext(ctx, "SELECT * FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Collect item_id → name mapping.
	nameByID := map[int64]string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		// cols[0] = item_id, cols[1] = name (proto field declaration order)
		id, ok := vals[0].(int64)
		g.Expect(ok).To(gomega.BeTrue(), "item_id should be int64")
		name, _ := vals[1].(string)
		nameByID[id] = name
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(nameByID).To(gomega.HaveLen(3))
	g.Expect(nameByID[1]).To(gomega.Equal("alpha"))
	g.Expect(nameByID[2]).To(gomega.Equal("updated"))
	g.Expect(nameByID[3]).To(gomega.Equal("gamma"))
}

func TestFDB_EmbeddedSelectWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_select_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_select_where")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sw_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, PRIMARY KEY (item_id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_select_where/items WITH TEMPLATE sw_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_select_where?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		"INSERT INTO Item (item_id, name) VALUES (1, 'apple'), (2, 'banana'), (3, 'cherry')")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT * FROM Item WHERE item_id = 2")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var count int
	var foundID any
	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		// item_id is the first field (field order from proto descriptor)
		foundID = vals[0]
		count++
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(count).To(gomega.Equal(1))
	g.Expect(foundID).To(gomega.Equal(int64(2)))
}

func TestFDB_InfoSchema_Schemata(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_is_schemata")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_schemata")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE is_schemata_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_is_schemata/schema1 WITH TEMPLATE is_schemata_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_is_schemata/schema2 WITH TEMPLATE is_schemata_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// System table queries do not require a schema in the DSN.
	dsn := fmt.Sprintf("fdbsql:///testdb_is_schemata?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.ConsistOf("CATALOG_NAME", "SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME", "SQL_PATH"))

	found := map[string]bool{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		// CATALOG_NAME is at index 0, SCHEMA_NAME at index 1.
		schemaName, _ := vals[1].(string)
		found[schemaName] = true
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(found).To(gomega.HaveKey("schema1"))
	g.Expect(found).To(gomega.HaveKey("schema2"))
}

func TestFDB_InfoSchema_Tables(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_is_tables")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_tables")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE is_tables_tmpl "+
			"CREATE TABLE T1 (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE T2 (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_is_tables/myschema WITH TEMPLATE is_tables_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_tables?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."TABLES"`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE",
		"REMARKS", "TYPE_CAT", "TYPE_SCHEM", "TYPE_NAME",
		"SELF_REFERENCING_COL_NAME", "REF_GENERATION",
	}))

	found := map[string]bool{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		tableName, _ := vals[2].(string)
		found[tableName] = true
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(found).To(gomega.HaveKey("T1"))
	g.Expect(found).To(gomega.HaveKey("T2"))
}

func TestFDB_InfoSchema_Columns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_is_columns")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_columns")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE is_columns_tmpl "+
			"CREATE TABLE Employee (emp_id BIGINT NOT NULL, name STRING, PRIMARY KEY (emp_id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_is_columns/hr WITH TEMPLATE is_columns_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_columns?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."COLUMNS"`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE",
	}))

	// Collect column info for the Employee table.
	type colRow struct {
		tableName string
		colName   string
		ordinal   int64
		nullable  string
		dataType  string
	}
	var colRows []colRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		dbCatalog, _ := vals[0].(string)
		tbl, _ := vals[2].(string)
		// Filter to this test's database only — other parallel tests may also
		// have an "Employee" table in a different database.
		if dbCatalog != "/testdb_is_columns" || tbl != "Employee" {
			continue
		}
		ordinal, _ := vals[4].(int64)
		colRows = append(colRows, colRow{
			tableName: tbl,
			colName:   vals[3].(string),
			ordinal:   ordinal,
			nullable:  vals[6].(string),
			dataType:  vals[7].(string),
		})
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(colRows).To(gomega.HaveLen(2))

	// Verify emp_id: NOT NULL, BIGINT (CodeLong).
	g.Expect(colRows[0].colName).To(gomega.Equal("emp_id"))
	g.Expect(colRows[0].ordinal).To(gomega.Equal(int64(1)))
	g.Expect(colRows[0].nullable).To(gomega.Equal("NO"))
	g.Expect(colRows[0].dataType).To(gomega.Equal("LONG"))

	// Verify name: nullable STRING (CodeString).
	g.Expect(colRows[1].colName).To(gomega.Equal("name"))
	g.Expect(colRows[1].ordinal).To(gomega.Equal(int64(2)))
	g.Expect(colRows[1].nullable).To(gomega.Equal("YES"))
	g.Expect(colRows[1].dataType).To(gomega.Equal("STRING"))
}

func TestFDB_ParameterizedQuery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_paramquery")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_paramquery")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE pq_tmpl "+
			"CREATE TABLE Widget (widget_id BIGINT NOT NULL, label STRING, PRIMARY KEY (widget_id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_paramquery/widgets WITH TEMPLATE pq_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_paramquery?cluster_file=%s&schema=widgets", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Parameterized INSERT.
	stmt, err := db.PrepareContext(ctx,
		"INSERT INTO Widget (widget_id, label) VALUES (?, ?)")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer stmt.Close()

	for i := int64(1); i <= 3; i++ {
		label := fmt.Sprintf("widget-%d", i)
		_, err = stmt.ExecContext(ctx, i, label)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// Parameterized SELECT WHERE.
	rows, err := db.QueryContext(ctx,
		"SELECT * FROM Widget WHERE widget_id = ?", int64(2))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var count int
	var foundID any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		foundID = vals[0]
		count++
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(count).To(gomega.Equal(1))
	g.Expect(foundID).To(gomega.Equal(int64(2)))

	// Parameterized DELETE WHERE.
	res, err := db.ExecContext(ctx,
		"DELETE FROM Widget WHERE widget_id = ?", int64(1))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	affected, err := res.RowsAffected()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(affected).To(gomega.Equal(int64(1)))

	// Verify 2 rows remain.
	rows2, err := db.QueryContext(ctx, "SELECT * FROM Widget")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var remaining int
	for rows2.Next() {
		remaining++
		vals2 := make([]any, len(cols))
		ptrs2 := make([]any, len(cols))
		for i := range vals2 {
			ptrs2[i] = &vals2[i]
		}
		g.Expect(rows2.Scan(ptrs2...)).To(gomega.Succeed())
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(remaining).To(gomega.Equal(2))
}

func TestFDB_InfoSchema_Indexes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_is_indexes")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_indexes")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE is_idx_tmpl "+
			"CREATE TABLE Product (prod_id BIGINT NOT NULL, name STRING, PRIMARY KEY (prod_id)) "+
			"CREATE INDEX by_name ON Product (name) "+
			"CREATE UNIQUE INDEX by_id ON Product (prod_id)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_is_indexes/catalog WITH TEMPLATE is_idx_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_indexes?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."INDEXES"`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME",
		"INDEX_NAME", "INDEX_TYPE", "IS_UNIQUE", "IS_SPARSE",
	}))

	type idxRow struct {
		tableName string
		indexName string
		isUnique  string
	}
	var idxRows []idxRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		dbCat, _ := vals[0].(string)
		if dbCat != "/testdb_is_indexes" {
			continue
		}
		idxRows = append(idxRows, idxRow{
			tableName: vals[2].(string),
			indexName: vals[3].(string),
			isUnique:  vals[5].(string),
		})
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(idxRows).To(gomega.HaveLen(2))

	// Build a name→row map for order-independent assertions.
	byName := map[string]idxRow{}
	for _, r := range idxRows {
		byName[r.indexName] = r
	}
	g.Expect(byName).To(gomega.HaveKey("by_name"))
	g.Expect(byName["by_name"].tableName).To(gomega.Equal("Product"))
	g.Expect(byName["by_name"].isUnique).To(gomega.Equal("NO"))

	g.Expect(byName).To(gomega.HaveKey("by_id"))
	g.Expect(byName["by_id"].tableName).To(gomega.Equal("Product"))
	g.Expect(byName["by_id"].isUnique).To(gomega.Equal("YES"))
}

func TestFDB_SelectColumnProjection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_proj")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_proj")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE proj_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_proj/store WITH TEMPLATE proj_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_proj?cluster_file=%s&schema=store", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO Item (item_id, name, price) VALUES (1, 'apple', 100), (2, 'banana', 50)")).Error().NotTo(gomega.HaveOccurred())

	// Single-column projection.
	rows, err := db.QueryContext(ctx, "SELECT name FROM Item WHERE item_id = ?", int64(1))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{"name"}))
	var names []string
	for rows.Next() {
		var n string
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		names = append(names, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"apple"}))

	// Multi-column projection.
	rows2, err := db.QueryContext(ctx, "SELECT item_id, price FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	cols2, err := rows2.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols2).To(gomega.Equal([]string{"item_id", "price"}))
	var itemCount int
	for rows2.Next() {
		var id, p any
		g.Expect(rows2.Scan(&id, &p)).To(gomega.Succeed())
		itemCount++
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(itemCount).To(gomega.Equal(2))
}

// TestFDB_ParameterizedQueryApostrophe verifies that a string with an
// apostrophe round-trips correctly through substituteParams → SQL → parser →
// FDB → SELECT. This catches the ”→' unescaping in evalConstant.
func TestFDB_ParameterizedQueryApostrophe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_apostrophe")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_apostrophe")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE apos_tmpl "+
			"CREATE TABLE Note (note_id BIGINT NOT NULL, body STRING, PRIMARY KEY (note_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_apostrophe/notes WITH TEMPLATE apos_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_apostrophe?cluster_file=%s&schema=notes", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	const wantBody = "it's a test"
	_, err = db.ExecContext(ctx,
		"INSERT INTO Note (note_id, body) VALUES (?, ?)", int64(1), wantBody)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT * FROM Note WHERE note_id = ?", int64(1))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var gotBody string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		gotBody, _ = vals[1].(string)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(gotBody).To(gomega.Equal(wantBody))
}

// TestFDB_InsertMissingPK verifies that INSERT without a required PRIMARY KEY
// column returns an error. Proto2 marks NOT NULL columns as "required", and
// proto serialization enforces required-field presence, so the INSERT fails
// with RecordSerializationError rather than silently inserting a zero-keyed row.
func TestFDB_InsertMissingPK(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_missing_pk")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_missing_pk")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE mpk_tmpl "+
			"CREATE TABLE Rec (rec_id BIGINT NOT NULL, val STRING, PRIMARY KEY (rec_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_missing_pk/recs WITH TEMPLATE mpk_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_missing_pk?cluster_file=%s&schema=recs", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// INSERT without pk — proto2 NOT NULL fields are "required"; protobuf
	// serialization rejects the message with RecordSerializationError.
	_, err = db.ExecContext(ctx, "INSERT INTO Rec (val) VALUES ('no-pk')")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("rec_id"))
}

// TestFDB_SelectWhereTypeMismatch verifies that comparing a BIGINT column
// against a string constant returns no rows (valuesEqual returns false)
// rather than panicking or erroring.
func TestFDB_SelectWhereTypeMismatch(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_type_mismatch")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_type_mismatch")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE tm_tmpl "+
			"CREATE TABLE Obj (obj_id BIGINT NOT NULL, name STRING, PRIMARY KEY (obj_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_type_mismatch/objs WITH TEMPLATE tm_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_type_mismatch?cluster_file=%s&schema=objs", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Obj (obj_id, name) VALUES (1, 'a'), (2, 'b')")).Error().NotTo(gomega.HaveOccurred())

	// Compare BIGINT column against a string — should return no rows (type mismatch → false predicate).
	rows, err := db.QueryContext(ctx, "SELECT * FROM Obj WHERE obj_id = 'notanumber'")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var count int
	cols, _ := rows.Columns()
	for rows.Next() {
		count++
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(count).To(gomega.Equal(0))
}

func TestFDB_SelectOrderBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_orderby/items WITH TEMPLATE ob_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_orderby?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (3, 300), (1, 100), (2, 200)")).Error().NotTo(gomega.HaveOccurred())

	// ORDER BY val ASC
	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id, val int64
		g.Expect(rows.Scan(&id, &val)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 2, 3}))
}

func TestFDB_SelectOrderByDesc(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby_desc")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby_desc")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE obdesc_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_orderby_desc/items WITH TEMPLATE obdesc_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_orderby_desc?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 100), (2, 200), (3, 300)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item ORDER BY val DESC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id, val int64
		g.Expect(rows.Scan(&id, &val)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{3, 2, 1}))
}

func TestFDB_SelectLimit(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_limit")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_limit")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE lim_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_limit/items WITH TEMPLATE lim_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_limit?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id) VALUES (1), (2), (3), (4), (5)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY item_id ASC LIMIT 3")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.HaveLen(3))
}

func TestFDB_SelectWhereAnd(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_and")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_and")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE wa_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_and/items WITH TEMPLATE wa_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_and?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// WHERE item_id = 2 AND val = 20 → matches only row 2
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE item_id = 2 AND val = 20")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2}))
}

func TestFDB_SelectWhereOr(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_or")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_or")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE wo_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_or/items WITH TEMPLATE wo_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_or?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// WHERE item_id = 1 OR item_id = 3 → rows 1 and 3
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE item_id = 1 OR item_id = 3 ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 3}))
}

func TestFDB_SelectWhereRangeComparison(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_range")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_range")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE wr_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_range/items WITH TEMPLATE wr_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_range?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)")).Error().NotTo(gomega.HaveOccurred())

	// WHERE val > 20 AND val <= 40 → rows with val 30 and 40
	rows, err := db.QueryContext(ctx, "SELECT val FROM Item WHERE val > 20 AND val <= 40 ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var vals []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(vals).To(gomega.Equal([]int64{30, 40}))
}

func TestFDB_DeleteWhereAnd(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_del_and")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_del_and")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE da_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_del_and/items WITH TEMPLATE da_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_del_and?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// DELETE WHERE item_id = 2 AND val = 20 — should delete only row 2.
	res, err := db.ExecContext(ctx, "DELETE FROM Item WHERE item_id = 2 AND val = 20")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	n, _ := res.RowsAffected()
	g.Expect(n).To(gomega.Equal(int64(1)))

	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 3}))
}

func TestFDB_UpdateWhereRange(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_upd_range")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_upd_range")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ur_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_upd_range/items WITH TEMPLATE ur_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_upd_range?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// UPDATE SET val = 99 WHERE val >= 20 — should update rows 2 and 3.
	res, err := db.ExecContext(ctx, "UPDATE Item SET val = 99 WHERE val >= 20")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	n, _ := res.RowsAffected()
	g.Expect(n).To(gomega.Equal(int64(2)))

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		var id, v int64
		g.Expect(rows.Scan(&id, &v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Row 1 unchanged (val=10), rows 2+3 updated to 99.
	g.Expect(vals).To(gomega.Equal([]int64{10, 99, 99}))
}

func TestFDB_SelectCountStar(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_count_star")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_count_star")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE cs_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_count_star/items WITH TEMPLATE cs_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_count_star?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// SELECT COUNT(*) should return 3.
	row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item")
	var count int64
	g.Expect(row.Scan(&count)).To(gomega.Succeed())
	g.Expect(count).To(gomega.Equal(int64(3)))

	// SELECT COUNT(*) WHERE val > 15 should return 2.
	row2 := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item WHERE val > 15")
	var count2 int64
	g.Expect(row2.Scan(&count2)).To(gomega.Succeed())
	g.Expect(count2).To(gomega.Equal(int64(2)))
}

func TestFDB_SelectWhereNot(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_not")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_not")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE wn_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_not/items WITH TEMPLATE wn_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_not?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id) VALUES (1), (2), (3)")).Error().NotTo(gomega.HaveOccurred())

	// WHERE NOT item_id = 2 → rows 1 and 3.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE NOT item_id = 2 ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 3}))
}

func TestFDB_SelectOrderByNotInProjection(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ob_noproj")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_ob_noproj")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE onp_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_ob_noproj/items WITH TEMPLATE onp_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ob_noproj?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())

	// ORDER BY on a column not in SELECT list must return an error.
	_, err = db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY val ASC")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("ORDER BY column"))
}

func TestFDB_SelectDistinct(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_distinct")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_distinct")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dist_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_distinct/items WITH TEMPLATE dist_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_distinct?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert 4 rows with only 2 distinct val values.
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 10), (3, 20), (4, 20)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT DISTINCT val FROM Item ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var vals []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Expect exactly 2 distinct values.
	g.Expect(vals).To(gomega.Equal([]int64{10, 20}))
}

func TestFDB_SelectWhereIn(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_in")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_in")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE in_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_in/items WITH TEMPLATE in_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_in?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30), (4, 40)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item WHERE val IN (10, 30) ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.val)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{1, 10}, {3, 30}}))
}

func TestFDB_SelectWhereNotIn(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_not_in")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_not_in")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nin_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_not_in/items WITH TEMPLATE nin_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_not_in?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30), (4, 40)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item WHERE val NOT IN (10, 30) ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.val)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{2, 20}, {4, 40}}))
}

func TestFDB_SelectWhereIsNull(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_is_null")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_is_null")).Error().NotTo(gomega.HaveOccurred())
	// val is nullable (no NOT NULL constraint) so unset fields appear as NULL.
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE isnull_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_is_null/items WITH TEMPLATE isnull_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_is_null?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// item_id=1 has val set, item_id=2 has no val (NULL).
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 42)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id) VALUES (2)")).Error().NotTo(gomega.HaveOccurred())

	// IS NULL — should return only item_id=2.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE val IS NULL ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2}))

	// IS NOT NULL — should return only item_id=1.
	rows2, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE val IS NOT NULL ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var ids2 []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids2 = append(ids2, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids2).To(gomega.Equal([]int64{1}))
}

func TestFDB_SelectWhereLike(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_like")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_like")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE like_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_like/items WITH TEMPLATE like_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_like?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, name) VALUES (1, 'apple'), (2, 'apricot'), (3, 'banana'), (4, 'cherry')")).Error().NotTo(gomega.HaveOccurred())

	// LIKE 'ap%' — should return apple, apricot.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE name LIKE 'ap%' ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 2}))

	// NOT LIKE 'ap%' — should return banana, cherry.
	rows2, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE name NOT LIKE 'ap%' ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var ids2 []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids2 = append(ids2, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids2).To(gomega.Equal([]int64{3, 4}))
}

func TestFDB_SelectWhereBetween(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_between")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_between")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE between_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_between/items WITH TEMPLATE between_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_between?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)")).Error().NotTo(gomega.HaveOccurred())

	// BETWEEN 20 AND 40 — inclusive, should return 2, 3, 4.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE val BETWEEN 20 AND 40 ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2, 3, 4}))

	// NOT BETWEEN 20 AND 40 — should return 1, 5.
	rows2, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE val NOT BETWEEN 20 AND 40 ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var ids2 []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids2 = append(ids2, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids2).To(gomega.Equal([]int64{1, 5}))
}

func TestFDB_SelectWhereLikeUnderscore(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_like_us")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_like_us")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE like_us_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_where_like_us/items WITH TEMPLATE like_us_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_like_us?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, name) VALUES (1, 'cat'), (2, 'car'), (3, 'bat'), (4, 'card')")).Error().NotTo(gomega.HaveOccurred())

	// '_a_' matches exactly 3-char strings with 'a' in middle.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE name LIKE '_a_' ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 2, 3})) // cat, car, bat — not card (4 chars)
}

func TestFDB_BeginCommit(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_begin_commit")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_begin_commit")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE bc_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_begin_commit/items WITH TEMPLATE bc_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_begin_commit?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert in a transaction and commit — row must be visible after.
	tx, err := db.BeginTx(ctx, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = tx.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 100)")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(tx.Commit()).To(gomega.Succeed())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.val)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{1, 100}}))
}

func TestFDB_BeginRollback(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_begin_rollback")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_begin_rollback")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE br_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_begin_rollback/items WITH TEMPLATE br_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_begin_rollback?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert in a transaction then rollback — row must NOT be visible after.
	tx, err := db.BeginTx(ctx, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = tx.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 100)")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(tx.Rollback()).To(gomega.Succeed())

	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeFalse()) // no rows
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
}

func TestFDB_TxMultiStatement(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_tx_multi")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_tx_multi")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE txm_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_tx_multi/items WITH TEMPLATE txm_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_tx_multi?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Multiple inserts + update in one transaction, all committed atomically.
	tx, err := db.BeginTx(ctx, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = tx.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10)")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = tx.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (2, 20)")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = tx.ExecContext(ctx, "UPDATE Item SET val = 99 WHERE item_id = 1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(tx.Commit()).To(gomega.Succeed())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.val)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{1, 99}, {2, 20}}))
}
