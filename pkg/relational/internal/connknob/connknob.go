// Package connknob sets embedded-connection values that are not part of the
// connection's API. Only this module's tests use it; package embedded
// installs the functions when it is initialised.
package connknob

import (
	"database/sql/driver"
	"time"
)

// SetStatementReadMaxAge sets how long after its first read an auto-commit
// statement's shared transaction may still take the statement's first page
// (embedded/statement_read.go, statementReadMaxAge by default). conn must be
// an *embedded.EmbeddedConnection, or it reports false; maxAge must be
// positive. The value lasts for the connection's life, across ResetSession.
var SetStatementReadMaxAge func(conn driver.Conn, maxAge time.Duration) bool
