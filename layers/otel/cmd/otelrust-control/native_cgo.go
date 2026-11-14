//go:build cgo && libfdbc

package main

import (
	"context"
	"os"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

var native = &nativeOpener{nativeSteps: nativeSteps{
	environ:    os.Environ,
	maps:       func() ([]byte, error) { return os.ReadFile("/proc/self/maps") },
	mountinfo:  func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") },
	apiVersion: cgofdb.APIVersion,
	disableMVC: func() error { return cgofdb.Options().SetDisableMultiVersionClientApi() },
	open:       libfdbc.Open,
	pin:        clientSHA256,
}}

// openNativeBackend opens the pinned libfdb_c 7.3.77 and returns the rule that proved
// its mapping; only a go_native ingest calls it.
func openNativeBackend(ctx context.Context, library, clusterFile string) (fdb.BackendDatabase, string, error) {
	return native.openBackend(ctx, library, clusterFile)
}
