package sqldriver_test

// RESTRICT_DDL_TO_SESSION_DATABASE — the opt-in confinement of DDL to the
// connection's own database — plus the unconditional /__SYS CREATE guard.
//
// The default (option OFF) is the Java-parity contract and is pinned here just
// as hard as the ON behaviour. Java's SemanticAnalyzer.parseSchemaURI splits a
// qualified "/db/SCHEMA" identifier lexically and never compares it to the
// connection's database, so `DROP SCHEMA /other/S` and `DROP DATABASE /other`
// are accepted from any connection; Java assumes authorization above the SQL
// engine. Flipping that default would diverge from Java on the shared surface,
// so if the OFF assertions below ever fail, the default changed and that is a
// conformance break, not a test to update.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// mtDDLAssert42501 requires err to be the cross-database DDL rejection: SQLSTATE
// 42501 on the *api.Error, and a *api.CrossDatabaseDDLError in the chain naming
// the session and target databases. Both are asserted structurally — no string
// matching on the message, whose wording is not API.
func mtDDLAssert42501(t *testing.T, what string, err error, wantSession, wantTarget string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected rejection, got nil error", what)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("%s: error is not an *api.Error: %v", what, err)
	}
	if apiErr.Code != api.ErrCodeInsufficientPrivilege {
		t.Fatalf("%s: SQLSTATE = %q, want %q (%v)",
			what, apiErr.Code, api.ErrCodeInsufficientPrivilege, err)
	}
	var scopeErr *api.CrossDatabaseDDLError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("%s: error chain has no *api.CrossDatabaseDDLError: %v", what, err)
	}
	if scopeErr.SessionDatabase != wantSession {
		t.Errorf("%s: SessionDatabase = %q, want %q", what, scopeErr.SessionDatabase, wantSession)
	}
	if scopeErr.TargetDatabase != wantTarget {
		t.Errorf("%s: TargetDatabase = %q, want %q", what, scopeErr.TargetDatabase, wantTarget)
	}
}

// mtDDLAssertTemplate42501 requires err to be the schema-template refusal:
// 42501 on the *api.Error and a *api.SchemaTemplateDDLRestrictedError in the
// chain. Unlike the cross-database rejection it names no target database,
// because a template has none.
func mtDDLAssertTemplate42501(t *testing.T, what string, err error, wantSession string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected rejection, got nil error", what)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("%s: error is not an *api.Error: %v", what, err)
	}
	if apiErr.Code != api.ErrCodeInsufficientPrivilege {
		t.Fatalf("%s: SQLSTATE = %q, want %q (%v)",
			what, apiErr.Code, api.ErrCodeInsufficientPrivilege, err)
	}
	var tmplErr *api.SchemaTemplateDDLRestrictedError
	if !errors.As(err, &tmplErr) {
		t.Fatalf("%s: error chain has no *api.SchemaTemplateDDLRestrictedError: %v", what, err)
	}
	if tmplErr.Operation != what {
		t.Errorf("%s: Operation = %q, want %q", what, tmplErr.Operation, what)
	}
	if tmplErr.SessionDatabase != wantSession {
		t.Errorf("%s: SessionDatabase = %q, want %q", what, tmplErr.SessionDatabase, wantSession)
	}
}

// mtDDLOpen opens a connection to dbPath, optionally with the restriction on.
func mtDDLOpen(t *testing.T, dbPath string, restrict bool) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFilePath)
	if restrict {
		dsn += "&restrict_ddl_to_session_database=true"
	}
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open %q: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_RestrictDDLToSessionDatabase(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const (
		home    = "/mt_ddl_home"
		foreign = "/mt_ddl_foreign"
	)

	setup := openTestDB(t, home)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+home)
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP DATABASE "+home) })
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+foreign)
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP DATABASE "+foreign) })
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mt_ddl_tmpl "+
			"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	t.Cleanup(func() { _, _ = setup.ExecContext(ctx, "DROP SCHEMA TEMPLATE mt_ddl_tmpl") })
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+foreign+"/victim WITH TEMPLATE mt_ddl_tmpl")

	t.Run("restricted_rejects_cross_database_ddl", func(t *testing.T) {
		db := mtDDLOpen(t, home, true)

		_, err := db.ExecContext(ctx, "DROP DATABASE "+foreign)
		mtDDLAssert42501(t, "DROP DATABASE /foreign", err, home, foreign)

		_, err = db.ExecContext(ctx, "CREATE DATABASE /mt_ddl_intruder")
		mtDDLAssert42501(t, "CREATE DATABASE /other", err, home, "/mt_ddl_intruder")

		_, err = db.ExecContext(ctx, "DROP SCHEMA "+foreign+"/victim")
		mtDDLAssert42501(t, "DROP SCHEMA /foreign/victim", err, home, foreign)

		_, err = db.ExecContext(ctx, "CREATE SCHEMA "+foreign+"/planted WITH TEMPLATE mt_ddl_tmpl")
		mtDDLAssert42501(t, "CREATE SCHEMA /foreign/planted", err, home, foreign)

		// The rejections must be rejections, not slow successes: the foreign
		// schema and database both survive, verified from the unrestricted
		// setup connection.
		got := mtScopeQueryRows(t, setup, ctx,
			`SELECT CATALOG_NAME, SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA" `+
				`WHERE CATALOG_NAME = '`+foreign+`'`)
		if len(got) != 0 {
			t.Fatalf("setup conn is bound to %s, so it must not see %s schemas: %v", home, foreign, got)
		}
		fdb := mtDDLOpen(t, foreign, false)
		got = mtScopeQueryRows(t, fdb, ctx,
			`SELECT CATALOG_NAME, SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
		mtScopeAssertRows(t, "foreign survived", got, []string{foreign + "\tVICTIM"})
	})

	t.Run("restricted_allows_own_database_ddl", func(t *testing.T) {
		db := mtDDLOpen(t, home, true)

		// Unqualified and fully-qualified forms of the connection's OWN
		// database both pass — the check runs on the RESOLVED path, so the
		// unqualified form resolves to the session database and is allowed.
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA "+home+"/own_qualified WITH TEMPLATE mt_ddl_tmpl")
		mwjoMustExec(t, db, ctx, "DROP SCHEMA "+home+"/own_qualified")
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA own_bare WITH TEMPLATE mt_ddl_tmpl")
		mwjoMustExec(t, db, ctx, "DROP SCHEMA own_bare")

		// Containment, not equality: a database nested under the session's own
		// path is inside the tenant's scope.
		nested := home + "/nested"
		mwjoMustExec(t, db, ctx, "CREATE DATABASE "+nested)
		mwjoMustExec(t, db, ctx, "DROP DATABASE "+nested)
	})

	// Segment granularity: /mt_ddl_home must not confer authority over a
	// database whose path merely starts with those characters.
	t.Run("restricted_scope_is_segment_granular", func(t *testing.T) {
		db := mtDDLOpen(t, home, true)
		const lookalike = home + "_lookalike"
		_, err := db.ExecContext(ctx, "CREATE DATABASE "+lookalike)
		mtDDLAssert42501(t, "CREATE DATABASE lookalike", err, home, lookalike)
	})

	// Schema-template DDL bypassed the restriction entirely: both handlers
	// called runDDL directly. Templates are cluster-global (the Templates
	// record has no owning database) and a schema resolves its stored template
	// version when it loads, so a restricted tenant could DROP a template
	// another tenant's schemas were created from — leaving those schemas
	// unloadable — or re-mint a name at a version they already reference.
	// Neither is expressible as "inside or outside my database", so a
	// restricted connection gets no template DDL at all.
	t.Run("restricted_rejects_schema_template_ddl", func(t *testing.T) {
		db := mtDDLOpen(t, home, true)

		_, err := db.ExecContext(ctx, "DROP SCHEMA TEMPLATE mt_ddl_tmpl")
		mtDDLAssertTemplate42501(t, "DROP SCHEMA TEMPLATE", err, home)

		_, err = db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE mt_ddl_intruder_tmpl CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
		mtDDLAssertTemplate42501(t, "CREATE SCHEMA TEMPLATE", err, home)

		// The refusal must be a refusal: the template the restricted connection
		// tried to drop is still usable, proven by creating a schema from it on
		// an unrestricted connection.
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+home+"/tmpl_survived WITH TEMPLATE mt_ddl_tmpl")
		mwjoMustExec(t, setup, ctx, "DROP SCHEMA "+home+"/tmpl_survived")
	})

	// The Java-parity default. This is a CONTRACT, not an accident.
	t.Run("unrestricted_is_java_parity", func(t *testing.T) {
		const target = "/mt_ddl_parity_target"
		db := mtDDLOpen(t, home, false)

		// Cross-database CREATE/DROP DATABASE from a foreign connection: Java
		// accepts these, so Go must too when the option is off.
		mwjoMustExec(t, db, ctx, "CREATE DATABASE "+target)
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA "+target+"/cross WITH TEMPLATE mt_ddl_tmpl")

		tdb := mtDDLOpen(t, target, false)
		got := mtScopeQueryRows(t, tdb, ctx,
			`SELECT CATALOG_NAME, SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
		mtScopeAssertRows(t, "cross-database CREATE SCHEMA took effect", got,
			[]string{target + "\tCROSS"})

		mwjoMustExec(t, db, ctx, "DROP SCHEMA "+target+"/cross")
		mwjoMustExec(t, db, ctx, "DROP DATABASE "+target)
	})
}

// TestFDB_CreateDatabaseRefusesSystemCatalogSpace pins the unconditional guard.
//
// DROP already refused /__SYS; CREATE did not, so a nested path could be
// planted inside the catalog's own keyspace (Java: RecordLayerStoreCatalog's
// catalogSchemaPath is /__SYS/CATALOG). Java has no reserved-name guard on
// CREATE at all — there `CREATE DATABASE /__SYS` fails only incidentally, via
// doesDatabaseExist → DATABASE_ALREADY_EXISTS, and the nested paths are not
// caught at all. Refusing the space outright cannot conflict with wire compat:
// it forbids writes Java never intended to allow.
func TestFDB_CreateDatabaseRefusesSystemCatalogSpace(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/mt_sys_guard")

	for _, path := range []string{"/__SYS/anything", "/__SYS/CATALOG", "/__SYS"} {
		_, err := db.ExecContext(ctx, "CREATE DATABASE "+path)
		if err == nil {
			t.Fatalf("CREATE DATABASE %s: expected rejection, got success", path)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("CREATE DATABASE %s: not an *api.Error: %v", path, err)
		}
		if apiErr.Code != api.ErrCodeInsufficientPrivilege {
			t.Fatalf("CREATE DATABASE %s: SQLSTATE = %q, want %q (%v)",
				path, apiErr.Code, api.ErrCodeInsufficientPrivilege, err)
		}
	}

	// A path that merely starts with the same characters is a normal database.
	const lookalike = "/__SYSTEM_mt_guard"
	mwjoMustExec(t, db, ctx, "CREATE DATABASE "+lookalike)
	mwjoMustExec(t, db, ctx, "DROP DATABASE "+lookalike)
}
