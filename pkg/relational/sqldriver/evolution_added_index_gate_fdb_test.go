package sqldriver_test

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
//     view off the OPENED store (PlanContext.java:249-251 does exactly that),
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
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// evolHarness wires a catalog over the same keyspace the sqldriver uses, so a
// template evolved here is the template the driver plans against. The evolution
// path itself is catalog-level (SaveSchemaTemplate at a higher version, then
// RepairSchema to rebind) because the SQL surface has no statement that rebinds
// a live schema — CREATE SCHEMA TEMPLATE only writes the template.
type evolHarness struct {
	db  *recordlayer.FDBDatabase
	cat *catalog.RecordLayerStoreCatalog
	ks  *keyspace.RelationalKeyspace
}

func newEvolHarness(t *testing.T) *evolHarness {
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
	return &evolHarness{db: recordlayer.NewFDBDatabase(rawDB), cat: cat, ks: ks}
}

func (h *evolHarness) mustRun(t *testing.T, what string, fn func(txn api.Transaction) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := h.db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// evolTemplate builds template `name` at `version` over one table T(PK,C,V),
// optionally carrying a value index on C and/or a grouped SUM index. The SUM
// index drags in the RFC-209 __GROUP_COUNT companion, emitted by Builder.Build.
func evolTemplate(t *testing.T, name string, version int, withValueIndex, withAggIndex bool) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName(name).SetVersion(version)
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PK", api.NewLongType(false), 1),
		metadata.NewColumnSpec("C", api.NewLongType(true), 2),
		metadata.NewColumnSpec("V", api.NewLongType(true), 3),
	}, []string{"PK"})
	if withValueIndex {
		b.AddIndex("T", "T_BY_C", []string{"C"}, false)
	}
	if withAggIndex {
		b.AddAggregateIndex("T", "T_SUM_V_BY_C", []string{"C"}, "SUM", "V")
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build template %s@%d: %v", name, version, err)
	}
	return tmpl
}

// evolSetup creates database + schema at template version 1 (no indexes),
// fills it with `rows` records, and returns an *sql.DB bound to it. Row i has
// PK=i, C=i%10, V=i*10.
func evolSetup(t *testing.T, h *evolHarness, dbPath, schemaName, tmplName string, rows int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	tmpl1 := evolTemplate(t, tmplName, 1, false, false)
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
		return ddl.NewCreateSchemaConstantAction(dbPath, schemaName, tmplName, h.cat, h.ks).Execute(txn)
	})

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for base := 0; base < rows; base += 50 {
		var vals []string
		for i := base + 1; i <= base+50 && i <= rows; i++ {
			vals = append(vals, fmt.Sprintf("(%d,%d,%d)", i, i%10, i*10))
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO T (PK,C,V) VALUES "+strings.Join(vals, ","))
	}
	return db
}

// evolReopen returns a FRESH *sql.DB, so the query below plans against the
// evolved metadata with nothing cached from before the rebind.
func evolReopen(t *testing.T, dbPath, schemaName string) *sql.DB {
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
func evolIndexStates(t *testing.T, dbPath, schemaName string) map[string]recordlayer.IndexState {
	t.Helper()
	rawDB, dbErr := fdb.OpenDatabase(clusterFilePath)
	if dbErr != nil {
		t.Fatalf("open db: %v", dbErr)
	}
	ss, err := keyspace.New(subspace.Sub()).SchemaSubspace(dbPath, strings.ToUpper(schemaName))
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

// evolQueryPairs runs q and returns its rows as "[a b]" strings.
func evolQueryPairs(t *testing.T, db *sql.DB, ctx context.Context, q string) (string, error) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, b int64
		if sErr := rows.Scan(&a, &b); sErr != nil {
			return "", sErr
		}
		out = append(out, fmt.Sprintf("[%d %d]", a, b))
	}
	if rErr := rows.Err(); rErr != nil {
		return "", rErr
	}
	return strings.Join(out, " "), nil
}

func evolExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

// TestFDB_EvolutionAddedValueIndexAnsweredBeforeReconciliation is the general
// case: a store above the 200-record MAX_RECORDS_FOR_REBUILD threshold, its
// metadata evolved to add a VALUE index, queried before anything has reconciled
// the store header.
//
// The 250 rows are load-bearing. Below the threshold the index is built inline
// under every rebuild policy there is, so a smaller store cannot distinguish
// "the planner was right to be optimistic" from "the threshold never applied".
func TestFDB_EvolutionAddedValueIndexAnsweredBeforeReconciliation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	h := newEvolHarness(t)

	const dbPath = "/testdb_evolvalue"
	const schemaName = "S"
	const tmplName = "evolvalue"

	db := evolSetup(t, h, dbPath, schemaName, tmplName, 250)

	// Baseline: with no index in the metadata the query is answerable, and the
	// plan does not name one. Without this the assertions below could pass on a
	// query that was never index-eligible at all.
	const q = "SELECT PK, V FROM T WHERE C = 3"
	const wantRows = "[3 30] [13 130] [23 230] [33 330] [43 430] [53 530] [63 630] [73 730] [83 830] " +
		"[93 930] [103 1030] [113 1130] [123 1230] [133 1330] [143 1430] [153 1530] [163 1630] " +
		"[173 1730] [183 1830] [193 1930] [203 2030] [213 2130] [223 2230] [233 2330] [243 2430]"
	got, err := evolQueryPairs(t, db, ctx, q)
	if err != nil {
		t.Fatalf("baseline query: %v", err)
	}
	if got != wantRows {
		t.Fatalf("baseline rows: got %s, want %s", got, wantRows)
	}

	// Evolve: add the value index. Nothing here opens the data store, so no
	// index state is written and the store header still records version 1.
	tmpl2 := evolTemplate(t, tmplName, 2, true, false)
	h.mustRun(t, "evolve", func(txn api.Transaction) error {
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmpl2, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return h.cat.RepairSchema(txn, dbPath, schemaName)
	})

	// The premise: the planner's view of index state is EMPTY, so T_BY_C reads
	// as readable to fetchReadableIndexes even though it holds no entries.
	if states := evolIndexStates(t, dbPath, schemaName); len(states) != 0 {
		t.Fatalf("an evolution-added index already has a stored state (%v).\n"+
			"This test exists because it does NOT: reconciliation happens at store open, "+
			"which is after planning. If something now writes the state earlier, the window "+
			"this test guards has moved and the test must be rewritten to find it.", states)
	}

	db2 := evolReopen(t, dbPath, schemaName)
	plan := evolExplain(t, db2, ctx, q)

	got, err = evolQueryPairs(t, db2, ctx, q)
	if err != nil {
		t.Fatalf("a query issued after metadata evolution and before any store "+
			"reconciliation FAILED: %v\n  plan: %s\n"+
			"The planner treats an index with no stored state as READABLE (Java's "+
			"RecordStoreState default). That is only safe while the store open that "+
			"follows builds the index inline in the same transaction. If the rebuild "+
			"policy now leaves it WRITE_ONLY or DISABLED — which is what Java does, "+
			"since getRecordCountForRebuildIndexes reports Long.MAX_VALUE for a "+
			"non-empty store (FDBRecordStore.java:4862-4880) — then fetchReadableIndexes "+
			"must stop assuming readability for an index added since the store's "+
			"recorded metadata version.", err, plan)
	}
	if got != wantRows {
		t.Fatalf("after metadata evolution the answer changed: got %s, want %s\n  plan: %s\n"+
			"Selecting an index that holds no entries returns a SUBSET with no indication "+
			"that it did — strictly worse than the execution error.", got, wantRows, plan)
	}
	t.Logf("plan on the evolved metadata: %s", plan)

	// AXIS 1 — RECONCILIATION. The store is above MAX_RECORDS_FOR_REBUILD, so
	// the store open that has now happened must have left T_BY_C DISABLED, not
	// built it inline. Without this the correct answer above proves nothing:
	// an inline rebuild produces exactly the same rows, and the whole planner
	// gate is untested because the index was never unreadable.
	evolRequireState(t, dbPath, schemaName, "T_BY_C", recordlayer.IndexStateDisabled,
		"a 250-record store is far above Java's MAX_RECORDS_FOR_REBUILD (200, "+
			"FDBRecordStore.java:194,2471), so an evolution-added index must be left for a "+
			"background build. READABLE here means getRecordCountForRebuildPolicy reported a "+
			"count small enough to rebuild inline — i.e. it is back to reporting 0 for a store "+
			"with no record-count key, and every evolution-added index on every store is now "+
			"built inside the store-open transaction.")

	// AXIS 2 — THE PLANNER GATE. A DISABLED index must not appear in the plan.
	// The correct answer above already implies this (a scan of it would have
	// errored), but naming it explicitly is what fails LOUDLY and locally if
	// the planner ever selects it again.
	if strings.Contains(plan, "T_BY_C") {
		t.Fatalf("the plan names the DISABLED evolution-added index: %s\n"+
			"fetchReadableIndexes must take its view from the OPENED store "+
			"(PlanContext.java:249-251), where reconciliation has already settled the state.", plan)
	}
}

// evolRequireState asserts the stored state of one index, with `why` explaining
// what a mismatch means.
func evolRequireState(t *testing.T, dbPath, schemaName, index string, want recordlayer.IndexState, why string) {
	t.Helper()
	states := evolIndexStates(t, dbPath, schemaName)
	got, stored := states[index]
	if !stored {
		// Absent means READABLE: only the exceptions are written down.
		got = recordlayer.IndexStateReadable
	}
	if got != want {
		t.Fatalf("index %s is %v after reconciliation, want %v (all states: %v)\n%s",
			index, got, want, states, why)
	}
}

// TestFDB_EvolutionAddedAggregateIndexAnsweredBeforeReconciliation is the
// RFC-209 shape: the DDL adds a grouped SUM index to an EXISTING populated
// store, so Builder.Build auto-emits the __GROUP_COUNT companion alongside it.
// BOTH are evolution-added and both are unbuilt in the pre-reconciliation
// window, and the companion is the one that decides which groups exist — a
// group-existence merge driven from an empty companion drops every live group.
//
// Separate from the value-index test above and not duplication: the aggregate
// family takes a different candidate builder, and a gate applied to one list
// and not the other passes there while leaving this wide open.
func TestFDB_EvolutionAddedAggregateIndexAnsweredBeforeReconciliation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	h := newEvolHarness(t)

	const dbPath = "/testdb_evolagg"
	const schemaName = "S"
	const tmplName = "evolagg"

	db := evolSetup(t, h, dbPath, schemaName, tmplName, 250)

	// C = PK%10 over PK 1..250: every group holds 25 rows and its SUM is
	// 10 * (sum of its PKs) — group 1 is PKs 1,11,…,241 = 3025, so 30250.
	const q = "SELECT C, SUM(V) FROM T GROUP BY C ORDER BY C"
	const wantRows = "[0 32500] [1 30250] [2 30500] [3 30750] [4 31000] " +
		"[5 31250] [6 31500] [7 31750] [8 32000] [9 32250]"
	got, err := evolQueryPairs(t, db, ctx, q)
	if err != nil {
		t.Fatalf("baseline query: %v", err)
	}
	if got != wantRows {
		t.Fatalf("baseline rows: got %s, want %s", got, wantRows)
	}

	tmpl2 := evolTemplate(t, tmplName, 2, false, true)
	h.mustRun(t, "evolve", func(txn api.Transaction) error {
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmpl2, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return h.cat.RepairSchema(txn, dbPath, schemaName)
	})

	if states := evolIndexStates(t, dbPath, schemaName); len(states) != 0 {
		t.Fatalf("an evolution-added aggregate index already has a stored state (%v); "+
			"the pre-reconciliation window this test guards has moved", states)
	}

	db2 := evolReopen(t, dbPath, schemaName)
	plan := evolExplain(t, db2, ctx, q)

	got, err = evolQueryPairs(t, db2, ctx, q)
	if err != nil {
		t.Fatalf("the RFC-209 shape FAILED on an existing populated store: %v\n  plan: %s\n"+
			"Adding a grouped SUM index to a live schema emits the __GROUP_COUNT companion "+
			"as a second evolution-added index. Neither has a stored state until the store "+
			"header is reconciled, so both read as READABLE to the planner while holding "+
			"no entries.", err, plan)
	}
	if got != wantRows {
		t.Fatalf("the RFC-209 shape returned WRONG ROWS on an existing populated store: "+
			"got %s, want %s\n  plan: %s\n"+
			"An unbuilt companion has a PARTIAL key set; driving the group-existence merge "+
			"from it drops live groups — a wrong answer, strictly worse than an error.",
			got, wantRows, plan)
	}
	t.Logf("plan on the evolved metadata: %s", plan)

	// AXIS 1 — both the owner AND its auto-emitted companion are
	// evolution-added, and on a store this size both must be left for a
	// background build. Checking only the owner would miss the companion,
	// which is the index that decides which groups EXIST: a merge driven from
	// an unbuilt companion drops live groups silently.
	const owner = "T_SUM_V_BY_C"
	companion := recordlayer.GroupCountCompanionName(owner)
	const why = "a 250-record store is above Java's MAX_RECORDS_FOR_REBUILD (200, " +
		"FDBRecordStore.java:194,2471), so neither the grouped SUM index nor its " +
		"__GROUP_COUNT companion may be rebuilt inline in the store-open transaction."
	evolRequireState(t, dbPath, schemaName, owner, recordlayer.IndexStateDisabled, why)
	evolRequireState(t, dbPath, schemaName, companion, recordlayer.IndexStateDisabled, why)

	// AXIS 2 — neither may appear in the plan; the answer above must come from
	// streaming aggregation over the base table.
	for _, name := range []string{owner, companion} {
		if strings.Contains(plan, name) {
			t.Fatalf("the plan names the DISABLED evolution-added index %s: %s\n"+
				"Falling back to streaming aggregation until the background build "+
				"completes is the CORRECT behaviour here; planning the unbuilt index is not.",
				name, plan)
		}
	}
}

// TestFDB_EvolutionAddedAggregateIndexOnEmptyStoreStillBuildsInline is the
// other side of the threshold, and it is what stops the fix above from
// degenerating into "never build anything inline".
//
// WHICH stores are still inline is worth being exact about, because "small
// stores build inline" is only true when a cheap count EXISTS. Java's
// getRecordCountForRebuildIndexes (FDBRecordStore.java:4836-4901) tries a
// record-type COUNT index, then a universal COUNT index, and only when both
// are absent does it fall back to the one-record scan — which reports
// Long.MAX_VALUE for ANY non-empty store, not a small number for a small one
// (FDBRecordStore.java:4862-4884). The relational metadata sets no
// record-count key, so at the SQL layer the fallback is always what runs and
// EMPTY is the only remaining inline case. A store with 20 records is
// therefore left for a background build exactly as a store with 250 is; that
// is Java's behaviour, not a Go conservatism.
//
// So: evolve an EMPTY store, and the RFC-209 companion must be built at open
// and the group-existence merge must fire on the rows loaded afterwards — the
// index is used, not fallen back from.
func TestFDB_EvolutionAddedAggregateIndexOnEmptyStoreStillBuildsInline(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	h := newEvolHarness(t)

	const dbPath = "/testdb_evolaggempty"
	const schemaName = "S"
	const tmplName = "evolaggempty"

	// rows = 0: the schema is created and the aggregate index is added before
	// any data lands, which is the ordinary "add an index up front" shape.
	db := evolSetup(t, h, dbPath, schemaName, tmplName, 0)
	_ = db

	tmpl2 := evolTemplate(t, tmplName, 2, false, true)
	h.mustRun(t, "evolve", func(txn api.Transaction) error {
		if err := ddl.NewSaveSchemaTemplateConstantAction(tmpl2, h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return h.cat.RepairSchema(txn, dbPath, schemaName)
	})

	if states := evolIndexStates(t, dbPath, schemaName); len(states) != 0 {
		t.Fatalf("an evolution-added aggregate index already has a stored state (%v); "+
			"the pre-reconciliation window this test guards has moved", states)
	}

	// Load the rows AFTER the evolution. Reconciliation happens on the store
	// open this INSERT performs; the index is built inline over an empty store
	// and then maintained by the writes.
	db2 := evolReopen(t, dbPath, schemaName)
	var vals []string
	for i := 1; i <= 20; i++ {
		vals = append(vals, fmt.Sprintf("(%d,%d,%d)", i, i%10, i*10))
	}
	mwjoMustExec(t, db2, ctx, "INSERT INTO T (PK,C,V) VALUES "+strings.Join(vals, ","))

	// C = PK%10 over PK 1..20: each group holds two rows. Group g (1..9) has
	// PKs g and g+10, so SUM(V) = 10*(2g+10) = 20g+100; group 0 has PKs 10 and
	// 20, SUM 300.
	const q = "SELECT C, SUM(V) FROM T GROUP BY C ORDER BY C"
	const wantRows = "[0 300] [1 120] [2 140] [3 160] [4 180] " +
		"[5 200] [6 220] [7 240] [8 260] [9 280]"

	plan := evolExplain(t, db2, ctx, q)
	got, err := evolQueryPairs(t, db2, ctx, q)
	if err != nil {
		t.Fatalf("query on an EMPTY-at-evolution store failed: %v\n  plan: %s", err, plan)
	}
	if got != wantRows {
		t.Fatalf("empty-at-evolution store returned wrong rows: got %s, want %s\n  plan: %s",
			got, wantRows, plan)
	}

	// The inline build must actually have happened: DISABLED here would mean
	// the record-count fallback stopped distinguishing an EMPTY store from an
	// unbounded one and now refuses every inline build — correct answers, but
	// Java's inline path silently gone.
	const owner = "T_SUM_V_BY_C"
	companion := recordlayer.GroupCountCompanionName(owner)
	const why = "an EMPTY store reports a record count of 0 in Java too " +
		"(FDBRecordStore.java:4884), which is <= MAX_RECORDS_FOR_REBUILD, so Java " +
		"builds the index inline at store open (FDBRecordStore.java:2471)."
	evolRequireState(t, dbPath, schemaName, owner, recordlayer.IndexStateReadable, why)
	evolRequireState(t, dbPath, schemaName, companion, recordlayer.IndexStateReadable, why)

	// And, the built index being readable, the group-existence merge must be
	// what answers the query — otherwise "built inline" is true but inert.
	if !strings.Contains(plan, owner) {
		t.Fatalf("the inline-built aggregate index is not in the plan: %s\n"+
			"Both indexes are READABLE, so the RFC-209 merge should be driving this "+
			"query rather than streaming aggregation over the base table.", plan)
	}
	t.Logf("plan on the empty-at-evolution store: %s", plan)
}
