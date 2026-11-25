//go:build libfdbc

package fdbclient

import (
	"fdb.dev/internal/fdbclient/libfdbc"
	"fdb.dev/pkg/fdbgo/fdb"
)

// Backend names the FDB client compiled into this binary.
const Backend = "libfdb_c"

// Open opens clusterFile on Apple's libfdb_c client (the cgo backend). This file
// is compiled only under -tags libfdbc; the default build uses the pure-Go
// client and never imports internal/fdbclient/libfdbc or links the C library.
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	return libfdbc.Open(clusterFile)
}

// SetKnob sets a client knob, "name=value", on libfdb_c.
func SetKnob(knob string) error {
	return libfdbc.SetKnob(knob)
}
