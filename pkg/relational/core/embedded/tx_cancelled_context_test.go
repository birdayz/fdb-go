package embedded

// RFC-198 criterion 9(b): a page fetched on a CANCELLED transaction context
// surfaces as 25F01, pinning the 1025 (transaction_cancelled) lane in
// translateFDBCode and the FDB tail of translateExecError.
//
// This is deliberately NOT one of Decision 3's four doors: every door sets the
// terminal flag and is asserted BEFORE the page runs. The 1025 lane covers the
// path the flag cannot — the FDB-level cancellation of the underlying
// transaction with no door involved (the client resolves every op on a
// cancelled handle with 1025; see pkg/fdbgo/client checkCancelled, matching
// C++ ReadYourWrites cancel()). An internal test, because reaching a live
// *embeddedTx's record context without tripping a door requires package
// access; the door-covered shapes live in
// sqldriver/tx_isolation_rfc198_fdb_test.go.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/simfdb"
)

// newSimConnection builds an EmbeddedConnection over a SimFDB backend with a
// bootstrapped database/schema, mirroring the sqldriver Connector.initialize
// recipe without going through database/sql.
func newSimConnection(t *testing.T, seed uint64) *EmbeddedConnection {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	fdbDB := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	factory := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)

	c := New("/simdb", fdbDB, cat, factory, ks)
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE /simdb",
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl",
	} {
		if _, err := c.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	c.SetSchema("s")
	return c
}

func TestCancelledTxContextPageFetchIs25F01(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 43)
	for i := 0; i < 8; i++ {
		if _, err := c.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Force multi-page pagination so a page fetch happens after the cancel.
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())

	if _, err := c.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if err != nil {
		t.Fatalf("in-tx query: %v", err)
	}
	defer rows.Close() //nolint:errcheck

	// Cancel the UNDERLYING record context directly — no door, so the
	// terminal flag stays clear and the next page genuinely reaches the
	// cancelled FDB transaction.
	c.activeTx.rctx.Cancel()

	dest := make([]driver.Value, 1)
	var iterErr error
	for {
		iterErr = rows.Next(dest)
		if iterErr != nil {
			break
		}
	}
	if iterErr == io.EOF {
		t.Fatalf("iteration completed cleanly on a cancelled transaction context — " +
			"the pages were served from somewhere other than the cancelled transaction")
	}
	var apiErr *api.Error
	if !errors.As(iterErr, &apiErr) {
		t.Fatalf("page fetch on a cancelled transaction context failed with %v (%T), "+
			"want *api.Error 25F01 (the 1025 transaction_cancelled lane)", iterErr, iterErr)
	}
	if apiErr.Code != api.ErrCodeTransactionInactive {
		t.Fatalf("page fetch on a cancelled transaction context: SQLSTATE %s, want %s (25F01): %v",
			apiErr.Code, api.ErrCodeTransactionInactive, iterErr)
	}
}
