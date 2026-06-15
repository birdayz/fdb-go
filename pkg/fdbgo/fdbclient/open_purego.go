//go:build !libfdbc

package fdbclient

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"

// Backend names the FDB client compiled into this binary.
const Backend = "pure-go"

// apiVersion is the FDB API version fdbclient selects when the app has not already
// selected one. It matches the 7.3.75 server (and the libfdb_c binding's 730).
const apiVersion = 730

// Open opens clusterFile on the from-scratch pure-Go FDB client (the default).
// Build with -tags libfdbc to open on Apple's libfdb_c instead, with no change
// to this call.
//
// Open selects the FDB API version if the app has not already (standard FDB is
// select-then-open). An app that selected its own version keeps it — we never
// override it; we only fill in the default so fdbclient.Open is a single call. The
// libfdb_c variant does the equivalent inside libfdbc.Open (it pins 730, the only
// version its binding speaks).
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	if _, err := fdb.GetAPIVersion(); err != nil {
		if err := fdb.APIVersion(apiVersion); err != nil {
			return nil, err
		}
	}
	return fdb.OpenDatabase(clusterFile)
}
