package sqldriver_test

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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/fleet"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// fleetRowsPerSchema is above zero on purpose: a relational template carries no
// record-count key, so getRecordCountForRebuildPolicy reports MaxInt64 for any
// NON-EMPTY store and the evolution-added index takes the DISABLED arm instead
// of being rebuilt inline. An empty store would take the inline arm and leave
// nothing for the fan-out to build.
const fleetRowsPerSchema = 60

type fleetHarness struct {
	db  *recordlayer.FDBDatabase
	cat *catalog.RecordLayerStoreCatalog
	ks  *keyspace.RelationalKeyspace
}

func newFleetHarness(t *testing.T) *fleetHarness {
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
	return &fleetHarness{db: recordlayer.NewFDBDatabase(rawDB), cat: cat, ks: ks}
}

func (h *fleetHarness) mustRun(t *testing.T, what string, fn func(txn api.Transaction) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := h.db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// fleetTemplate builds template `name` at `version` over one table T(PK,C,V),
// optionally carrying a value index on C. v1 has no index; v2 adds it, which
// is the migration every test below performs.
func fleetTemplate(t *testing.T, name string, version int, withIndex bool) *metadata.RecordLayerSchemaTemplate {
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

// fleetSetup creates one database holding len(schemas) schemas, all bound to
// the SAME template at version 1, each filled with fleetRowsPerSchema rows.
// This is the multi-tenant fixture: one template, many tenants.
func fleetSetup(t *testing.T, h *fleetHarness, dbPath, tmplName string, schemas []string) {
	t.Helper()
	ctx := context.Background()
	tmpl1 := fleetTemplate(t, tmplName, 1, false)
	h.mustRun(t, "bootstrap", func(txn api.Transaction) error {
		if err := h.cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmpl1, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		for _, s := range schemas {
			if err := ddl.NewCreateSchemaConstantAction(dbPath, s, tmplName, h.cat, h.ks).Execute(txn); err != nil {
				return err
			}
		}
		return nil
	})
	for _, s := range schemas {
		db := fleetOpen(t, dbPath, s)
		var vals []string
		for i := 1; i <= fleetRowsPerSchema; i++ {
			vals = append(vals, fmt.Sprintf("(%d,%d,%d)", i, i%10, i*10))
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO T (PK,C,V) VALUES "+strings.Join(vals, ","))
	}
}

func fleetOpen(t *testing.T, dbPath, schemaName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fleetTrace collects progress events. Fan-out runs targets concurrently, so
// the trace is keyed by target rather than ordered.
type fleetTrace struct {
	mu sync.Mutex
	ev map[string]fleet.Event
}

func newFleetTrace() *fleetTrace { return &fleetTrace{ev: map[string]fleet.Event{}} }

func (tr *fleetTrace) record(e fleet.Event) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.ev[e.Target.String()] = e
}

func (tr *fleetTrace) outcome(target string) fleet.Outcome {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.ev[target].Outcome
}

// fleetTargets lists the fan-out targets for one database. Narrowed to the
// test's own database on purpose: the catalog subspace is shared by every test
// in this package, so an unfiltered listing would race with concurrent tests.
func fleetTargets(t *testing.T, h *fleetHarness, dbPath string) []fleet.Target {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	targets, err := fleet.ListTargets(ctx, h.db, h.cat, dbPath)
	if err != nil {
		t.Fatalf("ListTargets(%s): %v", dbPath, err)
	}
	return targets
}

// fleetVersions maps schema name -> bound TEMPLATE_VERSION, read back from the
// catalog. This is the migration observable.
func fleetVersions(t *testing.T, h *fleetHarness, dbPath string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tg := range fleetTargets(t, h, dbPath) {
		out[tg.SchemaName] = tg.TemplateVersion
	}
	return out
}

// migrateFleet is the shared arrange step: save template v2 (adding the index)
// and rebind every schema, one transaction per schema.
func migrateFleet(t *testing.T, h *fleetHarness, dbPath, tmplName string, opts fleet.Options) (fleet.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmpl2 := fleetTemplate(t, tmplName, 2, true)
	return fleet.MigrateTemplate(ctx, h.db, h.cat, h.ks, dbPath, tmpl2, opts)
}

// TestFDB_FleetMigrateRebindsEveryTenantWithPerSchemaVersionBump is migration
// mode: one template save, then a per-schema transaction each. The assertion
// is per-schema — every tenant individually advanced v1 -> v2 — because a
// fan-out that rebinds only the first tenant and reports success is exactly
// the failure this design has to rule out.
func TestFDB_FleetMigrateRebindsEveryTenantWithPerSchemaVersionBump(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_migrate"
	const tmplName = "FLEETMIGRATE"
	schemas := []string{"S1", "S2", "S3"}
	fleetSetup(t, h, dbPath, tmplName, schemas)

	// Pre-state: every tenant is on v1. Without this the version assertions
	// below could pass on a fleet that was never on an older version.
	before := fleetVersions(t, h, dbPath)
	for _, s := range schemas {
		if before[s] != 1 {
			t.Fatalf("pre-state: schema %s bound to template version %d, want 1 (got %v)", s, before[s], before)
		}
	}

	trace := newFleetTrace()
	res, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{Progress: trace.record})
	if err != nil {
		t.Fatalf("MigrateTemplate: %v", err)
	}
	if res.Migrated != len(schemas) || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("migrate tally = %+v, want Migrated=%d Failed=0 Skipped=0", res, len(schemas))
	}

	after := fleetVersions(t, h, dbPath)
	for _, s := range schemas {
		if after[s] != 2 {
			t.Errorf("schema %s bound to template version %d after migration, want 2.\n"+
				"A per-schema fan-out must advance EVERY tenant; this one did not.", s, after[s])
		}
		ev := trace.ev[dbPath+"/"+s]
		if ev.Outcome != fleet.OutcomeMigrated {
			t.Errorf("schema %s progress outcome = %q, want %q", s, ev.Outcome, fleet.OutcomeMigrated)
		}
		if ev.FromVersion != 1 || ev.ToVersion != 2 {
			t.Errorf("schema %s progress reported %d -> %d, want 1 -> 2", s, ev.FromVersion, ev.ToVersion)
		}
	}
}

// TestFDB_FleetMigrateResumeSkipsAlreadyMigratedTenants pins idempotent
// resume. The observable is the SKIP itself, not the absence of an error: a
// second RepairSchema on an already-current schema succeeds as a no-op, so
// "no error on re-run" would stay green with the resume logic deleted. The
// assertion is that the second pass reports every tenant SKIPPED and rebinds
// none.
func TestFDB_FleetMigrateResumeSkipsAlreadyMigratedTenants(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_resume"
	const tmplName = "FLEETRESUME"
	schemas := []string{"S1", "S2", "S3"}
	fleetSetup(t, h, dbPath, tmplName, schemas)

	first, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{})
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if first.Migrated != len(schemas) {
		t.Fatalf("first pass migrated %d tenants, want %d (%+v)", first.Migrated, len(schemas), first)
	}

	// Second pass over the SAME targets at the SAME version. Every tenant is
	// already at v2, so every tenant must be skipped without a transaction.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	trace := newFleetTrace()
	second, err := fleet.Migrate(ctx, h.db, h.cat, h.ks,
		fleetTargets(t, h, dbPath), 2, fleet.Options{Progress: trace.record})
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if second.Skipped != len(schemas) || second.Migrated != 0 {
		t.Fatalf("resume pass tally = %+v, want Skipped=%d Migrated=0.\n"+
			"A resumed fan-out must SKIP tenants already at the target version. Re-rebinding "+
			"them is silently O(fleet) wasted work on every retry, and it is invisible unless "+
			"the skip is reported.", second, len(schemas))
	}
	for _, s := range schemas {
		if got := trace.outcome(dbPath + "/" + s); got != fleet.OutcomeSkipped {
			t.Errorf("resume: schema %s outcome = %q, want %q", s, got, fleet.OutcomeSkipped)
		}
	}
}

// TestFDB_FleetMigrateTemplateIsResumableAsAWhole pins that the ENTIRE
// migration call — template save plus fan-out — can simply be run again.
//
// The template save is a CREATE and rejects a version that already exists, so
// a naive MigrateTemplate dies on its own first step the second time and never
// reaches the tenants still owed a rebind. That turns "run it again" from the
// recovery procedure into a dead end, which is the whole point of trading
// atomicity for idempotence.
func TestFDB_FleetMigrateTemplateIsResumableAsAWhole(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_rerun"
	const tmplName = "FLEETRERUN"
	schemas := []string{"S1", "S2", "S3"}
	fleetSetup(t, h, dbPath, tmplName, schemas)

	first, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{})
	if err != nil {
		t.Fatalf("first MigrateTemplate: %v", err)
	}
	if first.Migrated != len(schemas) {
		t.Fatalf("first pass = %+v, want Migrated=%d", first, len(schemas))
	}

	// Same call, same template, same version — the shape a retry takes.
	trace := newFleetTrace()
	second, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{Progress: trace.record})
	if err != nil {
		t.Fatalf("re-running MigrateTemplate failed: %v\n"+
			"An interrupted migration is recovered by running it again. If the template save "+
			"refuses the already-stored version, the retry never reaches the tenants that "+
			"still need rebinding.", err)
	}
	if second.Skipped != len(schemas) || second.Migrated != 0 {
		t.Fatalf("re-run tally = %+v, want Skipped=%d Migrated=0", second, len(schemas))
	}
	for _, s := range schemas {
		if got := trace.outcome(dbPath + "/" + s); got != fleet.OutcomeSkipped {
			t.Errorf("re-run: schema %s outcome = %q, want %q", s, got, fleet.OutcomeSkipped)
		}
	}
}

// TestFDB_FleetBuildIndexesAcrossEveryTenant is index-build mode: after the
// migration every tenant holds a DISABLED index, and the fan-out drives all of
// them to READABLE.
func TestFDB_FleetBuildIndexesAcrossEveryTenant(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_build"
	const tmplName = "FLEETBUILD"
	schemas := []string{"S1", "S2", "S3"}
	fleetSetup(t, h, dbPath, tmplName, schemas)
	if _, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Pre-state: the index really is pending on every tenant. Without this the
	// build assertions would also pass on a fleet where the index had been
	// rebuilt inline at store open and there was nothing to build.
	targets := fleetTargets(t, h, dbPath)
	for _, tg := range targets {
		md, err := fleet.PinnedMetadata(ctx, h.db, h.cat, tg)
		if err != nil {
			t.Fatalf("pinned metadata %s: %v", tg, err)
		}
		ss, err := h.ks.SchemaSubspace(tg.DatabaseID, tg.SchemaName)
		if err != nil {
			t.Fatalf("subspace %s: %v", tg, err)
		}
		pending, err := fleet.PendingIndexes(ctx, h.db, md, ss)
		if err != nil {
			t.Fatalf("pending indexes %s: %v", tg, err)
		}
		if len(pending) != 1 || pending[0].Name != "T_BY_C" {
			t.Fatalf("pre-state: tenant %s has pending indexes %v, want exactly [T_BY_C].\n"+
				"If the index is already readable the store was rebuilt inline and this test "+
				"proves nothing about the fan-out.", tg, fleetIndexNames(pending))
		}
	}

	trace := newFleetTrace()
	res, err := fleet.BuildAll(ctx, h.db, h.cat, h.ks, dbPath, fleet.BuildOptions{
		Options: fleet.Options{Progress: trace.record},
		Limit:   20,
	})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if res.Built != len(schemas) || res.Failed != 0 {
		t.Fatalf("build tally = %+v, want Built=%d Failed=0", res, len(schemas))
	}
	for _, s := range schemas {
		ev := trace.ev[dbPath+"/"+s]
		if ev.Outcome != fleet.OutcomeBuilt {
			t.Errorf("schema %s outcome = %q, want %q", s, ev.Outcome, fleet.OutcomeBuilt)
		}
		if len(ev.Indexes) != 1 || ev.Indexes[0] != "T_BY_C" {
			t.Errorf("schema %s built %v, want [T_BY_C]", s, ev.Indexes)
		}
	}

	// Post-state: nothing is pending anywhere, and a re-run says so.
	again, err := fleet.BuildAll(ctx, h.db, h.cat, h.ks, dbPath, fleet.BuildOptions{})
	if err != nil {
		t.Fatalf("second BuildAll: %v", err)
	}
	if again.NoWork != len(schemas) || again.Built != 0 {
		t.Fatalf("re-run tally = %+v, want NoWork=%d Built=0 — a completed build must be "+
			"recognised as done, not rebuilt", again, len(schemas))
	}

	// The index is actually usable by the planner on every tenant.
	for _, s := range schemas {
		db := fleetOpen(t, dbPath, s)
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE C = 3").Scan(&n); err != nil {
			t.Errorf("schema %s query after build: %v", s, err)
			continue
		}
		if want := fleetRowsPerSchema / 10; n != want {
			t.Errorf("schema %s: got %d rows with C=3, want %d", s, n, want)
		}
	}
}

func fleetIndexNames(idx []*recordlayer.Index) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, i.Name)
	}
	return out
}

// TestFDB_FleetBuildIsolatesAPoisonedTenant is the failure-isolation property.
// One tenant's catalog row is deleted out from under the fan-out — the shape a
// tenant deleted concurrently with a fleet job actually takes — and the run
// must still complete the other tenants AND report the broken one by name.
//
// Both halves matter. A fan-out that aborts on the first failure strands the
// fleet; a fan-out that swallows the failure reports success over a tenant
// that never got its index.
func TestFDB_FleetBuildIsolatesAPoisonedTenant(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_poison"
	const tmplName = "FLEETPOISON"
	schemas := []string{"S1", "S2", "S3"}
	const poisoned = "S2"
	fleetSetup(t, h, dbPath, tmplName, schemas)
	if _, err := migrateFleet(t, h, dbPath, tmplName, fleet.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	targets := fleetTargets(t, h, dbPath)
	if len(targets) != len(schemas) {
		t.Fatalf("expected %d targets before poisoning, got %d", len(schemas), len(targets))
	}

	// Poison: delete the schema row, leaving the target list stale. Metadata
	// resolution for that tenant now fails.
	h.mustRun(t, "poison", func(txn api.Transaction) error {
		return h.cat.DeleteSchema(txn, dbPath, poisoned)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	trace := newFleetTrace()
	res, err := fleet.BuildIndexes(ctx, h.db, h.cat, h.ks, targets, fleet.BuildOptions{
		Options: fleet.Options{Progress: trace.record, Concurrency: 3},
		Limit:   20,
	})

	if err == nil {
		t.Fatal("BuildIndexes reported success with a poisoned tenant in the fleet.\n" +
			"A per-tenant failure must surface in the joined error — swallowing it reports " +
			"a fleet as migrated while a tenant silently has no index.")
	}
	if res.Failed != 1 {
		t.Fatalf("tally = %+v, want exactly 1 failure", res)
	}
	if res.Built != len(schemas)-1 {
		t.Fatalf("tally = %+v, want Built=%d — one poisoned tenant must NOT halt the fleet",
			res, len(schemas)-1)
	}

	// The failure names the tenant that broke.
	var te *fleet.TargetError
	if !errors.As(err, &te) {
		t.Fatalf("joined error is not a *fleet.TargetError: %T %v", err, err)
	}
	if len(res.Failures) != 1 || res.Failures[0].Target.SchemaName != poisoned {
		t.Fatalf("Failures = %v, want the single entry to name %s", res.Failures, poisoned)
	}
	if !strings.Contains(err.Error(), poisoned) {
		t.Errorf("joined error %q does not name the poisoned tenant %s", err.Error(), poisoned)
	}

	// The healthy tenants really did complete.
	for _, s := range schemas {
		if s == poisoned {
			if got := trace.outcome(dbPath + "/" + s); got != fleet.OutcomeFailed {
				t.Errorf("poisoned tenant %s outcome = %q, want %q", s, got, fleet.OutcomeFailed)
			}
			continue
		}
		if got := trace.outcome(dbPath + "/" + s); got != fleet.OutcomeBuilt {
			t.Errorf("healthy tenant %s outcome = %q, want %q — it was stranded by another "+
				"tenant's failure", s, got, fleet.OutcomeBuilt)
		}
	}
}

// TestFDB_FleetRefusesTheCatalogItself pins the guard end-to-end, and first
// pins the REACHABILITY that makes the guard necessary: the catalog persists
// its own /__SYS + CATALOG rows, so enumerating that database hands the
// catalog back as an ordinary fan-out target.
func TestFDB_FleetRefusesTheCatalogItself(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_guard"
	const tmplName = "FLEETGUARD"
	fleetSetup(t, h, dbPath, tmplName, []string{"S1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Reachability: the system database really does enumerate the catalog.
	sysTargets, err := fleet.ListTargets(ctx, h.db, h.cat, catalog.SysDatabaseID)
	if err != nil {
		t.Fatalf("ListTargets(%s): %v", catalog.SysDatabaseID, err)
	}
	var catalogTarget *fleet.Target
	for i := range sysTargets {
		if strings.EqualFold(sysTargets[i].SchemaName, catalog.CatalogConstant) {
			catalogTarget = &sysTargets[i]
			break
		}
	}
	if catalogTarget == nil {
		t.Fatalf("enumerating %s returned %v — the catalog no longer self-registers.\n"+
			"If that is now true the guard may be unreachable, but do NOT delete it without "+
			"re-establishing how a fan-out could otherwise reach the catalog.",
			catalog.SysDatabaseID, sysTargets)
	}

	// Both modes must refuse it.
	migRes, migErr := fleet.Migrate(ctx, h.db, h.cat, h.ks, []fleet.Target{*catalogTarget}, 99, fleet.Options{})
	assertCatalogRefused(t, "Migrate", migRes, migErr)

	buildRes, buildErr := fleet.BuildIndexes(ctx, h.db, h.cat, h.ks,
		[]fleet.Target{*catalogTarget}, fleet.BuildOptions{})
	assertCatalogRefused(t, "BuildIndexes", buildRes, buildErr)
}

func assertCatalogRefused(t *testing.T, what string, res fleet.Result, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted the relational catalog as a fan-out target.\n"+
			"The catalog describes every tenant; rebinding or index-building it from a fleet "+
			"job is how a whole cluster's metadata gets corrupted at once.", what)
	}
	var cte *fleet.CatalogTargetError
	if !errors.As(err, &cte) {
		t.Fatalf("%s: want a *fleet.CatalogTargetError, got %T: %v", what, err, err)
	}
	if res.Refused != 1 {
		t.Fatalf("%s tally = %+v, want Refused=1", what, res)
	}
	if res.Migrated != 0 || res.Built != 0 {
		t.Fatalf("%s tally = %+v, want no target touched", what, res)
	}
}

// TestFDB_FleetMigrateRefusesARebindThatDidNotAdvance pins the asserted
// read-back — the one property of migration mode that has no other observable.
//
// RepairSchema rebinds a schema to whatever its template's LATEST stored
// version happens to be; it cannot be asked for a particular version. So if the
// template save silently did not land, every RepairSchema call still SUCCEEDS,
// rebinding each tenant to the version it already held. Without reading the
// landed version back and asserting it advanced, the pass reports the whole
// fleet "migrated" while every tenant is still on the old metadata — the
// worst possible outcome, because it is indistinguishable from success.
//
// The scenario is constructed directly: ask for a version that was never
// saved. Every tenant must FAIL, none may be counted migrated, and the
// catalog must be unchanged because the failing assertion aborts each
// transaction before it commits.
func TestFDB_FleetMigrateRefusesARebindThatDidNotAdvance(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_noadvance"
	const tmplName = "FLEETNOADVANCE"
	schemas := []string{"S1", "S2", "S3"}
	fleetSetup(t, h, dbPath, tmplName, schemas)

	// Deliberately do NOT save v2 — this models the template save that did not
	// land. Everything else about the pass is a normal migration.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	trace := newFleetTrace()
	res, err := fleet.Migrate(ctx, h.db, h.cat, h.ks,
		fleetTargets(t, h, dbPath), 2, fleet.Options{Progress: trace.record})

	if err == nil {
		t.Fatalf("Migrate reported success with target version 2 that was never saved (%+v).\n"+
			"RepairSchema rebinds to the catalog's latest, so every tenant 'succeeded' while "+
			"staying on v1. The landed version must be read back and asserted to have advanced.", res)
	}
	if res.Migrated != 0 || res.Failed != len(schemas) {
		t.Fatalf("tally = %+v, want Migrated=0 Failed=%d — a rebind that did not advance is a "+
			"FAILURE, not a migration", res, len(schemas))
	}
	for _, s := range schemas {
		if got := trace.outcome(dbPath + "/" + s); got != fleet.OutcomeFailed {
			t.Errorf("schema %s outcome = %q, want %q", s, got, fleet.OutcomeFailed)
		}
	}
	// The catalog is untouched: the assertion fires inside the transaction, so
	// nothing commits.
	for s, v := range fleetVersions(t, h, dbPath) {
		if v != 1 {
			t.Errorf("schema %s is bound to version %d after a rejected rebind, want 1 — "+
				"the assertion must abort the transaction, not report after committing", s, v)
		}
	}
}

// TestFDB_FleetMigrateToLatestResolvesVersionPerTemplate pins that a database
// mixing schema templates migrates correctly.
//
// Templates advance independently. Resolving ONE destination version for the
// whole database — the maximum across templates, the obvious implementation —
// is wrong in the most damaging direction: a tenant whose own template has not
// reached that number cannot be rebound to it (RepairSchema only ever moves a
// schema to ITS OWN template's latest), so the asserted read-back correctly
// rejects it, the tenant is reported FAILED, and it never migrates at all.
// The tenants that most need the pass are exactly the ones it drops.
func TestFDB_FleetMigrateToLatestResolvesVersionPerTemplate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := newFleetHarness(t)
	const dbPath = "/testdb_fleet_mixed"
	const tmplAhead = "FLEETMIXEDAHEAD"   // reaches v2
	const tmplBehind = "FLEETMIXEDBEHIND" // stays at v1

	// Two templates in ONE database: S1/S2 on the one that will advance, S3 on
	// the one that will not.
	tmplA1 := fleetTemplate(t, tmplAhead, 1, false)
	tmplB1 := fleetTemplate(t, tmplBehind, 1, false)
	h.mustRun(t, "bootstrap mixed", func(txn api.Transaction) error {
		if err := h.cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmplA1, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmplB1, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		for _, s := range []string{"S1", "S2"} {
			if err := ddl.NewCreateSchemaConstantAction(dbPath, s, tmplAhead, h.cat, h.ks).Execute(txn); err != nil {
				return err
			}
		}
		return ddl.NewCreateSchemaConstantAction(dbPath, "S3", tmplBehind, h.cat, h.ks).Execute(txn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Only the AHEAD template gets a v2.
	if err := fleet.SaveTemplate(ctx, h.db, h.cat, fleetTemplate(t, tmplAhead, 2, true)); err != nil {
		t.Fatalf("save %s@2: %v", tmplAhead, err)
	}

	// Pre-state: the destinations really do differ. Without this the test would
	// also pass on a fleet where both templates sat at the same version, which
	// is precisely the case that cannot express the defect.
	targets := fleetTargets(t, h, dbPath)
	latest, err := fleet.LatestVersions(ctx, h.db, h.cat, targets)
	if err != nil {
		t.Fatalf("LatestVersions: %v", err)
	}
	if latest[tmplAhead] != 2 || latest[tmplBehind] != 1 {
		t.Fatalf("pre-state: latest versions = %v, want %s@2 and %s@1 — the two templates must "+
			"sit at DIFFERENT versions or this test cannot detect a fleet-wide resolution",
			latest, tmplAhead, tmplBehind)
	}

	trace := newFleetTrace()
	res, err := fleet.MigrateToLatest(ctx, h.db, h.cat, h.ks, targets,
		fleet.Options{Progress: trace.record})
	if err != nil {
		t.Fatalf("MigrateToLatest on a mixed-template database failed: %v\n"+
			"Resolving one fleet-wide destination version fails every tenant whose own template "+
			"has not reached it. Each template must be resolved separately.", err)
	}
	if res.Failed != 0 {
		t.Fatalf("tally = %+v, want Failed=0", res)
	}
	// S1/S2 advance to their template's v2; S3 is already at ITS template's
	// latest and must be skipped, not failed and not pushed to v2.
	if res.Migrated != 2 || res.Skipped != 1 {
		t.Fatalf("tally = %+v, want Migrated=2 (on %s) Skipped=1 (on %s, already at its own latest)",
			res, tmplAhead, tmplBehind)
	}
	if got := trace.outcome(dbPath + "/S3"); got != fleet.OutcomeSkipped {
		t.Errorf("S3 (bound to %s, already at its latest v1) outcome = %q, want %q",
			tmplBehind, got, fleet.OutcomeSkipped)
	}
	after := fleetVersions(t, h, dbPath)
	if after["S1"] != 2 || after["S2"] != 2 {
		t.Errorf("versions after = %v, want S1=S2=2", after)
	}
	if after["S3"] != 1 {
		t.Errorf("S3 is bound to version %d, want 1 — a schema must never be pushed past its "+
			"OWN template's latest version by another template's progress", after["S3"])
	}
}
