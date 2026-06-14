package fdb

import "fmt"

// Backend selects which FDB client implementation a Database runs on. It is a
// process-launch-time decision (RFC-109): the libfdb_c network thread is
// initialized once per process and is unrecoverable, so there is NO runtime
// switch between backends within a live process. Read the selection once at
// database construction.
type Backend int

const (
	// BackendGo is the default from-scratch pure-Go FDB client (this package).
	BackendGo Backend = iota
	// BackendLibFDBC is the Apple libfdb_c CGo backend — the decade-hardened
	// reference client, available as a config-selectable escape hatch when an
	// operator does not yet trust the pure-Go client under their workload.
	// Requires a cgo build with libfdb_c installed AND a blank import of
	// pkg/fdbgo/libfdbc to register the opener (see RegisterLibFDBCBackend).
	BackendLibFDBC
)

// BackendDatabase is the minimal database surface the record layer needs to
// drive its Run / RunRead path on any backend: the Transactor interface (which
// already takes the WritableTransaction / ReadTransaction interfaces, not the
// concrete pure-Go types) plus Close. The pure-Go Database satisfies it; the
// libfdb_c backend (pkg/fdbgo/libfdbc) provides an alternative implementation
// over cgofdb.
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

// libfdbcOpener is set by pkg/fdbgo/libfdbc's init() (when that package is
// imported and the binary is built with cgo). nil means the backend is not
// linked in — OpenDatabaseWithBackend(BackendLibFDBC, …) then returns a clear
// error rather than referencing a type that may not exist in this build.
var libfdbcOpener func(clusterFile string) (BackendDatabase, error)

// RegisterLibFDBCBackend wires the libfdb_c opener. It is called by
// pkg/fdbgo/libfdbc's init(); applications enable the backend by importing that
// package (a blank import suffices). Calling it directly is not expected.
func RegisterLibFDBCBackend(open func(clusterFile string) (BackendDatabase, error)) {
	libfdbcOpener = open
}

// OpenDatabaseWithBackend opens a database on the selected backend. BackendGo is
// the default pure-Go client (identical to OpenDatabase). BackendLibFDBC routes
// to the registered libfdb_c opener; it returns a clear error if that backend
// is not linked in (built without cgo, or pkg/fdbgo/libfdbc not imported).
//
// opts (pure-Go client Options such as WithTLSConfig / WithDialFunc) apply only
// to BackendGo. libfdb_c configures TLS and dialing natively via the cluster
// file and FDB_TLS_* / FDB_NETWORK_OPTION_* environment, so passing pure-Go
// opts with BackendLibFDBC is rejected rather than silently ignored.
func OpenDatabaseWithBackend(backend Backend, clusterFile string, opts ...Option) (BackendDatabase, error) {
	switch backend {
	case BackendGo:
		db, err := OpenDatabase(clusterFile, opts...)
		if err != nil {
			return nil, err
		}
		return db, nil
	case BackendLibFDBC:
		if len(opts) > 0 {
			return nil, fmt.Errorf("fdb: libfdb_c backend does not accept pure-Go client Options; configure TLS/dialer via the cluster file or FDB_TLS_* env")
		}
		if libfdbcOpener == nil {
			return nil, fmt.Errorf(`fdb: libfdb_c backend not registered: build with cgo and add a blank import _ "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/libfdbc"`)
		}
		return libfdbcOpener(clusterFile)
	default:
		return nil, fmt.Errorf("fdb: unknown backend %d", backend)
	}
}

// IsValid reports whether the Database is a live handle (non-zero). A zero
// Database{} is what an FDBDatabase constructed on a non-pure-Go backend carries
// in its concrete-db slot; the record layer checks this before using the
// pure-Go-only CreateTransaction / Locality paths.
func (db Database) IsValid() bool { return db.d != nil }
