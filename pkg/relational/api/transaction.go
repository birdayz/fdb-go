//go:generate go run go.uber.org/mock/mockgen -source=$GOFILE -destination=mocks_$GOFILE -package=api

package api

// Transaction is the SQL-layer's handle on a single unit of work
// against the catalog and/or record store.
//
// Mirrors Java's com.apple.foundationdb.relational.api.Transaction.
// Java extends AutoCloseable; in Go, Close is the idiomatic
// equivalent — call it via defer.
//
// Unwrap is the Go analogue of Java's `<T> T unwrap(Class<T> type)`:
// it returns the concrete backend handle (e.g. *recordlayer.FDBRecordContext
// for FDB-backed transactions, the transaction itself for the in-memory
// impl) so catalog code can reach the storage engine without a concrete
// type assertion on the Transaction interface. Wrapping decorators must
// forward Unwrap() to their inner transaction so the chain can still be
// pierced.
type Transaction interface {
	// Commit finalises the transaction. Returns an error if the
	// commit fails (storage-engine conflict, deadline exceeded, etc.)
	// or if the transaction has already been closed.
	Commit() error
	// Abort cancels the transaction and discards any pending writes.
	Abort() error
	// BoundSchemaTemplate returns the schema template bound to this
	// transaction, or nil if none is bound. Matches Java's
	// Optional<SchemaTemplate> getBoundSchemaTemplateMaybe() —
	// "maybe" is modelled as a nil return in Go.
	BoundSchemaTemplate() SchemaTemplate
	// SetBoundSchemaTemplate binds (or replaces) the schema template
	// for the duration of this transaction. Any subsequent
	// metadata-requiring operation uses the bound template.
	SetBoundSchemaTemplate(template SchemaTemplate)
	// UnsetBoundSchemaTemplate clears any previously bound template.
	UnsetBoundSchemaTemplate()
	// Close releases the underlying resources. Safe to call on an
	// already-committed or already-aborted transaction — second call
	// is a no-op that returns nil. Matches Java's AutoCloseable.close.
	Close() error
	// IsClosed reports whether Close has been called (directly or
	// via a successful Commit / Abort).
	IsClosed() bool
	// Unwrap returns the underlying backend-specific handle. Callers
	// perform a type assertion on the result. A future remote/gRPC
	// transaction impl that wraps a local one must forward Unwrap()
	// to preserve the chain.
	Unwrap() any
}
