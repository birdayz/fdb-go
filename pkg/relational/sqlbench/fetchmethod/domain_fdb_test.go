package fetchmethod

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/testutil/benchcluster"
)

// TestFDB_BenchmarkDatabasePathIsValid creates and drops an /FRL database
// through the driver, as every benchmark here does first. It fails with
// INVALID_PATH unless TestMain registered the FRL domain.
func TestFDB_BenchmarkDatabasePathIsValid(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clusterFile := benchcluster.ClusterFile(t)
	dbPath := fmt.Sprintf("/FRL/BENCH_DOMAIN_CHECK_%d", time.Now().UnixNano())
	catalog, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", clusterFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFile))
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
