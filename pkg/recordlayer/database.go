package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// FDBDatabase provides access to the underlying FoundationDB database or tenant
// and manages transaction execution with retry logic.
// This is the Record Layer equivalent of Java's FDBDatabase.
//
// The transactor field can be either an fdb.Database or fdb.Tenant, both of which
// implement the fdb.Transactor interface. This allows transparent support for both
// regular database operations and tenant-isolated operations.
type FDBDatabase struct {
	transactor fdb.Transactor
	// Keep original db/tenant for CreateTransaction which isn't on Transactor interface
	db     fdb.Database
	tenant fdb.Tenant
}

// NewFDBDatabase creates a new FDBDatabase wrapping the core FDB database
func NewFDBDatabase(db fdb.Database) *FDBDatabase {
	return &FDBDatabase{
		transactor: db,
		db:         db,
	}
}

// NewFDBDatabaseFromTenant creates a new FDBDatabase wrapping an FDB tenant
// for tenant-isolated operations. All operations will be scoped to the tenant's keyspace.
func NewFDBDatabaseFromTenant(tenant fdb.Tenant) *FDBDatabase {
	return &FDBDatabase{
		transactor: tenant,
		tenant:     tenant,
	}
}

// Run executes a function within a transaction with automatic retry handling.
// This matches the Java Record Layer pattern.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (interface{}, error)) (interface{}, error) {
	// Use FDB's built-in transactional function with retry logic
	// Works for both Database and Tenant since both implement Transactor
	return d.transactor.Transact(func(tx fdb.Transaction) (interface{}, error) {
		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}

		return fn(recordCtx)
	})
}

// CreateTransaction creates a new transaction without retry logic.
// This is primarily used for testing scenarios where manual transaction control is needed,
// such as testing isolation levels with concurrent transactions.
// For tenant-isolated databases, the transaction will be scoped to the tenant's keyspace.
func (d *FDBDatabase) CreateTransaction() (fdb.Transaction, error) {
	if d.tenant != (fdb.Tenant{}) {
		return d.tenant.CreateTransaction()
	}
	return d.db.CreateTransaction()
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