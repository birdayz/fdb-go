package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// ?transaction_timeout= bounds the connection's transactions on a real
// cluster, as Java's TRANSACTION_TIMEOUT connection option sets the FDB
// transaction timeout of every connection transaction.
func TestFDB_DSNTransactionTimeout(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_dsn_txtimeout")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_dsn_txtimeout")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dsn_txtimeout CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_dsn_txtimeout/s WITH TEMPLATE dsn_txtimeout")
	for _, tc := range []struct {
		timeout string
		expire  bool
	}{{"1000", true}, {"0", false}, {"-1", false}} {
		t.Run(tc.timeout, func(t *testing.T) {
			db, err := sql.Open("fdbsql", fmt.Sprintf(
				"fdbsql:///FRL/TESTDB_DSN_TXTIMEOUT?cluster_file=%s&schema=S&transaction_timeout=%s",
				testkit.ClusterFile(), tc.timeout))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			// Warm the metadata caches, whose own transactions carry the timeout too.
			testkit.MustExecCtx(t, db, ctx, "DELETE FROM t WHERE id = 1")
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback() //nolint:errcheck // the outcome is asserted below
			// The FDB timeout runs from transaction creation, so this outlives 1000 ms.
			time.Sleep(1500 * time.Millisecond)
			_, err = tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
			if err == nil {
				err = tx.Commit()
			}
			var apiErr *api.Error
			timedOut := errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeTransactionTimeout
			if timedOut != tc.expire || (!tc.expire && err != nil) {
				t.Fatalf("transaction_timeout=%s: %v (want timed out: %v)", tc.timeout, err, tc.expire)
			}
			if !tc.expire {
				testkit.MustExecCtx(t, db, ctx, "DELETE FROM t WHERE id = 1")
			}
		})
	}
}
