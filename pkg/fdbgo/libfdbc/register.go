package libfdbc

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"

// Registering on import (blank import is enough) lets fdb.OpenDatabaseWithBackend
// reach the libfdb_c opener without package fdb importing this cgo package — so
// the pure-Go client never links libfdb_c unless an app explicitly opts in. Open
// is defined in backend.go (cgo) or backend_stub.go (!cgo); the registration is
// the same either way (the stub's Open returns a clear "built without cgo" error).
func init() {
	fdb.RegisterLibFDBCBackend(Open)
}
