package testkit

// RFC-106a: per-statement resource governance — FDB integration tests.
//
// Each piece is OFF by default; these tests prove (a) that a configured
// limit surfaces as SQLSTATE 54F01 (api.ErrCodeExecutionLimitReached) and
// (b) — critically — that the no-option/default path is INERT
// (default-safety). The configs are installed on a pinned *sql.Conn's
// underlying *embedded.EmbeddedConnection via Raw, mirroring the
// installLogger pattern in plan_logging_fdb_test.go.

import (
	"context"
	"database/sql"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// pinEmbeddedConn pins a single *sql.Conn and hands back its underlying
// *embedded.EmbeddedConnection so a test can install the RFC-106a
// connection-local config (options / fail-on-scan / statement timeout /
// result-byte cap). The returned *sql.Conn MUST be used for every
// subsequent statement so the configured connection is the one that
// executes them.
func PinEmbeddedConn(t *testing.T, db *sql.DB, configure func(*embedded.EmbeddedConnection)) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("driver conn is %T, want *embedded.EmbeddedConnection", driverConn)
		}
		configure(ec)
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	return conn
}
