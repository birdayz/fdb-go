package sqldriver_test

// Two-tenant catalog-disclosure regression.
//
// One SQL database path is one tenant. Every INFORMATION_SCHEMA view and
// SHOW DATABASES used to enumerate the whole cluster, so tenant A's connection
// could read tenant B's schema, table, column and index names. The user's WHERE
// clause is no defence — it is applied after the rows are materialised.
//
// Java scopes the equivalent JDBC metadata reads: CatalogMetaData.getSchemas()
// passes conn.getPath().getPath() into the per-database catalog.listSchemas
// overload, and getTables/getColumns/getIndexInfo refuse outright without a
// database ("Cannot scan across Databases yet").
//
// The two tenants deliberately use disjoint schema, table, column, index and
// template names, so any leak shows up as a named foreign object rather than an
// off-by-one count. Assertions pin EXACT row sets for the scoped queries; that
// stays deterministic under t.Parallel because every asserted query is confined
// to one of this test's own database paths, while other tests' databases (and
// tenant B's) are exactly what a regression would drag in.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Fixture names are suffixed per test: every test in this package shares one
// FDB cluster and runs under t.Parallel(), so two tests creating the same
// database path collide.
type mtScopeFixture struct {
	db      *sql.DB // connection bound to tenant A
	tenantA string
	tenantB string
	templA  string
	templB  string
}

// mtScopeQueryRows runs q and returns each row as a tab-joined string, sorted,
// so row order does not make the assertions brittle.
func mtScopeQueryRows(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns for %q: %v", q, err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if scanErr := rows.Scan(cells...); scanErr != nil {
			t.Fatalf("scan %q: %v", q, scanErr)
		}
		parts := make([]string, len(cols))
		for i, c := range cells {
			parts[i] = c.(*sql.NullString).String
		}
		out = append(out, strings.Join(parts, "\t"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err for %q: %v", q, err)
	}
	sort.Strings(out)
	return out
}

func mtScopeAssertRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows %v, want %d rows %v", what, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d = %q, want %q (all: %v)", what, i, got[i], want[i], got)
		}
	}
}

// mtScopeSetup creates two tenants whose schema, table, column, index and
// template names are all disjoint, and returns a connection bound to tenant A.
func mtScopeSetup(t *testing.T, ctx context.Context, suffix string) mtScopeFixture {
	t.Helper()
	f := mtScopeFixture{
		tenantA: "/mt_scope_a_" + suffix,
		tenantB: "/mt_scope_b_" + suffix,
		templA:  "mt_tmpl_a_" + suffix,
		templB:  "mt_tmpl_b_" + suffix,
	}
	setup := openTestDB(t, f.tenantA)

	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+f.tenantA)
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP DATABASE "+f.tenantA) })
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+f.tenantB)
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP DATABASE "+f.tenantB) })

	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+f.templA+" "+
			"CREATE TABLE alpha_tbl (alpha_id BIGINT, alpha_name STRING, PRIMARY KEY (alpha_id)) "+
			"CREATE INDEX alpha_idx ON alpha_tbl (alpha_name)")
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP SCHEMA TEMPLATE "+f.templA) })
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+f.templB+" "+
			"CREATE TABLE bravo_tbl (bravo_id BIGINT, bravo_name STRING, PRIMARY KEY (bravo_id)) "+
			"CREATE INDEX bravo_idx ON bravo_tbl (bravo_name)")
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP SCHEMA TEMPLATE "+f.templB) })

	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+f.tenantA+"/alpha_schema WITH TEMPLATE "+f.templA)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+f.tenantB+"/bravo_schema WITH TEMPLATE "+f.templB)

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=alpha_schema", f.tenantA, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open tenant A: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f.db = db
	return f
}

func TestFDB_MultiTenantCatalogScoping(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	f := mtScopeSetup(t, ctx, "cat")
	db := f.db

	// Every assertion below is an EXACT row set for tenant A. Tenant B's
	// objects exist in the same catalog throughout, so an unscoped read cannot
	// pass by coincidence.

	t.Run("schemata_only_own_database", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx,
			`SELECT CATALOG_NAME, SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
		mtScopeAssertRows(t, "SCHEMATA", got, []string{f.tenantA + "\tALPHA_SCHEMA"})
	})

	t.Run("tables_only_own_database", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx,
			`SELECT TABLE_CATALOG, TABLE_SCHEMA, TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES"`)
		mtScopeAssertRows(t, "TABLES", got,
			[]string{f.tenantA + "\tALPHA_SCHEMA\tALPHA_TBL"})
	})

	t.Run("columns_only_own_database", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx,
			`SELECT TABLE_CATALOG, TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME FROM "INFORMATION_SCHEMA"."COLUMNS"`)
		mtScopeAssertRows(t, "COLUMNS", got, []string{
			f.tenantA + "\tALPHA_SCHEMA\tALPHA_TBL\tALPHA_ID",
			f.tenantA + "\tALPHA_SCHEMA\tALPHA_TBL\tALPHA_NAME",
		})
	})

	t.Run("indexes_only_own_database", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx,
			`SELECT TABLE_CATALOG, TABLE_SCHEMA, TABLE_NAME, INDEX_NAME FROM "INFORMATION_SCHEMA"."INDEXES"`)
		for _, row := range got {
			if !strings.HasPrefix(row, f.tenantA+"\t") {
				t.Fatalf("INDEXES leaked a foreign database: %q (all: %v)", row, got)
			}
		}
		// The exact index set depends on which implicit indexes the template
		// compiler emits, so pin the tenant-B index by NAME as absent and the
		// tenant-A one as present — both are unmistakable either way.
		var sawAlpha bool
		for _, row := range got {
			if strings.HasSuffix(row, "\tALPHA_IDX") {
				sawAlpha = true
			}
			if strings.Contains(row, "BRAVO") {
				t.Fatalf("INDEXES leaked tenant B: %q", row)
			}
		}
		if !sawAlpha {
			t.Fatalf("INDEXES did not report tenant A's own ALPHA_IDX: %v", got)
		}
	})

	// A WHERE clause naming the foreign database must not resurrect it. This is
	// the dimension the original bug hid behind: filterSysRows runs after
	// materialisation, so before the fix these rows were read and then filtered,
	// and a predicate SELECTING the foreign tenant returned them.
	t.Run("where_cannot_reach_foreign_database", func(t *testing.T) {
		for _, view := range []string{"SCHEMATA", "TABLES", "COLUMNS", "INDEXES"} {
			col := "TABLE_CATALOG"
			if view == "SCHEMATA" {
				col = "CATALOG_NAME"
			}
			q := fmt.Sprintf(`SELECT %s FROM "INFORMATION_SCHEMA".%q WHERE %s = '%s'`,
				col, view, col, f.tenantB)
			if got := mtScopeQueryRows(t, db, ctx, q); len(got) != 0 {
				t.Fatalf("%s WHERE %s='%s' returned %v, want no rows", view, col, f.tenantB, got)
			}
		}
	})

	t.Run("show_databases_defaults_to_session_database", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx, "SHOW DATABASES")
		mtScopeAssertRows(t, "SHOW DATABASES", got, []string{f.tenantA})
	})

	// Java's MetadataPlanVisitor.visitShowDatabasesStatement reads ctx.path()
	// and lets an explicit prefix win over the connection's dbUri. Go parsed the
	// same clause and threw the path away. (Java's own CatalogQueryFactory then
	// drops the prefix it was handed — "TODO(bfines) make use of this prefix" —
	// so upstream lists everything; we honour the scope the visitor states.)
	t.Run("show_databases_with_prefix_is_honored", func(t *testing.T) {
		got := mtScopeQueryRows(t, db, ctx, "SHOW DATABASES WITH PREFIX "+f.tenantB)
		mtScopeAssertRows(t, "SHOW DATABASES WITH PREFIX B", got, []string{f.tenantB})

		got = mtScopeQueryRows(t, db, ctx, "SHOW DATABASES WITH PREFIX "+f.tenantA)
		mtScopeAssertRows(t, "SHOW DATABASES WITH PREFIX A", got, []string{f.tenantA})
	})

	// Segment granularity: /mt_scope_tenant_a must not swallow a sibling whose
	// path merely starts with those characters.
	t.Run("prefix_match_is_segment_granular", func(t *testing.T) {
		sibling := f.tenantA + "_extra"
		mwjoMustExec(t, db, ctx, "CREATE DATABASE "+sibling)
		t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP DATABASE "+sibling) })

		got := mtScopeQueryRows(t, db, ctx, "SHOW DATABASES WITH PREFIX "+f.tenantA)
		mtScopeAssertRows(t, "SHOW DATABASES segment granularity", got, []string{f.tenantA})
	})

	// Nested paths ARE in scope — containment, not equality.
	t.Run("prefix_includes_nested_databases", func(t *testing.T) {
		nested := f.tenantA + "/sub"
		mwjoMustExec(t, db, ctx, "CREATE DATABASE "+nested)
		t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP DATABASE "+nested) })

		got := mtScopeQueryRows(t, db, ctx, "SHOW DATABASES")
		mtScopeAssertRows(t, "SHOW DATABASES nested", got, []string{f.tenantA, nested})
	})
}

// TestFDB_MultiTenantSchemaTemplatesAreGlobal pins a NEGATIVE result: schema
// templates are NOT scoped per tenant, and deliberately so.
//
// A schema template is a cluster-global object in the catalog wire format — the
// Templates record carries only TEMPLATE_NAME, TEMPLATE_VERSION and META_DATA,
// with no owning database — and Java agrees: getListSchemaTemplatesQueryAction
// takes no database argument and calls listTemplates(txn) unscoped. Scoping
// SHOW SCHEMA TEMPLATES would require adding an owner field to a record Java
// also reads and writes, i.e. a catalog wire-format change.
//
// If this test ever fails because templates became scoped, the change was a
// wire-format change to the Templates record and needs Java interop review, not
// a fix to this assertion. Tenants that need private templates namespace the
// template NAME.
func TestFDB_MultiTenantSchemaTemplatesAreGlobal(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	f := mtScopeSetup(t, ctx, "tmpl")
	db := f.db

	// Template names are stored verbatim — execCreateSchemaTemplate only strips
	// identifier quotes and does not upper-case, unlike schema names.
	ownName := f.templA
	foreignName := f.templB

	got := mtScopeQueryRows(t, db, ctx, "SHOW SCHEMA TEMPLATES")
	var sawOwn, sawForeign bool
	for _, row := range got {
		if strings.HasPrefix(row, ownName+"\t") {
			sawOwn = true
		}
		if strings.HasPrefix(row, foreignName+"\t") {
			sawForeign = true
		}
	}
	if !sawOwn {
		t.Fatalf("SHOW SCHEMA TEMPLATES omitted this tenant's own template: %v", got)
	}
	if !sawForeign {
		t.Fatalf("SHOW SCHEMA TEMPLATES no longer lists the other tenant's template. "+
			"Templates are cluster-global by wire format (the Templates record has no "+
			"owning database) and global in Java. If they were scoped, the Templates "+
			"record changed shape and Java interop needs review. rows=%v", got)
	}
}
