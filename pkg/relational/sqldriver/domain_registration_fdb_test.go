package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/sqldriver"
)

// A database path is /DOMAIN/DATABASE under a registered domain, as in Java
// (RelationalKeyspaceProvider.registerDomainIfNotExists, toDatabasePath): an
// unregistered domain or a one-segment path is INVALID_PATH, a malformed path
// is validateDatabaseUri's INVALID_PATH, and once registered a domain works
// like FRL.
func TestFDB_DatabaseDomainsAreRegistered(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	sys, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
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
	mwjoMustExec(t, sys, ctx, "CREATE DATABASE /DOMREG_MARIO/SHOP")
	t.Cleanup(func() { _, _ = sys.ExecContext(ctx, "DROP DATABASE /DOMREG_MARIO/SHOP") })
	mwjoMustExec(t, sys, ctx, "CREATE SCHEMA TEMPLATE domreg_t CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, sys, ctx, "CREATE SCHEMA /DOMREG_MARIO/SHOP/S WITH TEMPLATE domreg_t")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///DOMREG_MARIO/SHOP?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (7)")
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil || id != 7 {
		t.Fatalf("read back: %d, %v", id, err)
	}
}

// relationalStoreSubspace is the record-store subspace of a schema the driver
// created: Java's (domain, database, schema) longs, looked up (never interned)
// in the cluster under test. Names are the ones CREATE DATABASE / CREATE SCHEMA
// stored (an unquoted path or schema folds to upper case).
func relationalStoreSubspace(t *testing.T, dbPath, schema string) subspace.Subspace {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(context.Background(),
		recordlayer.NewFDBDatabase(rawDB), dbPath, schema)
	if err != nil {
		t.Fatalf("store subspace of %s/%s: %v", dbPath, schema, err)
	}
	return ss
}
