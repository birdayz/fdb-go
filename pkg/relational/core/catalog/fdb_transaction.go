package catalog

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// FDBTransaction bridges a *recordlayer.FDBRecordContext onto the
// api.Transaction interface. Mirrors Java's RecordContextTransaction:
// it owns no lifecycle of its own beyond the commit/abort/close state
// machine — the underlying FDBRecordContext handles the real FDB work.
//
// The transaction is considered "closed" once Commit or Abort returns
// successfully, or Close is called. Subsequent operations return
// ErrCodeTransactionInactive.
type FDBTransaction struct {
	mu            sync.Mutex
	ctx           *recordlayer.FDBRecordContext
	boundTemplate api.SchemaTemplate
	closed        bool
}

// NewFDBTransaction wraps an FDBRecordContext. The caller retains
// ownership of ctx; Close() does NOT cancel the underlying context
// (matches Java's behaviour where RecordContextTransaction.close is a
// no-op when the runner manages the context). Callers must Commit or
// Abort before discarding the transaction.
func NewFDBTransaction(ctx *recordlayer.FDBRecordContext) *FDBTransaction {
	return &FDBTransaction{ctx: ctx}
}

// Context returns the underlying record-layer context. Catalog impls
// use this to open record stores; equivalent to Java's
// Transaction.unwrap(FDBRecordContext.class).
func (t *FDBTransaction) Context() *recordlayer.FDBRecordContext {
	return t.ctx
}

// Commit finalises the underlying FDB transaction. After a successful
// commit the transaction is marked closed; further operations error.
// A failed commit leaves the transaction in the pre-commit state so
// callers may decide whether to Abort / Close.
func (t *FDBTransaction) Commit() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return api.NewError(api.ErrCodeTransactionInactive, "transaction already closed: Commit")
	}
	t.mu.Unlock()
	if err := t.ctx.Commit(); err != nil {
		return err
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// Abort cancels the underlying FDB transaction and marks this
// transaction closed. Idempotent with Commit — the first of the two
// wins; subsequent calls return ErrCodeTransactionInactive.
func (t *FDBTransaction) Abort() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return api.NewError(api.ErrCodeTransactionInactive, "transaction already closed: Abort")
	}
	t.ctx.Cancel()
	t.closed = true
	return nil
}

// Close releases state held by this bridge. Does NOT touch the
// underlying FDBRecordContext — the caller that created it owns its
// lifecycle. Safe to call multiple times.
func (t *FDBTransaction) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// IsClosed reports whether Commit / Abort / Close has been called.
func (t *FDBTransaction) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// BoundSchemaTemplate returns the template bound to this transaction,
// or nil.
func (t *FDBTransaction) BoundSchemaTemplate() api.SchemaTemplate {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.boundTemplate
}

// SetBoundSchemaTemplate binds or replaces the template.
func (t *FDBTransaction) SetBoundSchemaTemplate(template api.SchemaTemplate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.boundTemplate = template
}

// UnsetBoundSchemaTemplate clears any bound template.
func (t *FDBTransaction) UnsetBoundSchemaTemplate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.boundTemplate = nil
}

// unwrapFDB extracts the *FDBRecordContext from an api.Transaction or
// returns an *api.Error. Catalog impls go through this helper so the
// error path (wrong impl type, closed tx) is consistent.
func unwrapFDB(txn api.Transaction) (*recordlayer.FDBRecordContext, error) {
	ft, ok := txn.(*FDBTransaction)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"FDB catalog requires *catalog.FDBTransaction, got %T", txn)
	}
	if ft.IsClosed() {
		return nil, api.NewError(api.ErrCodeTransactionInactive, "transaction is closed")
	}
	return ft.Context(), nil
}
