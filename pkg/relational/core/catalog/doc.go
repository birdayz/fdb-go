// Package catalog contains concrete implementations of the
// api.StoreCatalog / api.SchemaTemplateCatalog / api.Transaction
// interfaces.
//
// The in-memory implementation (InMemoryStoreCatalog + friends) is
// intended for unit tests and development — it keeps the whole
// catalog in a mutex-protected map. It does NOT implement real
// multi-writer transactional isolation; concurrent SaveSchema /
// Commit across two live transactions may interleave in ways a
// real FDB-backed impl wouldn't.
//
// The Java-compatible FDB-backed catalog (RecordLayerStoreCatalog)
// is deferred to a later shift.
package catalog
