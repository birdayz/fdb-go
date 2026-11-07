package embedded

import (
	"context"
	"database/sql/driver"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/internal/connknob"
)

// statementReadMaxAge bounds how long after its first read an auto-commit
// statement's shared transaction may still take the first page: the page's
// own time budget (txPageTimeLimit, four seconds) is measured from the page's
// start, and both must fit FDB's five-second transaction lifetime with room
// for the commit.
const statementReadMaxAge = 250 * time.Millisecond

// Tests set a connection's bound (connknob) far above any latency when they
// need the shared first page, or tiny when they need a separate one, so
// neither depends on how fast the machine plans.
func init() {
	connknob.SetStatementReadMaxAge = func(conn driver.Conn, maxAge time.Duration) bool {
		c, ok := conn.(*EmbeddedConnection)
		if !ok || maxAge <= 0 {
			return false
		}
		c.stmtReadMaxAge = maxAge
		return true
	}
}

// stmtReadAgeBound is the connection's bound on the shared transaction's age
// at the first page: statementReadMaxAge unless a test set another.
func (c *EmbeddedConnection) stmtReadAgeBound() time.Duration {
	if c.stmtReadMaxAge > 0 {
		return c.stmtReadMaxAge
	}
	return statementReadMaxAge
}

// statementRead is the first transaction of an auto-commit QueryContext
// statement. The catalog read (ensureMetaData), the planner's index-state
// store open (fetchIndexStateSnapshot) and the first page join it, and the page
// reuses the planner's store, so a point query takes one read version and
// opens the store once instead of three transactions with a store open each.
//
// It is not an explicit transaction: no other statement sees it, the planner
// does not treat it as a single read version (later pages run their own
// transactions, as before), and nothing outside the statement can resume it.
// The first page commits it, which persists a store header the open wrote
// (checkPossiblyRebuild), before the page's rows are published. A retryable
// failure anywhere in it ends it, and the failed work reruns in a fresh,
// retrying auto-commit transaction exactly as it ran before: every page
// revalidates the plan's index-state dependency in its own transaction.
type statementRead struct {
	rctx   *recordlayer.FDBRecordContext
	opened time.Time
	stores map[string]*recordlayer.FDBRecordStore
}

// beginStatementRead arms the shared transaction of an auto-commit statement;
// it is created at the statement's first read. The returned func ends it. A
// statement that succeeded without a page committing it (EXPLAIN) commits it,
// as each of the transactions it replaces committed: a planning open's
// store-header upgrade or inline rebuild persists and post-commit hooks run.
// A failed or panicking statement cancels it; whatever it wrote is made again
// by the next transaction to open the store.
func (c *EmbeddedConnection) beginStatementRead() func(succeeded bool) {
	if c.activeTx != nil || c.stmtReadArmed {
		return func(bool) {}
	}
	c.stmtReadArmed = true
	return func(succeeded bool) {
		sr := c.stmtRead
		if !succeeded || sr == nil {
			c.dropStatementRead()
			return
		}
		c.stmtRead, c.stmtReadArmed = nil, false
		if err := sr.rctx.CommitWithHooks(); err != nil {
			sr.rctx.Cancel()
		}
	}
}

// statementReadFor returns the armed statement's shared transaction, creating
// it on first use, or nil when the statement has none (an explicit
// transaction, no statement armed, or a failure already ended it).
func (c *EmbeddedConnection) statementReadFor(ctx context.Context, opts *api.Options) *statementRead {
	if !c.stmtReadArmed || c.activeTx != nil || c.sess == nil || c.sess.DB == nil {
		return nil
	}
	if c.stmtRead != nil {
		return c.stmtRead
	}
	rctx, err := c.sess.DB.OpenRecordContext(ctx)
	if err != nil {
		c.stmtReadArmed = false
		return nil
	}
	if err := rctx.Configure(transactionConfigurer(opts)); err != nil {
		rctx.Cancel()
		c.stmtReadArmed = false
		return nil
	}
	c.stmtRead = &statementRead{rctx: rctx, opened: time.Now()}
	return c.stmtRead
}

// dropStatementRead cancels the shared transaction; the rest of the statement
// runs in fresh auto-commit transactions.
func (c *EmbeddedConnection) dropStatementRead() {
	c.stmtReadArmed = false
	if sr := c.stmtRead; sr != nil {
		c.stmtRead = nil
		sr.rctx.Cancel()
	}
}

// runStatementRead runs fn in the statement's shared transaction when it has
// one. ok is false when it has none, or when fn failed retryably there (the
// transaction is then dropped and the caller reruns fn in a fresh one); a
// non-retryable failure is fn's result, as a retrying runner would return it.
func (c *EmbeddedConnection) runStatementRead(ctx context.Context, opts *api.Options, fn func(*recordlayer.FDBRecordContext) (any, error)) (result any, err error, ok bool) {
	sr := c.statementReadFor(ctx, opts)
	if sr == nil {
		return nil, nil, false
	}
	result, err = fn(sr.rctx)
	if err == nil {
		return result, nil, true
	}
	c.dropStatementRead()
	if recordlayer.IsRetriable(err) {
		return nil, nil, false
	}
	return nil, err, true
}

// runFirstPage runs an auto-commit statement's first page in its shared
// transaction and commits it before the page is published. ok is false when
// the statement has no usable shared transaction (none, too old, or a
// retryable failure), and the caller runs the page as before.
func (c *EmbeddedConnection) runFirstPage(opts *api.Options, fn func(*recordlayer.FDBRecordContext) (any, error)) (result any, err error, ok bool) {
	// Whatever happens here, the statement's pages run their own
	// transactions from now on: a page is published only after the
	// transaction that produced it committed, which a shared transaction
	// created for a page (and committed after the statement) would not be.
	sr := c.stmtRead
	c.stmtReadArmed = false
	if sr == nil || c.activeTx != nil {
		c.stmtRead = nil
		return nil, nil, false
	}
	if time.Since(sr.opened) > c.stmtReadAgeBound() {
		c.stmtRead = nil
		sr.rctx.Cancel()
		return nil, nil, false
	}
	// Disarmed but still current while the page runs, so the page's storeIn
	// finds the planner's store (statementStore) and no other work joins.
	result, err = fn(sr.rctx)
	c.stmtRead = nil
	if err == nil {
		if err = sr.rctx.CommitWithHooks(); err == nil {
			return result, nil, true
		}
	}
	sr.rctx.Cancel()
	if recordlayer.IsRetriable(err) {
		return nil, nil, false
	}
	return nil, err, true
}

// runPage runs one page of a statement: an auto-commit statement's first
// page in its shared transaction when it has one, otherwise in tx or a fresh
// auto-commit transaction (runInCapturedTx).
func (c *EmbeddedConnection) runPage(ctx context.Context, tx *embeddedTx, opts *api.Options, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	if tx == nil {
		if result, err, ok := c.runFirstPage(opts, fn); ok {
			return result, err
		}
	}
	return c.runInCapturedTx(ctx, tx, opts, fn)
}

// statementStore returns the store the shared transaction rctx already opened
// for ss, or nil.
func (c *EmbeddedConnection) statementStore(rctx *recordlayer.FDBRecordContext, ss subspace.Subspace) *recordlayer.FDBRecordStore {
	if sr := c.stmtRead; sr != nil && sr.rctx == rctx {
		return sr.stores[string(ss.Bytes())]
	}
	return nil
}

// rememberStatementStore keeps a store opened in the shared transaction for
// the statement's later reads in it.
func (c *EmbeddedConnection) rememberStatementStore(rctx *recordlayer.FDBRecordContext, ss subspace.Subspace, store *recordlayer.FDBRecordStore) {
	if sr := c.stmtRead; sr != nil && sr.rctx == rctx {
		if sr.stores == nil {
			sr.stores = make(map[string]*recordlayer.FDBRecordStore)
		}
		sr.stores[string(ss.Bytes())] = store
	}
}
