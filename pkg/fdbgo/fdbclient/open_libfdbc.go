//go:build libfdbc

package fdbclient

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/libfdbc"
)

// Backend names the FDB client compiled into this binary.
const Backend = "libfdb_c"

// Open opens clusterFile on Apple's libfdb_c client (the cgo backend). This file
// is compiled only under -tags libfdbc; the default build uses the pure-Go
// client and never imports pkg/fdbgo/libfdbc or links the C library.
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	return libfdbc.Open(clusterFile)
}
