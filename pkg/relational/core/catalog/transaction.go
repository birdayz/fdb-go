package catalog

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// InMemoryTransaction is a minimal api.Transaction for tests.
// Concurrent calls to any method after Close / Commit / Abort
// return ErrCodeTransactionInactive. State is protected by a mutex;
// there is no MVCC, no write buffer — changes against the catalog
// are applied directly and a later Abort does NOT roll them back.
//
// Good enough for unit tests; a real FDB transaction is required
// before running SQL over live data.
type InMemoryTransaction struct {
	mu            sync.Mutex
	closed        bool
	boundTemplate api.SchemaTemplate
}

// NewInMemoryTransaction returns a fresh, open transaction.
func NewInMemoryTransaction() *InMemoryTransaction {
	return &InMemoryTransaction{}
}

// Commit marks the transaction closed. The in-memory catalog applies
// writes eagerly, so Commit has no remaining work to do; it is the
// "closed" transition that matters to callers.
func (t *InMemoryTransaction) Commit() error {
	return t.closeLocked("Commit")
}

// Abort marks the transaction closed WITHOUT rolling back prior
// writes — see package doc. Idempotent with Commit in the sense
// that the first of the two wins and subsequent calls error.
func (t *InMemoryTransaction) Abort() error {
	return t.closeLocked("Abort")
}

// Close is safe to call multiple times; only the first call
// transitions the state.
func (t *InMemoryTransaction) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// IsClosed reports whether the transaction has been
// Committed / Aborted / Closed.
func (t *InMemoryTransaction) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// BoundSchemaTemplate returns the bound template, or nil.
func (t *InMemoryTransaction) BoundSchemaTemplate() api.SchemaTemplate {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.boundTemplate
}

// SetBoundSchemaTemplate binds (or replaces) the template.
func (t *InMemoryTransaction) SetBoundSchemaTemplate(template api.SchemaTemplate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.boundTemplate = template
}

// UnsetBoundSchemaTemplate clears any bound template.
func (t *InMemoryTransaction) UnsetBoundSchemaTemplate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.boundTemplate = nil
}

// Unwrap returns the transaction itself because the in-memory catalog is
// the underlying storage backend. Satisfies api.Transaction.Unwrap.
func (t *InMemoryTransaction) Unwrap() any { return t }

// closeLocked transitions closed=true; returns an error if the
// transaction was already closed.
func (t *InMemoryTransaction) closeLocked(op string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return api.NewErrorf(api.ErrCodeTransactionInactive, "transaction already closed: %s", op)
	}
	t.closed = true
	return nil
}

// checkOpen returns an error if the transaction has been closed.
// Catalog impls use this before touching state.
func (t *InMemoryTransaction) checkOpen() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return api.NewError(api.ErrCodeTransactionInactive, "transaction is closed")
	}
	return nil
}
