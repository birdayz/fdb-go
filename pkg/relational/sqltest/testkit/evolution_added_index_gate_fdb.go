package testkit

// An index added by METADATA EVOLUTION has no index-state key until the store
// header is reconciled at store open (checkPossiblyRebuild). Two behaviours
// decide what a query issued in that window sees, and they are only correct
// TOGETHER — each one alone is a live defect:
//
//  1. WHAT RECONCILIATION DOES. getRecordCountForRebuildPolicy has no cheap
//     count to consult (the relational metadata sets no record-count key), so
//     it does what Java's getRecordCountForRebuildIndexes does
//     (FDBRecordStore.java:4862-4884): scan for a single record and report
//     MAX_VALUE for a non-empty store. Above MAX_RECORDS_FOR_REBUILD the
//     evolution-added index is therefore left DISABLED for a background build
//     instead of being rebuilt INLINE inside the store-open transaction — an
//     inline build on a large store is a full index build against the 5s /
//     10MB / 100k-key limits.
//
//  2. WHAT THE PLANNER SEES. fetchReadableIndexes takes the readable-index
//     view off the OPENED store (PlanContext.java:249-252 does exactly that),
//     so the state it consults is the state reconciliation has already
//     settled. Read speculatively from the index-state subspace instead, and
//     an evolution-added index — which has no state key yet — reads as
//     READABLE and gets planned into a scan the store then refuses.
//
// Fixing 1 without 2 ARMS the planner hole: the index really is DISABLED and
// the planner really would pick it. Fixing 2 without 1 hides it: everything is
// rebuilt inline, so the planner's guess is never wrong.
//
// The tests below pin the OBSERVABLE contract on both axes. Above the
// threshold: the query returns CORRECT rows, the plan does NOT name the
// unbuilt index, and the index really is DISABLED (so the fallback is being
// exercised, not bypassed by an inline rebuild). Below it: the inline build
// still happens, as it does in Java, and the index IS used.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// evolHarness wires a catalog over the same keyspace the sqldriver uses, so a
// template evolved here is the template the driver plans against. The evolution
// path itself is catalog-level (SaveSchemaTemplate at a higher version, then
// RepairSchema to rebind) because the SQL surface has no statement that rebinds
// a live schema — CREATE SCHEMA TEMPLATE only writes the template.
type EvolHarness struct {
	db  *recordlayer.FDBDatabase
	Cat *catalog.RecordLayerStoreCatalog
	Ks  *keyspace.RelationalKeyspace
}

func NewEvolHarness(t *testing.T) *EvolHarness {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return &EvolHarness{db: recordlayer.NewFDBDatabase(rawDB), Cat: cat, Ks: ks}
}

func (h *EvolHarness) MustRun(t *testing.T, what string, fn func(txn api.Transaction) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := h.db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// evolReopen returns a FRESH *sql.DB, so the query below plans against the
// evolved metadata with nothing cached from before the rebind.
func EvolReopen(t *testing.T, dbPath, schemaName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// evolIndexStates reads the schema's index-state subspace exactly as the
// planner does (recordlayer.LoadIndexStates, the same call fetchReadableIndexes
// makes). An empty map is "every index readable" as far as the planner knows.
func EvolIndexStates(t *testing.T, dbPath, schemaName string) map[string]recordlayer.IndexState {
	t.Helper()
	rawDB, dbErr := fdb.OpenDatabase(clusterFilePath)
	if dbErr != nil {
		t.Fatalf("open db: %v", dbErr)
	}
	ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(context.Background(),
		recordlayer.NewFDBDatabase(rawDB), dbPath, strings.ToUpper(schemaName))
	if err != nil {
		t.Fatalf("schema subspace: %v", err)
	}
	res, err := rawDB.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return recordlayer.LoadIndexStates(rtx, ss)
	})
	if err != nil {
		t.Fatalf("load index states: %v", err)
	}
	return res.(map[string]recordlayer.IndexState)
}
