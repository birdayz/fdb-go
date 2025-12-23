package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// FDBDatabase provides access to the underlying FoundationDB database
// and manages transaction execution with retry logic.
// This is the Record Layer equivalent of Java's FDBDatabase.
type FDBDatabase struct {
	db fdb.Database
}

// NewFDBDatabase creates a new FDBDatabase wrapping the core FDB database
func NewFDBDatabase(db fdb.Database) *FDBDatabase {
	return &FDBDatabase{db: db}
}

// Run executes a function within a transaction with automatic retry handling.
// This matches the Java Record Layer pattern.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (interface{}, error)) (interface{}, error) {
	// Use FDB's built-in transactional function with retry logic
	return d.db.Transact(func(tx fdb.Transaction) (interface{}, error) {
		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}

		return fn(recordCtx)
	})
}

// Database returns the underlying FDB database
func (d *FDBDatabase) Database() fdb.Database {
	return d.db
}

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
type FDBRecordContext struct {
	tx  fdb.Transaction
	ctx context.Context
}

// NewFDBRecordContext creates a new FDBRecordContext wrapping an FDB transaction.
// This is primarily used for testing scenarios where direct transaction control is needed.
func NewFDBRecordContext(tx fdb.Transaction) *FDBRecordContext {
	return &FDBRecordContext{
		tx:  tx,
		ctx: context.Background(),
	}
}

// Transaction returns the underlying FDB transaction
func (rc *FDBRecordContext) Transaction() fdb.Transaction {
	return rc.tx
}

// Context returns the Go context
func (rc *FDBRecordContext) Context() context.Context {
	return rc.ctx
}

// Commit commits the transaction
func (rc *FDBRecordContext) Commit() error {
	return rc.tx.Commit().Get()
}

// Cancel cancels the transaction
func (rc *FDBRecordContext) Cancel() {
	rc.tx.Cancel()
}