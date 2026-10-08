package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
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
	sys, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sys.Close() })
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

	sqldriver.RegisterDomainIfNotExists("DOMREG_MARIO")
	sqldriver.RegisterDomainIfNotExists("DOMREG_MARIO")
	testkit.MustExecCtx(t, sys, ctx, "CREATE DATABASE /DOMREG_MARIO/SHOP")
	t.Cleanup(func() { _, _ = sys.ExecContext(ctx, "DROP DATABASE /DOMREG_MARIO/SHOP") })
	testkit.MustExecCtx(t, sys, ctx, "CREATE SCHEMA TEMPLATE domreg_t CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, sys, ctx, "CREATE SCHEMA /DOMREG_MARIO/SHOP/S WITH TEMPLATE domreg_t")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///DOMREG_MARIO/SHOP?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (7)")
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil || id != 7 {
		t.Fatalf("read back: %d, %v", id, err)
	}
}
