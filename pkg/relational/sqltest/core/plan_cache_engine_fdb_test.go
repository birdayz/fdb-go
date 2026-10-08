package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_PlanCacheIsEngineWide pins Java's engine-wide RelationalPlanCache:
// every connection of one sql.DB (one connector, Go's engine) plans through
// the same cache, so a query planned on one connection is a plan-cache hit
// on another.
func TestFDB_PlanCacheIsEngineWide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/testdb_plan_cache_engine", "plan_cache_engine",
		"CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE INDEX t_v ON T (v)")

	var shared [2]*embedded.RelationalPlanCache
	for i := range shared {
		conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { shared[i] = ec.SharedPlanCache() })
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

// TestFDB_PlanCacheKeysTemporaryFunctions pins Java's transaction-bound key
// component (QueryCacheKey's auxiliary metadata): a query over a temporary
// function is cached under the function's definition, so the same text over
// the same definition hits in a later transaction, a different definition
// does not, and creating the function does not empty the cache.
func TestFDB_PlanCacheKeysTemporaryFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/testdb_plan_cache_tempfn", "plan_cache_tempfn",
		"CREATE TABLE T1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (10, 1), (30, 2), (50, 3)"); err != nil {
		t.Fatal(err)
	}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { cache = ec.SharedPlanCache() })
	run := func(bound int) []int64 {
		t.Helper()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"CREATE TEMPORARY FUNCTION f1() ON COMMIT DROP FUNCTION AS SELECT * FROM T1 WHERE id < %d", bound)); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM f1() WHERE id > 15")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	if got := run(50); len(got) != 1 || got[0] != 30 {
		t.Fatalf("f1 = id < 50: %v, want [30]", got)
	}
	before := cache.Counts().TertiaryHit
	if got := run(50); len(got) != 1 || got[0] != 30 {
		t.Fatalf("again: %v, want [30]", got)
	}
	if hits := cache.Counts().TertiaryHit; hits != before+1 {
		t.Errorf("the same definition did not hit the cache (%d -> %d)", before, hits)
	}
	if got := run(30); len(got) != 0 {
		t.Errorf("f1 = id < 30 served the id < 50 plan: %v, want []", got)
	}
}

// TestFDB_StoredQueriesWarmThePlanCache pins Java's OfflineStoredQueriesProcessor:
// a connector's start plans every template's stored queries into its plan
// cache, so a stored query's first execution on any schema of the template is
// a cache hit. A stored query with a DECLAREd function warms the plan of a
// transaction that declares that function as the template stores it.
func TestFDB_StoredQueriesWarmThePlanCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/testdb_stored_query_warm", "stored_query_warm",
		"CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE INDEX t_v ON T (v) "+
			"CREATE STORED QUERY q1 AS SELECT id FROM T WHERE v = 7 "+
			"CREATE STORED QUERY q2 DECLARE FUNCTION f(IN x BIGINT) AS (SELECT id FROM T WHERE id = x) AS SELECT * FROM f(1)")
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { cache = ec.SharedPlanCache() })
	if _, err := conn.ExecContext(ctx, "INSERT INTO T VALUES (1, 7), (2, 8)"); err != nil {
		t.Fatal(err)
	}
	hits := cache.Counts().TertiaryHit
	var id int64
	if err := conn.QueryRowContext(ctx, "SELECT id FROM T WHERE v = 7").Scan(&id); err != nil || id != 1 {
		t.Fatalf("q1 = %d, %v; want 1", id, err)
	}
	if got := cache.Counts().TertiaryHit; got != hits+1 {
		t.Fatalf("q1's first execution hit the cache %d times, want 1: the warm-up did not plan it", got-hits)
	}
	// Control: a query no template stores misses on its first execution.
	hits = cache.Counts().TertiaryHit
	if err := conn.QueryRowContext(ctx, "SELECT id FROM T WHERE v = 8").Scan(&id); err != nil || id != 2 {
		t.Fatalf("control = %d, %v; want 2", id, err)
	}
	if got := cache.Counts().TertiaryHit; got != hits {
		t.Fatalf("an unstored query hit the cache on its first execution")
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"CREATE TEMPORARY FUNCTION f(IN x BIGINT) ON COMMIT DROP FUNCTION AS SELECT id FROM T WHERE id = x"); err != nil {
		t.Fatal(err)
	}
	hits = cache.Counts().TertiaryHit
	if err := tx.QueryRowContext(ctx, "SELECT * FROM f(1)").Scan(&id); err != nil || id != 1 {
		t.Fatalf("q2 = %d, %v; want 1", id, err)
	}
	if got := cache.Counts().TertiaryHit; got != hits+1 {
		t.Fatalf("q2's first execution hit the cache %d times, want 1", got-hits)
	}
}
