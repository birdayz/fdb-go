package testkit

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/keyspace"
)

// relationalStoreSubspace is the record-store subspace of a schema the driver
// created: Java's (domain, database, schema) longs, looked up (never interned)
// in the cluster under test. Names are the ones CREATE DATABASE / CREATE SCHEMA
// stored (an unquoted path or schema folds to upper case).
func RelationalStoreSubspace(t *testing.T, dbPath, schema string) subspace.Subspace {
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
