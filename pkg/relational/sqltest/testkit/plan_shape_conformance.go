package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// planExplainVia retrieves the Cascades physical plan Explain string
// via the underlying EmbeddedConnection for the given query.
func ExplainVia(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()
	var plan string
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
		}
		p, err := ec.PlanExplain(ctx, query)
		if err != nil {
			return err
		}
		plan = p
		return nil
	}); err != nil {
		t.Fatalf("PlanExplain(%q): %v", query, err)
	}
	return plan
}

// setupPlanShapeDB creates a fresh database + schema for a plan-shape subtest.
// Returns a *sql.DB connected to the created schema.
func SetupPlanShapeDB(t *testing.T, suffix, templateDDL string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// A subtest's name has a '/', and a database path is exactly /DOMAIN/DB.
	dbPath := fmt.Sprintf("/FRL/planshape_%s_%s", suffix, strings.ReplaceAll(t.Name(), "/", "_"))
	setup := OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("ps_%s_%s", suffix, t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, templateDDL)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
