package embedded

// RFC-198 criterion 12: the catalog binds to the transaction — INCLUDING the
// cache. Three cases, because the read path and the cache path fail
// differently and a test of one is not a test of the other; each has its own
// opposed mutation direction (verified at introduction):
//
//	(a) the skew itself — reverting Decision 8b's ROUTING (catalog reads back
//	    to a separate auto-commit transaction) makes the in-transaction read
//	    plan against a post-DDL schema while its data sits at the
//	    transaction's snapshot: an index scan over an index that does not
//	    exist at that snapshot, ZERO ROWS WITH NO ERROR. (a) reddens; (b) and
//	    (c) do not isolate this.
//	(b) the cache hit across a transaction boundary — keeping the routing but
//	    letting the entry SURVIVE the transaction (serving it to the next
//	    transaction) reddens (b) while (a) stays green; that asymmetry is the
//	    point of having both.
//	(c) the accepted cost is actually paid — making the in-transaction
//	    catalog read SNAPSHOT (no conflict range) lets a concurrent DDL slip
//	    past the commit; (c) reddens while (a) and (b) stay green.
//
// Internal tests over SimFDB: (a) needs a mid-transaction cache invalidation
// (the shape runDDL's own-DDL collision produces) and (b) must not return the
// connection to a pool between its two transactions — both need package
// access. DROP SCHEMA also deletes the schema's record store (MEASURED —
// DropSchemaConstantAction.deleteFDBStore), so (c) keeps its transaction's
// reads planning-only on the dropped schema and writes to a DIFFERENT schema:
// the only key it shares with the concurrent DDL is the catalog record.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
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

// simBackend is one SimFDB-backed database + catalog shared by several
// connections, the way a sql.DB pool shares one Connector's pieces.
type simBackend struct {
	fdbDB   *recordlayer.FDBDatabase
	cat     *catalog.RecordLayerStoreCatalog
	ks      *keyspace.RelationalKeyspace
	factory *ddl.RecordLayerMetadataOperationsFactory
}

func newSimBackend(t *testing.T, seed uint64) *simBackend {
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
	return &simBackend{
		fdbDB:   fdbDB,
		cat:     cat,
		ks:      ks,
		factory: ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks),
	}
}

// connect builds a connection the way the SQL driver's Connector does —
// New(...) plus SetDefaultSchema — and NOT via SetSchema. The distinction is
// load-bearing and is pinned by TestSchemaNameNormalizationIsInSetDefaultSchema
// below: SetDefaultSchema normalizes the name (unquoted identifiers uppercase),
// SetSchema stores it raw, and DDL writes the catalog row under the normalized
// name. A harness that used the raw setter with a lowercase literal would look
// past the catalog row its own DDL just wrote.
func (b *simBackend) connect(schema string) *EmbeddedConnection {
	c := New("/simdb", b.fdbDB, b.cat, b.factory, b.ks)
	if schema != "" {
		c.SetDefaultSchema(schema)
	}
	return c
}

// TestSchemaNameNormalizationIsInSetDefaultSchema pins where SQL identifier
// normalization happens on the connection, because every SimFDB-backed test in
// this package depends on it and nothing else states it.
//
// CREATE SCHEMA /simdb/s writes the catalog row under the NORMALIZED name
// ("S"), so a session schema must be normalized before it can resolve. The
// driver gets that for free: the DSN's schema= goes through SetDefaultSchema,
// which strips quotes and uppercases. SetSchema is the raw setter and does
// neither — it is the api.Connection-shaped label setter with no production
// caller in this repo, so nothing else would notice.
//
// If SetSchema ever grows normalization (or SetDefaultSchema loses it), this
// test fails and the harnesses above can be simplified — that is the point of
// pinning it rather than leaving it as a comment.
func TestSchemaNameNormalizationIsInSetDefaultSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 107)
	admin := b.connect("")
	bootstrapTwoTemplates(t, admin)

	if got := admin.GetSchema(); got != "" {
		t.Fatalf("admin connection schema = %q, want empty", got)
	}

	normalized := b.connect("s")
	if got := normalized.GetSchema(); got != "S" {
		t.Fatalf("SetDefaultSchema(%q) left the session schema %q, want %q: the driver's "+
			"own entry point stopped normalizing, and every DSN with a lowercase "+
			"schema= now looks past the catalog row CREATE SCHEMA wrote", "s", got, "S")
	}
	if _, err := normalized.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 1)", nil); err != nil {
		t.Fatalf("insert through the normalized session: %v", err)
	}

	raw := b.connect("")
	raw.SetSchema("s")
	if got := raw.GetSchema(); got != "s" {
		t.Fatalf("SetSchema(%q) stored %q: the raw label setter now normalizes too, so the "+
			"harnesses in this package no longer need SetDefaultSchema", "s", got)
	}
	if _, err := raw.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 2)", nil); err == nil {
		t.Fatalf("a session schema set through the RAW setter resolved: normalization moved, " +
			"and the reason this package's harnesses must use SetDefaultSchema no longer holds")
	}
}

// bootstrapTwoTemplates creates the database, a plain template, an indexed
// template (idx_v on t(v)), and schema s from the PLAIN template.
func bootstrapTwoTemplates(t *testing.T, admin *EmbeddedConnection) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE /simdb",
		"CREATE SCHEMA TEMPLATE tmpl_plain CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA TEMPLATE tmpl_indexed CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_v ON t (v)",
		"CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl_plain",
	} {
		if _, err := admin.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// swapSchemaToIndexed is the cross-connection DDL: drop s and recreate it from
// the indexed template. Auto-commit on conn B.
func swapSchemaToIndexed(t *testing.T, b *EmbeddedConnection) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP SCHEMA /simdb/s",
		"CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl_indexed",
	} {
		if _, err := b.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// TestCatalogBinding_NoSilentZeroRows is criterion 12(a): an explicit
// transaction whose catalog binding is refreshed mid-transaction (the shape a
// mid-transaction DDL invalidation produces) must re-read the catalog INSIDE
// the transaction — at its own snapshot — and keep returning the right rows.
//
// MEASURED at introduction, and it narrows the RFC's claim: the mutated
// (unbound) read makes the next statement plan to idx_v, which does not exist
// at the transaction's snapshot — but the WRONG-ANSWER half of the skew is
// repaired by the record layer's own checkVersion on store open, which
// rebuilds the index from the transaction's snapshot inside the transaction
// (measured returning the correct row even at 250 records, where
// DefaultIndexRebuildPolicy was expected to disable it). Defense-in-depth
// catches the executable-store side; what the ROUTING deterministically owns
// is (1) which schema the plan is built from — asserted here directly: the
// plan must come from the TRANSACTION-SNAPSHOT schema, so it must NOT contain
// idx_v — plus (2) the conflict range (test (c)) and (3) the cache discipline
// (test (b)). The zero-rows-no-error form needs an index READABLE in the
// header but populated after the transaction's read version (an OnlineIndexer
// completion mid-transaction), which has no SQL surface to stage here.
func TestCatalogBinding_NoSilentZeroRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 73)
	connA := b.connect("")
	bootstrapTwoTemplates(t, connA)
	connA.SetDefaultSchema("s")
	connB := b.connect("s")

	const rows = 250 // past MAX_RECORDS_FOR_REBUILD (200)
	for lo := 0; lo < rows; lo += 50 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO t (id, v) VALUES ")
		for i := lo; i < lo+50; i++ {
			if i > lo {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "(%d, %d)", i, i)
		}
		if _, err := connA.ExecContext(ctx, sb.String(), nil); err != nil {
			t.Fatalf("insert batch at %d: %v", lo, err)
		}
	}

	if _, err := connA.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Statement 1 pins the transaction's read version and populates the
	// transaction-scoped catalog binding.
	if got := queryOneInt64(t, connA, "SELECT v FROM t WHERE id = 1"); got != 1 {
		t.Fatalf("stmt1 v = %d, want 1", got)
	}

	// The mid-transaction invalidation (what a DDL issued on this connection
	// does through invalidateSchemaCache): the binding entry is dropped and
	// statement 2 must RE-READ.
	connA.invalidateSchemaCache("/simdb", "s")

	// Cross-connection DDL commits a schema whose plan for the next statement
	// would be an index scan.
	swapSchemaToIndexed(t, connB)

	// The plan-source assert: statement 2's predicate is sargable on idx_v,
	// so a plan built from the post-DDL schema would contain the index scan.
	// Bound to the transaction, the re-read sees the transaction-snapshot
	// schema — no index — and the plan must not mention it.
	explain, eErr := connA.PlanExplain(ctx, "SELECT id FROM t WHERE v = 1")
	if eErr != nil {
		t.Fatalf("stmt2 explain: %v", eErr)
	}
	if strings.Contains(strings.ToUpper(explain), "IDX_V") {
		t.Fatalf("in-tx statement planned against the POST-DDL schema (plan contains idx_v): "+
			"the catalog resolved outside the transaction — RFC-198 Decision 8b's routing is "+
			"not load-bearing. Plan:\n%s", explain)
	}
	got := queryOneInt64(t, connA, "SELECT id FROM t WHERE v = 1")
	if got != 1 {
		t.Fatalf("in-tx SELECT WHERE v=1 returned id=%d, want 1: the catalog resolved at "+
			"a different version than the data — the plan-at-snapshot-A / rows-at-"+
			"snapshot-B skew RFC-198 Decision 8b closes", got)
	}
	// End the transaction; its commit outcome is (c)'s concern, not (a)'s.
	_ = connA.activeTx.Rollback()
}

// queryOneInt64 runs a single-column query on the driver connection and
// returns the one row's value; zero rows is reported as -1 so the caller's
// message can name the silent-zero-rows failure precisely.
func queryOneInt64(t *testing.T, c *EmbeddedConnection, sqlText string) int64 {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), sqlText, nil)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	defer rows.Close() //nolint:errcheck
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		if err == io.EOF {
			return -1 // zero rows, no error — the exact silent failure under test
		}
		t.Fatalf("%s next: %v", sqlText, err)
	}
	v, ok := dest[0].(int64)
	if !ok {
		t.Fatalf("%s returned %T, want int64", sqlText, dest[0])
	}
	return v
}

// TestCatalogBinding_CacheDoesNotCrossTransactionBoundary is criterion 12(b):
// on ONE connection (never returned to a pool — ResetSession must not be what
// saves it), a schema entry read inside transaction 1 must NOT be served to
// transaction 2. Transaction 2's fresh in-transaction read sees the
// cross-connection DDL and plans to the new index.
func TestCatalogBinding_CacheDoesNotCrossTransactionBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 79)
	connA := b.connect("")
	bootstrapTwoTemplates(t, connA)
	connA.SetDefaultSchema("s")
	connB := b.connect("s")

	if _, err := connA.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 1)", nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Transaction 1 populates the transaction-scoped binding, then commits.
	if _, err := connA.Begin(); err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if got := queryOneInt64(t, connA, "SELECT v FROM t WHERE id = 1"); got != 1 {
		t.Fatalf("tx1 v = %d, want 1", got)
	}
	if err := connA.activeTx.Commit(); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}

	// Cross-connection DDL lands between A's transactions.
	swapSchemaToIndexed(t, connB)

	// Transaction 2 on the SAME connection, no pool checkout in between: its
	// planning must re-read the catalog inside itself and see idx_v.
	if _, err := connA.Begin(); err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = connA.activeTx.Rollback() }()
	explain, err := connA.PlanExplain(ctx, "SELECT id FROM t WHERE v = 1")
	if err != nil {
		t.Fatalf("tx2 explain: %v", err)
	}
	// SQL identifiers normalize to upper case: the plan renders IDX_V.
	if !strings.Contains(strings.ToUpper(explain), "IDX_V") {
		t.Fatalf("tx2 planned without idx_v — the schema entry from transaction 1 was "+
			"served across the transaction boundary instead of being re-read inside "+
			"transaction 2 (RFC-198 Decision 8b, criterion 12(b)). Plan:\n%s", explain)
	}
}

// TestCatalogBinding_ConcurrentDDLConflictsCommit is criterion 12(c): the
// accepted cost is actually paid. The transaction's ONLY overlap with the
// concurrent DDL is the catalog record it read at planning time (its write
// goes to a different schema; DROP SCHEMA deletes the dropped schema's store,
// so any data read there would conflict on data ranges and mask what this
// test isolates). If the commit does not fail 40001, the in-transaction
// catalog read is not adding a conflict range and Decision 8b is not
// implemented, however green (a) looks.
func TestCatalogBinding_ConcurrentDDLConflictsCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 83)
	connA := b.connect("")
	bootstrapTwoTemplates(t, connA)
	// A second schema for A's write, untouched by B's DDL.
	if _, err := connA.ExecContext(ctx, "CREATE SCHEMA /simdb/other WITH TEMPLATE tmpl_plain", nil); err != nil {
		t.Fatalf("create other schema: %v", err)
	}
	connA.SetDefaultSchema("s")
	connB := b.connect("s")

	if _, err := connA.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Planning-only touch of schema s: reads the catalog record inside the
	// transaction (the conflict range under test), no data read.
	if _, err := connA.PlanExplain(ctx, "SELECT id FROM t WHERE v = 1"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The transaction's write lives in the OTHER schema.
	connA.SetDefaultSchema("other")
	if _, err := connA.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (100, 100)", nil); err != nil {
		t.Fatalf("insert into other: %v", err)
	}

	// Concurrent DDL rewrites the catalog record A read.
	swapSchemaToIndexed(t, connB)

	commitErr := connA.activeTx.Commit()
	if commitErr == nil {
		t.Fatalf("COMMIT succeeded after a concurrent DDL rewrote the catalog record this " +
			"transaction planned against: the in-transaction catalog read adds no conflict " +
			"range (RFC-198 Decision 8b's accepted cost is not being paid)")
	}
	var apiErr *api.Error
	if !errors.As(commitErr, &apiErr) {
		t.Fatalf("COMMIT failed with %v (%T), want *api.Error 40001", commitErr, commitErr)
	}
	if apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("COMMIT SQLSTATE %s, want %s (40001): %v",
			apiErr.Code, api.ErrCodeSerializationFailure, commitErr)
	}
}
