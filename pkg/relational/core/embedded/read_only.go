package embedded

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// SetReadOnly makes the connection read-only for the rest of its life; nothing
// makes it writable again, ResetSession included. It is Go-only: Java's
// relational connection has no such mode.
//
// A read-only connection plans only a SELECT, or EXPLAIN/DESCRIBE of one, and
// refuses every other statement and every multi-statement batch with 0A000
// (planOne). It cancels every transaction it makes instead of committing it:
// the statement's shared transaction, its pages, its catalog reads and an
// explicit transaction's Commit. So nothing a read does persists, such as the
// store-header upgrade or inline index rebuild an open makes on a store an
// older version wrote, or a catalog bootstrap. Schema names are looked up,
// never interned. The Go APIs that write (direct-access insert and delete,
// statistics collection, metadata operations) refuse too. BeginTx accepts
// TxOptions.ReadOnly.
func (c *EmbeddedConnection) SetReadOnly() { c.readOnly = true }

// errReadOnlyOperation refuses a writing Go API on a read-only connection.
func errReadOnlyOperation(what string) error {
	return api.NewErrorf(api.ErrCodeUnsupportedOperation, "read-only connection: %s is not allowed", what)
}

// errNotCommitted ends a cancel-only attempt whose body succeeded.
// FDBDatabase.Run commits every body that returns no error, so a body that
// must not commit returns this instead, and runCancelled reports the body's
// result: Run's retry loop, attempt bound, retry delays and context handling
// stay the database's.
var errNotCommitted = errors.New("read-only transaction: not committed")

// runCancelled runs fn as db.Run does, retrying retriable failures, but never
// commits: the transactor discards each attempt's transaction, so nothing fn
// wrote persists and no post-commit hook runs.
func runCancelled(ctx context.Context, db *recordlayer.FDBDatabase, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	var (
		result    any
		succeeded bool
	)
	_, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		succeeded = false
		out, err := fn(rctx)
		if err != nil {
			return nil, err
		}
		result, succeeded = out, true
		return nil, errNotCommitted
	})
	if succeeded {
		return result, nil
	}
	return nil, err
}

// cancellingRunner is a keyspace.Runner whose transactions are never
// committed (runCancelled).
type cancellingRunner struct{ db *recordlayer.FDBDatabase }

func (r cancellingRunner) Run(ctx context.Context, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	return runCancelled(ctx, r.db, fn)
}

// ReadOnlyRunner runs work on db as a read-only connection does: each
// transaction is cancelled, never committed. A driver that opens read-only
// connections reads the catalog at its start through it.
func ReadOnlyRunner(db *recordlayer.FDBDatabase) keyspace.Runner {
	return cancellingRunner{db: db}
}

// endTransaction ends a transaction the connection opened outside Run: it
// commits it, or cancels it on a read-only connection.
func (c *EmbeddedConnection) endTransaction(rctx *recordlayer.FDBRecordContext) error {
	if c.readOnly {
		rctx.Cancel()
		return nil
	}
	return rctx.CommitWithHooks()
}

// sessionSchemaSubspace resolves the session schema's store subspace. A
// read-only connection only looks the names up: resolving interns a missing
// name in a transaction of its own, which commits.
func (c *EmbeddedConnection) sessionSchemaSubspace(ctx context.Context) (subspace.Subspace, error) {
	run := c.runner(c.Options())
	if c.readOnly {
		return c.sess.Keyspace.LookupSchemaSubspace(ctx, run, c.sess.DBPath, c.sess.Schema)
	}
	return c.sess.Keyspace.SchemaSubspaceIn(ctx, run, c.sess.DBPath, c.sess.Schema)
}
