package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"

	"fdb.dev/internal/relational/keyspace"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/relational/sqldriver"
)

// A database path is /DOMAIN/DATABASE under a registered domain, as in Java
// (RelationalKeyspaceProvider.registerDomainIfNotExists, toDatabasePath): an
// unregistered domain or a one-segment path is INVALID_PATH, a malformed path
// is validateDatabaseUri's INVALID_PATH, and once registered a domain works
// like FRL.
func TestFDB_DatabaseDomainsAreRegistered(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	eng, err := sqldriver.NewEngine(sqldriver.EngineConfig{
		ClusterFile: testkit.ClusterFile(),
		Domains:     []string{"FRL"},
		Limits:      sqldriver.UnboundedJavaCompatLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	sys, err := eng.OpenDB("/__SYS")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	for stmt, want := range map[string]string{
		"CREATE DATABASE /DOMREG_NOWHERE/X": "</DOMREG_NOWHERE/X> is an invalid database path",
		"CREATE DATABASE /DOMREG_ONE":       "</DOMREG_ONE> is an invalid database path",
		"CREATE DATABASE /FRL/X/Y":          "</FRL/X/Y> is an invalid database path",
		"CREATE DATABASE /FRL/IL":           "</FRL/IL> is ambigous",
		`CREATE DATABASE "/FRL/"`:           "invalid database path '/FRL/'",
	} {
		_, err := sys.ExecContext(ctx, stmt)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidPath || apiErr.Message != want {
			t.Errorf("%s: %v, want 08F01 %q", stmt, err, want)
		}
	}

	eng.RegisterDomain("DOMREG_MARIO")
	eng.RegisterDomain("DOMREG_MARIO")
	testkit.MustExecCtx(t, sys, ctx, "CREATE DATABASE /DOMREG_MARIO/SHOP")
	t.Cleanup(func() { _, _ = sys.ExecContext(ctx, "DROP DATABASE /DOMREG_MARIO/SHOP") })
	testkit.MustExecCtx(t, sys, ctx, "CREATE SCHEMA TEMPLATE domreg_t CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, sys, ctx, "CREATE SCHEMA /DOMREG_MARIO/SHOP/S WITH TEMPLATE domreg_t")
	db, err := eng.OpenDB("/DOMREG_MARIO/SHOP", sqldriver.WithSchema("S"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (7)")
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil || id != 7 {
		t.Fatalf("read back: %d, %v", id, err)
	}

	// Legacy sql.Open("fdbsql", dsn) still enforces and honours the process-global
	// domain registry (keyspace.RegisterDomainIfNotExists).
	legacySys, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacySys.Close() })
	if _, err := legacySys.ExecContext(ctx, "CREATE DATABASE /DOMREG_LEGACY_NOWHERE/X"); err == nil {
		t.Fatal("legacy CREATE DATABASE under unregistered domain succeeded, want 08F01")
	} else {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidPath ||
			apiErr.Message != "</DOMREG_LEGACY_NOWHERE/X> is an invalid database path" {
			t.Errorf("legacy unregistered domain: %v, want 08F01", err)
		}
	}

	keyspace.RegisterDomainIfNotExists("DOMREG_LEGACY")
	keyspace.RegisterDomainIfNotExists("DOMREG_LEGACY")
	testkit.MustExecCtx(t, legacySys, ctx, "CREATE DATABASE /DOMREG_LEGACY/SHOP")
	t.Cleanup(func() { _, _ = legacySys.ExecContext(ctx, "DROP DATABASE /DOMREG_LEGACY/SHOP") })
	testkit.MustExecCtx(t, legacySys, ctx, "CREATE SCHEMA TEMPLATE domreg_legacy_t CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, legacySys, ctx, "CREATE SCHEMA /DOMREG_LEGACY/SHOP/S WITH TEMPLATE domreg_legacy_t")
	legacyDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///DOMREG_LEGACY/SHOP?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacyDB.Close() })
	testkit.MustExecCtx(t, legacyDB, ctx, "INSERT INTO t VALUES (9)")
	var legacyID int64
	if err := legacyDB.QueryRowContext(ctx, "SELECT id FROM t").Scan(&legacyID); err != nil || legacyID != 9 {
		t.Fatalf("legacy read back: %d, %v", legacyID, err)
	}
}
