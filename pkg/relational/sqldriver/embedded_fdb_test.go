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
	// ORDER BY on a column not in the SELECT list is now supported.
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

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 30), (2, 10), (3, 20)")).Error().NotTo(gomega.HaveOccurred())

	// ORDER BY val (not in SELECT list) — ids should come back sorted by val.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2, 3, 1}))
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

func TestFDB_SelectWhereNullNotIn(t *testing.T) {
	// NULL NOT IN (...) must be UNKNOWN (filtered out), not true.
	// Previously a bug returned true for rows with NULL column values.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_null_not_in")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_null_not_in")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE null_nin_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_null_not_in/items WITH TEMPLATE null_nin_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_null_not_in?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// item 1: val=10 (in list), item 2: val=NULL, item 3: val=30 (not in list)
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id) VALUES (2)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	// NOT IN (10): should return only item 3 (val=30). Item 2 has NULL val — must be filtered out.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE val NOT IN (10) ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{3}))
}

func TestFDB_SelectWhereConstantLeftSide(t *testing.T) {
	// Verify that constant <op> column comparisons work (e.g. 10 = item_id).
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_const_lhs")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_const_lhs")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE const_lhs_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_const_lhs/items WITH TEMPLATE const_lhs_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_const_lhs?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10), (2, 20), (3, 30)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE 20 = val ORDER BY item_id ASC")
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

func TestFDB_SelectColumnAlias(t *testing.T) {
	// SELECT col AS alias — result column name should use the alias.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_col_alias")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_col_alias")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE alias_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_col_alias/items WITH TEMPLATE alias_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_col_alias?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 42)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id AS id, val AS amount FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	// Verify column names use aliases.
	cols, colsErr := rows.Columns()
	g.Expect(colsErr).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{"id", "amount"}))

	var id, amount int64
	g.Expect(rows.Next()).To(gomega.BeTrue())
	g.Expect(rows.Scan(&id, &amount)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(amount).To(gomega.Equal(int64(42)))
}

func TestFDB_SelectOrderByNonProjectedColumn(t *testing.T) {
	// ORDER BY on a column not in the SELECT list should work.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby_nonproj")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby_nonproj")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_nonproj_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_orderby_nonproj/items WITH TEMPLATE ob_nonproj_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_orderby_nonproj?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val, name) VALUES (1, 30, 'c'), (2, 10, 'a'), (3, 20, 'b')")).Error().NotTo(gomega.HaveOccurred())

	// SELECT only name, ORDER BY val (not projected) — should return names sorted by val.
	rows, err := db.QueryContext(ctx, "SELECT name FROM Item ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"a", "b", "c"}))
}

func TestFDB_SQLCommitRollback(t *testing.T) {
	// Verifies that COMMIT/ROLLBACK can be sent as SQL text statements,
	// matching the behavior of tools/ORMs that manage transactions via raw SQL.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sql_txn")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_sql_txn")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sql_txn_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sql_txn/items WITH TEMPLATE sql_txn_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_sql_txn?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Use a single connection to control transaction boundaries via raw SQL.
	conn, err := db.Conn(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer conn.Close()

	// Insert + COMMIT → row visible.
	g.Expect(conn.ExecContext(ctx, "START TRANSACTION")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(conn.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(conn.ExecContext(ctx, "COMMIT")).Error().NotTo(gomega.HaveOccurred())

	rows, err := conn.QueryContext(ctx, "SELECT item_id FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	rows.Close()
	g.Expect(ids).To(gomega.Equal([]int64{1}))

	// Insert + ROLLBACK → row NOT visible.
	g.Expect(conn.ExecContext(ctx, "START TRANSACTION")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(conn.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (2, 20)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(conn.ExecContext(ctx, "ROLLBACK")).Error().NotTo(gomega.HaveOccurred())

	rows2, err := conn.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	ids = nil
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	rows2.Close()
	g.Expect(ids).To(gomega.Equal([]int64{1})) // only item 1 survived
}

func TestFDB_InsertWithoutColumnList(t *testing.T) {
	// INSERT INTO t VALUES (...) without explicit column list uses field
	// declaration order from the schema template.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ins_nocollist")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_ins_nocollist")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nocollist_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_ins_nocollist/items WITH TEMPLATE nocollist_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ins_nocollist?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert without column list: values in (item_id, val) order.
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item VALUES (1, 42)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT item_id, val FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var id, val int64
	g.Expect(rows.Scan(&id, &val)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(val).To(gomega.Equal(int64(42)))
}

func TestFDB_UpdateSetArithmetic(t *testing.T) {
	// UPDATE SET col = col + N — arithmetic with a column reference.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_upd_arith")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_upd_arith")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE upd_arith_tmpl "+
			"CREATE TABLE Counter (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_upd_arith/counters WITH TEMPLATE upd_arith_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_upd_arith?cluster_file=%s&schema=counters", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Counter (id, val) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())

	// Increment val by 5.
	g.Expect(db.ExecContext(ctx, "UPDATE Counter SET val = val + 5 WHERE id = 1")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT id, val FROM Counter WHERE id = 1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var id, val int64
	g.Expect(rows.Scan(&id, &val)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(val).To(gomega.Equal(int64(15)))
}

func TestFDB_GroupByCount(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_grpby")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_grpby")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE grpby_tmpl "+
			"CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_grpby/sales WITH TEMPLATE grpby_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_grpby?cluster_file=%s&schema=sales", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert: 2 east, 3 west.
	for _, row := range []struct {
		id     int
		region string
		amount int
	}{
		{1, "east", 100},
		{2, "east", 200},
		{3, "west", 50},
		{4, "west", 75},
		{5, "west", 25},
	} {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Sale (id, region, amount) VALUES (%d, '%s', %d)", row.id, row.region, row.amount))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	rows, err := db.QueryContext(ctx, "SELECT region, COUNT(*) FROM Sale GROUP BY region")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var region string
		var cnt int64
		g.Expect(rows.Scan(&region, &cnt)).To(gomega.Succeed())
		counts[region] = cnt
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(counts["east"]).To(gomega.Equal(int64(2)))
	g.Expect(counts["west"]).To(gomega.Equal(int64(3)))
}

func TestFDB_GroupByHaving(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_having")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_having")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE having_tmpl "+
			"CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_having/sales WITH TEMPLATE having_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_having?cluster_file=%s&schema=sales", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for _, row := range []struct {
		id     int
		region string
		amount int
	}{
		{1, "east", 100},
		{2, "east", 200},
		{3, "west", 50},
	} {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Sale (id, region, amount) VALUES (%d, '%s', %d)", row.id, row.region, row.amount))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// Only groups with COUNT(*) > 1 — should return only "east" (2 rows).
	rows, err := db.QueryContext(ctx, "SELECT region, COUNT(*) FROM Sale GROUP BY region HAVING COUNT(*) > 1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var regions []string
	for rows.Next() {
		var region string
		var cnt int64
		g.Expect(rows.Scan(&region, &cnt)).To(gomega.Succeed())
		regions = append(regions, region)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(regions).To(gomega.ConsistOf("east"))
}

func TestFDB_GroupByOrderBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_grpord")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_grpord")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE grpord_tmpl "+
			"CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_grpord/sales WITH TEMPLATE grpord_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_grpord?cluster_file=%s&schema=sales", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// 3 north, 2 south, 1 east — verify ORDER BY COUNT(*) DESC gives north, south, east.
	for _, row := range []struct {
		id     int
		region string
	}{
		{1, "north"},
		{2, "north"},
		{3, "north"},
		{4, "south"},
		{5, "south"},
		{6, "east"},
	} {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Sale (id, region) VALUES (%d, '%s')", row.id, row.region))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	rows, err := db.QueryContext(ctx,
		"SELECT region, COUNT(*) FROM Sale GROUP BY region ORDER BY COUNT(*) DESC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type regionCount struct {
		region string
		count  int64
	}
	var results []regionCount
	for rows.Next() {
		var rc regionCount
		g.Expect(rows.Scan(&rc.region, &rc.count)).To(gomega.Succeed())
		results = append(results, rc)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(results).To(gomega.HaveLen(3))
	g.Expect(results[0]).To(gomega.Equal(regionCount{"north", 3}))
	g.Expect(results[1]).To(gomega.Equal(regionCount{"south", 2}))
	g.Expect(results[2]).To(gomega.Equal(regionCount{"east", 1}))
}

func TestFDB_AggregateWithoutGroupBy(t *testing.T) {
	// SELECT COUNT(*), SUM(amount) FROM t without GROUP BY — single result row.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_aggno")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_aggno")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE aggno_tmpl "+
			"CREATE TABLE Item (id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_aggno/items WITH TEMPLATE aggno_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_aggno?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for i, amt := range []int{10, 20, 30} {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Item (id, amount) VALUES (%d, %d)", i+1, amt))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	rows, err := db.QueryContext(ctx, "SELECT COUNT(*), SUM(amount) FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var cnt int64
	var sum float64
	g.Expect(rows.Scan(&cnt, &sum)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(3)))
	g.Expect(sum).To(gomega.Equal(float64(60)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_SelectScalarExpression(t *testing.T) {
	// SELECT id, amount * 2 AS doubled FROM t — arithmetic in SELECT list.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_scalar_sel")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_scalar_sel")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE scalar_sel_tmpl "+
			"CREATE TABLE Item (id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_scalar_sel/items WITH TEMPLATE scalar_sel_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_scalar_sel?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (id, amount) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT id, amount * 2 AS doubled FROM Item WHERE id = 1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var id int64
	var doubled float64
	g.Expect(rows.Scan(&id, &doubled)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(doubled).To(gomega.Equal(float64(20)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_SelectCoalesce(t *testing.T) {
	// COALESCE(nullable_col, 0) returns the non-NULL value or default.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_coalesce")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_coalesce")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE coalesce_tmpl "+
			"CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_coalesce/items WITH TEMPLATE coalesce_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_coalesce?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert one row with val=10 and one with no val (NULL).
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (id, val) VALUES (1, 10)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (id) VALUES (2)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT id, COALESCE(val, 0) AS effective_val FROM Item ORDER BY id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var id, v int64
	g.Expect(rows.Scan(&id, &v)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(v).To(gomega.Equal(int64(10)))

	g.Expect(rows.Next()).To(gomega.BeTrue())
	g.Expect(rows.Scan(&id, &v)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(2)))
	g.Expect(v).To(gomega.Equal(int64(0)))

	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_LimitOffset(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_limit_offset")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_limit_offset")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE loff_tmpl CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_limit_offset/items WITH TEMPLATE loff_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_limit_offset?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for i := int64(1); i <= 5; i++ {
		_, err = db.ExecContext(ctx, `INSERT INTO Item (id, val) VALUES (?, ?)`, i, i*10)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// LIMIT 2 OFFSET 1 → rows 2, 3 (sorted by id)
	rows, err := db.QueryContext(ctx, `SELECT id FROM Item ORDER BY id ASC LIMIT 2 OFFSET 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(ids).To(gomega.Equal([]int64{2, 3}))
}

func TestFDB_CaseWhen(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_case_when")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_case_when")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE cw_tmpl CREATE TABLE Sale (id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_case_when/sales WITH TEMPLATE cw_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_case_when?cluster_file=%s&schema=sales", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, amount) VALUES (1, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, amount) VALUES (2, 150)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, amount) VALUES (3, 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id, CASE WHEN amount < 100 THEN 'small' WHEN amount < 200 THEN 'medium' ELSE 'large' END AS size FROM Sale ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type saleRow struct {
		id   int64
		size string
	}
	var got []saleRow
	for rows.Next() {
		var r saleRow
		g.Expect(rows.Scan(&r.id, &r.size)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(got).To(gomega.Equal([]saleRow{{1, "small"}, {2, "medium"}, {3, "large"}}))
}

func TestFDB_StringFunctions(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_strfuncs")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_strfuncs")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE sf_tmpl CREATE TABLE Word (id BIGINT NOT NULL, label STRING NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_strfuncs/words WITH TEMPLATE sf_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_strfuncs?cluster_file=%s&schema=words", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Word (id, label) VALUES (1, '  Hello  ')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT UPPER(TRIM(label)), LOWER(TRIM(label)), LENGTH(TRIM(label)) FROM Word WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var upper, lower string
	var length int64
	g.Expect(rows.Scan(&upper, &lower, &length)).To(gomega.Succeed())
	g.Expect(upper).To(gomega.Equal("HELLO"))
	g.Expect(lower).To(gomega.Equal("hello"))
	g.Expect(length).To(gomega.Equal(int64(5)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_ConcatNullIf(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_concat")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_concat")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE cn_tmpl CREATE TABLE Person (id BIGINT NOT NULL, first STRING NOT NULL, last STRING NOT NULL, score BIGINT, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_concat/people WITH TEMPLATE cn_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_concat?cluster_file=%s&schema=people", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Person (id, first, last, score) VALUES (1, 'Alice', 'Smith', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Person (id, first, last, score) VALUES (2, 'Bob', 'Jones', 0)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT CONCAT(first, ' ', last), NULLIF(score, 0) FROM Person ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	// Row 1: CONCAT = "Alice Smith", NULLIF(100, 0) = 100
	g.Expect(rows.Next()).To(gomega.BeTrue())
	var fullName string
	var score any
	g.Expect(rows.Scan(&fullName, &score)).To(gomega.Succeed())
	g.Expect(fullName).To(gomega.Equal("Alice Smith"))
	g.Expect(score).To(gomega.Equal(int64(100)))

	// Row 2: CONCAT = "Bob Jones", NULLIF(0, 0) = NULL
	g.Expect(rows.Next()).To(gomega.BeTrue())
	var fullName2 string
	var score2 any
	g.Expect(rows.Scan(&fullName2, &score2)).To(gomega.Succeed())
	g.Expect(fullName2).To(gomega.Equal("Bob Jones"))
	g.Expect(score2).To(gomega.BeNil())

	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_UnionAll(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_all")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_union_all")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ua_tmpl CREATE TABLE Item (id BIGINT NOT NULL, label STRING NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_union_all/store WITH TEMPLATE ua_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_union_all?cluster_file=%s&schema=store", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Item (id, label) VALUES (1, 'alpha'), (2, 'beta')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// UNION ALL: duplicates preserved.
	rows, err := db.QueryContext(ctx, `SELECT label FROM Item WHERE id = 1 UNION ALL SELECT label FROM Item WHERE id = 2`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var lbl string
		g.Expect(rows.Scan(&lbl)).To(gomega.Succeed())
		labels = append(labels, lbl)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(labels).To(gomega.ConsistOf("alpha", "beta"))
}

func TestFDB_UnionDistinct(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_distinct")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_union_distinct")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ud_tmpl CREATE TABLE Tag (id BIGINT NOT NULL, tag STRING NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_union_distinct/tags WITH TEMPLATE ud_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_union_distinct?cluster_file=%s&schema=tags", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Tag (id, tag) VALUES (1, 'go'), (2, 'go'), (3, 'fdb')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// UNION (implicit DISTINCT): duplicates removed.
	rows, err := db.QueryContext(ctx, `SELECT tag FROM Tag WHERE id = 1 UNION SELECT tag FROM Tag WHERE id = 2`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		g.Expect(rows.Scan(&tag)).To(gomega.Succeed())
		tags = append(tags, tag)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Two rows with tag='go' UNION'd → one row.
	g.Expect(tags).To(gomega.ConsistOf("go"))
}

func TestFDB_InfoSchema_SchemataWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_is_schemata_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_schemata_where")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE iswt_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_is_schemata_where/alpha WITH TEMPLATE iswt_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_is_schemata_where/beta WITH TEMPLATE iswt_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_schemata_where?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// WHERE filter: only 'alpha' schema.
	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE SCHEMA_NAME = 'alpha'`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var schemas []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		// SCHEMA_NAME is at index 1.
		if s, ok := vals[1].(string); ok {
			schemas = append(schemas, s)
		}
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(schemas).To(gomega.ConsistOf("alpha"))
}

func TestFDB_InsertSelect(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_insert_select")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_insert_select")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE is_tmpl CREATE TABLE Src (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id)) CREATE TABLE Dst (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_insert_select/data WITH TEMPLATE is_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_insert_select?cluster_file=%s&schema=data", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Src (id, val) VALUES (1, 10), (2, 20), (3, 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// INSERT INTO Dst SELECT * FROM Src WHERE val > 10
	_, err = db.ExecContext(ctx, `INSERT INTO Dst SELECT * FROM Src WHERE val > 10`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id, val FROM Dst ORDER BY id`)
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
	g.Expect(got).To(gomega.Equal([]row{{2, 20}, {3, 30}}))
}

func TestFDB_CastAndSubstring(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cast_substr")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cast_substr")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE cast_substr_tmpl CREATE TABLE Item (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cast_substr/shop WITH TEMPLATE cast_substr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cast_substr?cluster_file=%s&schema=shop", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Item (id, name, price) VALUES (1, 'Widget', 42), (2, 'Gadget', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CAST price to STRING
	rows, err := db.QueryContext(ctx, `SELECT CAST(price AS STRING) FROM Item WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	rows.Next()
	var priceStr string
	g.Expect(rows.Scan(&priceStr)).To(gomega.Succeed())
	g.Expect(priceStr).To(gomega.Equal("42"))

	// SUBSTRING
	rows2, err := db.QueryContext(ctx, `SELECT SUBSTRING(name, 1, 3) FROM Item WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	rows2.Next()
	var sub string
	g.Expect(rows2.Scan(&sub)).To(gomega.Succeed())
	g.Expect(sub).To(gomega.Equal("Wid"))

	// REPLACE
	rows3, err := db.QueryContext(ctx, `SELECT REPLACE(name, 'Widget', 'Thing') FROM Item WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows3.Close()
	rows3.Next()
	var replaced string
	g.Expect(rows3.Scan(&replaced)).To(gomega.Succeed())
	g.Expect(replaced).To(gomega.Equal("Thing"))

	// IF function
	rows4, err := db.QueryContext(ctx, `SELECT IF(price > 50, 'expensive', 'cheap') FROM Item ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows4.Close()
	var cats []string
	for rows4.Next() {
		var c string
		g.Expect(rows4.Scan(&c)).To(gomega.Succeed())
		cats = append(cats, c)
	}
	g.Expect(cats).To(gomega.Equal([]string{"cheap", "expensive"}))
}

func TestFDB_MathFunctions(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_math_funcs")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_math_funcs")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE mf_tmpl CREATE TABLE Num (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_math_funcs/data WITH TEMPLATE mf_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_math_funcs?cluster_file=%s&schema=data", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Num (id, val) VALUES (1, 7), (2, 3)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// MOD(7, 3) = 1
	rows, err := db.QueryContext(ctx, `SELECT MOD(val, 3) FROM Num WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	rows.Next()
	var mod int64
	g.Expect(rows.Scan(&mod)).To(gomega.Succeed())
	g.Expect(mod).To(gomega.Equal(int64(1)))

	// POWER(2, 3) = 8
	rows2, err := db.QueryContext(ctx, `SELECT POWER(2, 3) FROM Num WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	rows2.Next()
	var pow int64
	g.Expect(rows2.Scan(&pow)).To(gomega.Succeed())
	g.Expect(pow).To(gomega.Equal(int64(8)))
}

func TestFDB_HavingCompound(t *testing.T) {
	// HAVING with AND/OR compound conditions.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_having_compound")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_having_compound")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE hc_tmpl CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_having_compound/sales WITH TEMPLATE hc_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_having_compound?cluster_file=%s&schema=sales", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (1, 'north', 100), (2, 'south', 50), (3, 'north', 200), (4, 'south', 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// GROUP BY region HAVING SUM > 100 AND COUNT > 1
	rows, err := db.QueryContext(ctx, `SELECT region, SUM(amount), COUNT(*) FROM Sale GROUP BY region HAVING SUM(amount) > 100 AND COUNT(*) > 1 ORDER BY region`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var regions []string
	for rows.Next() {
		var region string
		var sum, cnt int64
		g.Expect(rows.Scan(&region, &sum, &cnt)).To(gomega.Succeed())
		regions = append(regions, region)
		g.Expect(sum).To(gomega.BeNumerically(">", int64(100)))
		g.Expect(cnt).To(gomega.BeNumerically(">", int64(1)))
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// north: SUM=300, COUNT=2 → passes; south: SUM=80, COUNT=2 → fails SUM>100
	g.Expect(regions).To(gomega.ConsistOf("north"))
}

func TestFDB_WhereExprComparison(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_where_expr")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_where_expr")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE we_tmpl CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_where_expr/products WITH TEMPLATE we_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_where_expr?cluster_file=%s&schema=products", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Widget', 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Gadget', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (3, 'Gizmo', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// WHERE with expression on left side: price * 2 > 50
	rows, err := db.QueryContext(ctx, `SELECT id FROM Product WHERE price * 2 > 50 ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	// price * 2: Widget=20, Gadget=100, Gizmo=200; > 50 → Gadget (2), Gizmo (3)
	g.Expect(ids).To(gomega.Equal([]int64{2, 3}))
}

func TestFDB_InnerJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_inner_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_inner_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ij_tmpl
		CREATE TABLE Order (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_inner_join/main WITH TEMPLATE ij_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_inner_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (10, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (11, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (12, 2, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// INNER JOIN: Customer JOIN Order ON Customer.id = Order.customer_id
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Order.amount
		FROM Customer
		INNER JOIN Order ON Customer.id = Order.customer_id
		ORDER BY Order.amount ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		name   string
		amount int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.amount)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{"Bob", 50},
		{"Alice", 100},
		{"Alice", 200},
	}))
}

func TestFDB_LeftJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_left_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_left_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE lj_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Order (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_left_join/main WITH TEMPLATE lj_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_left_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// Only Alice has orders.
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (10, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// LEFT JOIN: all customers, NULL amount for Bob.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Order.amount
		FROM Customer
		LEFT JOIN Order ON Customer.id = Order.customer_id
		ORDER BY Customer.id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	var amounts []any
	for rows.Next() {
		var name string
		var amount any
		g.Expect(rows.Scan(&name, &amount)).To(gomega.Succeed())
		names = append(names, name)
		amounts = append(amounts, amount)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Alice", "Bob"}))
	g.Expect(amounts[0]).To(gomega.Equal(int64(100)))
	g.Expect(amounts[1]).To(gomega.BeNil()) // Bob has no orders → NULL
}

func TestFDB_JoinWhere(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_join_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_join_where")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE jw_tmpl
		CREATE TABLE Category (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Product (id BIGINT NOT NULL, cat_id BIGINT NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_join_where/main WITH TEMPLATE jw_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_join_where?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, name) VALUES (1, 'Electronics')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, name) VALUES (2, 'Books')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, cat_id, price) VALUES (1, 1, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, cat_id, price) VALUES (2, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, cat_id, price) VALUES (3, 2, 15)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// JOIN + WHERE: electronics products with price > 300
	rows, err := db.QueryContext(ctx, `
		SELECT Product.id, Product.price
		FROM Product
		INNER JOIN Category ON Product.cat_id = Category.id
		WHERE Category.name = 'Electronics' AND Product.price > 300
		ORDER BY Product.price DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	var prices []int64
	for rows.Next() {
		var id, price int64
		g.Expect(rows.Scan(&id, &price)).To(gomega.Succeed())
		ids = append(ids, id)
		prices = append(prices, price)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1}))
	g.Expect(prices).To(gomega.Equal([]int64{500}))
}

func TestFDB_RightJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_right_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_right_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE rj_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Order (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_right_join/main WITH TEMPLATE rj_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_right_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// One order for Alice; one orphan order with no matching customer.
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (10, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (11, 99, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// RIGHT JOIN: all orders appear; orphan order has NULL customer name.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Order.amount
		FROM Customer
		RIGHT JOIN Order ON Customer.id = Order.customer_id
		ORDER BY Order.amount ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []any
	var amounts []int64
	for rows.Next() {
		var name any
		var amount int64
		g.Expect(rows.Scan(&name, &amount)).To(gomega.Succeed())
		names = append(names, name)
		amounts = append(amounts, amount)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(amounts).To(gomega.Equal([]int64{100, 200}))
	g.Expect(names[0]).To(gomega.Equal("Alice"))
	g.Expect(names[1]).To(gomega.BeNil()) // orphan order → NULL customer name
}

func TestFDB_CountDistinct(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_count_distinct")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_count_distinct")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cd_tmpl
		CREATE TABLE Sale (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, region STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_count_distinct/main WITH TEMPLATE cd_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_count_distinct?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, customer_id, region) VALUES (1, 1, 'US')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, customer_id, region) VALUES (2, 2, 'EU')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, customer_id, region) VALUES (3, 1, 'US')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, customer_id, region) VALUES (4, 3, 'US')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// COUNT(DISTINCT customer_id): 3 distinct customers (1, 2, 3).
	rows, err := db.QueryContext(ctx, `SELECT COUNT(DISTINCT customer_id) FROM Sale`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var n int64
	g.Expect(rows.Scan(&n)).To(gomega.Succeed())
	g.Expect(n).To(gomega.Equal(int64(3)))

	// COUNT(DISTINCT region) GROUP BY: grouped by region, count distinct customers per region.
	rows2, err := db.QueryContext(ctx, `SELECT region, COUNT(DISTINCT customer_id) FROM Sale GROUP BY region ORDER BY region ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	type regionCount struct {
		region string
		count  int64
	}
	var rc []regionCount
	for rows2.Next() {
		var r regionCount
		g.Expect(rows2.Scan(&r.region, &r.count)).To(gomega.Succeed())
		rc = append(rc, r)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(rc).To(gomega.Equal([]regionCount{
		{"EU", 1},
		{"US", 2}, // customers 1 and 3 in US
	}))
}

func TestFDB_GreatestLeast(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_greatest_least")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_greatest_least")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE gl_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, a BIGINT NOT NULL, b BIGINT NOT NULL, c BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_greatest_least/main WITH TEMPLATE gl_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_greatest_least?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, a, b, c) VALUES (1, 3, 1, 2)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, a, b, c) VALUES (2, 7, 9, 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT GREATEST(a, b, c), LEAST(a, b, c) FROM Product ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ greatest, least int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.greatest, &r.least)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{3, 1},
		{9, 5},
	}))
}

func TestFDB_SubqueryIN(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_subquery_in")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_subquery_in")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE subq_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE RestaurantOrder (order_id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (order_id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_subquery_in/main WITH TEMPLATE subq_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_subquery_in?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (3, 'Charlie')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO RestaurantOrder (order_id, customer_id, amount) VALUES (1, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO RestaurantOrder (order_id, customer_id, amount) VALUES (2, 2, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO RestaurantOrder (order_id, customer_id, amount) VALUES (3, 1, 150)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Alice has orders > 100, Bob does not, Charlie has no orders.
	rows, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE id IN (SELECT customer_id FROM RestaurantOrder WHERE amount > 100) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Alice"}))

	rows2, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE id NOT IN (SELECT customer_id FROM RestaurantOrder WHERE amount > 100) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var names2 []string
	for rows2.Next() {
		var name string
		g.Expect(rows2.Scan(&name)).To(gomega.Succeed())
		names2 = append(names2, name)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names2).To(gomega.ConsistOf("Bob", "Charlie"))
}

func TestFDB_JoinGroupBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_join_groupby")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_join_groupby")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE jgb_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Order (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_join_groupby/main WITH TEMPLATE jgb_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_join_groupby?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (10, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (11, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Order (id, customer_id, amount) VALUES (12, 2, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// JOIN + GROUP BY: count orders per customer.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, COUNT(*), SUM(Order.amount)
		FROM Customer
		INNER JOIN Order ON Customer.id = Order.customer_id
		GROUP BY Customer.name
		ORDER BY Customer.name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		name  string
		count int64
		total float64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.count, &r.total)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{"Alice", 2, 300},
		{"Bob", 1, 50},
	}))
}

func TestFDB_ExistsSubquery(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_exists_subquery")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_exists_subquery")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE exists_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Flag (id BIGINT NOT NULL, active BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_exists_subquery/main WITH TEMPLATE exists_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_exists_subquery?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Flag (id, active) VALUES (1, 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// EXISTS: Flag table has active=1 rows → all customers returned.
	rows, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE EXISTS (SELECT id FROM Flag WHERE active = 1) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Alice", "Bob"}))

	// NOT EXISTS with empty subquery → all customers returned.
	rows2, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE NOT EXISTS (SELECT id FROM Flag WHERE active = 99) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var names2 []string
	for rows2.Next() {
		var name string
		g.Expect(rows2.Scan(&name)).To(gomega.Succeed())
		names2 = append(names2, name)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names2).To(gomega.Equal([]string{"Alice", "Bob"}))

	// EXISTS with empty subquery → no customers returned.
	rows3, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE EXISTS (SELECT id FROM Flag WHERE active = 99) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows3.Close()

	var names3 []string
	for rows3.Next() {
		var name string
		g.Expect(rows3.Scan(&name)).To(gomega.Succeed())
		names3 = append(names3, name)
	}
	g.Expect(rows3.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names3).To(gomega.BeEmpty())
}

// TestFDB_CTE verifies WITH (CTE) support: materialization, WHERE filter,
// projection, and ORDER BY on the CTE result.
func TestFDB_CTE(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_cte")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE cte_tmpl "+
			"CREATE TABLE Product (id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte/store WITH TEMPLATE cte_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte?cluster_file=%s&schema=store", clusterFilePath)
	db, openErr := sql.Open("fdbsql", dsn)
	g.Expect(openErr).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Cheap', 50), (2, 'Pricey', 200), (3, 'Expensive', 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CTE with WHERE + projection + ORDER BY.
	rows, err := db.QueryContext(ctx,
		`WITH expensive AS (SELECT id, name FROM Product WHERE price > 100)
		 SELECT name FROM expensive ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Expensive", "Pricey"}))

	// CTE SELECT * (all columns).
	rows2, err := db.QueryContext(ctx,
		`WITH cheap AS (SELECT * FROM Product WHERE price < 100)
		 SELECT name FROM cheap ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()

	var names2 []string
	for rows2.Next() {
		var name string
		g.Expect(rows2.Scan(&name)).To(gomega.Succeed())
		names2 = append(names2, name)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names2).To(gomega.Equal([]string{"Cheap"}))
}

func TestFDB_SelectWithoutFrom(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	// SELECT without FROM doesn't need a real schema — just a valid DSN with a path.
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///select_no_from?cluster_file=%s", clusterFilePath))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT 1 + 2, 'hello', 42`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var a, c int64
	var b string
	g.Expect(rows.Scan(&a, &b, &c)).To(gomega.Succeed())
	g.Expect(a).To(gomega.Equal(int64(3)))
	g.Expect(b).To(gomega.Equal("hello"))
	g.Expect(c).To(gomega.Equal(int64(42)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
}

func TestFDB_DerivedTable(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_derived_table")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_derived_table")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE dt_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_derived_table/main WITH TEMPLATE dt_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_derived_table?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Cheap', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Expensive', 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (3, 'Pricey', 150)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `
		SELECT name FROM (SELECT id, name FROM Product WHERE price > 100) AS expensive ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Expensive", "Pricey"}))
}

func TestFDB_CTEChaining(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cte_chaining")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte_chaining")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE chain_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte_chaining/main WITH TEMPLATE chain_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte_chaining?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Cheap', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Mid', 150)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (3, 'Pricey', 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CTE b references CTE a.
	rows, err := db.QueryContext(ctx, `
		WITH over50 AS (SELECT id, name, price FROM Product WHERE price > 50),
		     over100 AS (SELECT id, name FROM over50 WHERE price > 100)
		SELECT name FROM over100 ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Mid", "Pricey"}))
}

func TestFDB_UpdateDeleteWithSubquery(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_upd_del_subq")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_upd_del_subq")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE uds_tmpl
		CREATE TABLE Category (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Product (id BIGINT NOT NULL, category_id BIGINT NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_upd_del_subq/main WITH TEMPLATE uds_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_upd_del_subq?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, name) VALUES (1, 'electronics')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, name) VALUES (2, 'books')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, category_id, price) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, category_id, price) VALUES (2, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, category_id, price) VALUES (3, 2, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// UPDATE products in electronics category (id IN (SELECT id FROM Category WHERE name = 'electronics')).
	_, err = db.ExecContext(ctx, `UPDATE Product SET price = 999 WHERE category_id IN (SELECT id FROM Category WHERE name = 'electronics')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT price FROM Product WHERE category_id = 1 ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var prices []int64
	for rows.Next() {
		var p int64
		g.Expect(rows.Scan(&p)).To(gomega.Succeed())
		prices = append(prices, p)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(prices).To(gomega.Equal([]int64{999, 999}))

	// DELETE products in books category.
	_, err = db.ExecContext(ctx, `DELETE FROM Product WHERE category_id IN (SELECT id FROM Category WHERE name = 'books')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows2, err := db.QueryContext(ctx, `SELECT id FROM Product ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var ids []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 2}))
}

func TestFDB_FunctionsInMapEval(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_fn_map")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_fn_map")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE fn_map_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Category (id BIGINT NOT NULL, label STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_fn_map/main WITH TEMPLATE fn_map_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_fn_map?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Widget', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Gadget', 120)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, label) VALUES (1, 'cheap')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Category (id, label) VALUES (2, 'pricey')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Function in WHERE on a CTE (map path).
	rows, err := db.QueryContext(ctx, `
		WITH products AS (SELECT id, name, price FROM Product)
		SELECT name FROM products WHERE UPPER(name) = 'WIDGET'`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		names = append(names, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Widget"}))

	// COALESCE in SELECT projection on a CTE.
	rows2, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name FROM Product)
		SELECT COALESCE(name, 'unknown') FROM p WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var vals []string
	for rows2.Next() {
		var v string
		g.Expect(rows2.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(vals).To(gomega.Equal([]string{"Widget"}))
}

func TestFDB_CaseInMapEval(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_case_map")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_case_map")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE case_map_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_case_map/main WITH TEMPLATE case_map_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_case_map?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Widget', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Gadget', 150)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (3, 'Gizmo', 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CASE WHEN in CTE SELECT projection.
	rows, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name, price FROM Product)
		SELECT name, CASE WHEN price < 100 THEN 'cheap' WHEN price < 200 THEN 'mid' ELSE 'pricey' END AS tier
		FROM p ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ name, tier string }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.tier)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{"Widget", "cheap"},
		{"Gadget", "mid"},
		{"Gizmo", "pricey"},
	}))
}

func TestFDB_SubqueryInCase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_subq_case")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_subq_case")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE sqc_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Discount (product_id BIGINT NOT NULL, PRIMARY KEY (product_id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_subq_case/main WITH TEMPLATE sqc_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_subq_case?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (1, 'Widget', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price) VALUES (2, 'Gadget', 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Discount (product_id) VALUES (1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CASE WHEN with subquery in the condition.
	rows, err := db.QueryContext(ctx, `
		SELECT name, CASE WHEN id IN (SELECT product_id FROM Discount) THEN 'discounted' ELSE 'full price' END
		FROM Product ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ name, status string }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.status)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{"Widget", "discounted"},
		{"Gadget", "full price"},
	}))
}

func TestFDB_AggregateOnCTE(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_agg_cte")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_agg_cte")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE agg_cte_tmpl
		CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_agg_cte/main WITH TEMPLATE agg_cte_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_agg_cte?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (1, 'west', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (2, 'west', 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (3, 'east', 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (4, 'east', 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// GROUP BY + SUM on a CTE, ORDER BY.
	rows, err := db.QueryContext(ctx, `
		WITH big_sales AS (SELECT id, region, amount FROM Sale WHERE amount >= 100)
		SELECT region, SUM(amount) FROM big_sales GROUP BY region ORDER BY region ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		region string
		total  int64
	}
	var got []row
	for rows.Next() {
		var r row
		var t any
		g.Expect(rows.Scan(&r.region, &t)).To(gomega.Succeed())
		switch v := t.(type) {
		case int64:
			r.total = v
		case float64:
			r.total = int64(v)
		}
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{"east", 300},
		{"west", 300},
	}))

	// COUNT(*) on derived table.
	var cnt int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT id FROM Sale WHERE amount > 100) AS big`).Scan(&cnt)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cnt).To(gomega.Equal(int64(2)))
}

func TestFDB_JoinGroupByOrderByLimit(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_join_gb_ol")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_join_gb_ol")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE join_gb_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_join_gb_ol/main WITH TEMPLATE join_gb_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_join_gb_ol?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (3, 'Carol')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 1, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (3, 2, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (4, 3, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (5, 3, 400)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// JOIN + GROUP BY + ORDER BY on aggregate + LIMIT. Previously LIMIT was silently ignored.
	rows, err := db.QueryContext(ctx, `
		SELECT name, SUM(amount) FROM Customer INNER JOIN Sales ON Customer.id = Sales.customer_id
		GROUP BY name ORDER BY name DESC LIMIT 2`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		name  string
		total int64
	}
	var got []row
	for rows.Next() {
		var r row
		var t any
		g.Expect(rows.Scan(&r.name, &t)).To(gomega.Succeed())
		switch v := t.(type) {
		case int64:
			r.total = v
		case float64:
			r.total = int64(v)
		}
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// DESC: Carol, Bob, Alice; LIMIT 2 → Carol, Bob.
	g.Expect(got).To(gomega.Equal([]row{
		{"Carol", 900},
		{"Bob", 50},
	}))
}

func TestFDB_CTEAggregateHaving(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cte_agg_having")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte_agg_having")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cte_agg_h_tmpl
		CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte_agg_having/main WITH TEMPLATE cte_agg_h_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte_agg_having?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for i, row := range []struct {
		region string
		amount int64
	}{
		{"west", 100},
		{"west", 200},
		{"west", 300},
		{"east", 50},
		{"east", 75},
		{"north", 1000},
	} {
		_, err = db.ExecContext(ctx, "INSERT INTO Sale (id, region, amount) VALUES (?, ?, ?)", int64(i+1), row.region, row.amount)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// CTE + GROUP BY + HAVING: only regions with total > 150.
	rows, err := db.QueryContext(ctx, `
		WITH large AS (SELECT id, region, amount FROM Sale WHERE amount > 20)
		SELECT region, SUM(amount) FROM large GROUP BY region HAVING SUM(amount) > 150 ORDER BY region ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type out struct {
		region string
		total  int64
	}
	var got []out
	for rows.Next() {
		var r out
		var t any
		g.Expect(rows.Scan(&r.region, &t)).To(gomega.Succeed())
		switch v := t.(type) {
		case int64:
			r.total = v
		case float64:
			r.total = int64(v)
		}
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// east: 50+75=125 (excluded), north: 1000 (included), west: 100+200+300=600 (included).
	g.Expect(got).To(gomega.Equal([]out{
		{"north", 1000},
		{"west", 600},
	}))
}

func TestFDB_JoinOnCTE(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_join_cte")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_join_cte")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE join_cte_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_join_cte/main WITH TEMPLATE join_cte_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_join_cte?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 1, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (3, 2, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CTE filters to big sales, then JOIN with Customer.
	rows, err := db.QueryContext(ctx, `
		WITH big_sales AS (SELECT id, customer_id, amount FROM Sales WHERE amount > 100)
		SELECT Customer.name, big_sales.amount
		FROM Customer INNER JOIN big_sales ON Customer.id = big_sales.customer_id
		ORDER BY big_sales.amount DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type r struct {
		name   string
		amount int64
	}
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.name, &rr.amount)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{{"Alice", 500}}))
}

func TestFDB_MultiTableFrom(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_multi_from")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_multi_from")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE multi_from_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_multi_from/main WITH TEMPLATE multi_from_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_multi_from?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 2, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Old-school implicit join via FROM a, b WHERE a.id = b.customer_id.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Sales.amount
		FROM Customer, Sales
		WHERE Customer.id = Sales.customer_id
		ORDER BY Sales.amount ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type r struct {
		name   string
		amount int64
	}
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.name, &rr.amount)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{
		{"Alice", 100},
		{"Bob", 200},
	}))
}

func TestFDB_ThreeTableFrom(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_three_from")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_three_from")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE three_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Region (id BIGINT NOT NULL, label STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, region_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_three_from/main WITH TEMPLATE three_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_three_from?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Region (id, label) VALUES (10, 'west')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, region_id, amount) VALUES (100, 1, 10, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Region.label, Sales.amount
		FROM Customer, Region, Sales
		WHERE Customer.id = Sales.customer_id AND Region.id = Sales.region_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var name, label string
	var amount int64
	rows.Next()
	g.Expect(rows.Scan(&name, &label, &amount)).To(gomega.Succeed())
	g.Expect(name).To(gomega.Equal("Alice"))
	g.Expect(label).To(gomega.Equal("west"))
	g.Expect(amount).To(gomega.Equal(int64(500)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_UpdateSetWithFunction(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_upd_fn")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_upd_fn")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE upd_fn_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_upd_fn/main WITH TEMPLATE upd_fn_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_upd_fn?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name) VALUES (1, 'widget')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name) VALUES (2, 'gadget')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// UPDATE with UPPER() scalar function.
	_, err = db.ExecContext(ctx, `UPDATE Product SET name = UPPER(name) WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var n string
	g.Expect(db.QueryRowContext(ctx, `SELECT name FROM Product WHERE id = 1`).Scan(&n)).To(gomega.Succeed())
	g.Expect(n).To(gomega.Equal("WIDGET"))

	// Unchanged row.
	g.Expect(db.QueryRowContext(ctx, `SELECT name FROM Product WHERE id = 2`).Scan(&n)).To(gomega.Succeed())
	g.Expect(n).To(gomega.Equal("gadget"))
}

func TestFDB_OrderByExpression(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ob_expr")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ob_expr")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ob_expr_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, name STRING NOT NULL, price BIGINT NOT NULL, qty BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ob_expr/main WITH TEMPLATE ob_expr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ob_expr?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price, qty) VALUES (1, 'a', 10, 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price, qty) VALUES (2, 'b', 7, 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, name, price, qty) VALUES (3, 'c', 100, 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// ORDER BY price * qty via CTE (price*qty: a=50, b=70, c=100 → ASC: a, b, c).
	rows, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name, price, qty FROM Product)
		SELECT name FROM p ORDER BY price * qty ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		names = append(names, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"a", "b", "c"}))

	// ORDER BY UPPER(name) DESC via CTE.
	rows2, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name FROM Product)
		SELECT id FROM p ORDER BY UPPER(name) DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var ids []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{3, 2, 1})) // c, b, a
}

func TestFDB_OrderByExpressionInJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ob_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ob_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ob_join_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ob_join/main WITH TEMPLATE ob_join_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ob_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'zebra')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'apple')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (3, 'middle')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 2, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (3, 3, 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// JOIN + ORDER BY UPPER(name): apple, middle, zebra.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Sales.amount
		FROM Customer INNER JOIN Sales ON Customer.id = Sales.customer_id
		ORDER BY UPPER(Customer.name) ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		var amt int64
		g.Expect(rows.Scan(&n, &amt)).To(gomega.Succeed())
		names = append(names, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"apple", "middle", "zebra"}))
}

func TestFDB_LtrimRtrim(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ltrim")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ltrim")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ltrim_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, s STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ltrim/main WITH TEMPLATE ltrim_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ltrim?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, s) VALUES (1, '  hello  ')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var l, r, both string
	g.Expect(db.QueryRowContext(ctx, `SELECT LTRIM(s), RTRIM(s), TRIM(s) FROM T WHERE id = 1`).
		Scan(&l, &r, &both)).To(gomega.Succeed())
	g.Expect(l).To(gomega.Equal("hello  "))
	g.Expect(r).To(gomega.Equal("  hello"))
	g.Expect(both).To(gomega.Equal("hello"))
}

func TestFDB_CTEWithJoinAndOrderByExpr(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cte_join_ob")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte_join_ob")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cte_join_ob_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte_join_ob/main WITH TEMPLATE cte_join_ob_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte_join_ob?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert Alice (id=1) FIRST but with LOW total, Bob (id=2) LAST but HIGH total.
	// Natural group-iteration order (by insertion / id) gives [Alice, Bob]; ORDER
	// BY SUM DESC must flip that to [Bob, Alice]. This catches a silent ORDER-BY
	// no-op on aggregate queries.
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 50)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 2, 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (3, 2, 1000)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CTE + JOIN + GROUP BY + HAVING + ORDER BY aggregate + LIMIT.
	rows, err := db.QueryContext(ctx, `
		WITH big AS (SELECT id, customer_id, amount FROM Sales WHERE amount >= 50)
		SELECT Customer.name, SUM(big.amount)
		FROM Customer INNER JOIN big ON Customer.id = big.customer_id
		GROUP BY Customer.name
		HAVING SUM(big.amount) > 0
		ORDER BY SUM(big.amount) DESC
		LIMIT 2`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type r struct {
		name  string
		total int64
	}
	var got []r
	for rows.Next() {
		var rr r
		var t any
		g.Expect(rows.Scan(&rr.name, &t)).To(gomega.Succeed())
		switch v := t.(type) {
		case int64:
			rr.total = v
		case float64:
			rr.total = int64(v)
		}
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{
		{"Bob", 1500},
		{"Alice", 50},
	}))
}

func TestFDB_UpdateDeleteWithExists(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ud_exists")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ud_exists")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ud_exists_tmpl
		CREATE TABLE Flag (name STRING NOT NULL, PRIMARY KEY (name))
		CREATE TABLE Product (id BIGINT NOT NULL, price BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ud_exists/main WITH TEMPLATE ud_exists_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ud_exists?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Flag (name) VALUES ('apply_discount')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, price) VALUES (1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Product (id, price) VALUES (2, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Non-correlated EXISTS: if any Flag row named apply_discount exists, halve prices.
	_, err = db.ExecContext(ctx, `UPDATE Product SET price = price / 2 WHERE EXISTS (SELECT name FROM Flag WHERE name = 'apply_discount')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id, price FROM Product ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type r struct {
		id    int64
		price int64
	}
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.id, &rr.price)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{{1, 50}, {2, 100}}))

	// NOT EXISTS with no matching rows → DELETE takes effect.
	_, err = db.ExecContext(ctx, `DELETE FROM Product WHERE NOT EXISTS (SELECT name FROM Flag WHERE name = 'disable_delete')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var cnt int64
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM Product`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(0)))
}

func TestFDB_NestedFunctionsAndCase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nested_fn_case")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_nested_fn_case")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE nfc_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, name STRING NOT NULL, qty BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_nested_fn_case/main WITH TEMPLATE nfc_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_nested_fn_case?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, qty) VALUES (1, ' alpha ', 3)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, qty) VALUES (2, 'BETA', 0)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, qty) VALUES (3, 'gamma', 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Proto path: CASE WHEN + nested scalar functions in projection + WHERE.
	rows, err := db.QueryContext(ctx, `
		SELECT LOWER(TRIM(name)) AS clean,
		       CASE WHEN qty = 0 THEN 'empty' WHEN qty < 5 THEN 'low' ELSE 'high' END AS tier
		FROM T WHERE LENGTH(TRIM(name)) > 3 ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type r struct{ clean, tier string }
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.clean, &rr.tier)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{
		{"alpha", "low"},
		{"beta", "empty"},
		{"gamma", "high"},
	}))

	// Map path via CTE: same expressions work via the unified evaluator core.
	rows2, err := db.QueryContext(ctx, `
		WITH cte AS (SELECT id, name, qty FROM T)
		SELECT LOWER(TRIM(name)) AS clean,
		       CASE WHEN qty = 0 THEN 'empty' WHEN qty < 5 THEN 'low' ELSE 'high' END AS tier
		FROM cte WHERE LENGTH(TRIM(name)) > 3 ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var got2 []r
	for rows2.Next() {
		var rr r
		g.Expect(rows2.Scan(&rr.clean, &rr.tier)).To(gomega.Succeed())
		got2 = append(got2, rr)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got2).To(gomega.Equal(got))
}

func TestFDB_FunctionWrappingCase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_fn_wrap_case")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_fn_wrap_case")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE fn_wc_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, qty BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_fn_wrap_case/main WITH TEMPLATE fn_wc_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_fn_wrap_case?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, qty) VALUES (1, 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, qty) VALUES (2, 0)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Scalar function (UPPER) wrapping CASE.
	rows, err := db.QueryContext(ctx, `
		SELECT UPPER(CASE WHEN qty > 0 THEN 'yes' ELSE 'no' END) FROM T ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var v string
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(vals).To(gomega.Equal([]string{"YES", "NO"}))
}

func TestFDB_AggregateOrderByStrict(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_agg_ob_strict")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_agg_ob_strict")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE agg_ob_strict_tmpl
		CREATE TABLE Sale (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_agg_ob_strict/main WITH TEMPLATE agg_ob_strict_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_agg_ob_strict?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Region 'a' inserted FIRST with LOW total; region 'z' inserted LAST with HIGH total.
	// So insertion order (likely natural scan order) is [a, z] but ORDER BY SUM DESC should be [z, a].
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (1, 'a', 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (2, 'a', 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (3, 'z', 500)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sale (id, region, amount) VALUES (4, 'z', 1000)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CTE + GROUP BY + ORDER BY SUM(amount) DESC. If ORDER BY is a no-op, we'd get [a, z].
	rows, err := db.QueryContext(ctx, `
		WITH s AS (SELECT id, region, amount FROM Sale)
		SELECT region, SUM(amount) FROM s GROUP BY region ORDER BY SUM(amount) DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var regions []string
	for rows.Next() {
		var r string
		var t any
		g.Expect(rows.Scan(&r, &t)).To(gomega.Succeed())
		regions = append(regions, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(regions).To(gomega.Equal([]string{"z", "a"})) // z has 1500, a has 30.
}

func TestFDB_OrderByArithmeticOnAggregateErrors(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ob_agg_err")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ob_agg_err")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ob_agg_err_tmpl
		CREATE TABLE S (id BIGINT NOT NULL, region STRING NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ob_agg_err/main WITH TEMPLATE ob_agg_err_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ob_agg_err?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO S (id, region, amount) VALUES (1, 'a', 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO S (id, region, amount) VALUES (2, 'b', 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// ORDER BY SUM(amount) * 2 — arithmetic wrapping an aggregate isn't supported
	// (aggregation shrinks rows, breaking per-row expression evaluation). Any
	// error is acceptable; what's NOT acceptable is a silent no-op ORDER BY.
	_, err = db.QueryContext(ctx, `
		WITH s AS (SELECT id, region, amount FROM S)
		SELECT region, SUM(amount) FROM s GROUP BY region ORDER BY SUM(amount) * 2 DESC`)
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestFDB_SelfJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_self_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_self_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE self_join_tmpl
		CREATE TABLE Employee (id BIGINT NOT NULL, name STRING NOT NULL, manager_id BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_self_join/main WITH TEMPLATE self_join_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_self_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Employee (id, name, manager_id) VALUES (1, 'CEO', NULL)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Employee (id, name, manager_id) VALUES (2, 'VP', 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Employee (id, name, manager_id) VALUES (3, 'Eng', 2)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Self-join via implicit cross: "SELECT e.name, m.name FROM Employee e, Employee m WHERE e.manager_id = m.id"
	// Note: alias via AS is required for the same table twice.
	rows, err := db.QueryContext(ctx, `
		SELECT e.name, m.name
		FROM Employee AS e, Employee AS m
		WHERE e.manager_id = m.id
		ORDER BY e.id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type r struct{ emp, mgr string }
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.emp, &rr.mgr)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{
		{"VP", "CEO"},
		{"Eng", "VP"},
	}))
}

func TestFDB_CaseInWhere(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_case_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_case_where")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE case_where_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, status STRING NOT NULL, priority BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_case_where/main WITH TEMPLATE case_where_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_case_where?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (1, 'open', 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (2, 'closed', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (3, 'open', 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CASE in WHERE: keep rows where (CASE status=open THEN priority<3 ELSE priority>50 END).
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM T WHERE CASE WHEN status = 'open' THEN priority < 3 ELSE priority > 50 END
		ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2, 3})) // id=2 closed+100>50, id=3 open+1<3
}

func TestFDB_InsertMultiRowWithExpressions(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_insert_multi_expr")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_insert_multi_expr")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ins_multi_expr_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, name STRING NOT NULL, doubled BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_insert_multi_expr/main WITH TEMPLATE ins_multi_expr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_insert_multi_expr?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert three rows where each value uses an expression.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, doubled) VALUES
		(1, UPPER('alpha'), 5 + 5),
		(2, LOWER('BETA'), 20 * 2),
		(3, CONCAT('a', 'b'), ABS(-42))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id, name, doubled FROM T ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type r struct {
		id      int64
		name    string
		doubled int64
	}
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.id, &rr.name, &rr.doubled)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{
		{1, "ALPHA", 10},
		{2, "beta", 40},
		{3, "ab", 42},
	}))
}

func TestFDB_EmptyResultEdgeCases(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_empty_edge")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_empty_edge")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE empty_edge_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_empty_edge/main WITH TEMPLATE empty_edge_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_empty_edge?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// No rows inserted — all queries should return empty result sets gracefully.

	// ORDER BY on empty result.
	rows, err := db.QueryContext(ctx, `SELECT id FROM T ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	rows.Close()

	// CTE over empty table + aggregate: COUNT(*) returns 0, SUM returns NULL.
	var cnt int64
	g.Expect(db.QueryRowContext(ctx, `WITH c AS (SELECT id FROM T) SELECT COUNT(*) FROM c`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(0)))

	// JOIN on empty + WHERE → empty.
	rows2, err := db.QueryContext(ctx, `
		WITH c AS (SELECT id FROM T)
		SELECT T.id FROM T INNER JOIN c ON T.id = c.id WHERE T.name = 'never'`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	g.Expect(rows2.Next()).To(gomega.BeFalse())

	// EXISTS on empty — false.
	var result int
	g.Expect(db.QueryRowContext(ctx,
		`SELECT CASE WHEN EXISTS (SELECT id FROM T) THEN 1 ELSE 0 END`).Scan(&result)).To(gomega.Succeed())
	g.Expect(result).To(gomega.Equal(0))
}

func TestFDB_InsertSelectFromCTE(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ins_sel_cte")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_ins_sel_cte")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE ins_sel_cte_tmpl
		CREATE TABLE Src (id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Dst (id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_ins_sel_cte/main WITH TEMPLATE ins_sel_cte_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ins_sel_cte?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for i, amt := range []int64{10, 20, 30, 40, 50} {
		_, err = db.ExecContext(ctx, "INSERT INTO Src (id, amount) VALUES (?, ?)", int64(i+1), amt)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// INSERT INTO Dst SELECT id, amount FROM Src WHERE amount >= 30.
	_, err = db.ExecContext(ctx, `INSERT INTO Dst SELECT id, amount FROM Src WHERE amount >= 30`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id, amount FROM Dst ORDER BY id ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type r struct {
		id, amt int64
	}
	var got []r
	for rows.Next() {
		var rr r
		g.Expect(rows.Scan(&rr.id, &rr.amt)).To(gomega.Succeed())
		got = append(got, rr)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]r{{3, 30}, {4, 40}, {5, 50}}))
}

func TestFDB_LeftRight(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_left_right")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_left_right")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE lr_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_left_right/main WITH TEMPLATE lr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_left_right?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name) VALUES (1, 'foobar')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name) VALUES (2, 'ab')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var l, r string
	g.Expect(db.QueryRowContext(ctx, `SELECT LEFT(name, 3), RIGHT(name, 3) FROM T WHERE id = 1`).
		Scan(&l, &r)).To(gomega.Succeed())
	g.Expect(l).To(gomega.Equal("foo"))
	g.Expect(r).To(gomega.Equal("bar"))

	// n larger than length: return whole string.
	g.Expect(db.QueryRowContext(ctx, `SELECT LEFT(name, 100), RIGHT(name, 100) FROM T WHERE id = 2`).
		Scan(&l, &r)).To(gomega.Succeed())
	g.Expect(l).To(gomega.Equal("ab"))
	g.Expect(r).To(gomega.Equal("ab"))
}

func TestFDB_ReversePosition(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_str_more")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_str_more")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE str_more_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, s STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_str_more/main WITH TEMPLATE str_more_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_str_more?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, s) VALUES (1, 'hello')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var rev string
	var pos int64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT REVERSE(s), POSITION('ll', s) FROM T WHERE id = 1`).
		Scan(&rev, &pos)).To(gomega.Succeed())
	g.Expect(rev).To(gomega.Equal("olleh"))
	g.Expect(pos).To(gomega.Equal(int64(3))) // POSITION(ll, hello) → 3 (1-based)

	var notFound int64
	g.Expect(db.QueryRowContext(ctx, `SELECT POSITION('zzz', s) FROM T WHERE id = 1`).Scan(&notFound)).To(gomega.Succeed())
	g.Expect(notFound).To(gomega.Equal(int64(0)))
}

func TestFDB_MathFunctionsTranscendental(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_math_fn")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_math_fn")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE math_fn_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, x BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_math_fn/main WITH TEMPLATE math_fn_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_math_fn?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, x) VALUES (1, 16)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// SQRT(16) = 4.0, EXP(0) = 1.0, LN(1) = 0, LOG(2, 8) = 3.
	var s, e, l, lb float64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT SQRT(x), EXP(0), LN(1), LOG(2, 8) FROM T WHERE id = 1`).
		Scan(&s, &e, &l, &lb)).To(gomega.Succeed())
	g.Expect(s).To(gomega.Equal(4.0))
	g.Expect(e).To(gomega.Equal(1.0))
	g.Expect(l).To(gomega.Equal(0.0))
	g.Expect(lb).To(gomega.BeNumerically("~", 3.0, 1e-9))

	// SQRT of negative → NULL.
	var v sql.NullFloat64
	g.Expect(db.QueryRowContext(ctx, `SELECT SQRT(-1) FROM T WHERE id = 1`).Scan(&v)).To(gomega.Succeed())
	g.Expect(v.Valid).To(gomega.BeFalse())
}

func TestFDB_ParameterizedSubquery(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_param_subq")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_param_subq")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE param_subq_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, tier STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Sales (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_param_subq/main WITH TEMPLATE param_subq_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_param_subq?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, tier) VALUES (1, 'gold')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, tier) VALUES (2, 'silver')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Sales (id, customer_id, amount) VALUES (2, 2, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Parameter in the subquery WHERE.
	var cnt int64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM Sales WHERE customer_id IN (SELECT id FROM Customer WHERE tier = ?)`,
		"gold").Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1)))

	// Parameter in both outer and inner.
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM Sales WHERE amount >= ? AND customer_id IN (SELECT id FROM Customer WHERE tier = ?)`,
		int64(150), "silver").Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1)))
}

func TestFDB_PiFunction(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pi")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_pi")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE pi_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_pi/main WITH TEMPLATE pi_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_pi?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	var pi float64
	g.Expect(db.QueryRowContext(ctx, `SELECT PI()`).Scan(&pi)).To(gomega.Succeed())
	g.Expect(pi).To(gomega.BeNumerically("~", 3.14159265358979, 1e-10))
}
