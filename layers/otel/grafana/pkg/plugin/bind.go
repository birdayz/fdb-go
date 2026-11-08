package plugin

import (
	"context"
	"database/sql/driver"
	"errors"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
)

// errRootAPIUnavailable marks the root-module entry points this branch is
// built without: otel.OpenReader and sqldriver.NewConnector land with the
// reader and sqldriver streams, and bind.go then calls them directly.
var errRootAPIUnavailable = errors.New("this build lacks otel.OpenReader / sqldriver.NewConnector")

// openReader is otel.OpenReader behind the otelReader interface.
func openReader(_ context.Context, _ *recordlayer.FDBDatabase, _, _ string, _ readerConfig) (otelReader, error) {
	return nil, errRootAPIUnavailable
}

// newConnector is sqldriver.NewConnector.
func newConnector(_ *recordlayer.FDBDatabase, _ string) (driver.Connector, error) {
	return nil, errRootAPIUnavailable
}

// isOwnershipError reports the reader's error for a schema that is not an
// OTEL_V1 store (no STORE_META layout).
func isOwnershipError(err error) bool {
	var o *otel.OwnershipError
	return errors.As(err, &o)
}
