//go:build !cgo

// This stub keeps the package compilable when something imports it in a
// CGO_ENABLED=0 build (e.g. `go build -tags libfdbc` without cgo, or tooling that
// type-checks every package). Open then fails gracefully with a clear message
// instead of the package failing to compile. The normal selector,
// pkg/fdbgo/fdbclient, imports this package only under -tags libfdbc, which is a
// cgo build — so this stub is a safety net for a nonsensical tag/cgo combination,
// not a path the default build ever reaches.
package libfdbc

import (
	"errors"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

var errNoCgo = errors.New(
	"libfdb_c backend unavailable: this binary was built without cgo (CGO_ENABLED=0); " +
		"rebuild with cgo enabled and libfdb_c installed (or drop -tags libfdbc to use the pure-Go client)",
)

// Open reports that the libfdb_c backend is not available in a non-cgo build.
func Open(string) (fdb.BackendDatabase, error) { return nil, errNoCgo }
