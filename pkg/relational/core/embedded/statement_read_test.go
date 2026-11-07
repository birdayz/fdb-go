package embedded

// Regression tests for the statement read (statement_read.go): an auto-commit
// QueryContext statement's catalog read, planner store open and FIRST page
// share one transaction, which the first page commits before its rows are
// published. SimFDB-backed, so the tests can count the standalone
// transactions a statement creates (srBackend), the DB.Run attempts it makes
// (the database's AttemptObserver), the stores it opens (StoreTimer) and the
// transactions it leaves open (simgate), and inject commit faults.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/internal/connknob"
	"fdb.dev/pkg/relational/internal/simgate"
	"fdb.dev/pkg/simfdb"
)

const (
	srDBPath       = "/FRL/SIMDB"
	srSchema       = "S"
	srTemplateName = "SRTMPL"
)

// srBackend is a SimFDB database behind simgate, which tracks the standalone
// transactions left neither committed nor cancelled, that also counts the
// standalone transactions it creates. A statement's shared transaction and an
// explicit transaction are standalone; a DB.Run attempt is not.
type srBackend struct {
	*simgate.Backend
	sim     *simfdb.SimDB
	created atomic.Int64
}

func (b *srBackend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	b.created.Add(1)
	return b.Backend.CreateWritableTransaction()
}

// srHarness is one SimFDB database, a catalog over the driver's keyspace
// layout for catalog-level changes (another connection's DDL), and
// connections each over its own FDBDatabase, keyspace and catalog, the way a
// Connector builds them.
type srHarness struct {
	be  *srBackend
	env *dst.Env
	db  *recordlayer.FDBDatabase
	cat *catalog.RecordLayerStoreCatalog
	ks  *keyspace.RelationalKeyspace
}

func newSRHarness(t *testing.T, seed uint64) *srHarness {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatal(err)
	}
	be := &srBackend{Backend: simgate.New(sim), sim: sim}
	return &srHarness{be: be, env: env, db: recordlayer.NewFDBDatabaseWithBackend(be).SetEnv(env), cat: cat, ks: ks}
}

// srProbe observes one connection's database.
type srProbe struct {
	timer    *recordlayer.StoreTimer
	attempts atomic.Int64 // DB.Run and RunRead attempts
}

func (p *srProbe) reset() {
	p.timer.Reset()
	p.attempts.Store(0)
}

// open is a connection on schema S of /FRL/SIMDB, with the engine-wide plan
// cache a driver installs. Its first page joins the shared transaction
// however long the statement took to plan.
func (h *srHarness) open(t *testing.T) (*EmbeddedConnection, *srProbe) {
	t.Helper()
	fdbDB := recordlayer.NewFDBDatabaseWithBackend(h.be).SetEnv(h.env)
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	p := &srProbe{timer: recordlayer.NewStoreTimer()}
	fdbDB.SetTimer(p.timer)
	fdbDB.SetAttemptObserver(func(recordlayer.AttemptCall, error) { p.attempts.Add(1) })
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatal(err)
	}
	c := New(srDBPath, fdbDB, cat, ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks), ks)
	c.SetPlanCache(NewRelationalPlanCache(c.Options()))
	c.SetDefaultSchema(srSchema)
	c.stmtReadMaxAge = time.Hour
	return c, p
}

// run runs fn in a committed transaction of the harness's own database.
func (h *srHarness) run(t *testing.T, what string, fn func(rctx *recordlayer.FDBRecordContext) error) {
	t.Helper()
	if _, err := h.db.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(rctx)
	}); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// srTemplate is template SRTMPL at version over T(PK, C, V) with index
// T_BY_C on C; evolved adds column W and index T_BY_W on it.
func srTemplate(t *testing.T, version int, evolved bool) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	cols := []metadata.ColumnSpec{
		metadata.NewColumnSpec("PK", api.NewLongType(false), 1),
		metadata.NewColumnSpec("C", api.NewLongType(true), 2),
		metadata.NewColumnSpec("V", api.NewLongType(true), 3),
	}
	if evolved {
		cols = append(cols, metadata.NewColumnSpec("W", api.NewLongType(true), 4))
	}
	b := metadata.NewSchemaTemplateBuilder().SetName(srTemplateName).SetVersion(version)
	b.AddTable("T", cols, []string{"PK"})
	b.AddIndex("T", "T_BY_C", []string{"C"}, false)
	if evolved {
		b.AddIndex("T", "T_BY_W", []string{"W"}, false)
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build %s@%d: %v", srTemplateName, version, err)
	}
	return tmpl
}

// bootstrap creates /FRL/SIMDB and schema S on SRTMPL@1.
func (h *srHarness) bootstrap(t *testing.T) {
	t.Helper()
	h.run(t, "bootstrap", func(rctx *recordlayer.FDBRecordContext) error {
		txn := catalog.NewFDBTransaction(rctx)
		if err := h.cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(srDBPath, h.cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(srTemplate(t, 1, false), h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return ddl.NewCreateSchemaConstantAction(srDBPath, srSchema, srTemplateName, h.cat, h.ks).Execute(txn)
	})
}

// evolve is another connection's schema change: SRTMPL@2 (column W, index
// T_BY_W) saved and S re-pointed to it. Nothing here opens S's store, so the
// next open of it rewrites its header.
func (h *srHarness) evolve(t *testing.T) {
	t.Helper()
	h.run(t, "evolve", func(rctx *recordlayer.FDBRecordContext) error {
		txn := catalog.NewFDBTransaction(rctx)
		if err := ddl.NewSaveSchemaTemplateConstantAction(srTemplate(t, 2, true), h.cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return h.cat.RepairSchema(txn, srDBPath, srSchema)
	})
}

// rewriteTemplateRow writes SRTMPL@version's catalog row raw, past the
// catalog's checks, as a restore or a Java writer would: same key, the bytes
// of tmpl.
func (h *srHarness) rewriteTemplateRow(t *testing.T, version int, tmpl *metadata.RecordLayerSchemaTemplate) {
	t.Helper()
	p, err := tmpl.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	md, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	catalogMD, err := catalog.BuildCatalogMetaData()
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, "rewrite template row", func(rctx *recordlayer.FDBRecordContext) error {
		store, err := recordlayer.NewStoreBuilder().SetContext(rctx).SetSubspace(h.ks.CatalogSubspace()).
			SetMetaDataProvider(catalogMD).CreateOrOpen()
		if err != nil {
			return err
		}
		_, err = store.SaveRecord(&gen.Templates{
			TEMPLATE_NAME: proto.String(srTemplateName), TEMPLATE_VERSION: proto.Int32(int32(version)), META_DATA: md,
		})
		return err
	})
}

// srInsertRows inserts PK = C = 1..n, V = 10*PK.
func srInsertRows(t *testing.T, c *EmbeddedConnection, n int) {
	t.Helper()
	sql := "INSERT INTO T (PK, C, V) VALUES "
	for i := 1; i <= n; i++ {
		if i > 1 {
			sql += ", "
		}
		sql += fmt.Sprintf("(%d, %d, %d)", i, i, 10*i)
	}
	if _, err := c.ExecContext(context.Background(), sql, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// srQuery runs sqlText to the end and closes its rows, returning them
// rendered ("[[3 30]]") or the statement's error.
func srQuery(c *EmbeddedConnection, sqlText string) (string, error) {
	rows, err := c.QueryContext(context.Background(), sqlText, nil)
	if err != nil {
		return "", err
	}
	var out [][]driver.Value
	for {
		dest := make([]driver.Value, len(rows.Columns()))
		if err := rows.Next(dest); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			_ = rows.Close()
			return "", err
		}
		out = append(out, dest)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	return fmt.Sprint(out), nil
}

func srMustQuery(t *testing.T, c *EmbeddedConnection, sqlText string) string {
	t.Helper()
	got, err := srQuery(c, sqlText)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	return got
}

func srResetSession(t *testing.T, c *EmbeddedConnection) {
	t.Helper()
	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
}

// srNoStatementRead asserts the statement left no shared transaction armed.
func srNoStatementRead(t *testing.T, c *EmbeddedConnection, what string) {
	t.Helper()
	if c.stmtRead != nil || c.stmtReadArmed {
		t.Fatalf("%s left the statement read behind (transaction %v, armed %v)", what, c.stmtRead != nil, c.stmtReadArmed)
	}
}

type srStatsLogger struct {
	mu    sync.Mutex
	stats []ExecutionStats
}

func (l *srStatsLogger) LogExecutionStats(_ context.Context, s ExecutionStats) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats = append(l.stats, s)
}

// TestStatementRead_PointSelectIsOneTransaction: on a pooled connection
// (ResetSession between statements), a point SELECT takes one standalone
// transaction and no DB.Run attempt, opens the store once (the planner's open
// serves the page; the catalog is checked by the witness, not reloaded), and
// commits it. Before the statement read it took three transactions: the
// catalog read, the planner's store open and the page.
func TestStatementRead_PointSelectIsOneTransaction(t *testing.T) {
	t.Parallel()
	h := newSRHarness(t, 211)
	h.bootstrap(t)
	c, p := h.open(t)
	srInsertRows(t, c, 5)
	const q = "SELECT PK, V FROM T WHERE PK = 3"
	if got := srMustQuery(t, c, q); got != "[[3 30]]" {
		t.Fatalf("warm-up rows %s, want [[3 30]]", got)
	}
	srResetSession(t, c)
	if n := h.be.OpenTransactions(); n != 0 {
		t.Fatalf("%d transactions open before the statement", n)
	}
	p.reset()
	created := h.be.created.Load()

	if got := srMustQuery(t, c, q); got != "[[3 30]]" {
		t.Fatalf("rows %s, want [[3 30]]", got)
	}
	if n := h.be.created.Load() - created; n != 1 {
		t.Fatalf("the statement created %d standalone transactions, want 1 (its shared transaction)", n)
	}
	if n := p.attempts.Load(); n != 0 {
		t.Fatalf("the statement made %d DB.Run attempts, want 0: its catalog read, store open or page left the shared transaction", n)
	}
	if n := p.timer.GetCount(recordlayer.EventOpenStore); n != 1 {
		t.Fatalf("the statement opened %d stores, want 1: the planner's open of T's store serves the page, "+
			"and the unchanged catalog is checked by its witness without opening the catalog store", n)
	}
	if n := h.be.OpenTransactions(); n != 0 {
		t.Fatalf("%d transactions left open: the first page did not commit the shared transaction", n)
	}
	srNoStatementRead(t, c, "the point SELECT")
}

// TestStatementRead_LaterPagesRunTheirOwnTransactions: with a two-row scan
// limit a seven-row SELECT pages; every row arrives exactly once, the first
// page runs in the statement's shared transaction and every later page in a
// DB.Run attempt of its own.
func TestStatementRead_LaterPagesRunTheirOwnTransactions(t *testing.T) {
	t.Parallel()
	h := newSRHarness(t, 223)
	h.bootstrap(t)
	c, p := h.open(t)
	srInsertRows(t, c, 7)
	// The connection's first QueryContext bootstraps the catalog (ensureCatalogInit, one DB.Run); warm it outside the count.
	srMustQuery(t, c, "SELECT PK FROM T WHERE PK = 1")
	srResetSession(t, c)
	if err := c.SetOption(api.OptExecutionScannedRowsLimit, 2); err != nil {
		t.Fatal(err)
	}
	stats := &srStatsLogger{}
	c.SetExecutionStatsLogger(stats)
	p.reset()
	created := h.be.created.Load()

	if got := srMustQuery(t, c, "SELECT PK FROM T"); got != "[[1] [2] [3] [4] [5] [6] [7]]" {
		t.Fatalf("rows %s, want every PK 1..7 exactly once", got)
	}
	if len(stats.stats) != 1 {
		t.Fatalf("%d execution records, want 1", len(stats.stats))
	}
	s := stats.stats[0]
	if s.Pages < 2 || s.Retries != 0 || s.Err != nil {
		t.Fatalf("execution %+v: want at least two pages, no retry and no error", s)
	}
	if n := p.attempts.Load(); n != int64(s.Pages-1) {
		t.Fatalf("%d DB.Run attempts over %d pages, want %d: one per page after the first", n, s.Pages, s.Pages-1)
	}
	if n := h.be.created.Load() - created; n != 1 {
		t.Fatalf("the statement created %d standalone transactions, want 1", n)
	}
	if n := h.be.OpenTransactions(); n != 0 {
		t.Fatalf("%d transactions left open", n)
	}
	srNoStatementRead(t, c, "the paged SELECT")
}

// TestStatementRead_NoPageLeavesNoTransaction: a statement that runs no page
// in its shared transaction cancels it, whether it returned rows that are no
// page (EXPLAIN), failed after its catalog read, or panicked inside the
// shared transaction.
func TestStatementRead_NoPageLeavesNoTransaction(t *testing.T) {
	t.Parallel()
	for name, stmt := range map[string]func(t *testing.T, h *srHarness, c *EmbeddedConnection) error{
		"EXPLAIN": func(t *testing.T, _ *srHarness, c *EmbeddedConnection) error {
			got, err := srQuery(c, "EXPLAIN SELECT PK FROM T WHERE PK = 3")
			if err == nil && got == "[]" {
				t.Fatal("EXPLAIN returned no rows")
			}
			return err
		},
		"planning failure": func(t *testing.T, _ *srHarness, c *EmbeddedConnection) error {
			_, err := srQuery(c, "SELECT NO_SUCH_COLUMN FROM T")
			if err == nil {
				t.Fatal("a SELECT of an undefined column succeeded")
			}
			return nil
		},
		"panic in the shared transaction": func(t *testing.T, h *srHarness, c *EmbeddedConnection) error {
			h.be.PanicOnce(simgate.Read)
			_, err := srQuery(c, "SELECT PK FROM T WHERE PK = 3")
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError {
				t.Fatalf("a statement whose read panicked = %v, want the recovered-panic error", err)
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newSRHarness(t, 227)
			h.bootstrap(t)
			c, p := h.open(t)
			srInsertRows(t, c, 3)
			srMustQuery(t, c, "SELECT PK FROM T WHERE PK = 3")
			srResetSession(t, c)
			p.reset()
			created := h.be.created.Load()
			if err := stmt(t, h, c); err != nil {
				t.Fatal(err)
			}
			if n := h.be.created.Load() - created; n != 1 {
				t.Fatalf("the statement created %d standalone transactions, want 1: its reads did not take the shared transaction", n)
			}
			if n := p.attempts.Load(); n != 0 {
				t.Fatalf("the statement made %d DB.Run attempts, want 0", n)
			}
			if n := h.be.OpenTransactions(); n != 0 {
				t.Fatalf("%d transactions left open: the statement did not cancel its shared transaction", n)
			}
			srNoStatementRead(t, c, name)
		})
	}
}

// TestStatementRead_ExplicitTransactionNeverArms: inside BeginTx every
// statement reads in the explicit transaction, as before: no shared
// transaction, no DB.Run attempt, the catalog read inside the transaction
// (the witness is not consulted) and read-your-writes.
func TestStatementRead_ExplicitTransactionNeverArms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newSRHarness(t, 229)
	h.bootstrap(t)
	c, p := h.open(t)
	srInsertRows(t, c, 3)
	srMustQuery(t, c, "SELECT PK FROM T WHERE PK = 3")
	srResetSession(t, c)

	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.reset()
	created := h.be.created.Load()
	drop := c.beginStatementRead()
	if c.stmtReadArmed {
		t.Fatal("beginStatementRead armed the statement read inside an explicit transaction")
	}
	drop(false)
	if sr := c.statementReadFor(ctx, c.Options()); sr != nil {
		t.Fatal("statementReadFor made a shared transaction inside an explicit transaction")
	}
	if got := srMustQuery(t, c, "SELECT PK, V FROM T WHERE PK = 3"); got != "[[3 30]]" {
		t.Fatalf("rows in the transaction %s, want [[3 30]]", got)
	}
	if n := p.timer.GetCount(recordlayer.EventOpenStore); n < 2 {
		t.Fatalf("the statement opened %d stores, want the catalog store too: a statement in an explicit "+
			"transaction reads its schema in the transaction, never through the witness", n)
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO T (PK, C, V) VALUES (9, 9, 90)", nil); err != nil {
		t.Fatal(err)
	}
	if got := srMustQuery(t, c, "SELECT PK, V FROM T WHERE PK = 9"); got != "[[9 90]]" {
		t.Fatalf("the transaction does not read its own write: %s", got)
	}
	if n := h.be.created.Load() - created; n != 0 {
		t.Fatalf("statements in the explicit transaction created %d standalone transactions, want 0", n)
	}
	if n := p.attempts.Load(); n != 0 {
		t.Fatalf("statements in the explicit transaction made %d DB.Run attempts, want 0", n)
	}
	srNoStatementRead(t, c, "a statement in an explicit transaction")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := srMustQuery(t, c, "SELECT PK FROM T WHERE PK = 9"); got != "[]" {
		t.Fatalf("the rolled-back insert is visible: %s", got)
	}
	if n := h.be.OpenTransactions(); n != 0 {
		t.Fatalf("%d transactions left open", n)
	}
}

// TestStatementRead_SharedCommitFailureRerunsThePage is the end-to-end
// fallback: another connection re-points the schema, so the statement's
// planner open rewrites the store header in the shared transaction, and the
// shared commit is failed (not_committed). The page reruns in one DB.Run
// attempt of its own and returns the right row over the new schema.
func TestStatementRead_SharedCommitFailureRerunsThePage(t *testing.T) {
	t.Parallel()
	h := newSRHarness(t, 233)
	h.bootstrap(t)
	c, p := h.open(t)
	srInsertRows(t, c, 5)
	srMustQuery(t, c, "SELECT PK, V FROM T WHERE PK = 3")
	h.evolve(t)
	srResetSession(t, c)
	p.reset()
	created := h.be.created.Load()
	// A read-only commit never reaches the injection; the shared commit
	// carries the header rewrite.
	h.be.sim.InjectOnce(1020)

	if got := srMustQuery(t, c, "SELECT PK, W FROM T WHERE PK = 3"); got != "[[3 <nil>]]" {
		t.Fatalf("rows %s, want [[3 <nil>]] over the re-pointed schema", got)
	}
	if n := p.attempts.Load(); n != 1 {
		t.Fatalf("%d DB.Run attempts, want 1: the page rerun after the shared commit failed "+
			"(0: the shared commit did not fail; 2: the injected fault reached the rerun instead)", n)
	}
	if n := h.be.created.Load() - created; n != 1 {
		t.Fatalf("the statement created %d standalone transactions, want 1", n)
	}
	if n := h.be.OpenTransactions(); n != 0 {
		t.Fatalf("%d transactions left open: the failed shared transaction was not cancelled", n)
	}
	srNoStatementRead(t, c, "the rerun statement")
	if md := c.cachedMetaData(); md == nil || md.GetIndex("T_BY_W") == nil {
		t.Fatal("the statement planned without SRTMPL@2's index T_BY_W")
	}
	// The injected fault was spent: the next writing commit commits first time.
	var attempts atomic.Int64
	writer := recordlayer.NewFDBDatabaseWithBackend(h.be).SetEnv(h.env)
	writer.SetAttemptObserver(func(recordlayer.AttemptCall, error) { attempts.Add(1) })
	if _, err := writer.Run(context.Background(), func(rctx *recordlayer.FDBRecordContext) (any, error) {
		rctx.Transaction().Set(fdb.Key(subspace.Sub("sr-probe", t.Name()).Pack(tuple.Tuple{"k"})), []byte("v"))
		return nil, nil
	}); err != nil || attempts.Load() != 1 {
		t.Fatalf("probe write = %v after %d attempts, want one clean attempt: the injected fault was still pending", err, attempts.Load())
	}
}

// srWork is a transaction body that records each transaction it runs in.
type srWork struct {
	in   []*recordlayer.FDBRecordContext
	body func(call int, rctx *recordlayer.FDBRecordContext) error
}

func (w *srWork) fn(rctx *recordlayer.FDBRecordContext) (any, error) {
	w.in = append(w.in, rctx)
	if w.body == nil {
		return nil, nil
	}
	return nil, w.body(len(w.in), rctx)
}

// srKeyValue reads key in a fresh transaction; nil when absent.
func (h *srHarness) srKeyValue(t *testing.T, key fdb.Key) []byte {
	t.Helper()
	var v []byte
	h.run(t, "read "+key.String(), func(rctx *recordlayer.FDBRecordContext) error {
		var err error
		v, err = rctx.Transaction().Get(key).Get()
		return err
	})
	return v
}

// TestStatementRead_RunInCapturedTx drives runStatementRead through
// runInCapturedTx, the route of a statement's planning reads: armed, they
// share one transaction; a retryable failure drops it and reruns the work in
// DB.Run; a non-retryable failure is the work's error; and once dropped, the
// statement's later reads use DB.Run.
func TestStatementRead_RunInCapturedTx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := func(t *testing.T) (*srHarness, *EmbeddedConnection, *srProbe, int64) {
		h := newSRHarness(t, 239)
		c, p := h.open(t)
		end := c.beginStatementRead()
		t.Cleanup(func() { end(false) })
		return h, c, p, h.be.created.Load()
	}

	t.Run("reads share the shared transaction", func(t *testing.T) {
		t.Parallel()
		h, c, p, created := setup(t)
		w := &srWork{}
		for range 2 {
			if _, err := c.runInCapturedTx(ctx, nil, c.Options(), w.fn); err != nil {
				t.Fatal(err)
			}
		}
		if c.stmtRead == nil || w.in[0] != c.stmtRead.rctx || w.in[1] != c.stmtRead.rctx {
			t.Fatal("the statement's reads did not run in its shared transaction")
		}
		if n := h.be.created.Load() - created; n != 1 || p.attempts.Load() != 0 {
			t.Fatalf("created %d standalone transactions and %d attempts, want 1 and 0", n, p.attempts.Load())
		}
		c.dropStatementRead()
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open after the statement ended", n)
		}
	})

	t.Run("a retryable failure reruns in DB.Run", func(t *testing.T) {
		t.Parallel()
		h, c, p, created := setup(t)
		w := &srWork{body: func(call int, _ *recordlayer.FDBRecordContext) error {
			if call == 1 {
				return fmt.Errorf("catalog read: %w", fdb.Error{Code: 1020})
			}
			return nil
		}}
		if _, err := c.runInCapturedTx(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if len(w.in) != 2 || w.in[0] == w.in[1] || p.attempts.Load() != 1 {
			t.Fatalf("ran %d times in %d DB.Run attempts, want twice: once shared, once in one attempt", len(w.in), p.attempts.Load())
		}
		srNoStatementRead(t, c, "a retryable failure")
		if _, err := c.runInCapturedTx(ctx, nil, c.Options(), (&srWork{}).fn); err != nil {
			t.Fatal(err)
		}
		if n := h.be.created.Load() - created; n != 1 || p.attempts.Load() != 2 {
			t.Fatalf("after the drop: %d standalone transactions and %d attempts, want 1 and 2 (a later read uses DB.Run)", n, p.attempts.Load())
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open: the failed shared transaction was not cancelled", n)
		}
	})

	t.Run("a non-retryable failure is the work's error", func(t *testing.T) {
		t.Parallel()
		h, c, p, _ := setup(t)
		boom := errors.New("boom")
		w := &srWork{body: func(int, *recordlayer.FDBRecordContext) error { return boom }}
		if _, err := c.runInCapturedTx(ctx, nil, c.Options(), w.fn); !errors.Is(err, boom) {
			t.Fatalf("runInCapturedTx = %v, want the work's error", err)
		}
		if len(w.in) != 1 || p.attempts.Load() != 0 {
			t.Fatalf("ran %d times in %d attempts, want once and no rerun", len(w.in), p.attempts.Load())
		}
		srNoStatementRead(t, c, "a non-retryable failure")
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open", n)
		}
	})

	t.Run("an explicit transaction is the work's transaction", func(t *testing.T) {
		t.Parallel()
		h := newSRHarness(t, 241)
		c, p := h.open(t)
		if _, err := c.BeginTx(ctx, driver.TxOptions{}); err != nil {
			t.Fatal(err)
		}
		created := h.be.created.Load()
		drop := c.beginStatementRead()
		w := &srWork{}
		if _, err := c.runInCapturedTx(ctx, c.activeTx, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		drop(false)
		if len(w.in) != 1 || w.in[0] != c.activeTx.rctx {
			t.Fatal("work in an explicit transaction did not run in it")
		}
		if n := h.be.created.Load() - created; n != 0 || p.attempts.Load() != 0 {
			t.Fatalf("created %d transactions and %d attempts, want none", n, p.attempts.Load())
		}
		if err := c.activeTx.Rollback(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStatementRead_RunPage drives runPage's first-page arm: the first page
// runs in the shared transaction and commits it before returning; an aged
// shared transaction is dropped; a retryable failure of the page or of its
// commit reruns the page in DB.Run with the shared writes discarded; a
// non-retryable one is the page's error; later pages, and pages of a
// statement with no shared transaction, run in DB.Run.
func TestStatementRead_RunPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// setup arms a statement and makes its shared transaction with a planning
	// read, as a statement's catalog read does.
	setup := func(t *testing.T) (*srHarness, *EmbeddedConnection, *srProbe, *recordlayer.FDBRecordContext) {
		h := newSRHarness(t, 251)
		c, p := h.open(t)
		end := c.beginStatementRead()
		t.Cleanup(func() { end(false) })
		w := &srWork{}
		if _, err := c.runInCapturedTx(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if c.stmtRead == nil || w.in[0] != c.stmtRead.rctx {
			t.Fatal("the planning read made no shared transaction")
		}
		p.reset()
		return h, c, p, w.in[0]
	}
	keys := func(t *testing.T) (value, marker fdb.Key) {
		ss := subspace.Sub("statement-read-page", t.Name())
		return ss.Pack(tuple.Tuple{"value"}), ss.Pack(tuple.Tuple{"marker"})
	}
	// writes sets value to the call number, and marker on the first call only.
	writes := func(value, marker fdb.Key) func(call int, rctx *recordlayer.FDBRecordContext) error {
		return func(call int, rctx *recordlayer.FDBRecordContext) error {
			rctx.Transaction().Set(value, []byte(fmt.Sprint(call)))
			if call == 1 {
				rctx.Transaction().Set(marker, []byte("shared"))
			}
			return nil
		}
	}

	t.Run("the first page commits the shared transaction", func(t *testing.T) {
		t.Parallel()
		h, c, p, shared := setup(t)
		value, marker := keys(t)
		w := &srWork{body: writes(value, marker)}
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if len(w.in) != 1 || w.in[0] != shared || p.attempts.Load() != 0 {
			t.Fatalf("the first page ran %d times, shared %v, in %d attempts; want once in the shared transaction",
				len(w.in), len(w.in) > 0 && w.in[0] == shared, p.attempts.Load())
		}
		if got := h.srKeyValue(t, value); string(got) != "1" {
			t.Fatalf("the first page's write is %q after it returned, want \"1\": the page was published uncommitted", got)
		}
		srNoStatementRead(t, c, "the first page")
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open after the first page", n)
		}
		later := &srWork{}
		if _, err := c.runPage(ctx, nil, c.Options(), later.fn); err != nil {
			t.Fatal(err)
		}
		if later.in[0] == shared || p.attempts.Load() != 1 {
			t.Fatalf("a later page ran in the shared transaction or outside DB.Run (%d attempts)", p.attempts.Load())
		}
	})

	t.Run("an aged shared transaction is dropped", func(t *testing.T) {
		t.Parallel()
		h, c, p, shared := setup(t)
		c.stmtRead.opened = time.Now().Add(-2 * c.stmtReadAgeBound())
		value, marker := keys(t)
		w := &srWork{body: writes(value, marker)}
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if len(w.in) != 1 || w.in[0] == shared || p.attempts.Load() != 1 {
			t.Fatalf("the page ran %d times in %d attempts (shared: %v), want once in DB.Run",
				len(w.in), p.attempts.Load(), len(w.in) > 0 && w.in[0] == shared)
		}
		if got := h.srKeyValue(t, value); string(got) != "1" {
			t.Fatalf("the page's write is %q, want \"1\"", got)
		}
		srNoStatementRead(t, c, "an aged shared transaction")
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open: the aged shared transaction was not cancelled", n)
		}
	})

	t.Run("a retryable commit failure reruns the page", func(t *testing.T) {
		t.Parallel()
		h, c, p, shared := setup(t)
		value, marker := keys(t)
		w := &srWork{body: writes(value, marker)}
		h.be.sim.InjectOnce(1020)
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if len(w.in) != 2 || w.in[0] != shared || w.in[1] == shared || p.attempts.Load() != 1 {
			t.Fatalf("the page ran %d times in %d attempts, want once shared and once in one DB.Run attempt", len(w.in), p.attempts.Load())
		}
		if got := h.srKeyValue(t, value); string(got) != "2" {
			t.Fatalf("value %q, want \"2\" (the rerun's)", got)
		}
		if got := h.srKeyValue(t, marker); got != nil {
			t.Fatalf("the failed shared commit's write %q is visible", got)
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open: the failed shared transaction was not cancelled", n)
		}
	})

	t.Run("a non-retryable commit failure is the page's error", func(t *testing.T) {
		t.Parallel()
		h, c, p, _ := setup(t)
		value, marker := keys(t)
		w := &srWork{body: writes(value, marker)}
		h.be.sim.InjectOnce(2101) // transaction_too_large
		_, err := c.runPage(ctx, nil, c.Options(), w.fn)
		var fe fdb.Error
		if !errors.As(err, &fe) || fe.Code != 2101 {
			t.Fatalf("runPage = %v, want the commit's 2101", err)
		}
		if len(w.in) != 1 || p.attempts.Load() != 0 {
			t.Fatalf("the page ran %d times in %d attempts, want once and no rerun", len(w.in), p.attempts.Load())
		}
		if got := h.srKeyValue(t, value); got != nil {
			t.Fatalf("the failed commit's write %q is visible", got)
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open", n)
		}
	})

	t.Run("a retryable page failure reruns the page", func(t *testing.T) {
		t.Parallel()
		h, c, p, shared := setup(t)
		w := &srWork{body: func(call int, _ *recordlayer.FDBRecordContext) error {
			if call == 1 {
				return fmt.Errorf("page: %w", fdb.Error{Code: 1007})
			}
			return nil
		}}
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if len(w.in) != 2 || w.in[0] != shared || w.in[1] == shared || p.attempts.Load() != 1 {
			t.Fatalf("the page ran %d times in %d attempts, want once shared and once in DB.Run", len(w.in), p.attempts.Load())
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open", n)
		}
	})

	t.Run("a non-retryable page failure is the page's error", func(t *testing.T) {
		t.Parallel()
		h, c, p, _ := setup(t)
		boom := errors.New("boom")
		w := &srWork{body: func(int, *recordlayer.FDBRecordContext) error { return boom }}
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); !errors.Is(err, boom) {
			t.Fatalf("runPage = %v, want the page's error", err)
		}
		if len(w.in) != 1 || p.attempts.Load() != 0 {
			t.Fatalf("the page ran %d times in %d attempts, want once", len(w.in), p.attempts.Load())
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions open", n)
		}
	})

	t.Run("no shared transaction", func(t *testing.T) {
		t.Parallel()
		h := newSRHarness(t, 257)
		c, p := h.open(t)
		created := h.be.created.Load()
		w := &srWork{}
		if _, err := c.runPage(ctx, nil, c.Options(), w.fn); err != nil {
			t.Fatal(err)
		}
		if n := h.be.created.Load() - created; n != 0 || p.attempts.Load() != 1 {
			t.Fatalf("an unarmed page created %d standalone transactions in %d attempts, want 0 and 1", n, p.attempts.Load())
		}
	})
}

// srOtherConn is a driver connection that is not an EmbeddedConnection.
type srOtherConn struct{ driver.Conn }

// TestStatementRead_AgeBound: a connection's first page joins the shared
// transaction up to statementReadMaxAge, 250 ms, after the statement's first
// read, unless connknob set another positive bound, which ResetSession keeps.
func TestStatementRead_AgeBound(t *testing.T) {
	t.Parallel()
	h := newSRHarness(t, 263)
	c := New(srDBPath, h.db, h.cat, ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(h.cat, h.ks), h.ks)
	if got := c.stmtReadAgeBound(); got != 250*time.Millisecond {
		t.Fatalf("a new connection's bound is %v, want 250ms", got)
	}
	if !connknob.SetStatementReadMaxAge(c, time.Second) || c.stmtReadAgeBound() != time.Second {
		t.Fatalf("connknob did not set the bound: %v", c.stmtReadAgeBound())
	}
	srResetSession(t, c)
	if got := c.stmtReadAgeBound(); got != time.Second {
		t.Fatalf("after ResetSession the bound is %v, want the 1s connknob set", got)
	}
	if connknob.SetStatementReadMaxAge(c, 0) || connknob.SetStatementReadMaxAge(srOtherConn{}, time.Second) {
		t.Fatal("connknob accepted a zero bound or a connection that is not embedded")
	}
	if got := c.stmtReadAgeBound(); got != time.Second {
		t.Fatalf("a refused connknob call changed the bound to %v", got)
	}
}
