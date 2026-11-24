package fetchmethod

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/testutil/benchcluster"
)

// TestFDB_BenchmarkDatabasePathIsValid creates and drops an /FRL database
// through an instance-scoped Engine, as every benchmark here does first.
func TestFDB_BenchmarkDatabasePathIsValid(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clusterFile := benchcluster.ClusterFile(t)
	eng, err := sqldriver.NewEngine(sqldriver.EngineConfig{
		ClusterFile: clusterFile,
		Domains:     []string{"FRL"},
		Limits:      sqldriver.UnboundedJavaCompatLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	dbPath := fmt.Sprintf("/FRL/BENCH_DOMAIN_CHECK_%d", time.Now().UnixNano())
	catalog, err := eng.OpenDB("/__SYS", sqldriver.WithSchema("CATALOG"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	setup, err := eng.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close() })
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", dbPath, err)
	}
	if _, err := catalog.ExecContext(ctx, "DROP DATABASE "+dbPath); err != nil {
		t.Fatalf("DROP DATABASE %s: %v", dbPath, err)
	}
}
