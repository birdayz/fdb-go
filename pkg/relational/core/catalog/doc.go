// Portions derived from FoundationDB Record Layer (RecordLayerStoreCatalog.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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
