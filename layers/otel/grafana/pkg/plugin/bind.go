package plugin

import (
	"context"
	"database/sql/driver"
	"errors"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
)

// errRootAPIUnavailable marks sqldriver.NewConnector, which this build lacks
// until the sqldriver stream lands; newConnector then calls it directly.
var errRootAPIUnavailable = errors.New("this build lacks sqldriver.NewConnector")

// openReader is otel.OpenReader behind the otelReader interface.
func openReader(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg otel.ReaderConfig) (otelReader, error) {
	r, err := otel.OpenReader(ctx, db, database, schema, cfg)
	if err != nil {
		return nil, err
	}
	return r, nil
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
