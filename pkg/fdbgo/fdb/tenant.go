package fdb

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// Tenant is a handle to a FoundationDB tenant.
// Operations on a Tenant are scoped to the tenant's key space.
type Tenant struct {
	db       Database
	tenantId int64
}

// ID returns the numeric tenant ID.
func (t Tenant) ID() int64 { return t.tenantId }

// Transact runs a transactional function with automatic retry, scoped to
// this tenant's key space. Matches Database.Transact but sets the tenant ID
// on the underlying transaction.
func (t Tenant) Transact(f func(WritableTransaction) (any, error)) (any, error) {
	return t.TransactCtx(t.db.d.ctx, f)
}

// TransactCtx is Transact bounded by ctx (RFC-090 / fdb.CtxTransactor). The dispatched
// commit + commit_unknown barrier run detached (in client.Database.Transact); ctx never
// cancels an in-flight commit.
func (t Tenant) TransactCtx(ctx context.Context, f func(WritableTransaction) (any, error)) (any, error) {
	var lastTx *transaction
	result, err := t.db.d.inner.Transact(ctx, func(tx *client.Transaction) (r any, e error) {
		defer func() { e = unconvertError(e) }()
		defer panicToError(&e)
		tx.SetTenantId(t.tenantId)
		txn := &transaction{
			inner:      tx,
			db:         t.db,
			ctx:        ctx,
			commitDone: make(chan struct{}),
		}
		t.db.applyTxDefaults(txn) // inherit DB-level option defaults — parity with Database.TransactCtx
		lastTx = txn
		return f(Transaction{t: txn})
	})
	if lastTx != nil && lastTx.commitDone != nil {
		select {
		case <-lastTx.commitDone:
		default:
			if err != nil {
				lastTx.commitErr = convertError(err)
			}
			close(lastTx.commitDone)
		}
	}
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// ReadTransact runs a read-only transactional function with automatic retry,
// scoped to this tenant's key space.
func (t Tenant) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return t.ReadTransactCtx(t.db.d.ctx, f)
}

// ReadTransactCtx is ReadTransact bounded by ctx (RFC-090 / fdb.CtxReadTransactor).
func (t Tenant) ReadTransactCtx(ctx context.Context, f func(ReadTransaction) (any, error)) (any, error) {
	result, err := t.db.d.inner.ReadTransact(ctx, func(tx *client.Transaction) (r any, e error) {
		defer func() { e = unconvertError(e) }()
		defer panicToError(&e)
		tx.SetTenantId(t.tenantId)
		txn := &transaction{
			inner: tx,
			db:    t.db,
			ctx:   ctx,
		}
		t.db.applyTxDefaults(txn) // inherit DB-level option defaults — parity with Database.ReadTransactCtx
		return f(Transaction{t: txn})
	})
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// CreateTransaction creates a new Transaction scoped to this tenant's key space.
// Like Database.CreateTransaction, it inherits database-level option defaults set
// via Options() (SetTransactionTimeout / SetTransactionRetryLimit / …) — matching
// libfdb_c, where DB transaction defaults are copied into every transaction.
func (t Tenant) CreateTransaction() (Transaction, error) {
	tx := t.db.d.inner.CreateTransaction()
	tx.SetTenantId(t.tenantId)
	txn := &transaction{
		inner:      tx,
		db:         t.db,
		ctx:        t.db.d.ctx,
		commitDone: make(chan struct{}),
	}
	t.db.applyTxDefaults(txn) // parity with Database.CreateTransaction + the Transact* paths
	return Transaction{t: txn}, nil
}
