package yamsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// The binding-shape table otherwise executes only in test-full; running it in
// the fast lane keeps a shape whose premise fails at execution from hiding.
func TestPlanCacheBindingShapesExecute(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	// Not the runner's "_conf_<scenario>" names: plan_cache_binding_shapes.yaml runs in parallel.
	name := sanitize(t.Name())
	dbPath, schema, tmpl := strings.ToUpper("/FRL/_"+name), "CONF", "TMPL_"+strings.ToUpper(name)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE " + tmpl + " " + testkit.PlanCacheBindingShapesSchema,
		"CREATE SCHEMA " + dbPath + "/" + schema + " WITH TEMPLATE " + tmpl,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbPath)
		_, _ = db.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+tmpl)
	})
	testkit.RunPlanCacheBindingShapes(t, db)
}
