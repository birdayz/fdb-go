package plugin

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/url"
	"strings"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
)

// openReader is otel.OpenReader behind the otelReader interface.
func openReader(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg otel.ReaderConfig) (otelReader, error) {
	r, err := otel.OpenReader(ctx, db, database, schema, cfg)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// newConnector is sqldriver.NewConnector over a /DOMAIN/DATABASE path, whose
// domain the keyspace must know before a path under it resolves.
func newConnector(db *recordlayer.FDBDatabase, dsn string) (driver.Connector, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	if domain, _, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/"); ok && domain != "" {
		sqldriver.RegisterDomainIfNotExists(domain)
	}
	return sqldriver.NewConnector(db, dsn)
}

// isOwnershipError reports the reader's error for a schema that is not an
// OTEL_V1 store (no STORE_META layout).
func isOwnershipError(err error) bool {
	var o *otel.OwnershipError
	return errors.As(err, &o)
}
