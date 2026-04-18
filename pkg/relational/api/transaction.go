package api

// Transaction is the SQL-layer's handle on a single unit of work
// against the catalog and/or record store.
//
// Mirrors Java's com.apple.foundationdb.relational.api.Transaction.
// Java extends AutoCloseable; in Go, Close is the idiomatic
// equivalent — call it via defer. Java's generic unwrap<T> has no
// Go interface-level counterpart: callers use a type assertion at
// the call site instead of pushing the cast into the interface.
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
}
