// Package sqldriver implements a database/sql driver for the
// FoundationDB Record Layer relational (SQL) layer.
//
// Register the driver by blank-importing this package, then open a
// connection:
//
//	import (
//	    "database/sql"
//	    _ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
//	)
//
//	db, err := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/etc/foundationdb/fdb.cluster")
//
// DSN shape mirrors Java's JDBC URI (minus the jdbc: prefix):
//
//	fdbsql:///PATH                          — embedded, default cluster file
//	fdbsql:///PATH?cluster_file=/path       — embedded, explicit cluster file
//	fdbsql://HOST:PORT/PATH                 — remote (gRPC) — NOT YET IMPLEMENTED
//
// This is the public entry point. Internally it wraps
// pkg/relational/core which implements the SQL engine over FDB.
//
// The port follows Java's fdb-relational-* modules 1:1 wherever
// reasonable; database/sql compatibility is the single intentional
// deviation — the Go-idiomatic driver surface is at the edge, the Java
// surface (pkg/relational/api.Connection etc.) is preserved underneath.
package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// DriverName is the database/sql driver name.
const DriverName = "fdbsql"

// Driver is the database/sql/driver.Driver for fdbsql.
//
// Implements driver.Driver and driver.DriverContext.
type Driver struct{}

// Open satisfies driver.Driver. Prefer OpenConnector (via
// driver.DriverContext) for lazy connection pooling.
func (d *Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector parses the DSN and returns a lazy Connector.
// Parsing errors are reported here so misconfigured DSNs surface at
// sql.Open time, not at first query.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	dsn, err := ParseDSN(name)
	if err != nil {
		return nil, err
	}
	return &Connector{driver: d, dsn: dsn}, nil
}

// Connector holds a parsed DSN and produces connections on demand.
type Connector struct {
	driver *Driver
	dsn    *DSN
}

// Connect opens a connection. Honors ctx.Done() for cancellation /
// deadline propagation (required by database/sql).
//
// Not yet implemented: returns api.ErrCodeUnsupportedOperation. The
// driver registration, DSN parsing, and type plumbing are in place; the
// underlying embedded connection arrives in a later phase.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.dsn.Mode == ModeRemote {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"remote (gRPC) mode is not yet implemented")
	}
	return nil, api.NewError(api.ErrCodeUnsupportedOperation,
		"embedded connection is not yet implemented")
}

// Driver returns the driver that created this Connector.
func (c *Connector) Driver() driver.Driver { return c.driver }

// DSN returns the parsed DSN. Exposed for diagnostics.
func (c *Connector) DSN() *DSN { return c.dsn }

// Static interface checks.
var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
	_ driver.Connector     = (*Connector)(nil)
)

// io.EOF is re-exported so callers don't need to import "io" just to
// compare against Rows.Next's terminal error.
var ErrNoMoreRows = io.EOF

func init() {
	sql.Register(DriverName, &Driver{})
}
