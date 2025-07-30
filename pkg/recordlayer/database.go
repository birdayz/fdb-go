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
func (fdb *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (interface{}, error)) (interface{}, error) {
	// TODO: Implement retry logic
	// For now, just execute once
	tx, err := fdb.db.CreateTransaction()
	if err != nil {
		return nil, err
	}

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
	}

	result, err := fn(recordCtx)
	if err != nil {
		tx.Cancel()
		return nil, err
	}

	err = tx.Commit().Get()
	if err != nil {
		return nil, err
	}

	return result, nil
}

// Database returns the underlying FDB database
func (fdb *FDBDatabase) Database() fdb.Database {
	return fdb.db
}

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
type FDBRecordContext struct {
	tx  fdb.Transaction
	ctx context.Context
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