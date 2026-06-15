//go:build !libfdbc

package fdbclient

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"

// Backend names the FDB client compiled into this binary.
const Backend = "pure-go"

// Open opens clusterFile on the from-scratch pure-Go FDB client (the default).
// Build with -tags libfdbc to open on Apple's libfdb_c instead, with no change
// to this call.
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	return fdb.OpenDatabase(clusterFile)
}
