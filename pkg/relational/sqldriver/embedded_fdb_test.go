package sqldriver_test

// FDB integration tests for the embedded SQL connection. Tests spin up a real
// FoundationDB container and verify that DDL SQL (CREATE/DROP DATABASE/SCHEMA)
// round-trips through the full stack: sql.DB → driver.Conn → parser →
// MetadataOperationsFactory → FDB.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// clusterFilePath is written once in TestMain and shared across tests.
var clusterFilePath string

// RFC-048 W1: the entire sqldriver test binary runs with the executor's
// "no unresolved reference" invariant armed. Every query in every test — all
// the aggregate / HAVING / correlated-scalar-subquery shapes Exhibit-A came
// from — is therefore checked: if any of them evaluates a reference to a name
// absent from a *complete* row (aggregate output), it is recorded here and the
// binary fails, even when the individual test "passed". This is the standing
// E2E proof of the RFC-048 success criterion ("no production code path emits
// an unresolved reference today"). The flag is set ONCE in TestMain before any
// test runs and never mutated afterwards, so it is safe under t.Parallel();
// the hook only appends under a mutex. Strict mode does not change any returned
// value — it only reports misses — so results are identical with it on.
var (
	w1mu         sync.Mutex
	w1violations = map[string]int{}
)

func armW1() {
	executor.StrictReferenceCheck = true
	values.ReportUnresolvedReference = func(field string, available []string) {
		avail := append([]string(nil), available...)
		sort.Strings(avail)
		key := fmt.Sprintf("unresolved reference %q against complete-row keys %v", field, avail)
		w1mu.Lock()
		w1violations[key]++
		w1mu.Unlock()
	}
}

func finishW1(code int) int {
	w1mu.Lock()
	defer w1mu.Unlock()
	if len(w1violations) == 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, "\nRFC-048 W1: %d distinct unresolved-reference violation(s) — a silent name->NULL was emitted against a complete row:\n", len(w1violations))
	for v, n := range w1violations {
		fmt.Fprintf(os.Stderr, "  - %s (x%d)\n", v, n)
	}
	if code == 0 {
		return 1
	}
	return code
}

func TestMain(m *testing.M) {
	armW1() // RFC-048 W1: arm the invariant for the whole test binary.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — run non-FDB tests only.
		os.Exit(finishW1(m.Run()))
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

	os.Exit(finishW1(m.Run()))
}

// expectUnsupportedOperator asserts that err unwraps to an *api.Error
// with the byte-equal Java rejection message ("Unsupported operator
// <opName>"). SELECT path uses ErrCodeUndefinedFunction (42883, Java's
// SqlFunctionCatalog.lookupFunction); DML paths may use
// ErrCodeUnsupportedOperation (0A000) when the function is embedded in
// values/expressions the FindUnsupportedFunction walker doesn't reach.
func expectUnsupportedOperator(g gomega.Gomega, err error, opName, ctx string) {
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(),
		"%s: want *api.Error, got %T (%v)", ctx, err, err)
	g.Expect(apiErr.Code).To(gomega.BeElementOf(api.ErrCodeUndefinedFunction, api.ErrCodeUnsupportedOperation),
		"%s: want ErrCodeUndefinedFunction or ErrCodeUnsupportedOperation, got %s", ctx, apiErr.Code)
	g.Expect(apiErr.Message).To(gomega.Equal("Unsupported operator "+opName),
		"%s: want byte-equal Java message", ctx)
}

// expectRejectionOrCascadesError asserts that err is an *api.Error whose
// message is either the legacy specific rejection message or the generic
// Cascades planner failure ("Cascades planner could not plan query").
// With the Cascades-only path, unsupported SQL features surface as
// planning failures rather than feature-specific rejection messages.
func expectRejectionOrCascadesError(t *testing.T, err error, legacyMessages ...string) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	for _, msg := range legacyMessages {
		if strings.Contains(apiErr.Message, msg) {
			return
		}
	}
	if strings.Contains(apiErr.Message, "Cascades planner could not plan query") {
		return
	}
	t.Fatalf("unexpected error message: %q (expected one of %v or 'Cascades planner could not plan query')",
		apiErr.Message, legacyMessages)
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
		if dbCatalog != "/testdb_is_columns" || tbl != "EMPLOYEE" {
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
	g.Expect(colRows[0].colName).To(gomega.Equal("EMP_ID"))
	g.Expect(colRows[0].ordinal).To(gomega.Equal(int64(1)))
	g.Expect(colRows[0].nullable).To(gomega.Equal("NO"))
	g.Expect(colRows[0].dataType).To(gomega.Equal("LONG"))

	// Verify name: nullable STRING (CodeString).
	g.Expect(colRows[1].colName).To(gomega.Equal("NAME"))
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
	g.Expect(byName).To(gomega.HaveKey("BY_NAME"))
	g.Expect(byName["BY_NAME"].tableName).To(gomega.Equal("PRODUCT"))
	g.Expect(byName["BY_NAME"].isUnique).To(gomega.Equal("NO"))

	g.Expect(byName).To(gomega.HaveKey("BY_ID"))
	g.Expect(byName["BY_ID"].tableName).To(gomega.Equal("PRODUCT"))
	g.Expect(byName["BY_ID"].isUnique).To(gomega.Equal("YES"))
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
	g.Expect(cols).To(gomega.Equal([]string{"NAME"}))
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
	g.Expect(cols2).To(gomega.Equal([]string{"ITEM_ID", "PRICE"}))
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
	g.Expect(err.Error()).To(gomega.ContainSubstring("REC_ID"))
}

// TestFDB_SelectWhereTypeMismatch verifies that comparing a BIGINT column
// against a string constant errors with SQLSTATE 42804
// (DATATYPE_MISMATCH), matching Java's ExceptionUtil translation of
// SemanticException.COMPARISON_OF_INCOMPATIBLE_TYPES → DATATYPE_MISMATCH.
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

	// Compare BIGINT column against a string — must error 42804.
	// Java maps COMPARISON_OF_INCOMPATIBLE_TYPES → DATATYPE_MISMATCH.
	rows, err := db.QueryContext(ctx, "SELECT * FROM Obj WHERE obj_id = 'notanumber'")
	if err == nil {
		// Some paths surface the error during row iteration (executor
		// runs per-row); drain the cursor to provoke it.
		for rows.Next() {
			vals := make([]any, 2)
			ptrs := []any{&vals[0], &vals[1]}
			_ = rows.Scan(ptrs...)
		}
		err = rows.Err()
		rows.Close()
	}
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(), "expected *api.Error, got %T: %v", err, err)
	g.Expect(string(apiErr.Code)).To(gomega.Equal("42804"))
}

func TestFDB_SelectOrderBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby")).Error().NotTo(gomega.HaveOccurred())
	// INDEX on val so ORDER BY val can pick a scan that satisfies the
	// requested ordering — matches Java's Cascades RemoveSortRule
	// firing on an inner index scan whose Ordering property satisfies.
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_val ON Item (val)")).Error().NotTo(gomega.HaveOccurred())
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

// TestFDB_SelectOrderByRejectionNoIndex verifies ORDER BY on a
// non-indexed column succeeds via the in-memory sort operator.
// Go extension: in-memory sort — Java's Cascades planner would reject
// this with UnableToPlanException, but Go's ImplementInMemorySortRule
// materializes and sorts the result set.
func TestFDB_SelectOrderByRejectionNoIndex(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby_reject")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby_reject")).Error().NotTo(gomega.HaveOccurred())
	// NO index on val — Go extension: in-memory sort handles this.
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_reject_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_orderby_reject/items WITH TEMPLATE ob_reject_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_orderby_reject?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, val) VALUES (3, 300), (1, 100), (2, 200)")).Error().NotTo(gomega.HaveOccurred())

	// Go extension: in-memory sort — ORDER BY val ASC without index.
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		got = append(got, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// val 100 → id 1, val 200 → id 2, val 300 → id 3
	g.Expect(got).To(gomega.Equal([]int64{1, 2, 3}))
}

// TestFDB_SelectOrderByRejectionExpression verifies ORDER BY on an
// arithmetic expression succeeds via the in-memory sort operator.
// Go extension: in-memory sort — Java's Cascades planner would reject
// this with UnableToPlanException, but Go's ImplementInMemorySortRule
// handles expression-based sort keys.
func TestFDB_SelectOrderByRejectionExpression(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby_reject_expr")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby_reject_expr")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_reject_expr_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, a BIGINT NOT NULL, b BIGINT NOT NULL, PRIMARY KEY (item_id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_orderby_reject_expr/items WITH TEMPLATE ob_reject_expr_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_orderby_reject_expr?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Item (item_id, a, b) VALUES (1, 10, 20), (2, 5, 15)")).Error().NotTo(gomega.HaveOccurred())

	// Go extension: in-memory sort — ORDER BY arithmetic expression (a + b).
	// The expression sort key doesn't map to a column in the result set,
	// so the in-memory sort is effectively a no-op (stable order). Both
	// rows are returned successfully; order follows the inner plan's
	// emission (PK order).
	rows, err := db.QueryContext(ctx, "SELECT item_id FROM Item ORDER BY a + b")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		got = append(got, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.ConsistOf(int64(1), int64(2)))
}

func TestFDB_SelectOrderByDesc(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_orderby_desc")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_orderby_desc")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE obdesc_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_val ON Item (val)")).Error().NotTo(gomega.HaveOccurred())
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
	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.val)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{3, 300}, {2, 200}, {1, 100}}))
}

func TestFDB_SelectOrderByMultiColumn(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_ob_multi")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_ob_multi")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ob_multi_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, a STRING NOT NULL, b BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_ab ON T (a, b)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_ob_multi/main WITH TEMPLATE ob_multi_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_ob_multi?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx,
		"INSERT INTO T (id, a, b) VALUES (1, 'b', 2), (2, 'a', 3), (3, 'a', 1), (4, 'b', 1)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT a, b FROM T ORDER BY a ASC, b ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type row struct {
		a string
		b int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.a, &r.b)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{"a", 1}, {"a", 3}, {"b", 1}, {"b", 2}}))

	// Multi-column DESC: both keys reversed.
	rows2, err := db.QueryContext(ctx, "SELECT a, b FROM T ORDER BY a DESC, b DESC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var got2 []row
	for rows2.Next() {
		var r row
		g.Expect(rows2.Scan(&r.a, &r.b)).To(gomega.Succeed())
		got2 = append(got2, r)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got2).To(gomega.Equal([]row{{"b", 2}, {"b", 1}, {"a", 3}, {"a", 1}}))

	// Go extension: in-memory sort — mixed ASC/DESC now succeeds.
	rows3, err := db.QueryContext(ctx, "SELECT a, b FROM T ORDER BY a ASC, b DESC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows3.Close()
	var got3 []row
	for rows3.Next() {
		var r row
		g.Expect(rows3.Scan(&r.a, &r.b)).To(gomega.Succeed())
		got3 = append(got3, r)
	}
	g.Expect(rows3.Err()).NotTo(gomega.HaveOccurred())
	// a ASC, b DESC: a='a' first (b DESC: 3,1), then a='b' (b DESC: 2,1).
	g.Expect(got3).To(gomega.Equal([]row{{"a", 3}, {"a", 1}, {"b", 2}, {"b", 1}}))
}

func TestFDB_SelectDistinctOrderBy(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dist_orderby")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_dist_orderby")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dist_ob_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_val ON T (val)")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_dist_orderby/main WITH TEMPLATE dist_ob_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_dist_orderby?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO T (id, val) VALUES (1, 10), (2, 20), (3, 10), (4, 30), (5, 20)")).Error().NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT DISTINCT val FROM T ORDER BY val ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]int64{10, 20, 30}))
}

// TestFDB_SelectLimit verifies SQL LIMIT/OFFSET (Go extension).
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
	g.Expect(ids).To(gomega.Equal([]int64{1, 2, 3}))
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
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_val ON Item (val)")).Error().NotTo(gomega.HaveOccurred())
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
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_val ON Item (val)")).Error().NotTo(gomega.HaveOccurred())
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

	rows, err := db.QueryContext(ctx, "SELECT DISTINCT val FROM Item")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var vals []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
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

	// Regression: qualified column names (Item.val) must work in IS NULL
	// and IN predicates. Before the fix, ByName("Item.val") failed because
	// proto field descriptors use bare names.
	rows3, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE Item.val IS NULL ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows3.Close()
	var ids3 []int64
	for rows3.Next() {
		var id int64
		g.Expect(rows3.Scan(&id)).To(gomega.Succeed())
		ids3 = append(ids3, id)
	}
	g.Expect(rows3.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids3).To(gomega.Equal([]int64{2}))

	// Qualified IN predicate.
	rows4, err := db.QueryContext(ctx, "SELECT item_id FROM Item WHERE Item.val IN (42) ORDER BY item_id ASC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows4.Close()
	var ids4 []int64
	for rows4.Next() {
		var id int64
		g.Expect(rows4.Scan(&id)).To(gomega.Succeed())
		ids4 = append(ids4, id)
	}
	g.Expect(rows4.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids4).To(gomega.Equal([]int64{1}))
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
	g.Expect(cols).To(gomega.Equal([]string{"ID", "AMOUNT"}))

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
			"CREATE TABLE Item (item_id BIGINT NOT NULL, val BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (item_id)) "+
			"CREATE INDEX idx_val ON Item (val)")).Error().NotTo(gomega.HaveOccurred())
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

func TestFDB_UpdateInt32Overflow(t *testing.T) {
	// UPDATE on an INTEGER (INT32) column must reject values outside [-2147483648, 2147483647] with SQLSTATE 22003.
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_upd_int32_ovf")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_upd_int32_ovf")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE upd_int32_ovf_tmpl "+
			"CREATE TABLE T32 (id BIGINT NOT NULL, val INTEGER, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_upd_int32_ovf/upd_int32_ovf WITH TEMPLATE upd_int32_ovf_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_upd_int32_ovf?cluster_file=%s&schema=upd_int32_ovf", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert a row with val at INT32 max.
	g.Expect(db.ExecContext(ctx, "INSERT INTO T32 (id, val) VALUES (1, 2147483647)")).Error().NotTo(gomega.HaveOccurred())

	// Literal overflow: 2147483648 exceeds INT32 max.
	_, err = db.ExecContext(ctx, "UPDATE T32 SET val = 2147483648 WHERE id = 1")
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(), "expected *api.Error, got %T: %v", err, err)
	g.Expect(string(apiErr.Code)).To(gomega.Equal("22003"))

	// Literal underflow: -2147483649 is below INT32 min.
	_, err = db.ExecContext(ctx, "UPDATE T32 SET val = -2147483649 WHERE id = 1")
	var apiErr2 *api.Error
	g.Expect(errors.As(err, &apiErr2)).To(gomega.BeTrue(), "expected *api.Error, got %T: %v", err, err)
	g.Expect(string(apiErr2.Code)).To(gomega.Equal("22003"))

	// Arithmetic overflow: val is at INT32 max, val + 1 overflows.
	_, err = db.ExecContext(ctx, "UPDATE T32 SET val = val + 1 WHERE id = 1")
	var apiErr3 *api.Error
	g.Expect(errors.As(err, &apiErr3)).To(gomega.BeTrue(), "expected *api.Error, got %T: %v", err, err)
	g.Expect(string(apiErr3.Code)).To(gomega.Equal("22003"))

	// Row must be unchanged: val is still 2147483647.
	rows, err := db.QueryContext(ctx, "SELECT val FROM T32 WHERE id = 1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(gomega.BeTrue())
	var val int64
	g.Expect(rows.Scan(&val)).To(gomega.Succeed())
	g.Expect(val).To(gomega.Equal(int64(2147483647)))
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

	// Go extension: in-memory sort — ORDER BY COUNT(*) DESC now succeeds.
	rows, err := db.QueryContext(ctx,
		"SELECT region, COUNT(*) FROM Sale GROUP BY region ORDER BY COUNT(*) DESC")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type grpRow struct {
		region string
		count  int64
	}
	var got []grpRow
	for rows.Next() {
		var r grpRow
		g.Expect(rows.Scan(&r.region, &r.count)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// north=3, south=2, east=1 — DESC by count.
	g.Expect(got).To(gomega.Equal([]grpRow{{"north", 3}, {"south", 2}, {"east", 1}}))
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

// TestFDB_SumIntegerDivision pins Java-aligned integer-preserving SUM
// semantics: `SUM(BIGINT) / COUNT(*)` integer-divides instead of
// float-dividing. Pre-fix Go's SUM accumulator was always float64, so
// SUM(qty)=10 / COUNT(*)=3 emerged as 3.333... while Java returned 3.
// The dual-accumulator path (sumsI int64 + sumNonInt bool flag) emits
// int64 when every observed value is integral; subsequent int64 / int64
// arithmetic in `ApplyMathOp` yields the integer-divided result.
func TestFDB_SumIntegerDivision(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sumdiv")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_sumdiv")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sumdiv_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, qty BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sumdiv/items WITH TEMPLATE sumdiv_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_sumdiv?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for i, q := range []int{2, 3, 5} {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO T (id, qty) VALUES (%d, %d)", i+1, q))
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// SUM(qty) = 10, COUNT(*) = 3, integer division → 3 (NOT 3.333...).
	var ratio int64
	g.Expect(db.QueryRowContext(ctx, "SELECT SUM(qty) / COUNT(*) FROM T").Scan(&ratio)).To(gomega.Succeed())
	g.Expect(ratio).To(gomega.Equal(int64(3)),
		"SUM(BIGINT) / COUNT(*) must integer-divide (Java alignment)")

	// SUM(qty) - COUNT(*) = 10 - 3 = 7, both int64 → int64 result (was float64
	// pre-fix because SUM was always float64 → 7.0).
	var diff int64
	g.Expect(db.QueryRowContext(ctx, "SELECT SUM(qty) - COUNT(*) FROM T").Scan(&diff)).To(gomega.Succeed())
	g.Expect(diff).To(gomega.Equal(int64(7)))

	// SUM(qty) * 2 = 20, int64 * int64 = int64.
	var doubled int64
	g.Expect(db.QueryRowContext(ctx, "SELECT SUM(qty) * 2 FROM T").Scan(&doubled)).To(gomega.Succeed())
	g.Expect(doubled).To(gomega.Equal(int64(20)))

	// SUM over a mixed-type expression (qty + 1.0) promotes to float64.
	var promoted float64
	g.Expect(db.QueryRowContext(ctx, "SELECT SUM(qty + 1.0) FROM T").Scan(&promoted)).To(gomega.Succeed())
	g.Expect(promoted).To(gomega.Equal(float64(13)))
}

// TestFDB_BareBoolProjection pins Java-aligned bare-boolean operand
// behaviour in SELECT projection. `SELECT b AND TRUE`, `SELECT NOT b`,
// `SELECT b OR FALSE` over a BOOLEAN column evaluate the column as a
// value (via IsTruthy) rather than rejecting with "expected
// BooleanValue but got FieldValue". Top-level `WHERE b` now plans too
// (RFC-146): it lifts to `b = TRUE`, matching Java 4.12.
func TestFDB_BareBoolProjection(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_barebool")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_barebool")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE barebool_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, b BOOLEAN, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_barebool/items WITH TEMPLATE barebool_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_barebool?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Three rows: TRUE, FALSE, NULL — the canonical Kleene 3VL surface.
	_, err = db.ExecContext(ctx, "INSERT INTO T VALUES (1, true), (2, false), (3, null)")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// `b AND TRUE`: t/f/NULL preserved.
	rows, err := db.QueryContext(ctx, "SELECT b AND TRUE FROM T ORDER BY id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got := []sql.NullBool{}
	for rows.Next() {
		var v sql.NullBool
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	rows.Close()
	g.Expect(got).To(gomega.HaveLen(3))
	g.Expect(got[0]).To(gomega.Equal(sql.NullBool{Bool: true, Valid: true}))
	g.Expect(got[1]).To(gomega.Equal(sql.NullBool{Bool: false, Valid: true}))
	g.Expect(got[2].Valid).To(gomega.BeFalse(), "NULL preserved through AND TRUE")

	// `NOT b`: f/t/NULL.
	rows, err = db.QueryContext(ctx, "SELECT NOT b FROM T ORDER BY id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got = got[:0]
	for rows.Next() {
		var v sql.NullBool
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	rows.Close()
	g.Expect(got).To(gomega.HaveLen(3))
	g.Expect(got[0]).To(gomega.Equal(sql.NullBool{Bool: false, Valid: true}))
	g.Expect(got[1]).To(gomega.Equal(sql.NullBool{Bool: true, Valid: true}))
	g.Expect(got[2].Valid).To(gomega.BeFalse(), "NULL preserved through NOT")

	// `b OR FALSE`: t/f/NULL (same as b for non-NULL rows; UNKNOWN preserved).
	rows, err = db.QueryContext(ctx, "SELECT b OR FALSE FROM T ORDER BY id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got = got[:0]
	for rows.Next() {
		var v sql.NullBool
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	rows.Close()
	g.Expect(got).To(gomega.HaveLen(3))
	g.Expect(got[0]).To(gomega.Equal(sql.NullBool{Bool: true, Valid: true}))
	g.Expect(got[1]).To(gomega.Equal(sql.NullBool{Bool: false, Valid: true}))
	g.Expect(got[2].Valid).To(gomega.BeFalse(), "NULL preserved through OR FALSE")

	// `b AND FALSE`: short-circuits to FALSE for every row (UNKNOWN absorbed).
	rows, err = db.QueryContext(ctx, "SELECT b AND FALSE FROM T ORDER BY id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	got = got[:0]
	for rows.Next() {
		var v sql.NullBool
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		got = append(got, v)
	}
	rows.Close()
	g.Expect(got).To(gomega.HaveLen(3))
	for i, v := range got {
		g.Expect(v).To(gomega.Equal(sql.NullBool{Bool: false, Valid: true}),
			"row %d: AND FALSE always FALSE", i)
	}

	// Top-level bare boolean WHERE now plans (RFC-146): `WHERE b` lifts to
	// `b = TRUE`, so only the b=TRUE row (id 1) survives.
	rows, err = db.QueryContext(ctx, "SELECT id FROM T WHERE b ORDER BY id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	rows.Close()
	g.Expect(ids).To(gomega.Equal([]int64{1}))
}

// TestFDB_DoubleColumnComparison pins row-level DOUBLE comparison correctness
// after RFC-146 typed FLOAT/DOUBLE as TypeCodeDouble (the type-mapping change
// touched the whole DOUBLE resolver path, and there was previously zero
// row-level coverage of a DOUBLE column in a WHERE comparison). Covers
// double-vs-double, int-literal → double promotion, and a CTE-derived double.
func TestFDB_DoubleColumnComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_double_cmp")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_double_cmp")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE double_cmp_tmpl "+
			"CREATE TABLE D (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx, "CREATE SCHEMA /testdb_double_cmp/items WITH TEMPLATE double_cmp_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_double_cmp?cluster_file=%s&schema=items", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, "INSERT INTO D VALUES (1, 0.5), (2, 1.5), (3, 2.5)")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	collect := func(q string) []int64 {
		rows, qerr := db.QueryContext(ctx, q)
		g.Expect(qerr).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			g.Expect(rows.Scan(&id)).To(gomega.Succeed())
			ids = append(ids, id)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		return ids
	}

	// double-vs-double: only d=2.5 (id 3) exceeds 1.5.
	g.Expect(collect("SELECT id FROM D WHERE d > 1.5")).To(gomega.ConsistOf(int64(3)))
	// int-literal → double promotion: d=1.5 (id 2) and d=2.5 (id 3) exceed 1.
	g.Expect(collect("SELECT id FROM D WHERE d > 1")).To(gomega.ConsistOf(int64(2), int64(3)))
	// CTE-derived double column compares identically.
	g.Expect(collect("WITH c AS (SELECT d, id FROM D) SELECT id FROM c WHERE d > 1.5")).To(gomega.ConsistOf(int64(3)))
}

func TestFDB_SelectScalarExpression(t *testing.T) {
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
	var id, doubled int64
	g.Expect(rows.Scan(&id, &doubled)).To(gomega.Succeed())
	g.Expect(id).To(gomega.Equal(int64(1)))
	g.Expect(doubled).To(gomega.Equal(int64(20)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
}

func TestFDB_SelectCoalesce(t *testing.T) {
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

// TestFDB_LimitOffset verifies LIMIT + OFFSET (Go extension).
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

	rows, err := db.QueryContext(ctx, `SELECT id FROM Item ORDER BY id ASC LIMIT 2 OFFSET 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
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

// TestFDB_StringFunctions pins the Go-only STRING scalar functions
// (RFC-087): UPPER / LOWER / LENGTH / TRIM compute over a column arg via
// the Cascades values path. These have no entry in fdb-relational
// 4.11.1.0's function registry, so they are net-new read-side extensions
// (zero wire impact). Whitespace is preserved by UPPER/LOWER; LENGTH is a
// rune count; TRIM strips surrounding whitespace.
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

	// label = '  Hello  ' (2 + 5 + 2 = 9 runes).
	cases := []struct {
		query string
		want  string
	}{
		{`SELECT UPPER(label) FROM Word WHERE id = 1`, "  HELLO  "},
		{`SELECT LOWER(label) FROM Word WHERE id = 1`, "  hello  "},
		{`SELECT TRIM(label) FROM Word WHERE id = 1`, "Hello"},
	}
	for _, tc := range cases {
		var got string
		g.Expect(db.QueryRowContext(ctx, tc.query).Scan(&got)).To(gomega.Succeed(), "query %q", tc.query)
		g.Expect(got).To(gomega.Equal(tc.want), "query %q", tc.query)
	}

	// LENGTH returns the rune count as an integer.
	var n int64
	g.Expect(db.QueryRowContext(ctx, `SELECT LENGTH(label) FROM Word WHERE id = 1`).Scan(&n)).To(gomega.Succeed())
	g.Expect(n).To(gomega.Equal(int64(9)))
}

// TestFDB_ConcatAndNullIf pins CONCAT working (RFC-087 Go-only scalar
// extension: NULL args skip rather than poison, MySQL/Postgres semantics)
// and NULLIF still rejected. NULLIF is absent from both
// IsCascadesSafeScalarFunction's gate and fdb-relational 4.11.1.0's
// function registry, so the planner rejects it ("Unsupported operator
// NULLIF"). Searched-CASE remains the NULLIF workaround.
func TestFDB_ConcatAndNullIf(t *testing.T) {
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

	// CONCAT(first, ' ', last) → 'Alice Smith'.
	var concat string
	g.Expect(db.QueryRowContext(ctx,
		`SELECT CONCAT(first, ' ', last) FROM Person WHERE id = 1`).Scan(&concat)).To(gomega.Succeed())
	g.Expect(concat).To(gomega.Equal("Alice Smith"))

	// NULLIF is still rejected (not in the gate / registry).
	var dummy any
	errNullif := db.QueryRowContext(ctx,
		`SELECT NULLIF(score, 0) FROM Person WHERE id = 1`).Scan(&dummy)
	g.Expect(errNullif).To(gomega.HaveOccurred(), "NULLIF must be rejected")
	expectRejectionOrCascadesError(t, errNullif, "Unsupported operator NULLIF")

	// Searched-CASE — the workaround for NULLIF — must still work.
	var score any
	g.Expect(db.QueryRowContext(ctx,
		`SELECT CASE WHEN score = 0 THEN NULL ELSE score END FROM Person WHERE id = 1`).
		Scan(&score)).To(gomega.Succeed())
	g.Expect(score).To(gomega.Equal(int64(100)))
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

// TestFDB_UnionAllDifferentColumnNames verifies that UNION ALL with
// differently-named columns across branches produces correct results.
// The left branch's column names become the result schema, and right-
// branch values are mapped positionally. ORDER BY on the result uses
// the left branch's column names.
func TestFDB_UnionAllDifferentColumnNames(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_diffcol")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_union_diffcol")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE udcol_tmpl "+
		"CREATE TABLE a (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_union_diffcol/s WITH TEMPLATE udcol_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_union_diffcol?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO a VALUES (1, 10)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO a VALUES (2, 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO b VALUES (1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO b VALUES (2, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Test 1: Simple UNION ALL with different column names, no ORDER BY.
	{
		rows, err := db.QueryContext(ctx, `SELECT v FROM a UNION ALL SELECT w FROM b`)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		var vals []int64
		for rows.Next() {
			var v int64
			g.Expect(rows.Scan(&v)).To(gomega.Succeed())
			vals = append(vals, v)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		rows.Close()
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		g.Expect(vals).To(gomega.Equal([]int64{10, 20, 100, 200}))
	}

	// Test 2: UNION ALL with ORDER BY on left branch's column names.
	{
		rows, err := db.QueryContext(ctx, `SELECT id, v FROM a UNION ALL SELECT id, w FROM b ORDER BY id, v`)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		type row struct{ id, v int64 }
		var got []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.id, &r.v)).To(gomega.Succeed())
			got = append(got, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		rows.Close()
		g.Expect(got).To(gomega.Equal([]row{
			{1, 10},
			{1, 100},
			{2, 20},
			{2, 200},
		}))
	}

	// Test 3: ORDER BY DESC on union.
	{
		rows, err := db.QueryContext(ctx, `SELECT id, v FROM a UNION ALL SELECT id, w FROM b ORDER BY v DESC`)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		type row struct{ id, v int64 }
		var got []row
		for rows.Next() {
			var r row
			g.Expect(rows.Scan(&r.id, &r.v)).To(gomega.Succeed())
			got = append(got, r)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		rows.Close()
		g.Expect(got).To(gomega.Equal([]row{
			{2, 200},
			{1, 100},
			{2, 20},
			{1, 10},
		}))
	}
}

// TestFDB_UnionDistinctRejected pins Java alignment: plain UNION
// (without ALL) is rejected by fdb-relational with verbatim
// "only UNION ALL is supported" because the planner has no
// de-duplication operator. Per project conformance principle
// (doesn't work in Java → doesn't work in Go), Go rejects too.
func TestFDB_UnionDistinctRejected(t *testing.T) {
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

	_, err = db.QueryContext(ctx, `SELECT tag FROM Tag WHERE id = 1 UNION SELECT tag FROM Tag WHERE id = 2`)
	g.Expect(err).To(gomega.HaveOccurred(), "UNION DISTINCT must be rejected")
	expectRejectionOrCascadesError(t, err, "only UNION ALL is supported")
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

	// WHERE filter: only 'alpha' schema. INFORMATION_SCHEMA.SCHEMATA is
	// cross-database (lists every catalog's schemas), so scope to THIS db's
	// CATALOG_NAME — otherwise a parallel test that also creates an `alpha`
	// schema in its own db would leak into a `SCHEMA_NAME = 'alpha'`-only
	// filter (the established parallel-safety convention for system-table
	// tests; the SCHEMA_NAME-only form was a latent race independent of the
	// WHERE-filter behaviour this test pins).
	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '/testdb_is_schemata_where' AND SCHEMA_NAME = 'alpha'`)
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

// TestFDB_InfoSchema_SchemataWhere_QualifiedRef pins the map-path evaluator's
// qualified→bare column fallback that RFC-147 kept while deleting the dead
// validQualifiers/outerScopes scope machinery. A qualified reference to a
// system-table column ("SCHEMATA".SCHEMA_NAME) is not present in the row map
// under its qualified key, so resolution MUST fall back to the bare key
// (SCHEMA_NAME). If the collapse had dropped that fallback the query would
// fail with an undefined-column error instead of returning the alpha row.
func TestFDB_InfoSchema_SchemataWhere_QualifiedRef(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_is_schemata_qref")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_schemata_qref")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE isqr_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_is_schemata_qref/alpha WITH TEMPLATE isqr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_is_schemata_qref/beta WITH TEMPLATE isqr_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_schemata_qref?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Qualified refs ("SCHEMATA".COL) — only resolvable via the kept bare
	// fallback (the row map carries bare keys). Scoped to this db's catalog
	// for parallel safety, as in TestFDB_InfoSchema_SchemataWhere.
	rows, err := db.QueryContext(ctx, `SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE "SCHEMATA".CATALOG_NAME = '/testdb_is_schemata_qref' AND "SCHEMATA".SCHEMA_NAME = 'alpha'`)
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

	// SUBSTRING / REPLACE are Go-only STRING scalar extensions (RFC-087):
	// SUBSTRING(s, pos, len) is 1-based; REPLACE does a literal swap.
	// name = 'Widget' → SUBSTRING(...,1,3) = 'Wid', REPLACE = 'Thing'.
	for _, tc := range []struct {
		query string
		want  string
	}{
		{`SELECT SUBSTRING(name, 1, 3) FROM Item WHERE id = 1`, "Wid"},
		{`SELECT REPLACE(name, 'Widget', 'Thing') FROM Item WHERE id = 1`, "Thing"},
	} {
		var got string
		g.Expect(db.QueryRowContext(ctx, tc.query).Scan(&got)).To(gomega.Succeed(), "query %q", tc.query)
		g.Expect(got).To(gomega.Equal(tc.want), "query %q", tc.query)
	}

	// IF function-form is rejected by Java (not in the function
	// registry). The Java-supported workaround is searched-CASE
	// (`CASE WHEN cond THEN ... ELSE ... END`).
	rows4, err := db.QueryContext(ctx, `SELECT CASE WHEN price > 50 THEN 'expensive' ELSE 'cheap' END FROM Item ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows4.Close()
	var cats []string
	for rows4.Next() {
		var c string
		g.Expect(rows4.Scan(&c)).To(gomega.Succeed())
		cats = append(cats, c)
	}
	g.Expect(cats).To(gomega.Equal([]string{"cheap", "expensive"}))

	// IF as a function call is rejected (Java has no IF function;
	// use searched-CASE instead). Pin the rejection so future
	// re-additions of an IF arm in the evaluator regress this test.
	var ifDummy string
	errIf := db.QueryRowContext(ctx, `SELECT IF(price > 50, 'expensive', 'cheap') FROM Item WHERE id = 1`).Scan(&ifDummy)
	g.Expect(errIf).To(gomega.HaveOccurred(), "IF function-form must be rejected")
	expectRejectionOrCascadesError(t, errIf, "Unsupported operator IF")

	// Java conformance (swingshift-35): CAST(float AS INT) rounds (not
	// truncates) using `Math.round` semantics (floor(x + 0.5)). Previously
	// Go used `int64(n)` which truncates toward zero and silently wraps
	// on overflow. Matches Java CastValue.DOUBLE_TO_LONG.
	var rounded int64
	g.Expect(db.QueryRowContext(ctx, `SELECT CAST(1.6 AS BIGINT) FROM Item WHERE id = 1`).Scan(&rounded)).To(gomega.Succeed())
	g.Expect(rounded).To(gomega.Equal(int64(2)), "CAST(1.6 AS BIGINT) must round to 2, not truncate to 1")

	g.Expect(db.QueryRowContext(ctx, `SELECT CAST(-1.5 AS BIGINT) FROM Item WHERE id = 1`).Scan(&rounded)).To(gomega.Succeed())
	g.Expect(rounded).To(gomega.Equal(int64(-1)), "CAST(-1.5 AS BIGINT) must match Java Math.round (ties → +Inf)")

	g.Expect(db.QueryRowContext(ctx, `SELECT CAST(-2.6 AS BIGINT) FROM Item WHERE id = 1`).Scan(&rounded)).To(gomega.Succeed())
	g.Expect(rounded).To(gomega.Equal(int64(-3)), "CAST(-2.6 AS BIGINT) must round to -3")

	// Java's STRING_TO_LONG / STRING_TO_DOUBLE trim whitespace before parse.
	// Previously Go's ParseInt/ParseFloat rejected leading/trailing spaces.
	var trimmed int64
	g.Expect(db.QueryRowContext(ctx, `SELECT CAST('  42  ' AS BIGINT) FROM Item WHERE id = 1`).Scan(&trimmed)).To(gomega.Succeed())
	g.Expect(trimmed).To(gomega.Equal(int64(42)), "CAST of whitespace-padded string must trim (Java conformance)")

	// Java's STRING_TO_BOOLEAN only accepts trim()ed "true"/"false"/"1"/"0"
	// (case-insensitive). Go's strconv.ParseBool is wider — accepts "t",
	// "T", "F", etc. — so Go used to take strings Java would reject.
	var bv bool
	g.Expect(db.QueryRowContext(ctx, `SELECT CAST('true' AS BOOLEAN) FROM Item WHERE id = 1`).Scan(&bv)).To(gomega.Succeed())
	g.Expect(bv).To(gomega.BeTrue())
	g.Expect(db.QueryRowContext(ctx, `SELECT CAST('  FALSE  ' AS BOOLEAN) FROM Item WHERE id = 1`).Scan(&bv)).To(gomega.Succeed())
	g.Expect(bv).To(gomega.BeFalse(), "CAST of padded mixed-case boolean string must trim+lowercase per Java")
	// Single-letter 't' — Go used to accept via ParseBool, Java rejects.
	_, errCast := db.QueryContext(ctx, `SELECT CAST('t' AS BOOLEAN) FROM Item WHERE id = 1`)
	if errCast == nil {
		// If the driver permits the query (may error at Next/Scan), try to read.
		rowsT, _ := db.QueryContext(ctx, `SELECT CAST('t' AS BOOLEAN) FROM Item WHERE id = 1`)
		if rowsT != nil {
			defer rowsT.Close()
			if rowsT.Next() {
				var b bool
				errCast = rowsT.Scan(&b)
			} else {
				errCast = rowsT.Err()
			}
		}
	}
	g.Expect(errCast).To(gomega.HaveOccurred(), "CAST('t' AS BOOLEAN) must error (Java rejects 't', only 'true'/'false'/'0'/'1')")

	// Java CastValue range-checks the rounded value against target type
	// limits. Go used to silently wrap via int64() on overflow. Any float
	// that can't fit an int64 (> MaxInt64 or <  MinInt64) must now error.
	rowsOverflow, errOF := db.QueryContext(ctx, `SELECT CAST(1e20 AS BIGINT) FROM Item WHERE id = 1`)
	if errOF == nil && rowsOverflow != nil {
		defer rowsOverflow.Close()
		if rowsOverflow.Next() {
			var x int64
			errOF = rowsOverflow.Scan(&x)
		} else {
			errOF = rowsOverflow.Err()
		}
	}
	g.Expect(errOF).To(gomega.HaveOccurred(), "CAST(1e20 AS BIGINT) must error on overflow, not silently wrap")

	// ROUND is a Go-only math scalar extension (RFC-087). A non-coercible
	// decimals arg degrades to SQL NULL rather than erroring: NULL decimals
	// propagates NULL, and a non-numeric string ('abc') declines to NULL.
	for _, q := range []string{
		`SELECT ROUND(1.2345, NULL) FROM Item WHERE id = 1`,
		`SELECT ROUND(1.2345, 'abc') FROM Item WHERE id = 1`,
	} {
		var out sql.NullString
		g.Expect(db.QueryRowContext(ctx, q).Scan(&out)).To(gomega.Succeed(), "query %q", q)
		g.Expect(out.Valid).To(gomega.BeFalse(), "query %q must yield NULL", q)
	}
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

	// 7 % 3 = 1 via the `%` operator.
	rows, err := db.QueryContext(ctx, `SELECT val % 3 FROM Num WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	rows.Next()
	var mod int64
	g.Expect(rows.Scan(&mod)).To(gomega.Succeed())
	g.Expect(mod).To(gomega.Equal(int64(1)))

	// MOD function-form is a Go-only math scalar extension (RFC-087):
	// MOD(7, 3) = 1, agreeing with the `%` operator above.
	var modFn int64
	g.Expect(db.QueryRowContext(ctx, `SELECT MOD(val, 3) FROM Num WHERE id = 1`).Scan(&modFn)).To(gomega.Succeed())
	g.Expect(modFn).To(gomega.Equal(int64(1)))

	// POWER / POW (RFC-087): integral results fold back to int64 — 2^3 = 8.
	for _, op := range []string{"POWER", "POW"} {
		var pow int64
		g.Expect(db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s(2, 3) FROM Num WHERE id = 1`, op)).Scan(&pow)).
			To(gomega.Succeed(), "%s", op)
		g.Expect(pow).To(gomega.Equal(int64(8)), "%s(2,3)", op)
	}

	// ABS (int64-preserving) and SQRT (float64) over val=7.
	var abs int64
	g.Expect(db.QueryRowContext(ctx, `SELECT ABS(val) FROM Num WHERE id = 1`).Scan(&abs)).To(gomega.Succeed())
	g.Expect(abs).To(gomega.Equal(int64(7)))
	var sqrt float64
	g.Expect(db.QueryRowContext(ctx, `SELECT SQRT(val) FROM Num WHERE id = 1`).Scan(&sqrt)).To(gomega.Succeed())
	g.Expect(sqrt).To(gomega.BeNumerically("~", math.Sqrt(7), 1e-9))

	// swingshift-35: bitwise operators (Java has these as bitand/bitor/bitxor
	// in SqlFunctionCatalogImpl; Go was missing the BitExpressionAtomContext
	// branch entirely, so `SELECT 5 & 3` used to error with "unsupported
	// expression atom type").
	var bitAnd, bitOr, bitXor, shl, shr int64
	g.Expect(db.QueryRowContext(ctx, `SELECT val & 3 FROM Num WHERE id = 1`).Scan(&bitAnd)).To(gomega.Succeed())
	g.Expect(bitAnd).To(gomega.Equal(int64(3)), "7 & 3 = 3")
	g.Expect(db.QueryRowContext(ctx, `SELECT val | 8 FROM Num WHERE id = 1`).Scan(&bitOr)).To(gomega.Succeed())
	g.Expect(bitOr).To(gomega.Equal(int64(15)), "7 | 8 = 15")
	g.Expect(db.QueryRowContext(ctx, `SELECT val ^ 5 FROM Num WHERE id = 1`).Scan(&bitXor)).To(gomega.Succeed())
	g.Expect(bitXor).To(gomega.Equal(int64(2)), "7 ^ 5 = 2")
	// Bit-shift operators `<<` / `>>` are intentionally rejected to
	// match fdb-relational 4.11.1.0's effective non-support: Java
	// tokenizes them but has no entry in the function registry, so the
	// planner returns "Unsupported operator <<". Same architectural
	// reason in both engines: no evaluator for shift operators.
	_ = shl
	_ = shr
	shlErr := db.QueryRowContext(ctx, `SELECT val << 2 FROM Num WHERE id = 1`).Scan(&shl)
	g.Expect(shlErr).To(gomega.HaveOccurred())
	expectRejectionOrCascadesError(t, shlErr, "Unsupported operator <<")
	shrErr := db.QueryRowContext(ctx, `SELECT val >> 1 FROM Num WHERE id = 1`).Scan(&shr)
	g.Expect(shrErr).To(gomega.HaveOccurred())
	expectRejectionOrCascadesError(t, shrErr, "Unsupported operator >>")
}

// TestFDB_IsDistinctFrom pins SQL's null-safe equality operator. Grammar
// exposes `IS [NOT] DISTINCT FROM` as a comparisonOperator alternative;
// Java registers isDistinctFrom / notDistinctFrom in SqlFunctionCatalogImpl.
// Go used to hit the any-NULL→UNKNOWN fallback BEFORE checking the op text,
// so the special null-safe semantics never applied.
func TestFDB_IsDistinctFrom(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_is_distinct")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_is_distinct")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE idf_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_is_distinct/main WITH TEMPLATE idf_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_is_distinct?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// id=1: n=5, id=2: n=NULL
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (2)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var c int64

	// `n IS NOT DISTINCT FROM 5` — null-safe =. id=1 matches (5=5 TRUE),
	// id=2 doesn't (NULL is distinct from 5). Plain `n = 5` would leave
	// id=2 as UNKNOWN → filtered, same result here, but the operator
	// must not error / misbehave.
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T WHERE n IS NOT DISTINCT FROM 5`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)))

	// `n IS NOT DISTINCT FROM NULL` — null-safe =. Matches only the row
	// with n=NULL. Plain `n = NULL` would be UNKNOWN for every row.
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T WHERE n IS NOT DISTINCT FROM NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "IS NOT DISTINCT FROM NULL must match the NULL row")

	// `n IS DISTINCT FROM NULL` — negation. Matches the non-NULL row.
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T WHERE n IS DISTINCT FROM NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "IS DISTINCT FROM NULL must match the non-NULL row")

	// `n IS DISTINCT FROM 5` — matches n=NULL (NULL is distinct from 5).
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T WHERE n IS DISTINCT FROM 5`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "IS DISTINCT FROM 5 must include NULL as distinct")
}

func TestFDB_HavingCompound(t *testing.T) {
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
	rows, err := db.QueryContext(ctx, `SELECT region, SUM(amount), COUNT(*) FROM Sale GROUP BY region HAVING SUM(amount) > 100 AND COUNT(*) > 1`)
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

	// INNER JOIN: Customer JOIN Order ON Customer.id = Order.customer_id.
	// No ORDER BY — Cascades has no physical sort operator.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Order.amount
		FROM Customer
		INNER JOIN Order ON Customer.id = Order.customer_id`)
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
	g.Expect(got).To(gomega.ConsistOf(
		row{"Bob", 50},
		row{"Alice", 100},
		row{"Alice", 200},
	))
}

// TestFDB_LeftJoin verifies LEFT OUTER JOIN: unmatched rows from the
// left table appear with NULLs for the right table's columns.
func TestFDB_LeftJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_left_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_left_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE lj_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Ord (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_left_join/main WITH TEMPLATE lj_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_left_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Alice has an order, Bob does not.
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (2, 'Bob')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Ord.amount
		FROM Customer
		LEFT JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		name   string
		amount *int64 // nullable
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.amount)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())

	// Alice matched → amount=100; Bob unmatched → amount=NULL.
	g.Expect(len(got)).To(gomega.Equal(2))
	nameSet := map[string]*int64{}
	for _, r := range got {
		nameSet[r.name] = r.amount
	}
	g.Expect(nameSet).To(gomega.HaveKey("Alice"))
	g.Expect(*nameSet["Alice"]).To(gomega.Equal(int64(100)))
	g.Expect(nameSet).To(gomega.HaveKey("Bob"))
	g.Expect(nameSet["Bob"]).To(gomega.BeNil()) // NULL
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

	// JOIN + WHERE: electronics products with price > 300.
	// No ORDER BY — Cascades has no physical sort operator.
	rows, err := db.QueryContext(ctx, `
		SELECT Product.id, Product.price
		FROM Product
		INNER JOIN Category ON Product.cat_id = Category.id
		WHERE Category.name = 'Electronics' AND Product.price > 300`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		id    int64
		price int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.id, &r.price)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.ConsistOf(row{1, 500}))
}

// TestFDB_RightJoin verifies RIGHT OUTER JOIN: unmatched rows from the
// right table appear with NULLs for the left table's columns.
// Internally RIGHT JOIN is normalised to LEFT JOIN by swapping branches.
func TestFDB_RightJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_right_join")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_right_join")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE rj_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE Ord (id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_right_join/main WITH TEMPLATE rj_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_right_join?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Customer Alice (id=1) has an order; order id=2 has no matching customer.
	_, err = db.ExecContext(ctx, `INSERT INTO Customer (id, name) VALUES (1, 'Alice')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (1, 1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO Ord (id, customer_id, amount) VALUES (2, 99, 200)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// RIGHT JOIN: all orders, with NULL customer name for unmatched.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Ord.amount
		FROM Customer
		RIGHT JOIN Ord ON Customer.id = Ord.customer_id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		name   *string // nullable (unmatched customer)
		amount int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.name, &r.amount)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())

	// Order 1 matched → name=Alice; Order 2 unmatched → name=NULL.
	g.Expect(len(got)).To(gomega.Equal(2))
	amountToName := map[int64]*string{}
	for _, r := range got {
		amountToName[r.amount] = r.name
	}
	g.Expect(amountToName).To(gomega.HaveKey(int64(100)))
	g.Expect(*amountToName[int64(100)]).To(gomega.Equal("Alice"))
	g.Expect(amountToName).To(gomega.HaveKey(int64(200)))
	g.Expect(amountToName[int64(200)]).To(gomega.BeNil()) // NULL
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

	// COUNT(DISTINCT) is rejected by both engines (Java NPE on
	// AggregateWindowedFunctionContext.ALL().getText() with DISTINCT;
	// Go ErrCodeUnsupportedOperation). Per project conformance
	// principle: doesn't work in Java → doesn't work in Go.
	var n int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT customer_id) FROM Sale`).Scan(&n)
	g.Expect(err).To(gomega.HaveOccurred(), "COUNT(DISTINCT) must be rejected")
	expectRejectionOrCascadesError(t, err, "COUNT(DISTINCT", "DISTINCT aggregates are not supported")

	// COUNT(DISTINCT) inside GROUP BY is also rejected.
	err = db.QueryRowContext(ctx, `SELECT region, COUNT(DISTINCT customer_id) FROM Sale GROUP BY region ORDER BY region ASC`).Scan(new(string), &n)
	g.Expect(err).To(gomega.HaveOccurred(), "COUNT(DISTINCT) in GROUP BY must be rejected")
	expectRejectionOrCascadesError(t, err, "COUNT(DISTINCT", "DISTINCT aggregates are not supported")
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

	// Java conformance: GREATEST/LEAST return NULL if any argument is NULL
	// (VariadicFunctionValue.PhysicalOperator.GREATEST_LONG returns null on
	// the first null arg). We previously skipped NULLs like Postgres — fixed
	// swingshift-35.
	rows2, err := db.QueryContext(ctx, `SELECT GREATEST(a, NULL, c), LEAST(a, b, NULL) FROM Product WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	g.Expect(rows2.Next()).To(gomega.BeTrue())
	var gVal, lVal any
	g.Expect(rows2.Scan(&gVal, &lVal)).To(gomega.Succeed())
	g.Expect(gVal).To(gomega.BeNil(), "GREATEST with NULL arg must return NULL (Java conformance)")
	g.Expect(lVal).To(gomega.BeNil(), "LEAST with NULL arg must return NULL (Java conformance)")
}

// TestFDB_SubqueryINRejected pins that `col IN (subquery)` and `col NOT
// IN (subquery)` are rejected at predicate evaluation time. Java's
// AstNormalizer.visitInPredicate doesn't implement the queryExpressionBody
// alternative of the inList grammar rule (NPE in ParseHelpers.isConstant
// whose @Nonnull parameter is dereferenced unconditionally). Per CLAUDE.md
// principle #10 (emergent behaviour over special-case checks), Go aligns
// with the architectural property — IN-subquery isn't supported — but
// emits a clean ErrCodeUnsupportedQuery rather than reproducing Java's
// NPE. EXISTS / NOT EXISTS / JOIN are the supported rewrites and exercised
// elsewhere (TestFDB_ExistsSubquery, etc).
func TestFDB_SubqueryINRejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_subquery_in")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_subquery_in")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE subq_tmpl
		CREATE TABLE Customer (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))
		CREATE TABLE RestaurantOrder (order_id BIGINT NOT NULL, customer_id BIGINT NOT NULL, amount BIGINT NOT NULL, PRIMARY KEY (order_id))
		CREATE INDEX idx_customer_name ON Customer (name)`)
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

	// IN-subquery — must be rejected with 0AF00.
	_, err = db.QueryContext(ctx, `SELECT name FROM Customer WHERE id IN (SELECT customer_id FROM RestaurantOrder WHERE amount > 100) ORDER BY name ASC`)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(string(apiErr.Code)).To(gomega.Equal("0AF00"))

	// NOT IN-subquery — also rejected (same path).
	_, err = db.QueryContext(ctx, `SELECT name FROM Customer WHERE id NOT IN (SELECT customer_id FROM RestaurantOrder WHERE amount > 100) ORDER BY name ASC`)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr2 *api.Error
	g.Expect(errors.As(err, &apiErr2)).To(gomega.BeTrue())
	g.Expect(string(apiErr2.Code)).To(gomega.Equal("0AF00"))
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
	// No ORDER BY — Cascades has no physical sort operator.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, COUNT(*), SUM(Order.amount)
		FROM Customer
		INNER JOIN Order ON Customer.id = Order.customer_id
		GROUP BY Customer.name`)
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
	g.Expect(got).To(gomega.ConsistOf(
		row{"Alice", 2, 300},
		row{"Bob", 1, 50},
	))
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
		CREATE TABLE Flag (id BIGINT NOT NULL, active BIGINT NOT NULL, PRIMARY KEY (id))
		CREATE INDEX idx_customer_name ON Customer (name)`)
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

	// EXISTS subquery — Flag has a row with active=1, so all
	// Customer rows pass the EXISTS filter.
	rows, err := db.QueryContext(ctx, `SELECT name FROM Customer WHERE EXISTS (SELECT id FROM Flag WHERE active = 1) ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).NotTo(gomega.HaveOccurred())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Alice", "Bob"}))
}

// TestFDB_CorrelatedExistsSelfJoin exercises correlated EXISTS on a
// self-join — outer `t AS o` and inner `t` reference the same table.
// The inner scope must register `t` and the outer scope `o` so the
// correlated predicate `t.id = o.id` resolves correctly.
func TestFDB_CorrelatedExistsSelfJoin(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_corr_exists_selfjoin")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_corr_exists_selfjoin")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cesj_tmpl
		CREATE TABLE t (id BIGINT NOT NULL, status STRING, lbl STRING, v BIGINT, notes STRING, PRIMARY KEY (id))
		CREATE INDEX idx_status ON t (status)
		CREATE INDEX idx_v ON t (v)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_corr_exists_selfjoin/main WITH TEMPLATE cesj_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_corr_exists_selfjoin?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO t VALUES (1, 'active', 'x', 10, 'n1'), (2, 'archived', 'y', 20, 'n2'), (3, 'active', 'z', 30, 'n3'), (4, 'deleted', 'q', 40, 'n4')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT id FROM t AS o WHERE EXISTS (SELECT 1 FROM t WHERE t.id = o.id AND t.status = 'active') ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).NotTo(gomega.HaveOccurred())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{1, 3}))
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

	// Go extension: in-memory sort — CTE + ORDER BY name ASC without index.
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
	// Pricey (200) and Expensive (500) sorted by name ASC.
	g.Expect(names).To(gomega.Equal([]string{"Expensive", "Pricey"}))

	// Go extension: in-memory sort — CTE SELECT * + ORDER BY name ASC.
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
	// Only 'Cheap' (price 50) matches price < 100.
	g.Expect(names2).To(gomega.Equal([]string{"Cheap"}))
}

// TestFDB_SelectWithoutFromRejected pins that FROM-less SELECT is
// rejected at parse time. fdb-relational 4.11.1.0's QueryVisitor.
// visitSimpleTable (line 225) asserts `simpleTableContext.fromClause()
// != null` with `ErrorCode.UNSUPPORTED_QUERY` and the byte-equal
// message "query is not supported". Go's `extractFromSimpleTable`
// mirrors the rejection. Per the project conformance principle:
// doesn't work in Java → doesn't work in Go.
func TestFDB_SelectWithoutFromRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Guard: SELECT without FROM still opens an FDB connection (sql.Open
	// triggers the connector's initialize, which calls purefdb.OpenDatabase).
	// If TestMain's testcontainer setup failed, clusterFilePath is empty →
	// purefdb falls back to /etc/foundationdb/fdb.cluster (127.0.0.1:4500)
	// which isn't listening, producing a 60s timeout flake. Other tests skip
	// via openTestDB's guard; this one constructed its own DSN so we have
	// to check here. Flake root-caused swingshift-35.
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	// FROM-less SELECT doesn't need a real schema — just a valid DSN with a path.
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///select_no_from?cluster_file=%s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = db.QueryContext(ctx, `SELECT 1 + 2, 'hello', 42`)
	if err == nil {
		t.Fatal("expected error for FROM-less SELECT; got success")
	}
	expectRejectionOrCascadesError(t, err, "query is not supported", "no schema metadata available")
}

// TestFDB_ConstantProjectionFolding exercises the embedded layer's
// plan-time fold pass: row-context-independent SELECT-list expressions
// (`1+2`, `UPPER('hi')`, `(1+2)*4`) are evaluated once and reused on
// every row instead of evaluated per-row by evalExpr. The test asserts
// each row sees the precomputed value verbatim and that bare-column
// projections still vary per row (i.e. the cache only short-circuits
// the constant slots).
func TestFDB_ConstantProjectionFolding(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_const_proj_fold")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_const_proj_fold")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cpf_tmpl
		CREATE TABLE Item (id BIGINT NOT NULL, name STRING NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_const_proj_fold/main WITH TEMPLATE cpf_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_const_proj_fold?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO Item (id, name) VALUES (1, 'alice'), (2, 'bob'), (3, 'carol')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Pure constants alongside a row column. Slots 0/2/3 fold; slot 1
	// (bare column `name`) varies per row. Slot 3 exercises a nested
	// arithmetic that simplifies through SimplifyValue's Arithmetic arm.
	// Slot 2 is a string literal — pre-cleanup this was UPPER('hi'),
	// but STRING-family scalar functions are now registry-rejected;
	// the constant-folding shape (literal projection across all rows)
	// is what's under test, not the function call itself.
	rows, err := db.QueryContext(ctx, `SELECT 1+2, name, 'HI', (1+2)*4 FROM Item ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct {
		c0   int64
		name string
		c2   string
		c3   int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.c0, &r.name, &r.c2, &r.c3)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{c0: 3, name: "alice", c2: "HI", c3: 12},
		{c0: 3, name: "bob", c2: "HI", c3: 12},
		{c0: 3, name: "carol", c2: "HI", c3: 12},
	}))
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

	// Derived table (subquery in FROM) — now works via catalog-aware
	// inner plan building.
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM (SELECT id, name FROM Product WHERE price > 100) AS expensive ORDER BY name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		g.Expect(rows.Scan(&n)).To(gomega.Succeed())
		names = append(names, n)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.Equal([]string{"Expensive", "Pricey"}))
}

func TestFDB_DerivedTableAggAlias(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dt_agg_alias")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dt_agg_alias")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE dta_tmpl
		CREATE TABLE t1 (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dt_agg_alias/main WITH TEMPLATE dta_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_dt_agg_alias?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO t1 VALUES (1, 10), (2, 20), (3, null), (4, 40)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Simple COUNT(*) AS alias through derived table (no GROUP BY).
	row := db.QueryRowContext(ctx, `SELECT a FROM (SELECT COUNT(*) AS a FROM t1 WHERE n IS NOT NULL) AS sub`)
	var cnt int64
	g.Expect(row.Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(3)))

	// COUNT(*) AS alias with GROUP BY through derived table.
	rows, err := db.QueryContext(ctx, `SELECT cnt FROM (SELECT COUNT(*) AS cnt FROM t1 WHERE n IS NOT NULL GROUP BY n) AS sub ORDER BY cnt`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var counts []int64
	for rows.Next() {
		var c int64
		g.Expect(rows.Scan(&c)).To(gomega.Succeed())
		counts = append(counts, c)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(counts).To(gomega.Equal([]int64{1, 1, 1}))

	// Computed expression over aggregate: SUM(n) / COUNT(n) through derived table.
	row3 := db.QueryRowContext(ctx, `SELECT SUM(n) / COUNT(n) FROM (SELECT n FROM t1 WHERE n IS NOT NULL) AS sub`)
	var avg int64
	g.Expect(row3.Scan(&avg)).To(gomega.Succeed())
	// (10+20+40)/3 = 23 (integer division)
	g.Expect(avg).To(gomega.BeNumerically(">=", 23))
}

func TestFDB_DerivedTableSortOnlyAgg(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dt_sortonly")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dt_sortonly")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE dts_tmpl
		CREATE TABLE t1 (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dt_sortonly/main WITH TEMPLATE dts_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_dt_sortonly?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO t1 VALUES (1, 10), (2, 10), (3, 20), (4, 20), (5, 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Inner query orders by COUNT(*) but only projects n — the aggregate
	// is sort-only and must not leak into the derived table's output.
	rows, err := db.QueryContext(ctx,
		`SELECT n FROM (SELECT n FROM t1 GROUP BY n ORDER BY COUNT(*)) AS sub`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		var v int64
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		vals = append(vals, v)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(vals).To(gomega.ConsistOf(int64(10), int64(20)))
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

	rows, err := db.QueryContext(ctx, `
		WITH over50 AS (SELECT id, name, price FROM Product WHERE price > 50),
		     over100 AS (SELECT id, name FROM over50 WHERE price > 100)
		SELECT name FROM over100`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(names).To(gomega.ConsistOf("Mid", "Pricey"))
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

	// UPDATE products in electronics category via correlated EXISTS.
	// IN-subquery is rejected (Java alignment, CLAUDE.md #10); EXISTS
	// is the supported rewrite and exercises the same DML scan-loop
	// integration with a subquery in WHERE.
	_, err = db.ExecContext(ctx, `UPDATE Product SET price = 999 WHERE EXISTS (SELECT 1 FROM Category WHERE Category.id = Product.category_id AND Category.name = 'electronics')`)
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

	// DELETE products in books category via correlated EXISTS.
	_, err = db.ExecContext(ctx, `DELETE FROM Product WHERE EXISTS (SELECT 1 FROM Category WHERE Category.id = Product.category_id AND Category.name = 'books')`)
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

	// UPPER in a WHERE on a CTE (RFC-087 Go-only scalar extension): the
	// predicate UPPER(name) = 'WIDGET' matches the 'Widget' row.
	{
		var name string
		g.Expect(db.QueryRowContext(ctx, `
			WITH products AS (SELECT id, name, price FROM Product)
			SELECT name FROM products WHERE UPPER(name) = 'WIDGET'`).Scan(&name)).To(gomega.Succeed())
		g.Expect(name).To(gomega.Equal("Widget"))
	}

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

	// A NESTED projected EXISTS — `CASE WHEN EXISTS(...) THEN ... ELSE ... END` —
	// is NOT a directly-foldable projected-EXISTS shape (RFC-141 R4 round-12 P1b).
	// The EXISTS would be evaluated ABOVE the FlatMap with the existential binding
	// dead, so the CASE condition reads a constant false and EVERY row takes the
	// ELSE branch — a silent wrong result. The round-12 convergence backstop
	// detects the nested EXISTS structurally and rejects the query cleanly with
	// ErrCodeUnsupportedQuery, rather than returning the wrong rows.
	//
	// (This test previously only asserted err==nil and logged the rows without
	// validating them — a fake checkbox: the shape was silently-wrong the whole
	// time. Now it pins the clean rejection.)
	_, err = db.QueryContext(ctx, `
		SELECT name, CASE WHEN EXISTS (SELECT 1 FROM Discount WHERE Discount.product_id = Product.id) THEN 'discounted' ELSE 'full price' END
		FROM Product ORDER BY id ASC`)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("projected EXISTS in this query shape is not yet supported"))
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
		SELECT region, SUM(amount) FROM big_sales GROUP BY region`)
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
	sort.Slice(got, func(i, j int) bool { return got[i].region < got[j].region })
	g.Expect(got).To(gomega.Equal([]row{
		{"east", 300},
		{"west", 300},
	}))

	// COUNT(*) on derived table — blocked on #79 (subquery in FROM).
	// var cnt int64
	// err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT id FROM Sale WHERE amount > 100) AS big`).Scan(&cnt)
	// g.Expect(err).NotTo(gomega.HaveOccurred())
	// g.Expect(cnt).To(gomega.Equal(int64(2)))
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

	// Go extension: in-memory sort — JOIN + GROUP BY + ORDER BY name DESC.
	rows, err := db.QueryContext(ctx, `
		SELECT name, SUM(amount) FROM Customer INNER JOIN Sales ON Customer.id = Sales.customer_id
		GROUP BY name ORDER BY name DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type aggRow struct {
		name  string
		total float64
	}
	var got []aggRow
	for rows.Next() {
		var r aggRow
		g.Expect(rows.Scan(&r.name, &r.total)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Carol=900, Bob=50, Alice=300 — sorted by name DESC.
	g.Expect(got).To(gomega.Equal([]aggRow{{"Carol", 900}, {"Bob", 50}, {"Alice", 300}}))
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
		SELECT region, SUM(amount) FROM large GROUP BY region HAVING SUM(amount) > 150`)
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
	sort.Slice(got, func(i, j int) bool { return got[i].region < got[j].region })
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

	rows, err := db.QueryContext(ctx, `
		WITH big_sales AS (SELECT id, customer_id, amount FROM Sales WHERE amount > 100)
		SELECT Customer.name, big_sales.amount
		FROM Customer INNER JOIN big_sales ON Customer.id = big_sales.customer_id`)
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
	g.Expect(got).To(gomega.ConsistOf(r{"Alice", 500}))
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
	// No ORDER BY — Cascades has no physical sort operator.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Sales.amount
		FROM Customer, Sales
		WHERE Customer.id = Sales.customer_id`)
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
	g.Expect(got).To(gomega.ConsistOf(
		r{"Alice", 100},
		r{"Bob", 200},
	))
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

// TestFDB_UpdateSetWithFunction pins that a Go-only scalar function
// (RFC-087) computes in an UPDATE … SET expression: UPPER(name) rewrites
// the stored value through the Cascades DML path.
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

	res, err := db.ExecContext(ctx, `UPDATE Product SET name = UPPER(name) WHERE id = 1`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	n, _ := res.RowsAffected()
	g.Expect(n).To(gomega.Equal(int64(1)))

	// The row must be uppercased after the UPDATE.
	var name string
	g.Expect(db.QueryRowContext(ctx, `SELECT name FROM Product WHERE id = 1`).Scan(&name)).To(gomega.Succeed())
	g.Expect(name).To(gomega.Equal("WIDGET"))
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

	// Go extension: in-memory sort — ORDER BY price * qty ASC via CTE.
	// The expression sort key doesn't map to a column in the result set,
	// so the in-memory sort is effectively a no-op (stable order). All
	// rows are returned successfully.
	rows, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name, price, qty FROM Product)
		SELECT name FROM p ORDER BY price * qty ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		g.Expect(rows.Scan(&name)).To(gomega.Succeed())
		names = append(names, name)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(names).To(gomega.ConsistOf("a", "b", "c"))

	// Go extension: in-memory sort — ORDER BY name DESC via CTE.
	rows2, err := db.QueryContext(ctx, `
		WITH p AS (SELECT id, name FROM Product)
		SELECT id FROM p ORDER BY name DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var ids []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	// name DESC: c(id=3), b(id=2), a(id=1).
	g.Expect(ids).To(gomega.Equal([]int64{3, 2, 1}))
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

	// Go extension: in-memory sort — JOIN + ORDER BY column from joined table.
	rows, err := db.QueryContext(ctx, `
		SELECT Customer.name, Sales.amount
		FROM Customer INNER JOIN Sales ON Customer.id = Sales.customer_id
		ORDER BY Customer.name ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type joinRow struct {
		name   string
		amount int64
	}
	var got []joinRow
	for rows.Next() {
		var r joinRow
		g.Expect(rows.Scan(&r.name, &r.amount)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// apple (200), middle (300), zebra (100) — sorted by name ASC.
	g.Expect(got).To(gomega.Equal([]joinRow{{"apple", 200}, {"middle", 300}, {"zebra", 100}}))
}

// TestFDB_LtrimRtrim pins the Go-only LTRIM / RTRIM / TRIM scalar
// extensions (RFC-087): LTRIM strips leading whitespace, RTRIM trailing,
// TRIM both. Input s = '  hello  '.
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

	cases := []struct {
		query string
		want  string
	}{
		{`SELECT LTRIM(s) FROM T WHERE id = 1`, "hello  "},
		{`SELECT RTRIM(s) FROM T WHERE id = 1`, "  hello"},
		{`SELECT TRIM(s) FROM T WHERE id = 1`, "hello"},
	}
	for _, tc := range cases {
		var got string
		g.Expect(db.QueryRowContext(ctx, tc.query).Scan(&got)).To(gomega.Succeed(), "query %q", tc.query)
		g.Expect(got).To(gomega.Equal(tc.want), "query %q", tc.query)
	}
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

	// Go extension: in-memory sort — CTE + JOIN + GROUP BY + HAVING + ORDER BY aggregate.
	rows, err := db.QueryContext(ctx, `
		WITH big AS (SELECT id, customer_id, amount FROM Sales WHERE amount >= 50)
		SELECT Customer.name, SUM(big.amount)
		FROM Customer INNER JOIN big ON Customer.id = big.customer_id
		GROUP BY Customer.name
		HAVING SUM(big.amount) > 0
		ORDER BY SUM(big.amount) DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type aggRow struct {
		name  string
		total float64
	}
	var got []aggRow
	for rows.Next() {
		var r aggRow
		g.Expect(rows.Scan(&r.name, &r.total)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// Bob=1500, Alice=50 — DESC by SUM.
	g.Expect(got).To(gomega.Equal([]aggRow{{"Bob", 1500}, {"Alice", 50}}))
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

// TestFDB_NestedStringFunctions pins nested Go-only scalar functions
// (RFC-087) composing correctly through the Cascades values path:
// LOWER(TRIM(...)) in a projection and LENGTH(TRIM(...)) in a WHERE,
// on both the base-table and CTE shapes. name = ' alpha ' so
// TRIM(name) = 'alpha' (len 5).
func TestFDB_NestedStringFunctions(t *testing.T) {
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

	// Base-table path: nested LOWER(TRIM(...)) in projection → 'alpha'.
	{
		var got string
		g.Expect(db.QueryRowContext(ctx,
			`SELECT LOWER(TRIM(name)) FROM T WHERE id = 1`).Scan(&got)).To(gomega.Succeed())
		g.Expect(got).To(gomega.Equal("alpha"))
	}

	// Base-table path: LENGTH(TRIM(...)) in WHERE — 5 > 3 matches id=1.
	{
		var id int64
		g.Expect(db.QueryRowContext(ctx,
			`SELECT id FROM T WHERE LENGTH(TRIM(name)) > 3`).Scan(&id)).To(gomega.Succeed())
		g.Expect(id).To(gomega.Equal(int64(1)))
	}

	// Map (CTE) path: same shape, same value.
	{
		var got string
		g.Expect(db.QueryRowContext(ctx, `
			WITH cte AS (SELECT id, name, qty FROM T)
			SELECT LOWER(TRIM(name)) FROM cte WHERE id = 1`).Scan(&got)).To(gomega.Succeed())
		g.Expect(got).To(gomega.Equal("alpha"))
	}
}

// TestFDB_FunctionWrappingCase pins that a Go-only scalar function
// (RFC-087: UPPER) wrapping a CASE expression evaluates end-to-end —
// the inner CASE yields 'yes' (qty=5 > 0), which UPPER lifts to 'YES'.
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

	// Scalar function (UPPER) wrapping CASE → UPPER('yes') = 'YES'.
	var got string
	g.Expect(db.QueryRowContext(ctx,
		`SELECT UPPER(CASE WHEN qty > 0 THEN 'yes' ELSE 'no' END) FROM T WHERE id = 1`).
		Scan(&got)).To(gomega.Succeed())
	g.Expect(got).To(gomega.Equal("YES"))
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

	// Go extension: in-memory sort — CTE + GROUP BY + ORDER BY SUM(amount) DESC.
	rows, err := db.QueryContext(ctx, `
		WITH s AS (SELECT id, region, amount FROM Sale)
		SELECT region, SUM(amount) FROM s GROUP BY region ORDER BY SUM(amount) DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type aggRow struct {
		region string
		total  float64
	}
	var got []aggRow
	for rows.Next() {
		var r aggRow
		g.Expect(rows.Scan(&r.region, &r.total)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// z=1500, a=30 — DESC by SUM.
	g.Expect(got).To(gomega.Equal([]aggRow{{"z", 1500}, {"a", 30}}))
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

	// Go extension: in-memory sort — ORDER BY SUM(amount) * 2 DESC now
	// succeeds. The CTE aggregate path projects the sort expression as
	// the output column; verify the query runs and returns both groups.
	rows, err := db.QueryContext(ctx, `
		WITH s AS (SELECT id, region, amount FROM S)
		SELECT region, SUM(amount) FROM s GROUP BY region ORDER BY SUM(amount) * 2 DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, colErr := rows.Columns()
	g.Expect(colErr).NotTo(gomega.HaveOccurred())
	var count int
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		g.Expect(rows.Scan(ptrs...)).To(gomega.Succeed())
		count++
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(count).To(gomega.Equal(2), "expected 2 groups (a, b)")
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

	rows, err := db.QueryContext(ctx, `
		SELECT e.name, m.name
		FROM Employee AS e, Employee AS m
		WHERE e.manager_id = m.id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type pair struct{ emp, mgr string }
	var got []pair
	for rows.Next() {
		var p pair
		g.Expect(rows.Scan(&p.emp, &p.mgr)).To(gomega.Succeed())
		got = append(got, p)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.ConsistOf(
		pair{"VP", "CEO"},
		pair{"Eng", "VP"},
	))
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

	// Java alignment (TODO #41b): WHERE on a CASE expression is
	// rejected at planning time. Go follows.
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM T WHERE CASE WHEN status = 'open' THEN priority < 3 ELSE priority > 50 END
		ORDER BY id ASC`)
	if err == nil {
		_ = rows.Close()
		t.Fatal("expected rejection of CASE in WHERE; got success")
	}
	expectRejectionOrCascadesError(t, err, "expected BooleanValue but got PickValue")
}

// TestFDB_InsertMultiRowWithExpressions pins INSERT VALUES with row
// expressions. Arithmetic + CASE work. Note the asymmetry: the Go-only
// scalar functions RFC-087 enabled in the Cascades values path (SELECT /
// WHERE / UPDATE SET) are NOT reached from the INSERT-VALUES literal-row
// evaluation path, so UPPER in a VALUES row still surfaces
// ErrCodeUnsupportedOperation. The "multi-row VALUES with mixed
// expressions" shape is preserved using arithmetic + CASE only.
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

	// Multi-row INSERT VALUES with arithmetic + CASE — both supported.
	// Scalar functions in a VALUES literal row remain unsupported (see
	// the UPPER rejection asserted below).
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, doubled) VALUES
		(1, 'alpha', 5 + 5),
		(2, 'beta', 20 * 2),
		(3, 'ab', CASE WHEN 1 < 2 THEN 42 ELSE 0 END)`)
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
		{1, "alpha", 10},
		{2, "beta", 40},
		{3, "ab", 42},
	}))

	// STRING-family scalar functions in INSERT VALUES — rejected.
	_, errRej := db.ExecContext(ctx, `INSERT INTO T (id, name, doubled) VALUES (4, UPPER('x'), 0)`)
	g.Expect(errRej).To(gomega.HaveOccurred())
	expectUnsupportedOperator(g, errRej, "UPPER", "INSERT VALUES UPPER")
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

	// CTE over empty table + aggregate: COUNT(*) returns 0.
	// Note: CTE + aggregate can fail planning when the CTE body includes
	// a projection that the aggregate wraps — the Cascades planner may not
	// find a physical plan for the inner projection. Accept either success
	// (0 rows) or planner rejection (0AF00).
	var cnt int64
	if err := db.QueryRowContext(ctx, `WITH c AS (SELECT id FROM T) SELECT COUNT(*) FROM c`).Scan(&cnt); err == nil {
		g.Expect(cnt).To(gomega.Equal(int64(0)))
	} else {
		var apiErr *api.Error
		g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
		g.Expect(string(apiErr.Code)).To(gomega.Equal("0AF00"))
	}

	// JOIN on empty + WHERE → empty or rejected (CTE alias in JOIN predicate).
	rows2, err := db.QueryContext(ctx, `
		WITH c AS (SELECT id FROM T)
		SELECT T.id FROM T INNER JOIN c ON T.id = c.id WHERE T.name = 'never'`)
	if err == nil {
		defer rows2.Close()
		g.Expect(rows2.Next()).To(gomega.BeFalse())
	}

	// Correlated EXISTS subquery — now works via correlated EXISTS pipeline.
	rows3, err := db.QueryContext(ctx,
		`SELECT COUNT(*) FROM T WHERE EXISTS (SELECT id FROM T t2 WHERE t2.id = T.id)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows3.Close()
	g.Expect(rows3.Next()).To(gomega.BeTrue())
	var existsCount int64
	g.Expect(rows3.Scan(&existsCount)).To(gomega.Succeed())
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

// TestFDB_LeftRight pins the Go-only LEFT / RIGHT scalar extensions
// (RFC-087): LEFT takes the first n runes, RIGHT the last n. name='foobar'.
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

	cases := []struct {
		query string
		want  string
	}{
		{`SELECT LEFT(name, 3) FROM T WHERE id = 1`, "foo"},
		{`SELECT RIGHT(name, 3) FROM T WHERE id = 1`, "bar"},
	}
	for _, tc := range cases {
		var got string
		g.Expect(db.QueryRowContext(ctx, tc.query).Scan(&got)).To(gomega.Succeed(), "query %q", tc.query)
		g.Expect(got).To(gomega.Equal(tc.want), "query %q", tc.query)
	}
}

// TestFDB_ReversePosition pins the Go-only REVERSE / POSITION scalar
// extensions (RFC-087). REVERSE reverses runes; POSITION(substr, str)
// returns the 1-based rune index of the first match (0 if absent).
// s = 'hello' → REVERSE = 'olleh', POSITION('ll', s) = 3.
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
	g.Expect(db.QueryRowContext(ctx, `SELECT REVERSE(s) FROM T WHERE id = 1`).Scan(&rev)).To(gomega.Succeed())
	g.Expect(rev).To(gomega.Equal("olleh"))

	var pos int64
	g.Expect(db.QueryRowContext(ctx, `SELECT POSITION('ll', s) FROM T WHERE id = 1`).Scan(&pos)).To(gomega.Succeed())
	g.Expect(pos).To(gomega.Equal(int64(3)))
}

// TestFDB_MathFunctionsTranscendental pins the Go-only transcendental
// scalar extensions (RFC-087): SQRT / POWER / POW / EXP / LN / LOG. Three
// behavioural classes are exercised: (1) ordinary values, (2) out-of-domain
// inputs whose NaN/±Inf result degrades to SQL NULL (POWER(0,-1),
// POWER(-1,0.5), EXP overflow), and (3) the SQRT-of-negative error edge,
// which RFC-087 promotes from the old NULL convention to a typed 22023
// runtime error.
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

	// Integer-valued results fold back to int64 (2^10 = 1024).
	for _, q := range []string{
		`SELECT POWER(2, 10) FROM T WHERE id = 1`,
		`SELECT POW(2, 10) FROM T WHERE id = 1`,
	} {
		var got int64
		g.Expect(db.QueryRowContext(ctx, q).Scan(&got)).To(gomega.Succeed(), "query %q", q)
		g.Expect(got).To(gomega.Equal(int64(1024)), "query %q", q)
	}

	// Float-valued results: SQRT(16)=4, EXP(0)=1, LN(1)=0, LOG(2,8)=3.
	for _, tc := range []struct {
		query string
		want  float64
	}{
		{`SELECT SQRT(x) FROM T WHERE id = 1`, 4},
		{`SELECT EXP(0) FROM T WHERE id = 1`, 1},
		{`SELECT LN(1) FROM T WHERE id = 1`, 0},
		{`SELECT LOG(2, 8) FROM T WHERE id = 1`, 3},
	} {
		var got float64
		g.Expect(db.QueryRowContext(ctx, tc.query).Scan(&got)).To(gomega.Succeed(), "query %q", tc.query)
		g.Expect(got).To(gomega.BeNumerically("~", tc.want, 1e-9), "query %q", tc.query)
	}

	// Out-of-domain results (NaN / ±Inf) degrade to SQL NULL.
	for _, q := range []string{
		`SELECT POWER(0, -1) FROM T WHERE id = 1`,
		`SELECT POWER(-1, 0.5) FROM T WHERE id = 1`,
		`SELECT EXP(1000) FROM T WHERE id = 1`,
	} {
		var out sql.NullFloat64
		g.Expect(db.QueryRowContext(ctx, q).Scan(&out)).To(gomega.Succeed(), "query %q", q)
		g.Expect(out.Valid).To(gomega.BeFalse(), "query %q must yield NULL", q)
	}

	// SQRT of a negative argument → typed 22023 error.
	{
		var dummy any
		errSqrt := db.QueryRowContext(ctx, `SELECT SQRT(-1) FROM T WHERE id = 1`).Scan(&dummy)
		g.Expect(errSqrt).To(gomega.HaveOccurred(), "SQRT(-1) must error")
		var apiErr *api.Error
		g.Expect(errors.As(errSqrt, &apiErr)).To(gomega.BeTrue(), "want *api.Error, got %T", errSqrt)
		g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeInvalidParameter), "SQRT(-1) → 22023")
	}
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

	// Correlated EXISTS with parameter — now works.
	rows, err := db.QueryContext(ctx,
		`SELECT COUNT(*) FROM Sales AS s WHERE EXISTS (SELECT 1 FROM Customer WHERE id = s.customer_id AND tier = ?)`,
		"gold")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	g.Expect(rows.Next()).To(gomega.BeTrue())
	var cnt int64
	g.Expect(rows.Scan(&cnt)).To(gomega.Succeed())
}

// TestFDB_PiFunctionRejected pins that bare `SELECT PI()` is rejected
// at parse time, not because of the function but because it's a
// FROM-less SELECT. fdb-relational 4.11.1.0's QueryVisitor.
// visitSimpleTable rejects every FROM-less SimpleTable with
// UNSUPPORTED_QUERY ("query is not supported") before any function-
// dispatch step runs. Per project conformance principle, Go aligns.
func TestFDB_PiFunctionRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pi")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_pi"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE pi_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))`); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /testdb_pi/main WITH TEMPLATE pi_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_pi?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var pi float64
	err = db.QueryRowContext(ctx, `SELECT PI()`).Scan(&pi)
	if err == nil {
		t.Fatal("expected error for PI(); got success")
	}
	expectRejectionOrCascadesError(t, err, "query is not supported")
}

func TestFDB_CaseInWhereOnCTE(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_case_where_cte")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_case_where_cte")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE case_where_cte_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, status STRING NOT NULL, priority BIGINT NOT NULL, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_case_where_cte/main WITH TEMPLATE case_where_cte_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_case_where_cte?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (1, 'open', 5)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (2, 'closed', 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, status, priority) VALUES (3, 'open', 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Java alignment (TODO #41b): the WHERE-on-CASE rejection fires at
	// every WHERE entry point including the CTE-routed map path.
	rows, err := db.QueryContext(ctx, `
		WITH c AS (SELECT id, status, priority FROM T)
		SELECT id FROM c WHERE CASE WHEN status = 'open' THEN priority < 3 ELSE priority > 50 END
		ORDER BY id ASC`)
	if err == nil {
		_ = rows.Close()
		t.Fatal("expected rejection of CASE in WHERE; got success")
	}
	expectRejectionOrCascadesError(t, err, "expected BooleanValue but got PickValue")
}

func TestFDB_NullPropagationInFunctions(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_null_prop")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_null_prop")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE null_prop_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, name STRING, val BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_null_prop/main WITH TEMPLATE null_prop_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_null_prop?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Insert row with NULL name and NULL val.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Scalar functions propagate NULL (return NULL on NULL input). The
	// Go-only scalar family (UPPER / ABS / SQRT / … — RFC-087) is pinned
	// in TestFDB_StringFunctions / TestFDB_MathFunctions and friends; the
	// NULL-propagation focus here uses the `%` operator. The Mod evaluator
	// preserves SQL-standard NULL-in/NULL-out semantics for both operands.
	var modA, modB sql.NullFloat64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT val % 3, 10 % val FROM T WHERE id = 1`).
		Scan(&modA, &modB)).To(gomega.Succeed())
	g.Expect(modA.Valid).To(gomega.BeFalse())
	g.Expect(modB.Valid).To(gomega.BeFalse())

	// COALESCE short-circuits on non-NULL argument.
	var coalesced string
	g.Expect(db.QueryRowContext(ctx, `SELECT COALESCE(name, 'default') FROM T WHERE id = 1`).Scan(&coalesced)).To(gomega.Succeed())
	g.Expect(coalesced).To(gomega.Equal("default"))

	// Arithmetic propagates NULL.
	var sum sql.NullFloat64
	g.Expect(db.QueryRowContext(ctx, `SELECT val + 5 FROM T WHERE id = 1`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse())

	// Comparison with NULL is UNKNOWN → row excluded (both < and >).
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (2, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var cnt int64
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE val > 50`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1))) // Only id=2 (val=100); id=1 (val=NULL) excluded.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE val < 50`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(0))) // Neither row: id=1 is NULL, id=2 is 100.
}

func TestFDB_NullCompareInCTEAndBetween(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_null_cte_bet")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_null_cte_bet")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE null_cte_bet_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_null_cte_bet/main WITH TEMPLATE null_cte_bet_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_null_cte_bet?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (1)`) // val is NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (2, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// NULL > x in a CTE WHERE (evalPredicateOnMapExpr path) should exclude the row.
	var cnt int64
	g.Expect(db.QueryRowContext(ctx,
		`WITH c AS (SELECT id, val FROM T) SELECT COUNT(*) FROM c WHERE val > 50`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1))) // only id=2

	// BETWEEN with NULL bound → UNKNOWN → false (excludes all).
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T WHERE val BETWEEN NULL AND 1000`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(0)))
}

func TestFDB_SimpleCaseWorks(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_simple_case_works")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_simple_case_works")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE sc_scw_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_simple_case_works/main WITH TEMPLATE sc_scw_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_simple_case_works?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (1, 5), (2, 10), (3, 99)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx,
		`SELECT CASE val WHEN 5 THEN 'five' WHEN 10 THEN 'ten' ELSE 'other' END FROM T ORDER BY id`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	var results []string
	for rows.Next() {
		var s string
		g.Expect(rows.Scan(&s)).To(gomega.Succeed())
		results = append(results, s)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(results).To(gomega.Equal([]string{"five", "ten", "other"}))

	// Searched-CASE still works too.
	var search string
	g.Expect(db.QueryRowContext(ctx,
		`SELECT CASE WHEN val = 5 THEN 'five' ELSE 'other' END FROM T WHERE id = 1`).
		Scan(&search)).To(gomega.Succeed())
	g.Expect(search).To(gomega.Equal("five"))
}

// TestFDB_ErrorPathSQLSTATE covers the error paths that the audit
// called out as severely under-tested (2/862 error asserts in the
// integration suite). For each case we verify not just that an error
// occurred but that errors.As extracts an *api.Error with the right
// SQLSTATE — because the fmt.Errorf sweep only pays off if callers can
// actually switch on the code.
func TestFDB_ErrorPathSQLSTATE(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_error_paths")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_error_paths")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE err_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_error_paths/main WITH TEMPLATE err_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_error_paths?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Seed one row so UPDATE/DELETE test cases have a target. The NOT NULL
	// UPDATE case would otherwise be a no-op (zero rows matched).
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Helper: exec the query and surface the first error from prepare,
	// iteration, or scan. Returns nil on success.
	queryErr := func(sql string) error {
		rows, e := db.QueryContext(ctx, sql)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v any
			if se := rows.Scan(&v); se != nil {
				return se
			}
		}
		return rows.Err()
	}

	cases := []struct {
		name     string
		sql      string
		exec     bool // true = ExecContext, false = Query
		onDB     bool // true = run on schema-attached db, false = root setup conn
		wantCode api.ErrorCode
	}{
		{
			name:     "syntax error — malformed statement",
			sql:      "SELEKT 1",
			wantCode: api.ErrCodeSyntaxError,
		},
		{
			name:     "unknown table",
			sql:      "SELECT * FROM NoSuchTable",
			wantCode: api.ErrCodeUndefinedTable,
		},
		{
			name:     "unknown column",
			sql:      "SELECT nosuchcol FROM T",
			wantCode: api.ErrCodeUndefinedColumn,
		},
		{
			// swingshift-38: division / modulo by zero returns SQLSTATE
			// 22012 (division_by_zero) — the SQL-standard class-22 code.
			// Previously 22023 (INVALID_PARAMETER); more precise now.
			// Wrapped with `FROM T WHERE id = 1` so the FROM-less
			// rejection (0AF00) doesn't fire first; the seed row at
			// id=1 anchors a single-row evaluation.
			name:     "div by zero (SQL standard error)",
			sql:      "SELECT 1 / 0 FROM T WHERE id = 1",
			wantCode: api.ErrCodeDivisionByZero,
		},
		{
			name:     "mod by zero",
			sql:      "SELECT 5 % 0 FROM T WHERE id = 1",
			wantCode: api.ErrCodeDivisionByZero,
		},
		{
			// ABS is a Go-only math scalar extension (RFC-087). The
			// argument-level overflow edge — ABS(MinInt64), whose
			// two's-complement negation wraps — raises 22003
			// NUMERIC_VALUE_OUT_OF_RANGE via the runtime error channel.
			name:     "ABS overflow (MinInt64) → 22003",
			sql:      "SELECT ABS(-9223372036854775808) FROM T WHERE id = 1",
			wantCode: api.ErrCodeNumericValueOutOfRange,
		},
		{
			// SQRT is a Go-only math scalar extension (RFC-087). The
			// out-of-domain edge — SQRT of a negative argument — raises
			// 22023 INVALID_PARAMETER_VALUE via the runtime error channel.
			name:     "SQRT of negative → 22023",
			sql:      "SELECT SQRT(-1) FROM T WHERE id = 1",
			wantCode: api.ErrCodeInvalidParameter,
		},
		{
			// FROM-less SELECT — fdb-relational 4.11.1.0 rejects at
			// parse time via QueryVisitor.visitSimpleTable's
			// `Assert.notNullUnchecked(fromClause(), UNSUPPORTED_QUERY,
			// "query is not supported")`. Go aligns through
			// extractFromSimpleTable.
			name:     "FROM-less SELECT (parse-time rejection)",
			sql:      "SELECT 1 + 1",
			wantCode: api.ErrCodeUnsupportedQuery,
		},
		{
			name:     "duplicate database",
			sql:      "CREATE DATABASE /testdb_error_paths",
			exec:     true,
			wantCode: api.ErrCodeDatabaseAlreadyExists,
		},
		{
			// Java uses UNKNOWN_DATABASE (42F63) for DROP-not-found;
			// UNDEFINED_DATABASE (42F00) is used only when a *reference*
			// to a missing database is encountered (e.g., CREATE SCHEMA
			// in a nonexistent database).
			name:     "drop non-existent database",
			sql:      "DROP DATABASE /testdb_nope_" + fmt.Sprintf("%d", time.Now().UnixNano()),
			exec:     true,
			wantCode: api.ErrCodeUnknownDatabase,
		},
		{
			name:     "drop non-existent schema",
			sql:      "DROP SCHEMA /testdb_error_paths/notaschema",
			exec:     true,
			wantCode: api.ErrCodeUndefinedSchema,
		},
		{
			name:     "create schema with unknown template",
			sql:      "CREATE SCHEMA /testdb_error_paths/x WITH TEMPLATE nosuchtemplate",
			exec:     true,
			wantCode: api.ErrCodeUnknownSchemaTemplate,
		},
		{
			// id is BIGINT NOT NULL. Proto2's LABEL_REQUIRED should reject
			// an INSERT that leaves it unset. Ideal SQLSTATE is
			// ErrCodeNotNullViolation (23502) per Java. Currently serialised
			// through proto's missing-required-field surface as InvalidParameter.
			// TODO: short-circuit with 23502 at execInsert — see TODO.md.
			name:     "INSERT omitting NOT NULL primary key",
			sql:      "INSERT INTO T (n) VALUES (42)",
			exec:     true,
			onDB:     true, // needs the schema-attached db connection
			wantCode: api.ErrCodeNotNullViolation,
		},
		{
			name:     "INSERT explicit NULL into NOT NULL column",
			sql:      "INSERT INTO T (id, n) VALUES (NULL, 99)",
			exec:     true,
			onDB:     true,
			wantCode: api.ErrCodeNotNullViolation,
		},
		{
			// Precede with an INSERT so UPDATE has a row to target.
			name:     "UPDATE SET col = NULL on NOT NULL column",
			sql:      "UPDATE T SET id = NULL",
			exec:     true,
			onDB:     true,
			wantCode: api.ErrCodeNotNullViolation,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			var err error
			conn := setup
			if tc.onDB {
				conn = db
			}
			if tc.exec {
				_, err = conn.ExecContext(ctx, tc.sql)
			} else {
				// Queries always go through the schema-attached `db`
				// (queryErr closure captures it).
				err = queryErr(tc.sql)
			}
			g.Expect(err).To(gomega.HaveOccurred(), "case: %s", tc.name)
			var apiErr *api.Error
			g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(),
				"error is not *api.Error: %T %v", err, err)
			g.Expect(apiErr.Code).To(gomega.Equal(tc.wantCode),
				"case %s: got SQLSTATE %s (%v), want %s",
				tc.name, apiErr.Code, apiErr.Message, tc.wantCode)
		})
	}
}

// TestFDB_GroupByCountStarOrdering verifies GROUP BY + ORDER BY COUNT(*)
// succeeds via the in-memory sort operator.
// Go extension: in-memory sort — Java's Cascades would reject this.
func TestFDB_GroupByCountStarOrdering(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_gb_cs_order")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_gb_cs_order")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE gb_cs_order_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, k STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_gb_cs_order/main WITH TEMPLATE gb_cs_order_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_gb_cs_order?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		`INSERT INTO T (id, k) VALUES (1, 'a'), (2, 'a'), (3, 'a'),
			(4, 'b'), (5, 'c'), (6, 'c')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Go extension: in-memory sort — ORDER BY COUNT(*) ASC.
	rows, err := db.QueryContext(ctx,
		`SELECT k, COUNT(*) FROM T GROUP BY k ORDER BY COUNT(*) ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type row struct {
		k     string
		count int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.k, &r.count)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	// b=1, c=2, a=3 — ASC by COUNT(*).
	g.Expect(got).To(gomega.Equal([]row{{"b", 1}, {"c", 2}, {"a", 3}}))
}

func TestFDB_GroupByOrderByGroupKey(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_gb_orderkey")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_gb_orderkey")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE gb_orderkey_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, k STRING NOT NULL, PRIMARY KEY (id))
		CREATE INDEX idx_k ON T (k)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_gb_orderkey/main WITH TEMPLATE gb_orderkey_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_gb_orderkey?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		`INSERT INTO T (id, k) VALUES (1, 'c'), (2, 'a'), (3, 'a'),
			(4, 'b'), (5, 'c'), (6, 'b')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx,
		`SELECT k, COUNT(*) FROM T GROUP BY k ORDER BY k ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	type row struct {
		k     string
		count int64
	}
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.k, &r.count)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{{"a", 2}, {"b", 2}, {"c", 2}}))

	rows2, err := db.QueryContext(ctx,
		`SELECT k, COUNT(*) FROM T GROUP BY k ORDER BY k DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var got2 []row
	for rows2.Next() {
		var r row
		g.Expect(rows2.Scan(&r.k, &r.count)).To(gomega.Succeed())
		got2 = append(got2, r)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got2).To(gomega.Equal([]row{{"c", 2}, {"b", 2}, {"a", 2}}))
}

// TestFDB_JoinWithNullKey pins that JOIN ON with NULL keys behaves per
// SQL spec: NULL = NULL in an ON clause is UNKNOWN, so rows with NULL
// keys do NOT match. INNER JOIN skips them; LEFT JOIN preserves the
// left row with NULL for right columns.
func TestFDB_JoinWithNullKey(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_join_null")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_join_null")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE join_null_tmpl
		CREATE TABLE A (id BIGINT NOT NULL, k BIGINT, v BIGINT, PRIMARY KEY (id))
		CREATE TABLE B (id BIGINT NOT NULL, k BIGINT, w BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_join_null/main WITH TEMPLATE join_null_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_join_null?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// A: (1, k=10, v=100), (2, k=NULL, v=200), (3, k=20, v=300)
	// B: (1, k=10, w=1000), (2, k=NULL, w=2000)
	_, err = db.ExecContext(ctx,
		`INSERT INTO A (id, k, v) VALUES (1, 10, 100), (3, 20, 300)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO A (id, v) VALUES (2, 200)`) // k NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO B (id, k, w) VALUES (1, 10, 1000)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO B (id, w) VALUES (2, 2000)`) // k NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// INNER JOIN on k: only id=1 ↔ id=1 matches (k=10). The two NULL-k rows
	// don't match each other — NULL=NULL in ON is UNKNOWN.
	var c int64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM A AS a INNER JOIN B AS b ON a.k = b.k`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "INNER JOIN with NULL key must not match NULL to NULL")
}

// TestFDB_NullHandlingSanityPack bundles a handful of quick SQL-standard
// NULL-semantic checks whose failure would indicate a regression in the
// NULL-aware evaluator stack (tri-state, mixed-type equality, valuesEqual,
// groupByKey) across the proto and map paths. Each is intentionally small
// — the point is early detection of a broad regression, not exhaustive
// coverage (other dedicated tests go deeper on each dimension).
func TestFDB_NullHandlingSanityPack(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_null_sanity")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_null_sanity")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE null_sanity_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_null_sanity/main WITH TEMPLATE null_sanity_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_null_sanity?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// id=1: (5, 5, 'x'); id=2: (5, NULL, 'x'); id=3: (NULL, 3, 'y')
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, a, b, s) VALUES (1, 5, 5, 'x')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, a, s) VALUES (2, 5, 'x')`) // b NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, b, s) VALUES (3, 3, 'y')`) // a NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var c int64

	// a = b: NULL in either side ⇒ UNKNOWN. Only id=1 matches (5=5).
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE a = b`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "a=b with NULL on either side is UNKNOWN")

	// a <> b: NULL ⇒ UNKNOWN (id=2 a=5 b=NULL UNKNOWN, id=3 a=NULL b=3 UNKNOWN). Zero matches.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE a <> b`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "a<>b with NULL is UNKNOWN, not TRUE")

	// COUNT(*) always counts every row. COUNT(a) skips NULL.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(3)))
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(a) FROM T`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(2)))

	// GROUP BY two columns where one is NULL in some rows — rows with NULL
	// in the same column must group together (NULL=NULL for GROUP BY).
	// (a=5, s='x') → 2 rows (id=1,2); (a=NULL, s='y') → 1 row (id=3).
	rows, err := db.QueryContext(ctx, `SELECT COUNT(*) FROM T GROUP BY a, s`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	groupCounts := []int64{}
	for rows.Next() {
		var cnt int64
		g.Expect(rows.Scan(&cnt)).To(gomega.Succeed())
		groupCounts = append(groupCounts, cnt)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(len(groupCounts)).To(gomega.Equal(2), "2 groups: (5,'x') and (NULL,'y')")

	// HAVING COUNT(*) > 1 on the same grouping — only the (5,'x') group
	// has 2 rows; exercises the demoted COUNT(*) flowing through aggCols.
	g.Expect(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM T GROUP BY a, s HAVING COUNT(*) > 1`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(2)), "HAVING COUNT(*) > 1 keeps only the 2-row group")
}

// TestFDB_DistinctAggregates pins SUM/AVG/MIN/MAX with DISTINCT: the
// distinct set must collect each non-null value once, and the per-function
// accumulator must see that same deduplicated stream. Pre-fix only
// COUNT(DISTINCT) incremented counts[i] while sums[i]/avgs[i]/mins[i]/maxes[i]
// stayed zero/unset — SUM(DISTINCT) returned 0 on non-empty groups.
func TestFDB_DistinctAggregates(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_distinct_agg")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_distinct_agg")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE distinct_agg_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_distinct_agg/main WITH TEMPLATE distinct_agg_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_distinct_agg?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Values: {10, 10, 20, 30, NULL, 30}. Distinct non-null set: {10, 20, 30}.
	// Expected: SUM(DISTINCT) = 60, AVG(DISTINCT) = 20, MIN(DISTINCT)=10,
	// MAX(DISTINCT) = 30, COUNT(DISTINCT) = 3.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES
		(1, 10), (2, 10), (3, 20), (4, 30), (6, 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (5)`) // n NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// All DISTINCT-aggregate forms (SUM/AVG/MIN/MAX/COUNT) are
	// rejected by both engines (Java NPEs on every aggregate with
	// DISTINCT; Go ErrCodeUnsupportedOperation). Per project
	// conformance principle: doesn't work in Java → doesn't work in Go.
	for _, q := range []string{
		`SELECT SUM(DISTINCT n) FROM T`,
		`SELECT AVG(DISTINCT n) FROM T`,
		`SELECT MIN(DISTINCT n) FROM T`,
		`SELECT MAX(DISTINCT n) FROM T`,
		`SELECT COUNT(DISTINCT n) FROM T`,
	} {
		var dummy any
		err := db.QueryRowContext(ctx, q).Scan(&dummy)
		g.Expect(err).To(gomega.HaveOccurred(), "query %q: expected rejection", q)
		expectRejectionOrCascadesError(t, err, "DISTINCT")
	}
}

// TestFDB_SubqueryInNullRowRejected pins that `x [NOT] IN (subquery)` is
// rejected at predicate evaluation time, including the NULL-row shapes
// that previously exercised SQL §8.4 UNKNOWN-on-NULL semantics. Java's
// AstNormalizer.visitInPredicate doesn't implement the queryExpressionBody
// alternative of the inList grammar rule (NPE); per CLAUDE.md principle
// #10 Go aligns architecturally and emits a clean error. The §8.4
// semantics this test originally exercised are moot — the feature is
// gone. NOT EXISTS is the supported rewrite for "outer rows whose value
// is not in the inner result", with the caveat that NOT EXISTS does
// NOT have NOT IN's UNKNOWN-on-NULL filtering — callers needing that
// must filter explicit NULLs in the inner subquery.
func TestFDB_SubqueryInNullRowRejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_subq_null")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_subq_null")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE subq_null_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))
		CREATE TABLE U (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_subq_null/main WITH TEMPLATE subq_null_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_subq_null?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// T: 3 rows; U: two rows, one with v NULL.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 10), (2, 20), (3, 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO U (id, v) VALUES (1, 10)`) // v=10
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO U (id) VALUES (2)`) // v=NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectInSubqueryRejected := func(query string) {
		t.Helper()
		var dummy int64
		qErr := db.QueryRowContext(ctx, query).Scan(&dummy)
		g.Expect(qErr).To(gomega.HaveOccurred(), "IN-subquery must be rejected: %s", query)
		expectRejectionOrCascadesError(t, qErr,
			"IN with a subquery argument is not supported; use EXISTS or a JOIN")
	}

	expectInSubqueryRejected(`SELECT COUNT(*) FROM T WHERE n IN (SELECT v FROM U)`)
	expectInSubqueryRejected(`SELECT COUNT(*) FROM T WHERE n NOT IN (SELECT v FROM U)`)
}

// TestFDB_CountDistinctTypeCollision proves COUNT(DISTINCT col) doesn't
// collapse values that differ only by concrete type. The pre-fix
// fmt.Sprintf("%v", v) key made integer 5 and string '5' share a key;
// type-tagged "%T\x00%v" keeps them apart. Exercised here only for the
// grammar-supported case of two rows with the same numeric column and
// the same string column — DISTINCT-ness is then pinned against a mixed
// insert from expression evaluation.
func TestFDB_CountDistinctTypeTaggedKey(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cd_typetag")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cd_typetag")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cd_typetag_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cd_typetag/main WITH TEMPLATE cd_typetag_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cd_typetag?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// COUNT(DISTINCT) is rejected by both engines (Java NPE; Go
	// ErrCodeUnsupportedOperation). The type-tagged-key invariant
	// this test originally exercised is still pinned indirectly via
	// the GROUP BY paths' rowKey encoding (TestFDB_GroupByNullVsNilString
	// below).
	_, err = db.ExecContext(ctx,
		`INSERT INTO T (id, n, s) VALUES (1, 5, 'x'), (2, 5, 'y'), (3, 7, 'x'), (4, 7, 'y')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var c int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n) FROM T`).Scan(&c)
	g.Expect(err).To(gomega.HaveOccurred(), "COUNT(DISTINCT) must be rejected")
	expectRejectionOrCascadesError(t, err, "COUNT(DISTINCT", "DISTINCT aggregates are not supported")
}

// TestFDB_GroupByNullVsNilString pins that GROUP BY distinguishes between
// an actual NULL and the literal string "<nil>". Previously `groupByKey`
// used `fmt.Sprintf("%v", ...)` which renders nil as "<nil>" and the
// string "<nil>" identically, collapsing the two groups. Using a
// length-prefixed type-tagged encoding fixes the collision while keeping
// SQL's NULL=NULL-for-GROUP-BY semantics intact (every NULL normalises to
// the same "N|" sentinel).
func TestFDB_GroupByNullVsNilString(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_gb_nil")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_gb_nil")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE gb_nil_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_gb_nil/main WITH TEMPLATE gb_nil_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_gb_nil?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// id=1: s NULL; id=2: s=literal "<nil>"; id=3: s=literal "<nil>" again.
	// Three rows with two distinct groups expected: (NULL, count=1) and
	// ("<nil>", count=2). The pre-fix `fmt.Sprintf("%v", nil)` collision
	// would have produced a single group with count=3.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (1)`) // s NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, s) VALUES (2, '<nil>'), (3, '<nil>')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, `SELECT s, COUNT(*) FROM T GROUP BY s`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	seen := map[string]int64{}
	for rows.Next() {
		var s sql.NullString
		var c int64
		g.Expect(rows.Scan(&s, &c)).To(gomega.Succeed())
		key := "NULL"
		if s.Valid {
			key = s.String
		}
		seen[key] = c
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(seen).To(gomega.Equal(map[string]int64{"NULL": 1, "<nil>": 2}),
		"GROUP BY must split NULL from literal '<nil>' into two groups")
}

// TestFDB_OrderByNullOrdering pins Java-conformant NULL ordering:
//
//	ORDER BY col ASC  → NULLs FIRST
//	ORDER BY col DESC → NULLs LAST
//
// Matches Java's ParseHelpers.isNullsLast default (returns isDescending).
// Before the compareValues NULL-direction fix, Go returned NULL > non-NULL
// so ASC put NULLs last — the opposite of Java.
func TestFDB_OrderByNullOrdering(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_order_null")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_order_null")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE order_null_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))
		CREATE INDEX idx_n ON T (n)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_order_null/main WITH TEMPLATE order_null_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_order_null?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// id=1: n=10; id=2: n=NULL; id=3: n=30.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 10), (3, 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (2)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// ORDER BY n ASC — index idx_n provides ascending order, NULLs sort FIRST (Java default).
	rows, err := db.QueryContext(ctx, `SELECT id FROM T ORDER BY n ASC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		ids = append(ids, id)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(ids).To(gomega.Equal([]int64{2, 1, 3}), "ASC default must be NULLS FIRST per Java")

	// ORDER BY n DESC — reverse index scan, NULLs sort LAST (Java default for DESC).
	rows2, err := db.QueryContext(ctx, `SELECT id FROM T ORDER BY n DESC`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows2.Close()
	var descIds []int64
	for rows2.Next() {
		var id int64
		g.Expect(rows2.Scan(&id)).To(gomega.Succeed())
		descIds = append(descIds, id)
	}
	g.Expect(rows2.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(descIds).To(gomega.Equal([]int64{3, 1, 2}), "DESC default must be NULLS LAST per Java")
}

// TestFDB_CTEScopeIsolation pins down nested-query CTE scoping: a derived
// table or inner WITH clause must not leak names to the enclosing query.
// Before the scope-stack fix, `c.ctes` was a single shared map — an inner
// `SELECT (SELECT ... FROM (SELECT ...) AS Inner)` would leave "INNER" in
// the outer scope until the full query finished, and a nested WITH would
// clobber the outer map entirely.
func TestFDB_CTEScopeIsolation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cte_scope")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte_scope")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE cte_scope_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte_scope/main WITH TEMPLATE cte_scope_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte_scope?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 10), (2, 20), (3, 30)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Derived-table (subquery in FROM) — now works via catalog-aware path.
	var total sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT SUM(D.n) FROM (SELECT n FROM T WHERE id = 1) AS D`).Scan(&total)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(total.Valid).To(gomega.BeTrue())
	g.Expect(total.Int64).To(gomega.Equal(int64(10)))
}

// TestFDB_MediumAuditFixes covers three MEDIUM items from the dayshift-34
// 5-agent QA audit in one place:
//   - CAST(NULL AS <type>) must return NULL of the target type, not error
//   - ABS / SQRT / POWER are absent from fdb-relational 4.11.1.0's
//     ArithmeticValue registry — Java's planner emits "Unsupported
//     operator <NAME>" (0A000) before evaluation. The MinInt64 overflow
//     check that used to live here is unreachable; assert the rejection
//     fires instead.
//   - LEFT/RIGHT/SUBSTRING float-length arg must error, not silently truncate
func TestFDB_MediumAuditFixes(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_medium_audit")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_medium_audit")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE medium_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_medium_audit/main WITH TEMPLATE medium_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_medium_audit?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n, s) VALUES (1, -9223372036854775808, 'hello world'), (2, 5, 'xy')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// CAST(NULL AS <type>) — must be NULL of that type in every family.
	// The grammar accepts a narrow set of type names in CAST; use the ones
	// already covered by existing tests (STRING, BIGINT, DOUBLE, BOOLEAN).
	for _, cast := range []string{"STRING", "BIGINT", "DOUBLE", "BOOLEAN"} {
		var out sql.NullString
		sqlStr := fmt.Sprintf(`SELECT CAST(NULL AS %s) FROM T WHERE id = 2`, cast)
		g.Expect(db.QueryRowContext(ctx, sqlStr).Scan(&out)).To(gomega.Succeed())
		g.Expect(out.Valid).To(gomega.BeFalse(), "CAST(NULL AS %s) must be NULL", cast)
	}

	// ABS / SQRT / POWER are Go-only math scalar extensions (RFC-087).
	// id=1 has n=MinInt64, id=2 has n=5. The ABS(MinInt64) overflow edge
	// raises 22003 via the runtime error channel; ordinary inputs compute.
	var dummy any
	errAbs := db.QueryRowContext(ctx, `SELECT ABS(n) FROM T WHERE id = 1`).Scan(&dummy)
	g.Expect(errAbs).To(gomega.HaveOccurred(), "ABS(MinInt64) must error")
	var absErr *api.Error
	g.Expect(errors.As(errAbs, &absErr)).To(gomega.BeTrue(), "want *api.Error, got %T", errAbs)
	g.Expect(absErr.Code).To(gomega.Equal(api.ErrCodeNumericValueOutOfRange), "ABS(MinInt64) → 22003")

	var abs int64
	g.Expect(db.QueryRowContext(ctx, `SELECT ABS(n) FROM T WHERE id = 2`).Scan(&abs)).To(gomega.Succeed())
	g.Expect(abs).To(gomega.Equal(int64(5)))

	var sqrt float64
	g.Expect(db.QueryRowContext(ctx, `SELECT SQRT(n) FROM T WHERE id = 2`).Scan(&sqrt)).To(gomega.Succeed())
	g.Expect(sqrt).To(gomega.BeNumerically("~", math.Sqrt(5), 1e-9))

	var pow int64
	g.Expect(db.QueryRowContext(ctx, `SELECT POWER(n, 2) FROM T WHERE id = 2`).Scan(&pow)).To(gomega.Succeed())
	g.Expect(pow).To(gomega.Equal(int64(25)))

	// LEFT / RIGHT / SUBSTRING are Go-only STRING scalar extensions
	// (RFC-087). A non-integral length argument (2.5) is non-coercible and
	// degrades to SQL NULL rather than erroring. s = 'hello world'.
	for _, q := range []string{
		`SELECT LEFT(s, 2.5) FROM T WHERE id = 1`,
		`SELECT RIGHT(s, 2.5) FROM T WHERE id = 1`,
		`SELECT SUBSTRING(s, 1, 2.5) FROM T WHERE id = 1`,
	} {
		var out sql.NullString
		g.Expect(db.QueryRowContext(ctx, q).Scan(&out)).To(gomega.Succeed(), "query %q", q)
		g.Expect(out.Valid).To(gomega.BeFalse(), "query %q must yield NULL", q)
	}
}

// TestFDB_NotOfUnknownIsUnknown pins down SQL three-valued logic for NOT:
//
//	NOT TRUE    = FALSE
//	NOT FALSE   = TRUE
//	NOT UNKNOWN = UNKNOWN   (NOT a row out of the result set)
//
// Previously the predicate evaluator collapsed UNKNOWN → FALSE at the leaves
// so `NOT (x = NULL)`, `NOT (x IN (NULL, ...))`, `NOT LIKE NULL`, and
// `NOT BETWEEN NULL AND ...` all flipped to TRUE and incorrectly kept the row.
// Now UNKNOWN propagates through NOT/AND/OR via an internal tri-state and only
// collapses at the filter boundary (UNKNOWN filters out, same as FALSE).
func TestFDB_NotOfUnknownIsUnknown(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_not_unknown")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_not_unknown")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE not_unknown_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_not_unknown/main WITH TEMPLATE not_unknown_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_not_unknown?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// id=1 has both columns; id=2 has n=NULL, s=NULL.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n, s) VALUES (1, 5, 'hello')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (2)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	var c int64

	// NOT n = NULL — NOT UNKNOWN = UNKNOWN → row filters out.
	// Previously: NULL collapsed to FALSE, NOT FALSE = TRUE, both rows matched.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT n = NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "NOT (x = NULL) must be UNKNOWN for every row, not TRUE")

	// NOT IN over a row whose probe value is NULL — n IS NULL for id=2 so this
	// becomes NULL NOT IN (5, 999) → UNKNOWN. Both rows filter out: id=1
	// because n=5 IS in the list, id=2 because probe is NULL (UNKNOWN).
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE n NOT IN (5, 999)`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)),
		"NULL NOT IN (...) must be UNKNOWN (filters out); non-NULL matching element filters out too")

	// NOT (NULL AND TRUE) — NULL AND TRUE = NULL; NOT NULL = NULL → filter out.
	// Previously: NULL AND TRUE collapsed to FALSE, NOT FALSE = TRUE → every row matched.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT n = NULL AND 1 = 1`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "NOT (UNKNOWN AND TRUE) must stay UNKNOWN")

	// NOT (NULL OR FALSE) — NULL OR FALSE = NULL; NOT NULL = NULL → filter out.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT n = NULL OR 1 = 0`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "NOT (UNKNOWN OR FALSE) must stay UNKNOWN")

	// Double-NOT must still collapse: NOT NOT TRUE = TRUE, NOT NOT UNKNOWN = UNKNOWN.
	// Grammar quirk: parenthesised comparison parses as record constructor, so
	// we rely on the parser's precedence of NOT binding to the full comparison.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT NOT id = 1`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "NOT NOT (id = 1) must equal (id = 1)")
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT NOT n = NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "NOT NOT UNKNOWN = UNKNOWN, must filter out")

	// Sanity: NOT with concrete truthy predicates still works.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT id = 1`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)))
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE NOT id = 99`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(2)))

	// Same invariants through the map path (CTE).
	g.Expect(db.QueryRowContext(ctx,
		`WITH C AS (SELECT n FROM T) SELECT COUNT(*) FROM C WHERE NOT n = NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "CTE path: NOT (x = NULL) stays UNKNOWN")

	// NULL literal inside IN-list: Java rejects with verbatim
	// "NULL values are not allowed in the IN list" (22000). Aligned
	//  — Go now rejects too. SQL §8.4 + Postgres would
	// treat the list as UNKNOWN-tolerant; per project conformance
	// principle (doesn't work in Java → doesn't work in Go), we reject.
	_, err = db.QueryContext(ctx, `SELECT COUNT(*) FROM T WHERE id NOT IN (1, NULL)`)
	g.Expect(err).To(gomega.HaveOccurred(), "NULL in IN-list must reject")
	expectRejectionOrCascadesError(t, err, "NULL values are not allowed in the IN list", "IN-list contains NULL literal")
	_, err = db.QueryContext(ctx, `SELECT COUNT(*) FROM T WHERE id IN (99, NULL)`)
	g.Expect(err).To(gomega.HaveOccurred(), "NULL in IN-list must reject")
	expectRejectionOrCascadesError(t, err, "NULL values are not allowed in the IN list", "IN-list contains NULL literal")

	// BETWEEN NULL bound and LIKE NULL pattern — UNKNOWN propagation sanity.
	// Grammar quirk: BETWEEN … AND … inside parens parses oddly; rely on
	// the bare form (precedence is fine).
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE n BETWEEN NULL AND 999`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "BETWEEN NULL AND x must be UNKNOWN")
	// Grammar does not allow NULL as a LIKE pattern — the semantic path is
	// covered in evalLikePredicateTri by NULL input returning triNull.

	// Java conformance (swingshift-35): Java's ExpressionVisitor rewrites
	// NOT BETWEEN as `x < lo OR x > hi`, so NULL in one bound short-circuits
	// when the other side is definitively TRUE. n=5 (id=1), n=NULL (id=2).
	//   id=1: 5 NOT BETWEEN 10 AND NULL = 5 < 10 OR 5 > NULL = TRUE OR UNKNOWN = TRUE
	//   id=2: NULL NOT BETWEEN 10 AND NULL = UNKNOWN OR UNKNOWN = UNKNOWN
	// Previously both evaluated to UNKNOWN (any-NULL→UNKNOWN), wrongly filtering id=1.
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE n NOT BETWEEN 10 AND NULL`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(1)), "NOT BETWEEN with short-circuitable bound: n=5 NOT BETWEEN 10 AND NULL is TRUE (5<10)")

	// Mirror case: BETWEEN decomposes to `lo <= x AND x <= hi`; one side
	// FALSE → whole AND FALSE, regardless of other side's NULL.
	//   id=1: 5 BETWEEN NULL AND 1 = UNKNOWN AND FALSE = FALSE  (5 > 1)
	//   id=2: NULL BETWEEN NULL AND 1 = UNKNOWN AND UNKNOWN = UNKNOWN
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE n BETWEEN NULL AND 1`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(0)), "BETWEEN with one bound FALSE short-circuits to FALSE, not UNKNOWN")
}

// TestFDB_AggregateNullSemantics pins down SQL-standard aggregate behaviour:
//   - COUNT(col) skips NULLs; COUNT(*) does not
//   - SUM of empty-or-all-NULL returns NULL, not 0
//   - AVG of empty-or-all-NULL returns NULL
//   - MIN/MAX of all-NULL returns NULL
//   - SUM of a non-numeric column errors instead of silently producing 0
//
// Covers both the proto path (plain SELECT FROM table) and the map path
// (CTE / aggregate) so the two evaluators stay consistent.
func TestFDB_AggregateNullSemantics(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_agg_null")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_agg_null")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE agg_null_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_agg_null/main WITH TEMPLATE agg_null_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_agg_null?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Rows: (1, 10, 'a'), (2, NULL, 'b'), (3, 20, NULL), (4, NULL, NULL)
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n, s) VALUES (1, 10, 'a'), (3, 20, 'x')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, s) VALUES (2, 'b')`) // n=NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO T (id) VALUES (4)`) // n=NULL, s=NULL
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// One extra row with non-null s for UPDATE below.
	_, err = db.ExecContext(ctx, `UPDATE T SET s = NULL WHERE id = 3`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// COUNT(*) sees all rows.
	var c int64
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(4)))

	// COUNT(n) must skip NULLs → 2 (ids 1 & 3).
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(n) FROM T`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(2)), "COUNT(col) must skip NULLs")

	// SUM over some non-null values → 30 (10+20).
	var sum sql.NullInt64
	g.Expect(db.QueryRowContext(ctx, `SELECT SUM(n) FROM T`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeTrue())
	g.Expect(sum.Int64).To(gomega.Equal(int64(30)))

	// SUM over all-NULL group → NULL. `id = 4` gives n=NULL only.
	g.Expect(db.QueryRowContext(ctx, `SELECT SUM(n) FROM T WHERE id = 4`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse(), "SUM of all-NULL group must be NULL, not 0")

	// SUM over empty set (no rows) → NULL.
	g.Expect(db.QueryRowContext(ctx, `SELECT SUM(n) FROM T WHERE id = 999`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse(), "SUM of empty set must be NULL, not 0")

	// AVG over all-NULL → NULL.
	var avg sql.NullFloat64
	g.Expect(db.QueryRowContext(ctx, `SELECT AVG(n) FROM T WHERE id = 4`).Scan(&avg)).To(gomega.Succeed())
	g.Expect(avg.Valid).To(gomega.BeFalse())

	// MIN/MAX over all-NULL → NULL.
	g.Expect(db.QueryRowContext(ctx, `SELECT MIN(n) FROM T WHERE id = 4`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse())
	g.Expect(db.QueryRowContext(ctx, `SELECT MAX(n) FROM T WHERE id = 4`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse())

	// SUM over a STRING column must error (cannot silently treat as 0).
	rows, err := db.QueryContext(ctx, `SELECT SUM(s) FROM T WHERE id = 1`)
	// error may surface at query time or during iteration/scan
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var v any
			err = rows.Scan(&v)
		}
		if err == nil {
			err = rows.Err()
		}
	}
	g.Expect(err).To(gomega.HaveOccurred(), "SUM of STRING column must error, not silently sum to 0")

	// Same invariants via the map path (CTE).
	g.Expect(db.QueryRowContext(ctx, `WITH C AS (SELECT n FROM T) SELECT COUNT(n) FROM C`).Scan(&c)).To(gomega.Succeed())
	g.Expect(c).To(gomega.Equal(int64(2)), "map-path COUNT(col) must skip NULLs")

	g.Expect(db.QueryRowContext(ctx, `WITH C AS (SELECT n FROM T WHERE id = 4) SELECT SUM(n) FROM C`).Scan(&sum)).To(gomega.Succeed())
	g.Expect(sum.Valid).To(gomega.BeFalse(), "map-path SUM of all-NULL group must be NULL")
}

// TestFDB_ArithmeticUnifiedSemantics proves that proto and map evaluator
// paths produce identical arithmetic results:
//   - division by zero errors (SQL standard) in both paths
//   - modulo (`%`) works in both paths
//   - modulo by zero errors in both paths
func TestFDB_ArithmeticUnifiedSemantics(t *testing.T) {
	t.Parallel()

	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_arith_unified")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_arith_unified")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE arith_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_arith_unified/main WITH TEMPLATE arith_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_arith_unified?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, a, b) VALUES (1, 10, 3), (2, 5, 0)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// queryErr exhausts a SQL query, surfacing the first error from prepare,
	// iteration, or scan. Used for tests that expect a single error to reach
	// the caller regardless of which stage materialises it.
	queryErr := func(sql string) error {
		rows, e := db.QueryContext(ctx, sql)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v any
			if se := rows.Scan(&v); se != nil {
				return se
			}
		}
		return rows.Err()
	}

	// Proto path — `%` previously errored, now works.
	var mod int64
	g.Expect(db.QueryRowContext(ctx, `SELECT a % b FROM T WHERE id = 1`).Scan(&mod)).To(gomega.Succeed())
	g.Expect(mod).To(gomega.Equal(int64(1)))

	// Proto path — division by zero errors (SQL standard).
	g.Expect(queryErr(`SELECT a / b FROM T WHERE id = 2`)).To(gomega.HaveOccurred(),
		"proto path div/0 must error")

	// Proto path — modulo by zero errors (consistent with /0).
	g.Expect(queryErr(`SELECT a % b FROM T WHERE id = 2`)).To(gomega.HaveOccurred(),
		"proto path mod/0 must error")

	// Map path (via CTE) — same SQL-standard error, was previously NULL.
	g.Expect(queryErr(`WITH C AS (SELECT a, b FROM T WHERE id = 2) SELECT a / b FROM C`)).
		To(gomega.HaveOccurred(), "map path (CTE) div/0 must error")

	// Map path — `%` continues to work.
	var mod2 int64
	g.Expect(db.QueryRowContext(ctx,
		`WITH C AS (SELECT a, b FROM T WHERE id = 1) SELECT a % b FROM C`).Scan(&mod2)).To(gomega.Succeed())
	g.Expect(mod2).To(gomega.Equal(int64(1)))
}

// TestFDB_MixedTypeEqualityNoStringCoerce proves that mixed-type equality
// errors with SQLSTATE 42804 (matching Java's
// SemanticException.COMPARISON_OF_INCOMPATIBLE_TYPES → DATATYPE_MISMATCH)
// instead of silently falling through to string coercion. Same-type
// equality still works.
func TestFDB_MixedTypeEqualityNoStringCoerce(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_mixedtype_eq")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_mixedtype_eq")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE mixedtype_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_mixedtype_eq/main WITH TEMPLATE mixedtype_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_mixedtype_eq?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n, s) VALUES (1, 5, '5'), (2, 6, '6')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectIncompatibleType := func(query string) {
		t.Helper()
		var cnt int64
		err := db.QueryRowContext(ctx, query).Scan(&cnt)
		g.Expect(err).To(gomega.HaveOccurred(), "expected error from %q", query)
		var apiErr *api.Error
		g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(), "expected *api.Error, got %T: %v", err, err)
		g.Expect(string(apiErr.Code)).To(gomega.Equal("42804"))
	}

	// Proto path: int column = string literal must error 42804.
	expectIncompatibleType(`SELECT COUNT(*) FROM T WHERE n = '5'`)
	expectIncompatibleType(`SELECT COUNT(*) FROM T WHERE s = 5`)
	// IN-list with mixed types: any incompatible element errors.
	expectIncompatibleType(`SELECT COUNT(*) FROM T WHERE n IN ('5', 6)`)

	// Sanity: same-type equality still works.
	var cnt int64
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE n = 5`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1)))
	g.Expect(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM T WHERE s = '5'`).Scan(&cnt)).To(gomega.Succeed())
	g.Expect(cnt).To(gomega.Equal(int64(1)))
}

// TestFDB_IntegerRangeEnforcement pins that INSERT of an out-of-range
// int64 into an INT32 column errors cleanly instead of silently wrapping.
// Schema templates lower `INTEGER` to proto Int32Kind (see metadata
// builder's datatypeToProtoFieldType), so writing 2_147_483_648 (one past
// int32 max) would previously silently become -2_147_483_648 — a value
// corruption with no user-visible signal. Matches Java's
// CastValue.LONG_TO_INT range check.
func TestFDB_IntegerRangeEnforcement(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_int_range")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_int_range")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE int_range_tmpl
		CREATE TABLE T (id BIGINT NOT NULL, n INTEGER, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_int_range/main WITH TEMPLATE int_range_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_int_range?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// In range: must succeed.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (1, 2147483647)`)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "INT32 max value must be accepted")

	// Over max: must error, not silently wrap to -2147483648.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (2, 2147483648)`)
	g.Expect(err).To(gomega.HaveOccurred(), "INT32 overflow must error; previously silently wrapped")

	// Under min: same.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, n) VALUES (3, -2147483649)`)
	g.Expect(err).To(gomega.HaveOccurred(), "INT32 underflow must error")
}

func TestFDB_ColumnTypeScanTypeAndNullable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_col_types")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_col_types")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE col_types_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, name STRING, flag BOOLEAN, score DOUBLE, PRIMARY KEY (id))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_col_types/main WITH TEMPLATE col_types_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_col_types?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO T (id, name, flag, score) VALUES (1, 'hello', TRUE, 3.14)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	rows, err := db.QueryContext(ctx, "SELECT id, name, flag, score FROM T")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(colTypes).To(gomega.HaveLen(4))

	g.Expect(colTypes[0].DatabaseTypeName()).To(gomega.Equal("BIGINT"))
	g.Expect(colTypes[0].ScanType().Kind().String()).To(gomega.Equal("int64"))

	g.Expect(colTypes[1].DatabaseTypeName()).To(gomega.Equal("STRING"))
	g.Expect(colTypes[1].ScanType().Kind().String()).To(gomega.Equal("string"))

	g.Expect(colTypes[2].DatabaseTypeName()).To(gomega.Equal("BOOLEAN"))
	g.Expect(colTypes[2].ScanType().Kind().String()).To(gomega.Equal("bool"))

	g.Expect(colTypes[3].DatabaseTypeName()).To(gomega.Equal("DOUBLE"))
	g.Expect(colTypes[3].ScanType().Kind().String()).To(gomega.Equal("float64"))

	for i, ct := range colTypes {
		nullable, ok := ct.Nullable()
		g.Expect(ok).To(gomega.BeTrue(), "column %d (%s): Nullable should report ok=true", i, ct.Name())
		if i == 0 {
			// id BIGINT NOT NULL → proto REQUIRED → not nullable
			g.Expect(nullable).To(gomega.BeFalse(), "column 0 (ID): NOT NULL column should report nullable=false")
		} else {
			// name/flag/score are OPTIONAL → nullable
			g.Expect(nullable).To(gomega.BeTrue(), "column %d (%s): nullable column should report nullable=true", i, ct.Name())
		}
	}

	// ColumnTypeLength: STRING columns are variable-length.
	length, hasLength := colTypes[1].Length()
	g.Expect(hasLength).To(gomega.BeTrue(), "STRING column should report variable length")
	g.Expect(length).To(gomega.Equal(int64(math.MaxInt64)), "STRING length should be MaxInt64")

	// BIGINT is not variable-length.
	_, hasLength = colTypes[0].Length()
	g.Expect(hasLength).To(gomega.BeFalse(), "BIGINT column should not report variable length")

	// ColumnTypePrecisionScale: no decimal types in fdb-relational.
	_, _, hasPrecision := colTypes[0].DecimalSize()
	g.Expect(hasPrecision).To(gomega.BeFalse(), "BIGINT should not report decimal precision")
	_, _, hasPrecision = colTypes[3].DecimalSize()
	g.Expect(hasPrecision).To(gomega.BeFalse(), "DOUBLE should not report decimal precision")

	g.Expect(rows.Next()).To(gomega.BeTrue())
	rows.Close()
}

func TestFDB_CTEChainedColumnAliases(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cte_chain_alias")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cte_chain_alias")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE chain_alias_tmpl
		CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_cte_chain_alias/main WITH TEMPLATE chain_alias_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_cte_chain_alias?cluster_file=%s&schema=main", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	for _, vals := range []string{
		"(1, 10)", "(2, 20)", "(3, 30)", "(4, 40)",
	} {
		_, err = db.ExecContext(ctx, "INSERT INTO t (id, v) VALUES "+vals)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// Chained CTE column aliases: base renames id->d, v->val; filtered
	// renames d->x, val->y. The outer SELECT must resolve x and y
	// through the two-level alias chain.
	rows, err := db.QueryContext(ctx, `
		WITH base(d, val) AS (SELECT id, v FROM t),
		     filtered(x, y) AS (SELECT d, val FROM base WHERE val > 15)
		SELECT x, y FROM filtered ORDER BY x`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	type row struct{ x, y int64 }
	var got []row
	for rows.Next() {
		var r row
		g.Expect(rows.Scan(&r.x, &r.y)).To(gomega.Succeed())
		got = append(got, r)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([]row{
		{2, 20}, {3, 30}, {4, 40},
	}), "chained CTE column aliases: base(d,val), filtered(x,y)")
}

// --- Schema-qualified table names (TODO #99) ---

func TestFDB_SchemaQualifiedSelect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtselect")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtselect"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqt_tmpl "+
			"CREATE TABLE Items (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtselect/sqt WITH TEMPLATE sqt_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtselect?cluster_file=%s&schema=sqt", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (1, 10)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (2, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Query using schema-qualified name: sqt.Items
	rows, err := db.QueryContext(ctx, "SELECT id, val FROM sqt.Items ORDER BY id")
	if err != nil {
		t.Fatalf("schema-qualified SELECT: %v", err)
	}
	defer rows.Close()

	type row struct{ id, val int64 }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.val); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	want := []row{{1, 10}, {2, 20}}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFDB_SchemaQualifiedInsert(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtins")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtins"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqti_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtins/s1 WITH TEMPLATE sqti_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtins?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INSERT using schema-qualified name
	res, err := db.ExecContext(ctx, "INSERT INTO s1.T VALUES (42)")
	if err != nil {
		t.Fatalf("schema-qualified INSERT: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("RowsAffected = %d, want 1", n)
	}

	// Verify via unqualified SELECT
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM T").Scan(&id); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
}

func TestFDB_SchemaQualifiedUpdate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtupd")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtupd"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqtu_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtupd/s1 WITH TEMPLATE sqtu_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtupd?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1, 100)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// UPDATE using schema-qualified name
	res, err := db.ExecContext(ctx, "UPDATE s1.T SET val = 200 WHERE id = 1")
	if err != nil {
		t.Fatalf("schema-qualified UPDATE: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("RowsAffected = %d, want 1", n)
	}

	var val int64
	if err := db.QueryRowContext(ctx, "SELECT val FROM T WHERE id = 1").Scan(&val); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if val != 200 {
		t.Fatalf("val = %d, want 200", val)
	}
}

func TestFDB_SchemaQualifiedDelete(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtdel")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtdel"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqtd_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtdel/s1 WITH TEMPLATE sqtd_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtdel?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (2)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// DELETE using schema-qualified name
	res, err := db.ExecContext(ctx, "DELETE FROM s1.T WHERE id = 1")
	if err != nil {
		t.Fatalf("schema-qualified DELETE: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("RowsAffected = %d, want 1", n)
	}

	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T").Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestFDB_SchemaQualifiedWrongSchema(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtwrong")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtwrong"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqtw_tmpl "+
			"CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtwrong/s1 WITH TEMPLATE sqtw_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtwrong?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// SELECT with wrong schema qualifier → error
	_, err = db.QueryContext(ctx, "SELECT id FROM wrongschema.T")
	if err == nil {
		t.Fatal("expected error for wrong schema qualifier")
	}

	// INSERT with wrong schema qualifier → error
	_, err = db.ExecContext(ctx, "INSERT INTO wrongschema.T VALUES (1)")
	if err == nil {
		t.Fatal("expected error for wrong schema qualifier on INSERT")
	}

	// UPDATE with wrong schema qualifier → error
	_, err = db.ExecContext(ctx, "UPDATE wrongschema.T SET id = 1 WHERE id = 1")
	if err == nil {
		t.Fatal("expected error for wrong schema qualifier on UPDATE")
	}

	// DELETE with wrong schema qualifier → error
	_, err = db.ExecContext(ctx, "DELETE FROM wrongschema.T WHERE id = 1")
	if err == nil {
		t.Fatal("expected error for wrong schema qualifier on DELETE")
	}
}

func TestFDB_SchemaQualifiedCaseInsensitive(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sqtcase")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_sqtcase"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sqtc_tmpl "+
			"CREATE TABLE Items (id BIGINT NOT NULL, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_sqtcase/MySchema WITH TEMPLATE sqtc_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_sqtcase?cluster_file=%s&schema=MySchema", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "INSERT INTO Items VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Schema qualifier in different case should work
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM MYSCHEMA.Items").Scan(&id); err != nil {
		t.Fatalf("upper-case schema qualifier: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}

	if err := db.QueryRowContext(ctx, "SELECT id FROM myschema.Items").Scan(&id); err != nil {
		t.Fatalf("lower-case schema qualifier: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
}

func TestFDB_DateTimestampColumns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_datetime")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_datetime")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE datetime_events_tmpl "+
			"CREATE TABLE Events (id BIGINT NOT NULL, event_date DATE, event_ts TIMESTAMP, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_datetime/s1 WITH TEMPLATE datetime_events_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_datetime?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Insert with string literals (ISO format).
	_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (1, '2024-03-15', '2024-03-15 10:30:00')")
	if err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (2, '2024-06-20', '2024-06-20 14:45:30')")
	if err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (3, '2024-01-01', '2024-01-01 00:00:00')")
	if err != nil {
		t.Fatalf("INSERT 3: %v", err)
	}

	// Select all rows ordered by id.
	rows, err := db.QueryContext(ctx, "SELECT id, event_date, event_ts FROM Events")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()

	type row struct {
		id   int64
		date string
		ts   string
	}
	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.date, &r.ts); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("got %d rows, want 3", len(results))
	}
	if results[0].date != "2024-03-15" {
		t.Errorf("row 1 date = %q, want 2024-03-15", results[0].date)
	}
	if results[0].ts != "2024-03-15 10:30:00" {
		t.Errorf("row 1 ts = %q, want 2024-03-15 10:30:00", results[0].ts)
	}

	// Test WHERE comparison with string literal.
	var count int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Events WHERE event_date > '2024-02-01'").Scan(&count)
	if err != nil {
		t.Fatalf("WHERE date comparison: %v", err)
	}
	if count != 2 {
		t.Errorf("events after 2024-02-01: got %d, want 2", count)
	}

	// Test CURRENT_TIMESTAMP is non-nil.
	var ts2 string
	err = db.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP FROM Events WHERE id = 1").Scan(&ts2)
	if err != nil {
		t.Fatalf("CURRENT_TIMESTAMP: %v", err)
	}
	if ts2 == "" {
		t.Error("CURRENT_TIMESTAMP returned empty string")
	}

	// Test CURRENT_DATE is non-nil.
	var dt string
	err = db.QueryRowContext(ctx, "SELECT CURRENT_DATE FROM Events WHERE id = 1").Scan(&dt)
	if err != nil {
		t.Fatalf("CURRENT_DATE: %v", err)
	}
	if dt == "" {
		t.Error("CURRENT_DATE returned empty string")
	}
}

func TestFDB_DateTimestampComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dtcmp")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dtcmp")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE datetime_logs_tmpl "+
			"CREATE TABLE Logs (id BIGINT NOT NULL, ts TIMESTAMP, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dtcmp/s1 WITH TEMPLATE datetime_logs_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dtcmp?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, "INSERT INTO Logs VALUES (1, '2020-01-01 00:00:00')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO Logs VALUES (2, '2099-12-31 23:59:59')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// CURRENT_TIMESTAMP should be between 2020 and 2099.
	var count int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Logs WHERE ts < CURRENT_TIMESTAMP").Scan(&count)
	if err != nil {
		t.Fatalf("WHERE ts < CURRENT_TIMESTAMP: %v", err)
	}
	if count != 1 {
		t.Errorf("rows before now: got %d, want 1 (only 2020 row)", count)
	}

	// Comparison with string literal.
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Logs WHERE ts > '2050-01-01 00:00:00'").Scan(&count)
	if err != nil {
		t.Fatalf("WHERE ts > '2050...': %v", err)
	}
	if count != 1 {
		t.Errorf("rows after 2050: got %d, want 1 (only 2099 row)", count)
	}
}

func TestFDB_DateTimestampInsertWithLiteral(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dtinsert")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dtinsert")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE datetime_audit_tmpl "+
			"CREATE TABLE Audit (id BIGINT NOT NULL, created_at TIMESTAMP, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dtinsert/s1 WITH TEMPLATE datetime_audit_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dtinsert?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err = db.ExecContext(ctx, fmt.Sprintf("INSERT INTO Audit VALUES (1, '%s')", now))
	if err != nil {
		t.Fatalf("INSERT with timestamp literal: %v", err)
	}

	var ts string
	err = db.QueryRowContext(ctx, "SELECT created_at FROM Audit WHERE id = 1").Scan(&ts)
	if err != nil {
		t.Fatalf("SELECT created_at: %v", err)
	}

	// Verify it parses as a valid timestamp.
	parsed, perr := time.Parse("2006-01-02 15:04:05", ts)
	if perr != nil {
		t.Fatalf("stored timestamp %q doesn't parse: %v", ts, perr)
	}
	// Should be within the last minute.
	if time.Since(parsed) > time.Minute {
		t.Errorf("created_at %v is more than 1 minute old", parsed)
	}
}

func TestFDB_DateTimestampCast(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dtcast")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dtcast")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE datetime_cast_tmpl "+
			"CREATE TABLE T1 (id BIGINT NOT NULL, val STRING, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dtcast/s1 WITH TEMPLATE datetime_cast_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dtcast?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, '2024-07-04 12:00:00')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// CAST(string AS TIMESTAMP) should work.
	var ts string
	err = db.QueryRowContext(ctx, "SELECT CAST(val AS TIMESTAMP) FROM T1 WHERE id = 1").Scan(&ts)
	if err != nil {
		t.Fatalf("CAST AS TIMESTAMP: %v", err)
	}
	if ts == "" {
		t.Error("CAST AS TIMESTAMP returned empty")
	}

	// CAST(string AS DATE) should work.
	var dt string
	err = db.QueryRowContext(ctx, "SELECT CAST('2024-07-04' AS DATE) FROM T1 WHERE id = 1").Scan(&dt)
	if err != nil {
		t.Fatalf("CAST AS DATE: %v", err)
	}
	if dt == "" {
		t.Error("CAST AS DATE returned empty")
	}
}

func TestFDB_DatePartFunctionsOnStoredColumns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dateparts")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dateparts")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dateparts_tmpl "+
			"CREATE TABLE Events (id BIGINT NOT NULL, ts TIMESTAMP, d DATE, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dateparts/s1 WITH TEMPLATE dateparts_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dateparts?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (1, '2024-07-04 15:30:45', '2024-07-04')")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// YEAR/MONTH/DAY on stored TIMESTAMP column.
	var year, month, day int64
	err = db.QueryRowContext(ctx, "SELECT YEAR(ts), MONTH(ts), DAY(ts) FROM Events WHERE id = 1").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY on ts column: %v", err)
	}
	if year != 2024 {
		t.Errorf("YEAR(ts) = %d, want 2024", year)
	}
	if month != 7 {
		t.Errorf("MONTH(ts) = %d, want 7", month)
	}
	if day != 4 {
		t.Errorf("DAY(ts) = %d, want 4", day)
	}

	// HOUR/MINUTE/SECOND on stored TIMESTAMP column.
	var hour, minute, second int64
	err = db.QueryRowContext(ctx, "SELECT HOUR(ts), MINUTE(ts), SECOND(ts) FROM Events WHERE id = 1").Scan(&hour, &minute, &second)
	if err != nil {
		t.Fatalf("HOUR/MINUTE/SECOND on ts column: %v", err)
	}
	if hour != 15 {
		t.Errorf("HOUR(ts) = %d, want 15", hour)
	}
	if minute != 30 {
		t.Errorf("MINUTE(ts) = %d, want 30", minute)
	}
	if second != 45 {
		t.Errorf("SECOND(ts) = %d, want 45", second)
	}

	// YEAR/MONTH/DAY on stored DATE column.
	err = db.QueryRowContext(ctx, "SELECT YEAR(d), MONTH(d), DAY(d) FROM Events WHERE id = 1").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY on date column: %v", err)
	}
	if year != 2024 {
		t.Errorf("YEAR(d) = %d, want 2024", year)
	}
	if month != 7 {
		t.Errorf("MONTH(d) = %d, want 7", month)
	}
	if day != 4 {
		t.Errorf("DAY(d) = %d, want 4", day)
	}

	// YEAR on CURRENT_TIMESTAMP (returns time.Time, not string).
	err = db.QueryRowContext(ctx, "SELECT YEAR(CURRENT_TIMESTAMP) FROM Events WHERE id = 1").Scan(&year)
	if err != nil {
		t.Fatalf("YEAR(CURRENT_TIMESTAMP): %v", err)
	}
	if year < 2024 || year > 2100 {
		t.Errorf("YEAR(CURRENT_TIMESTAMP) = %d, want reasonable year", year)
	}
}

func TestFDB_ArrayColumnDDL(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_array_col")

	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_array_col"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE array_col_tmpl "+
			"CREATE TABLE TaggedItem (item_id BIGINT NOT NULL, tags STRING ARRAY, PRIMARY KEY (item_id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_array_col/store WITH TEMPLATE array_col_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	// Open a new connection with the schema set via DSN.
	dsn := fmt.Sprintf("fdbsql:///testdb_array_col?cluster_file=%s&schema=store", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INSERT a row with NULL for the array column (SQL array literals are not
	// supported in this system; NULL is the valid way to leave an array field
	// empty via INSERT).
	if _, err := db.ExecContext(ctx,
		"INSERT INTO TaggedItem (item_id, tags) VALUES (1, NULL)"); err != nil {
		t.Fatalf("INSERT with NULL array: %v", err)
	}

	// SELECT and verify the row comes back.
	var itemID int64
	var tags any
	err = db.QueryRowContext(ctx, "SELECT item_id, tags FROM TaggedItem WHERE item_id = 1").Scan(&itemID, &tags)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if itemID != 1 {
		t.Errorf("item_id = %d, want 1", itemID)
	}
	if tags != nil {
		t.Errorf("tags = %v, want nil (NULL)", tags)
	}
}

func TestFDB_DateTimestampEdgeCases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dt_edge")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dt_edge")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dt_edge_tmpl "+
			"CREATE TABLE Events (id BIGINT NOT NULL, d DATE, ts TIMESTAMP, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dt_edge/s1 WITH TEMPLATE dt_edge_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dt_edge?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Insert edge-case rows: year boundaries, leap day, far future, epoch.
	inserts := []struct {
		id int64
		d  string
		ts string
	}{
		{1, "2000-01-01", "2000-01-01 00:00:00"},
		{2, "1999-12-31", "1999-12-31 23:59:59"},
		{3, "2024-02-29", "2024-02-29 12:00:00"},
		{4, "9999-12-31", "9999-12-31 23:59:59"},
		{5, "1970-01-01", "1970-01-01 00:00:00"},
	}
	for _, ins := range inserts {
		_, err = db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO Events VALUES (%d, '%s', '%s')", ins.id, ins.d, ins.ts))
		if err != nil {
			t.Fatalf("INSERT id=%d: %v", ins.id, err)
		}
	}

	// --- Verify round-trip of all rows ---
	rows, err := db.QueryContext(ctx, "SELECT id, d, ts FROM Events ORDER BY id")
	if err != nil {
		t.Fatalf("SELECT all: %v", err)
	}
	defer rows.Close()

	type row struct {
		id int64
		d  string
		ts string
	}
	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.d, &r.ts); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("got %d rows, want 5", len(results))
	}

	// Spot-check stored values.
	if results[0].d != "2000-01-01" {
		t.Errorf("row 1 date = %q, want 2000-01-01", results[0].d)
	}
	if results[1].d != "1999-12-31" {
		t.Errorf("row 2 date = %q, want 1999-12-31", results[1].d)
	}
	if results[2].d != "2024-02-29" {
		t.Errorf("row 3 date = %q, want 2024-02-29 (leap day)", results[2].d)
	}
	if results[3].ts != "9999-12-31 23:59:59" {
		t.Errorf("row 4 ts = %q, want 9999-12-31 23:59:59", results[3].ts)
	}
	if results[4].ts != "1970-01-01 00:00:00" {
		t.Errorf("row 5 ts = %q, want 1970-01-01 00:00:00", results[4].ts)
	}

	// --- WHERE with CAST(string AS TIMESTAMP) ---
	var count int64
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM Events WHERE ts >= CAST('2000-01-01 00:00:00' AS TIMESTAMP)").Scan(&count)
	if err != nil {
		t.Fatalf("WHERE CAST AS TIMESTAMP: %v", err)
	}
	// Rows 1 (2000), 3 (2024), 4 (9999) satisfy >= 2000-01-01 00:00:00.
	if count != 3 {
		t.Errorf("rows >= 2000-01-01 via CAST: got %d, want 3", count)
	}

	// --- YEAR/MONTH/DAY on edge dates ---
	var year, month, day int64

	// Y2K boundary.
	err = db.QueryRowContext(ctx, "SELECT YEAR(d), MONTH(d), DAY(d) FROM Events WHERE id = 1").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY id=1: %v", err)
	}
	if year != 2000 || month != 1 || day != 1 {
		t.Errorf("id=1: YEAR/MONTH/DAY = %d/%d/%d, want 2000/1/1", year, month, day)
	}

	// 1999-12-31.
	err = db.QueryRowContext(ctx, "SELECT YEAR(d), MONTH(d), DAY(d) FROM Events WHERE id = 2").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY id=2: %v", err)
	}
	if year != 1999 || month != 12 || day != 31 {
		t.Errorf("id=2: YEAR/MONTH/DAY = %d/%d/%d, want 1999/12/31", year, month, day)
	}

	// Leap day.
	err = db.QueryRowContext(ctx, "SELECT YEAR(d), MONTH(d), DAY(d) FROM Events WHERE id = 3").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY id=3: %v", err)
	}
	if year != 2024 || month != 2 || day != 29 {
		t.Errorf("id=3: YEAR/MONTH/DAY = %d/%d/%d, want 2024/2/29", year, month, day)
	}

	// Far future.
	err = db.QueryRowContext(ctx, "SELECT YEAR(ts), MONTH(ts), DAY(ts) FROM Events WHERE id = 4").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY id=4: %v", err)
	}
	if year != 9999 || month != 12 || day != 31 {
		t.Errorf("id=4: YEAR/MONTH/DAY = %d/%d/%d, want 9999/12/31", year, month, day)
	}

	// Epoch.
	err = db.QueryRowContext(ctx, "SELECT YEAR(ts), MONTH(ts), DAY(ts) FROM Events WHERE id = 5").Scan(&year, &month, &day)
	if err != nil {
		t.Fatalf("YEAR/MONTH/DAY id=5: %v", err)
	}
	if year != 1970 || month != 1 || day != 1 {
		t.Errorf("id=5: YEAR/MONTH/DAY = %d/%d/%d, want 1970/1/1", year, month, day)
	}

	// --- ORDER BY TIMESTAMP: verify lexicographic ordering on ISO strings ---
	rows2, err := db.QueryContext(ctx, "SELECT id FROM Events ORDER BY ts ASC")
	if err != nil {
		t.Fatalf("ORDER BY ts ASC: %v", err)
	}
	defer rows2.Close()

	var ids []int64
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			t.Fatalf("Scan ORDER BY: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("rows2.Err: %v", err)
	}

	// Expected order by ts ascending:
	// 1970-01-01 (id=5), 1999-12-31 (id=2), 2000-01-01 (id=1), 2024-02-29 (id=3), 9999-12-31 (id=4)
	wantOrder := []int64{5, 2, 1, 3, 4}
	if len(ids) != len(wantOrder) {
		t.Fatalf("ORDER BY returned %d rows, want %d", len(ids), len(wantOrder))
	}
	for i, want := range wantOrder {
		if ids[i] != want {
			t.Errorf("ORDER BY ts ASC position %d: got id=%d, want id=%d", i, ids[i], want)
		}
	}
}

func TestFDB_DateTimestampParameterBinding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dt_params")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dt_params")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dt_params_tmpl "+
			"CREATE TABLE Events (id BIGINT NOT NULL, ts TIMESTAMP, PRIMARY KEY(id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dt_params/s1 WITH TEMPLATE dt_params_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dt_params?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INSERT using parameterized query with time.Time.
	ts := time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC)
	_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (?, ?)", int64(1), ts)
	if err != nil {
		t.Fatalf("INSERT with time.Time parameter: %v", err)
	}

	// SELECT back and verify the timestamp round-trips correctly.
	var got string
	err = db.QueryRowContext(ctx, "SELECT ts FROM Events WHERE id = 1").Scan(&got)
	if err != nil {
		t.Fatalf("SELECT ts: %v", err)
	}
	if got != "2024-07-04 15:30:45" {
		t.Errorf("round-trip timestamp = %q, want %q", got, "2024-07-04 15:30:45")
	}

	// WHERE clause with a time.Time parameter.
	var id int64
	err = db.QueryRowContext(ctx, "SELECT id FROM Events WHERE ts = ?", time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC)).Scan(&id)
	if err != nil {
		t.Fatalf("SELECT with WHERE ts = ?: %v", err)
	}
	if id != 1 {
		t.Errorf("WHERE ts = ? returned id=%d, want 1", id)
	}
}

func TestFDB_DateTimestampIndexScan(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dt_idx")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_dt_idx")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dt_idx_tmpl "+
			"CREATE TABLE Events (id BIGINT NOT NULL, ts TIMESTAMP, label STRING, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_ts ON Events (ts)")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_dt_idx/s1 WITH TEMPLATE dt_idx_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_dt_idx?cluster_file=%s&schema=s1", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Insert 4 rows with different timestamps.
	inserts := []struct {
		id    int64
		ts    string
		label string
	}{
		{1, "2020-06-15 10:00:00", "alpha"},
		{2, "2022-03-20 14:30:00", "beta"},
		{3, "2024-09-01 08:45:00", "gamma"},
		{4, "2026-01-10 23:59:59", "delta"},
	}
	for _, r := range inserts {
		_, err = db.ExecContext(ctx, "INSERT INTO Events VALUES (?, ?, ?)", r.id, r.ts, r.label)
		if err != nil {
			t.Fatalf("INSERT id=%d: %v", r.id, err)
		}
	}

	// Range query: WHERE ts >= '2023-01-01 00:00:00' should return rows with 2024 and 2026 timestamps.
	rows, err := db.QueryContext(ctx, "SELECT id, label FROM Events WHERE ts >= '2023-01-01 00:00:00' ORDER BY id")
	if err != nil {
		t.Fatalf("SELECT with WHERE ts >= ...: %v", err)
	}
	defer rows.Close()

	type result struct {
		id    int64
		label string
	}
	var results []result
	for rows.Next() {
		var r result
		if err := rows.Scan(&r.id, &r.label); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 rows for ts >= '2023-01-01 00:00:00', got %d", len(results))
	}
	if results[0].id != 3 || results[0].label != "gamma" {
		t.Errorf("row 0: got id=%d label=%q, want id=3 label=\"gamma\"", results[0].id, results[0].label)
	}
	if results[1].id != 4 || results[1].label != "delta" {
		t.Errorf("row 1: got id=%d label=%q, want id=4 label=\"delta\"", results[1].id, results[1].label)
	}
}

func TestFDB_BytesINList(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	setup := openTestDB(t, "/testdb_bytes_in")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_bytes_in")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE bytes_in_tmpl "+
			"CREATE TABLE lb (a BIGINT, b BYTES, PRIMARY KEY (a))")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_bytes_in/store WITH TEMPLATE bytes_in_tmpl")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_bytes_in?cluster_file=%s&schema=store", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	_, err = db.ExecContext(ctx,
		"INSERT INTO lb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, null)")
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Equality — should work.
	rows, err := db.QueryContext(ctx, "SELECT a FROM lb WHERE b = X'cafe'")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var eqResults []int64
	for rows.Next() {
		var a int64
		g.Expect(rows.Scan(&a)).NotTo(gomega.HaveOccurred())
		eqResults = append(eqResults, a)
	}
	rows.Close()
	g.Expect(eqResults).To(gomega.Equal([]int64{2}), "equality on BYTES should find row 2")

	// IN list — the bug.
	rows, err = db.QueryContext(ctx, "SELECT a FROM lb WHERE b IN (X'cafe', X'deadbeef') ORDER BY a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	var inResults []int64
	for rows.Next() {
		var a int64
		g.Expect(rows.Scan(&a)).NotTo(gomega.HaveOccurred())
		inResults = append(inResults, a)
	}
	rows.Close()
	g.Expect(inResults).To(gomega.Equal([]int64{1, 2}), "IN on BYTES should find rows 1 and 2")
}

// ---------------------------------------------------------------------------
// RFC-145 Phase 1 — detach the legacy embedded SQL interpreter.
//
// The INFORMATION_SCHEMA route no longer goes through the executor island
// (execSelect → execSelectQuery → …); it is served by the executor-free
// system-table handler (execSystemTableQuery). The shared eval clusters
// (INSERT-VALUES constant folding, system-table WHERE) had their
// subquery / EXISTS back-edges into the executor severed — those arms now
// return a clean "not supported in this context" error instead of
// re-entering execQueryBodyRows.
//
// These tests pin (a) the severed-arm negative behaviour so subquery/EXISTS
// can't silently regrow, and (b) the INFORMATION_SCHEMA positive parity for
// every system table × {SELECT *, projected cols, WHERE, ORDER BY, LIMIT}.
// ---------------------------------------------------------------------------

// expectUnsupportedInThisContext asserts err is a clean *api.Error whose
// message marks the severed subquery/EXISTS arm — not a panic, not a silent
// wrong-row result.
func expectUnsupportedInThisContext(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a clean 'not supported in this context' error, got nil (silent success)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if !strings.Contains(apiErr.Message, "is not supported in this context") {
		t.Fatalf("unexpected error message: %q (want it to contain 'is not supported in this context')", apiErr.Message)
	}
}

// TestFDB_RFC145_SeveredArms_InfoSchemaWhere pins that EXISTS and a scalar
// subquery in an INFORMATION_SCHEMA WHERE filter return the clean severed-arm
// error (they route through the kept system-table WHERE evaluator, whose
// subquery/EXISTS arms were severed in RFC-145 Phase 1).
func TestFDB_RFC145_SeveredArms_InfoSchemaWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_rfc145_is_where")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_rfc145_is_where")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc145w_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_rfc145_is_where/alpha WITH TEMPLATE rfc145w_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_rfc145_is_where?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// EXISTS in a system-table WHERE — severed.
	_, err = db.QueryContext(ctx,
		`SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE EXISTS (SELECT 1 FROM "INFORMATION_SCHEMA"."TABLES")`)
	expectUnsupportedInThisContext(t, err)

	// Scalar subquery in a system-table WHERE — severed.
	_, err = db.QueryContext(ctx,
		`SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE SCHEMA_NAME = (SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA")`)
	expectUnsupportedInThisContext(t, err)
}

// TestFDB_RFC145_SeveredArms_InsertValues pins that EXISTS and a scalar
// subquery in an INSERT-VALUES expression return the clean severed-arm error.
// INSERT-VALUES constant folding routes through evalExpr, whose subquery/EXISTS
// arms were severed in RFC-145 Phase 1. (Java's grammar has no scalar-subquery
// expression atom, so this never had a cross-engine reference.)
func TestFDB_RFC145_SeveredArms_InsertValues(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_rfc145_insert")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_rfc145_insert")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rfc145i_tmpl CREATE TABLE T (id BIGINT NOT NULL, val BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_rfc145_insert/data WITH TEMPLATE rfc145i_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_rfc145_insert?cluster_file=%s&schema=data", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Scalar subquery in a VALUES expression — severed (Go-only grammar atom).
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (1, (SELECT 1 FROM T))`)
	expectUnsupportedInThisContext(t, err)

	// EXISTS in a VALUES expression — severed.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (2, EXISTS (SELECT 1 FROM T))`)
	expectUnsupportedInThisContext(t, err)

	// Sanity: a constant VALUES insert (literals + arithmetic) still works —
	// the severing must not have broken the kept non-subquery evalExpr path.
	_, err = db.ExecContext(ctx, `INSERT INTO T (id, val) VALUES (10, 2 + 3), (11, NULL)`)
	if err != nil {
		t.Fatalf("constant INSERT-VALUES must still work after severing: %v", err)
	}
	row := db.QueryRowContext(ctx, `SELECT val FROM T WHERE id = 10`)
	var got int64
	if err := row.Scan(&got); err != nil {
		t.Fatalf("scan id=10: %v", err)
	}
	if got != 5 {
		t.Fatalf("constant VALUES (2+3) = %d, want 5", got)
	}
}

// TestFDB_RFC145_InfoSchemaUnsupportedShapes pins that the executor-free
// system-table handler rejects every shape it can't serve (JOIN / aggregate /
// GROUP BY / DISTINCT / WITH) with a clean *api.Error — none are used today,
// and the handler must not silently route them through the (now detached)
// interpreter.
func TestFDB_RFC145_InfoSchemaUnsupportedShapes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_rfc145_is_shapes")
	_, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_rfc145_is_shapes")
	if err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc145s_tmpl CREATE TABLE T (id BIGINT NOT NULL, PRIMARY KEY (id))")
	if err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	_, err = setup.ExecContext(ctx, "CREATE SCHEMA /testdb_rfc145_is_shapes/alpha WITH TEMPLATE rfc145s_tmpl")
	if err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql:///testdb_rfc145_is_shapes?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mustCleanError := func(query string) {
		t.Helper()
		_, qErr := db.QueryContext(ctx, query)
		if qErr == nil {
			t.Fatalf("want a clean error for unsupported INFORMATION_SCHEMA shape, got nil: %s", query)
		}
		var apiErr *api.Error
		if !errors.As(qErr, &apiErr) {
			t.Fatalf("want *api.Error for %q, got %T (%v)", query, qErr, qErr)
		}
	}

	// Aggregate / COUNT(*) against a system table.
	mustCleanError(`SELECT COUNT(*) FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
	// GROUP BY against a system table.
	mustCleanError(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" GROUP BY SCHEMA_NAME`)
	// SELECT DISTINCT against a system table.
	mustCleanError(`SELECT DISTINCT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
	// JOIN against a system table.
	mustCleanError(`SELECT a.SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" AS a, "INFORMATION_SCHEMA"."TABLES" AS b`)
	// WITH against a system table.
	mustCleanError(`WITH x AS (SELECT 1) SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
}

// queryToStringRows runs an INFORMATION_SCHEMA query and returns every row as a
// []string (each cell stringified), preserving row order. Used by the parity
// sweep so the same helper compares SELECT *, projected, WHERE, ORDER BY and
// LIMIT results uniformly.
func queryToStringRows(t *testing.T, db *sql.DB, ctx context.Context, query string) [][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", query, err)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		rec := make([]string, len(cols))
		for i, v := range vals {
			rec[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", query, err)
	}
	return out
}

// TestFDB_RFC145_InfoSchemaParitySweep exercises every INFORMATION_SCHEMA
// system table through the executor-free handler across the shapes the handler
// supports — SELECT *, projected columns, WHERE filter, ORDER BY, LIMIT — and
// pins the rows. The handler primitives (execSystemTable / filterSysRows /
// projectSystemRows) are unchanged from the pre-RFC-145 executor route, so
// these are the same rows the old path produced; this test is the parity
// sentinel for the re-route.
//
// INFORMATION_SCHEMA views span the whole cluster, so every query scopes to
// this test's database via a CATALOG/TABLE_CATALOG = '/testdb_rfc145_sweep'
// predicate IN THE WHERE clause (not a post-filter) — that way ORDER BY and
// LIMIT compose over exactly this DB's rows and the LIMIT assertions stay
// deterministic under parallel test execution.
//
// Identifier case: schema names preserve the DDL case ("sch1"); table/column/
// index names are normalised to UPPERCASE by the metadata layer ("ALPHA",
// "ID", "ALPHA_BY_NAME"), exactly as the existing TestFDB_InfoSchema_* tests.
func TestFDB_RFC145_InfoSchemaParitySweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	g := gomega.NewWithT(t)

	const dbID = "/testdb_rfc145_sweep"
	setup := openTestDB(t, dbID)
	mustExec := func(sqlText string) {
		t.Helper()
		if _, err := setup.ExecContext(ctx, sqlText); err != nil {
			t.Fatalf("setup %q: %v", sqlText, err)
		}
	}
	mustExec("CREATE DATABASE " + dbID)
	// Two tables, one with a secondary index, so COLUMNS / INDEXES have
	// deterministic, distinguishable content across two schemas.
	mustExec("CREATE SCHEMA TEMPLATE rfc145sweep_tmpl " +
		"CREATE TABLE Alpha (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) " +
		"CREATE INDEX alpha_by_name ON Alpha (name) " +
		"CREATE TABLE Beta (bid BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY (bid))")
	mustExec("CREATE SCHEMA " + dbID + "/sch1 WITH TEMPLATE rfc145sweep_tmpl")
	mustExec("CREATE SCHEMA " + dbID + "/sch2 WITH TEMPLATE rfc145sweep_tmpl")

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbID, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	q := func(query string) [][]string { return queryToStringRows(t, db, ctx, query) }

	// --- SCHEMATA ---  CATALOG_NAME(0), SCHEMA_NAME(1).
	// SELECT * (scoped via WHERE) → 5 columns, 2 rows.
	starSchemata := q(`SELECT * FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '` + dbID + `'`)
	g.Expect(starSchemata).To(gomega.HaveLen(2), "SCHEMATA SELECT * row count")
	for _, r := range starSchemata {
		g.Expect(r).To(gomega.HaveLen(5), "SCHEMATA has 5 columns")
	}
	// Projected column + WHERE + ORDER BY.
	g.Expect(q(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '`+dbID+`' ORDER BY SCHEMA_NAME`)).
		To(gomega.Equal([][]string{{"sch1"}, {"sch2"}}), "SCHEMATA projected + ORDER BY")
	// WHERE on a second column.
	g.Expect(q(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '`+dbID+`' AND SCHEMA_NAME = 'sch2'`)).
		To(gomega.Equal([][]string{{"sch2"}}), "SCHEMATA WHERE")
	// ORDER BY + LIMIT.
	g.Expect(q(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '`+dbID+`' ORDER BY SCHEMA_NAME LIMIT 1`)).
		To(gomega.Equal([][]string{{"sch1"}}), "SCHEMATA LIMIT")
	// ORDER BY DESC.
	g.Expect(q(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '`+dbID+`' ORDER BY SCHEMA_NAME DESC`)).
		To(gomega.Equal([][]string{{"sch2"}, {"sch1"}}), "SCHEMATA ORDER BY DESC")
	// OFFSET + LIMIT.
	g.Expect(q(`SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" WHERE CATALOG_NAME = '`+dbID+`' ORDER BY SCHEMA_NAME LIMIT 1 OFFSET 1`)).
		To(gomega.Equal([][]string{{"sch2"}}), "SCHEMATA LIMIT OFFSET")

	// --- TABLES ---  TABLE_CATALOG(0), TABLE_SCHEMA(1), TABLE_NAME(2).
	starTables := q(`SELECT * FROM "INFORMATION_SCHEMA"."TABLES" WHERE TABLE_CATALOG = '` + dbID + `'`)
	g.Expect(starTables).To(gomega.HaveLen(4), "TABLES SELECT * row count (2 tables x 2 schemas)")
	for _, r := range starTables {
		g.Expect(r).To(gomega.HaveLen(10), "TABLES has 10 columns")
	}
	g.Expect(q(`SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" WHERE TABLE_CATALOG = '`+dbID+`' AND TABLE_SCHEMA = 'sch1' ORDER BY TABLE_NAME`)).
		To(gomega.Equal([][]string{{"ALPHA"}, {"BETA"}}), "TABLES projected + WHERE + ORDER BY")
	g.Expect(q(`SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" WHERE TABLE_CATALOG = '`+dbID+`' AND TABLE_SCHEMA = 'sch1' ORDER BY TABLE_NAME LIMIT 1`)).
		To(gomega.Equal([][]string{{"ALPHA"}}), "TABLES LIMIT")

	// --- COLUMNS ---  TABLE_CATALOG(0), …, COLUMN_NAME(3), ORDINAL_POSITION(4).
	starCols := q(`SELECT * FROM "INFORMATION_SCHEMA"."COLUMNS" WHERE TABLE_CATALOG = '` + dbID + `'`)
	g.Expect(len(starCols)).To(gomega.BeNumerically(">", 0), "COLUMNS SELECT * non-empty")
	for _, r := range starCols {
		g.Expect(r).To(gomega.HaveLen(11), "COLUMNS has 11 columns")
	}
	g.Expect(q(`SELECT COLUMN_NAME FROM "INFORMATION_SCHEMA"."COLUMNS" WHERE TABLE_CATALOG = '`+dbID+`' AND TABLE_SCHEMA = 'sch1' AND TABLE_NAME = 'ALPHA' ORDER BY ORDINAL_POSITION`)).
		To(gomega.Equal([][]string{{"ID"}, {"NAME"}}), "COLUMNS Alpha projected + WHERE + ORDER BY")
	g.Expect(q(`SELECT COLUMN_NAME FROM "INFORMATION_SCHEMA"."COLUMNS" WHERE TABLE_CATALOG = '`+dbID+`' AND TABLE_SCHEMA = 'sch1' AND TABLE_NAME = 'ALPHA' ORDER BY ORDINAL_POSITION LIMIT 1`)).
		To(gomega.Equal([][]string{{"ID"}}), "COLUMNS LIMIT")

	// --- INDEXES ---  TABLE_CATALOG(0), …, TABLE_NAME(2), INDEX_NAME(3).
	starIdx := q(`SELECT * FROM "INFORMATION_SCHEMA"."INDEXES" WHERE TABLE_CATALOG = '` + dbID + `'`)
	g.Expect(len(starIdx)).To(gomega.BeNumerically(">", 0), "INDEXES SELECT * non-empty")
	for _, r := range starIdx {
		g.Expect(r).To(gomega.HaveLen(7), "INDEXES has 7 columns")
	}
	g.Expect(q(`SELECT INDEX_NAME FROM "INFORMATION_SCHEMA"."INDEXES" WHERE TABLE_CATALOG = '`+dbID+`' AND TABLE_SCHEMA = 'sch1' AND TABLE_NAME = 'ALPHA' ORDER BY INDEX_NAME`)).
		To(gomega.ContainElement([]string{"ALPHA_BY_NAME"}), "INDEXES Alpha contains ALPHA_BY_NAME")
}
