//go:build !cgo

// This stub makes the package compile in a CGO_ENABLED=0 build: importing it then
// links cleanly and OpenDatabaseWithBackend(BackendLibFDBC, …) fails gracefully
// with a clear message, instead of referencing a type that does not exist without
// cgo (Torvalds: the default build must compile and fail gracefully).
package libfdbc

import (
	"errors"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

var errNoCgo = errors.New(
	"libfdb_c backend unavailable: this binary was built without cgo (CGO_ENABLED=0); " +
		"rebuild with cgo enabled and libfdb_c installed to use BackendLibFDBC",
)

// Open reports that the libfdb_c backend is not available in a non-cgo build.
func Open(string) (fdb.BackendDatabase, error) { return nil, errNoCgo }
