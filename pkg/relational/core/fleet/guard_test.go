package fleet

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TestGuardNotCatalogRefusesSystemDatabase pins the guard on the target the
// catalog itself hands back. RecordLayerStoreCatalog.Initialize persists a
// /__SYS database row and a /__SYS/CATALOG schema row, so an unfiltered
// ListSchemas really does enumerate the catalog as an ordinary fan-out target.
func TestGuardNotCatalogRefusesSystemDatabase(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub())
	target := Target{DatabaseID: catalog.SysDatabaseID, SchemaName: catalog.CatalogConstant}

	err := GuardNotCatalog(ks, target)
	if err == nil {
		t.Fatal("GuardNotCatalog accepted the relational catalog as a fan-out target.\n" +
			"The catalog self-registers (/__SYS + CATALOG rows written by Initialize), so an " +
			"unfiltered fan-out WILL reach it. Accepting it means the fan-out rebinds or " +
			"index-builds the catalog that describes every tenant.")
	}
	var cte *CatalogTargetError
	if !errors.As(err, &cte) {
		t.Fatalf("want *CatalogTargetError, got %T: %v", err, err)
	}
}

// TestGuardNotCatalogSubspaceCheckCannotCatchTheCatalogSchema is a NEGATIVE
// result, pinned because it is the reason the name check exists at all.
//
// The frl CLI guards writes with a byte-prefix overlap test against
// CatalogSubspace. That test CANNOT fire for the catalog's own schema row,
// because the two keys have different shapes: CatalogSubspace is the 3-tuple
// ("__SYS","__SYS","CATALOG") while any schema store is the 2-tuple
// (dbPath, schemaName) — here ("/__SYS","CATALOG"). Different first element,
// different arity, no shared prefix.
//
// If this test ever goes red, the keyspace layout has changed so that the
// subspace check DOES cover the catalog schema, and the name check may be
// reconsidered. Until then, deleting the name check silently re-arms
// fan-out-writes-the-catalog.
func TestGuardNotCatalogSubspaceCheckCannotCatchTheCatalogSchema(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub())

	catalogBytes := ks.CatalogSubspace().Bytes()
	schemaSS, err := ks.SchemaSubspace(catalog.SysDatabaseID, catalog.CatalogConstant)
	if err != nil {
		t.Fatalf("SchemaSubspace: %v", err)
	}
	target := schemaSS.Bytes()

	if bytes.HasPrefix(target, catalogBytes) || bytes.HasPrefix(catalogBytes, target) {
		t.Fatalf("the catalog subspace and the (%s,%s) schema subspace NOW overlap\n"+
			"  catalog: %q\n  schema:  %q\n"+
			"The subspace-overlap check alone would now catch the catalog schema. That is a "+
			"keyspace-layout change, not a bug in this test — re-evaluate whether "+
			"GuardNotCatalog still needs its separate name check.",
			catalog.SysDatabaseID, catalog.CatalogConstant, catalogBytes, target)
	}
}

// TestGuardNotCatalogAllowsTenantSchemas keeps the guard from being a blanket
// refusal — a guard that refuses everything passes the test above while
// breaking every real fan-out. A schema legitimately NAMED "CATALOG" inside a
// user database must be accepted: it is not the system catalog.
func TestGuardNotCatalogAllowsTenantSchemas(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub())
	for _, target := range []Target{
		{DatabaseID: "/tenants/acme", SchemaName: "PUBLIC"},
		{DatabaseID: "/tenants/globex", SchemaName: "S1"},
		{DatabaseID: "/tenants/initech", SchemaName: catalog.CatalogConstant},
	} {
		if err := GuardNotCatalog(ks, target); err != nil {
			t.Errorf("GuardNotCatalog refused legitimate tenant %s: %v", target, err)
		}
	}
}

// TestFilterByTemplateIsCaseInsensitiveAndTotal pins the two properties a
// fan-out selection depends on: an empty filter selects everything (so
// "migrate this database" does not silently become "migrate nothing"), and
// matching ignores case the way the catalog's own identifiers do.
func TestFilterByTemplateIsCaseInsensitiveAndTotal(t *testing.T) {
	t.Parallel()
	targets := []Target{
		{DatabaseID: "/db", SchemaName: "A", TemplateName: "TMPL"},
		{DatabaseID: "/db", SchemaName: "B", TemplateName: "OTHER"},
		{DatabaseID: "/db", SchemaName: "C", TemplateName: "tmpl"},
	}
	if got := FilterByTemplate(targets, ""); len(got) != 3 {
		t.Fatalf("empty template filter selected %d targets, want all 3", len(got))
	}
	got := FilterByTemplate(targets, "tmpl")
	if len(got) != 2 {
		t.Fatalf("FilterByTemplate(%q) selected %d, want 2 (case-insensitive)", "tmpl", len(got))
	}
	for _, g := range got {
		if g.SchemaName == "B" {
			t.Fatalf("FilterByTemplate selected a target bound to a different template: %+v", g)
		}
	}
}
