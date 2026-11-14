//go:build !cgo || !libfdbc_backend

package main

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb"
)

// compiledFlavor is control-pure: no native client is compiled or linked in.
var compiledFlavor = flavor{tags: []string{}}

// openNativeBackend fails: a build without the native client has no go_native arm.
func openNativeBackend(context.Context, string, string) (fdb.BackendDatabase, string, error) {
	return nil, "", fault("client", "this executable was built without the native libfdb_c client")
}
