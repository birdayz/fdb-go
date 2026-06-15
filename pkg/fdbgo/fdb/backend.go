package fdb

// BackendDatabase is the minimal database surface the record layer needs to
// drive its Run / RunRead path on any backend: the Transactor interface (which
// already takes the WritableTransaction / ReadTransaction interfaces, not the
// concrete pure-Go types) plus Close. The pure-Go Database satisfies it; the
// libfdb_c backend (pkg/fdbgo/libfdbc) provides an alternative implementation
// over cgofdb.
//
// Which implementation a binary carries is a BUILD-time choice (a build tag),
// not a runtime one — see pkg/fdbgo/fdbclient. That mirrors the physical
// reality: libfdb_c's network thread is initialized once per process and is
// unrecoverable, so there is no live switch between backends anyway.
//
// It deliberately does NOT include CreateTransaction / Locality / tenant ops:
// those return concrete pure-Go handles a cgo backend cannot build and are not
// on the Transactor-driven gold path, so they remain pure-Go-only in v1 (the
// libfdb_c escape hatch covers the non-tenant record-store Run/RunRead path —
// the same scope boundary the RFC already draws around tenants).
type BackendDatabase interface {
	Transactor
	Close()
}

// IsValid reports whether the Database is a live handle (non-zero). A zero
// Database{} is what an FDBDatabase constructed on a non-pure-Go backend carries
// in its concrete-db slot; the record layer checks this before using the
// pure-Go-only CreateTransaction / Locality paths.
func (db Database) IsValid() bool { return db.d != nil }
