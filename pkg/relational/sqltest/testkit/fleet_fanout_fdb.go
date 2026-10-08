package testkit

// Fan-out across a tenant fleet. These tests exist because the fan-out is NOT
// one transaction (RFC-204's original step 6+7 said it was), and every
// property that replaces atomicity has to be observable:
//
//   - failure isolation — one poisoned tenant must not halt the fleet, and its
//     failure must be REPORTED rather than swallowed;
//   - idempotent resume — a re-run must SKIP finished tenants, observably, not
//     merely avoid erroring;
//   - the catalog guard — the catalog self-registers as a schema, so an
//     unfiltered fan-out really does reach it.
//
// Each test asserts the pre-state it depends on (indexes really are DISABLED,
// versions really are v1) so it cannot degrade into passing while proving
// nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/fleet"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

type FleetHarness struct {
	DB  *recordlayer.FDBDatabase
	Cat *catalog.RecordLayerStoreCatalog
	Ks  *keyspace.RelationalKeyspace
}

func NewFleetHarness(t *testing.T) *FleetHarness {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Root keyspace, as the sqldriver DSN resolves it. Tests stay isolated by
	// using a UNIQUE database path each, never by rooting the keyspace
	// elsewhere — the driver would not find a relocated store.
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return &FleetHarness{DB: recordlayer.NewFDBDatabase(rawDB), Cat: cat, Ks: ks}
}

func (h *FleetHarness) MustRun(t *testing.T, what string, fn func(txn api.Transaction) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := h.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// fleetTemplate builds template `name` at `version` over one table T(PK,C,V),
// optionally carrying a value index on C. v1 has no index; v2 adds it, which
// is the migration every test below performs.
func FleetTemplate(t *testing.T, name string, version int, withIndex bool) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName(name).SetVersion(version)
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PK", api.NewLongType(false), 1),
		metadata.NewColumnSpec("C", api.NewLongType(true), 2),
		metadata.NewColumnSpec("V", api.NewLongType(true), 3),
	}, []string{"PK"})
	if withIndex {
		b.AddIndex("T", "T_BY_C", []string{"C"}, false)
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build template %s@%d: %v", name, version, err)
	}
	return tmpl
}

func FleetOpen(t *testing.T, dbPath, schemaName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fleetTargets lists the fan-out targets for one database. Narrowed to the
// test's own database on purpose: the catalog subspace is shared by every test
// in this package, so an unfiltered listing would race with concurrent tests.
func FleetTargets(t *testing.T, h *FleetHarness, dbPath string) []fleet.Target {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	targets, err := fleet.ListTargets(ctx, h.DB, h.Cat, dbPath)
	if err != nil {
		t.Fatalf("ListTargets(%s): %v", dbPath, err)
	}
	return targets
}

// fleetVersions maps schema name -> bound TEMPLATE_VERSION, read back from the
// catalog. This is the migration observable.
func FleetVersions(t *testing.T, h *FleetHarness, dbPath string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tg := range FleetTargets(t, h, dbPath) {
		out[tg.SchemaName] = tg.TemplateVersion
	}
	return out
}

// migrateFleet is the shared arrange step: save template v2 (adding the index)
// and rebind every schema, one transaction per schema.
func MigrateFleet(t *testing.T, h *FleetHarness, dbPath, tmplName string, opts fleet.Options) (fleet.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmpl2 := FleetTemplate(t, tmplName, 2, true)
	return fleet.MigrateTemplate(ctx, h.DB, h.Cat, h.Ks, dbPath, tmpl2, opts)
}

func FleetIndexNames(idx []*recordlayer.Index) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, i.Name)
	}
	return out
}
