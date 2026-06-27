package fdb

// BackendDatabase is the minimal database surface the record layer needs to
// drive its Run / RunRead path on any backend: the Transactor interface (which
// already takes the WritableTransaction / ReadTransaction interfaces, not the
// concrete pure-Go types) plus Close. The pure-Go Database satisfies it; the
// libfdb_c backend (pkg/fdbgo/libfdbc) provides an alternative implementation
// over cgofdb.
//
// Which implementation a binary carries is a BUILD-time choice (a build tag),
// not a runtime one — see pkg/internal/fdbclient. That mirrors the physical
// reality: libfdb_c's network thread is initialized once per process and is
// unrecoverable, so there is no live switch between backends anyway.
//
// It also exposes CreateWritableTransaction, which returns a standalone
// (non-retry) transaction as the WritableTransaction INTERFACE — the
// backend-agnostic analog of the pure-Go Database.CreateTransaction (which
// returns the concrete pure-Go type). The record layer needs a long-lived
// transaction handle for database/sql explicit transactions (BeginTx / COMMIT),
// which span multiple driver calls and so cannot use the closure-based
// Transactor gold path. libfdb_c can create transactions just as well as the
// pure-Go client (it does so internally for its own Transact loop) — exposing it
// here is what makes BeginTx work on either backend, rather than the SQL engine
// being silently pure-Go-only. (CGo-allocated transactions are GC-finalized by
// Apple's binding, exactly like the pure-Go ones, so the caller manages no extra
// lifecycle.)
//
// It also exposes LocalityGetBoundaryKeys (FDB shard boundaries), which the
// online MUTUAL indexer uses to partition the keyspace into fragments for
// concurrent building. Like a transaction creation, libfdb_c can do this — it's
// a read of the \xff/keyServers system range, identical bytes on either client —
// so exposing it here makes mutual indexing parallel on libfdb_c instead of
// degrading to a single fragment.
//
// It deliberately still does NOT include tenant ops: those return concrete
// pure-Go handles a cgo backend cannot build and remain pure-Go-only in v1.
type BackendDatabase interface {
	Transactor
	// CreateWritableTransaction creates a standalone, non-retry transaction. The
	// caller owns its lifecycle (commit / cancel); the underlying handle is
	// GC-finalized on both backends.
	CreateWritableTransaction() (WritableTransaction, error)
	// LocalityGetBoundaryKeys returns the shard boundary keys within r (a read of
	// the \xff/keyServers system range). readVersion 0 = use the latest.
	LocalityGetBoundaryKeys(r ExactRange, limit int, readVersion int64) ([]Key, error)
	Close()
}

// IsValid reports whether the Database is a live handle (non-zero). A zero
// Database{} is what an FDBDatabase constructed on a non-pure-Go backend carries
// in its concrete-db slot; the record layer checks this before using the
// pure-Go-only CreateTransaction / Locality paths.
func (db Database) IsValid() bool { return db.d != nil }
