package sqldriver_test

// An index added by METADATA EVOLUTION has no index-state key until the store
// header is reconciled at store open (checkPossiblyRebuild). The planner reads
// index states BEFORE any store is opened — fetchReadableIndexes runs on the
// planning path, ahead of the plan-cache lookup — and treats "no stored state"
// as READABLE, which is Java's RecordStoreState default.
//
// That optimism is only safe because reconciliation happens in the SAME
// transaction that then executes the scan, and because Go's
// getRecordCountForRebuildPolicy reports 0 records for a store with no record
// count key (the relational metadata never sets one), so
// DefaultIndexRebuildPolicy always returns READABLE and the index is built
// inline at open, before the scan reads it.
//
// The tests below pin the OBSERVABLE contract, not that mechanism: a query
// issued after a metadata evolution and before any reconciliation must return
// CORRECT rows. It holds under the current inline-rebuild behaviour and it
// would equally hold under a planner gate that declined the unbuilt index and
// fell back to a base-table scan. What it does NOT tolerate is the planner
// selecting an index the store then refuses.
//
// This is a live tripwire, not decoration. Java's
// getRecordCountForRebuildIndexes (FDBRecordStore.java:4862-4880) falls back to
// a one-record scan and reports Long.MAX_VALUE for a NON-EMPTY store, so under
// Java's default checker the evolution-added index is left DISABLED rather than
// built inline. Aligning Go's fallback with Java's turns the planner's
// optimistic default into a live defect, and these tests are what says so:
// measured under exactly that change, both fail with
//
//	index is not readable: T_BY_C is DISABLED
//
// on a plan that still names the index. If they redden, the remedy is in the
// planner gate (fetchReadableIndexes must not assume an index added since the
// store's recorded metadata version is readable), not here.

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
}
