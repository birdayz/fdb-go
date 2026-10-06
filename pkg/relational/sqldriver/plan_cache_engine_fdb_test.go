package sqldriver_test

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_PlanCacheIsEngineWide pins Java's engine-wide RelationalPlanCache:
// every connection of one sql.DB (one connector, Go's engine) plans through
// the same cache, so a query planned on one connection is a plan-cache hit
// on another.
func TestFDB_PlanCacheIsEngineWide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := setupErrorTestDB(t, "/testdb_plan_cache_engine", "plan_cache_engine",
		"CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE INDEX t_v ON T (v)")

	var shared [2]*embedded.RelationalPlanCache
	for i := range shared {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { shared[i] = ec.SharedPlanCache() })
		rows, err := conn.QueryContext(ctx, "SELECT id FROM T WHERE v = 7")
		if err != nil {
			t.Fatalf("query on connection %d: %v", i, err)
		}
		rows.Close()
		if i == 0 {
			if got := shared[0].Counts().TertiaryHit; got != 0 {
				t.Fatalf("first planning hit the cache (%d hits)", got)
			}
		}
	}
	if shared[0] == nil || shared[0] != shared[1] {
		t.Fatalf("connections plan through different caches: %p, %p", shared[0], shared[1])
	}
	if got := shared[0].Counts().TertiaryHit; got != 1 {
		t.Errorf("the second connection's query hit the cache %d times, want 1", got)
	}
}
