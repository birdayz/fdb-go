package keyspace_test

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/simfdb"
)

// TestResolvedSchemaSubspace is the no-I/O lookup the SQL layer's store
// prefetch uses: nothing before a resolution, the resolved subspace after one
// the keyspace committed in its own transaction, and nothing for a resolution
// made in an outside transaction, which may still roll back (the same rule
// the keyspace's cache follows for SchemaSubspace).
func TestResolvedSchemaSubspace(t *testing.T) {
	t.Parallel()
	keyspace.RegisterDomainIfNotExists("FRL")
	ctx := context.Background()
	env := dst.NewSim(41)
	env.Buggify = dst.DisabledBuggifier()
	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	ks := keyspace.New(subspace.Sub())

	if _, ok := ks.ResolvedSchemaSubspace("/FRL/DB", "S"); ok {
		t.Fatal("ResolvedSchemaSubspace answered before any resolution")
	}
	want, err := ks.SchemaSubspaceIn(ctx, db, "/FRL/DB", "S")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ks.ResolvedSchemaSubspace("/FRL/DB", "S")
	if !ok || !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("ResolvedSchemaSubspace after SchemaSubspaceIn = %v, %v; want %x", got, ok, want.Bytes())
	}
	if _, ok := ks.ResolvedSchemaSubspace("/FRL/DB", "T"); ok {
		t.Fatal("ResolvedSchemaSubspace answered for a schema never resolved")
	}
	if _, ok := keyspace.New(subspace.Sub()).ResolvedSchemaSubspace("/FRL/DB", "S"); ok {
		t.Fatal("a fresh keyspace answered from another keyspace's resolution")
	}

	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Cancel()
	outside := recordlayer.NewFDBRecordContext(tx, nil)
	if _, err := ks.SchemaSubspace(outside, "/FRL/DB", "U"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ks.ResolvedSchemaSubspace("/FRL/DB", "U"); ok {
		t.Fatal("ResolvedSchemaSubspace answered from a resolution in an uncommitted outside transaction")
	}
}
