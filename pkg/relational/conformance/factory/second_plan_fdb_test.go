package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/factory"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

	_ "fdb.dev/pkg/relational/sqldriver"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "FATAL: FDB container startup failed in CI: %v\n", err)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster file: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-factory-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "write cluster file: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// TestFDB_SecondPlanIndexFreePreconditionStaysRetired pins why the second-plan
// oracle does not demand an index-free MatchLeafRule-disabled plan.
//
// MatchLeafRule is the sole seed of PartialMatch objects, so disabling it
// starves the match/data-access pipeline, but that pipeline is not the only
// builder of index scans: OrderedIndexScanRule reads the match candidates
// directly and still plans a full-range ordered index scan, so the precondition
// would report a correct engine as a broken planner option.
//
// The correlated EXISTS probe is no longer such a counterexample. The EXISTS
// body keeps its WHERE below FirstOrDefault, so its `[=]` probe is the match
// pipeline's and goes away with MatchLeafRule, while the two plans still
// differ — which is all the oracle requires.
//
// If the ordered scan stops surviving, the precondition is worth reconsidering
// — that is a real finding, not a test to relax.
func TestFDB_SecondPlanIndexFreePreconditionStaysRetired(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The shape rowdiff renders for a correlated [NOT] EXISTS: the inner query
	// aliases the same table as `r` and correlates on a column the case
	// indexes, which is what gives the planner a probe to build.
	//
	// BOTH columns are indexed, and that is what makes the case discriminating.
	// The outer `a > 2` is matched through MatchLeafRule, so disabling the rule
	// collapses the outer leg to a full scan and the two plans genuinely
	// differ — which is the oracle's precondition. The INNER probe is built by
	// a different path entirely and does not move. With only `b` indexed the
	// outer leg is a full scan either way, the two plans come out identical,
	// and the case proves nothing.
	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)"
	const query = "SELECT id, a FROM t WHERE (a > 2) AND NOT EXISTS " +
		"(SELECT 1 FROM t AS r WHERE r.b = t.b)"

	db := openFactorySchema(t, ctx, "spcorr", ddl)

	defaultConn, err := factory.PinConn(ctx, db, nil)
	if err != nil {
		t.Fatalf("pin default conn: %v", err)
	}
	defer defaultConn.Close() //nolint:errcheck
	altConn, err := factory.PinConn(ctx, db, factory.SecondPlanRules())
	if err != nil {
		t.Fatalf("pin second-plan conn: %v", err)
	}
	defer altConn.Close() //nolint:errcheck

	// The option is live on THIS schema, proved on a query it does bite: a
	// plain indexed equality plans to an index scan by default and to a
	// filtered full scan with MatchLeafRule off. Without this control the
	// assertions below would also pass on a run where the option was silently
	// ignored and nothing was ever disabled.
	const control = "SELECT id, a FROM t WHERE a = 5"
	if base, alt := explainVia(t, ctx, defaultConn, control), explainVia(t, ctx, altConn, control); base == alt {
		t.Fatalf("DISABLED_PLANNER_RULES=%v did not change the plan for %q (%s); the option is being accepted "+
			"and ignored, and every second-plan comparison in the factory is a tautology",
			factory.SecondPlanRules(), control, base)
	}

	basePlan := explainVia(t, ctx, defaultConn, query)
	altPlan := explainVia(t, ctx, altConn, query)
	t.Logf("baseline plan: %s", basePlan)
	t.Logf("second  plan:  %s", altPlan)
	if !strings.Contains(basePlan, "FirstOrDefault(IndexScan(IDX_B, [=]))") {
		t.Fatalf("the default plan does not probe the EXISTS body by its index:\n  %s", basePlan)
	}
	if strings.Contains(altPlan, "IDX_B") || basePlan == altPlan {
		t.Fatalf("with MatchLeafRule disabled the EXISTS probe should be gone and the plan different:\n"+
			"  default = %s\n  second  = %s", basePlan, altPlan)
	}

	// The counterexample itself: an ordered index scan built without a
	// PartialMatch survives.
	const ordered = "SELECT id, a FROM t ORDER BY a"
	if orderedPlan := explainVia(t, ctx, altConn, ordered); !strings.Contains(orderedPlan, "IndexScan(IDX_A, [*])") {
		t.Fatalf("the MatchLeafRule-disabled plan for %q has no ordered index scan:\n  %s\nThe oracle's retired "+
			"index-free precondition would now hold for this shape, so the reasoning that retired it needs "+
			"re-deriving before anyone relies on it again.", ordered, orderedPlan)
	}
}

// Outer index matching gives the second-plan oracle a genuinely different
// access path for correlated EXISTS; compare non-empty answers under both.
func TestFDB_SecondPlanReachesCorrelatedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)"
	db := openFactorySchema(t, ctx, "spblind", ddl)

	defaultConn, err := factory.PinConn(ctx, db, nil)
	if err != nil {
		t.Fatalf("pin default conn: %v", err)
	}
	defer defaultConn.Close() //nolint:errcheck
	altConn, err := factory.PinConn(ctx, db, factory.SecondPlanRules())
	if err != nil {
		t.Fatalf("pin second-plan conn: %v", err)
	}
	defer altConn.Close() //nolint:errcheck

	if _, err := defaultConn.ExecContext(ctx, "INSERT INTO t VALUES (1,5,NULL),(2,5,10),(3,3,NULL),(4,1,10),(5,9,20)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  [][]any
	}{
		{"SELECT id, a FROM t WHERE a = 5 AND NOT EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b)", [][]any{{int64(1), int64(5)}}},
		{"SELECT id, a FROM t WHERE a = 5 AND EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b)", [][]any{{int64(2), int64(5)}}},
		{"SELECT id, a FROM t WHERE a > 2 AND NOT EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b AND r.a > 1)", [][]any{{int64(1), int64(5)}, {int64(3), int64(3)}}},
	} {
		base, alt := explainVia(t, ctx, defaultConn, tc.query), explainVia(t, ctx, altConn, tc.query)
		if base == alt {
			t.Fatalf("second-plan oracle cannot compare identical plans: %s", base)
		}
		baseRows, altRows := selectRows(t, ctx, defaultConn, tc.query), selectRows(t, ctx, altConn, tc.query)
		if d := factory.RowsDiffForTest(false, tc.want, baseRows); d != "" {
			t.Fatalf("baseline rows: %s", d)
		}
		if d := factory.RowsDiffForTest(false, tc.want, altRows); d != "" {
			t.Fatalf("alternate rows: %s", d)
		}
	}
}

// TestFDB_SecondPlanOracleComparesRowsUnderBothPlans pins the oracle end to
// end on a live engine: two genuinely different plans for one query must be
// executed and their rows compared, with the run counted as KEPT.
//
// The unit detectors prove the comparator can see a difference; this proves the
// comparator is reached at all. A precondition that never holds, an EXPLAIN
// that fails, or a connection that silently drops its options would each leave
// every case counted as a skip — indistinguishable in a log from an engine that
// simply never uses an index.
func TestFDB_SecondPlanOracleComparesRowsUnderBothPlans(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_a ON t (a)"
	db := openFactorySchema(t, ctx, "spord", ddl)

	defaultConn, err := factory.PinConn(ctx, db, nil)
	if err != nil {
		t.Fatalf("pin default conn: %v", err)
	}
	defer defaultConn.Close() //nolint:errcheck
	if _, err := defaultConn.ExecContext(ctx,
		"INSERT INTO t VALUES (1, 5, 10), (2, 3, 20), (3, 5, 30), (4, 1, 40), (5, 9, 50)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	altConn, err := factory.PinConn(ctx, db, factory.SecondPlanRules())
	if err != nil {
		t.Fatalf("pin second-plan conn: %v", err)
	}
	defer altConn.Close() //nolint:errcheck

	// ORDER BY a, id is a TOTAL order (the generator always suffixes the
	// primary key for exactly this reason), so the two plans must agree
	// position by position and a sequence comparison cannot false-positive.
	const query = "SELECT id, a FROM t WHERE a > 1 ORDER BY a, id"

	basePlan := explainVia(t, ctx, defaultConn, query)
	altPlan := explainVia(t, ctx, altConn, query)
	if basePlan == altPlan {
		t.Fatalf("the two connections planned the SAME query identically (%s); with an index on the filtered "+
			"column and MatchLeafRule disabled they must differ, or the oracle has nothing to compare", basePlan)
	}

	baseRows := selectRows(t, ctx, defaultConn, query)
	altRows := selectRows(t, ctx, altConn, query)
	if len(baseRows) != 4 {
		t.Fatalf("baseline returned %d rows, want 4", len(baseRows))
	}
	if d := factory.RowsDiffForTest(true, baseRows, altRows); d != "" {
		t.Fatalf("two plans for one ORDERED query returned different row SEQUENCES: %s\n  baseline: %s\n"+
			"  second:   %s\n  rows: %v vs %v", d, basePlan, altPlan, baseRows, altRows)
	}
}

func openFactorySchema(t *testing.T, ctx context.Context, name, ddl string) *sql.DB {
	t.Helper()
	dbPath := "/" + name
	setupDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", strings.ToUpper(dbPath), clusterFilePath))
	if err != nil {
		t.Fatalf("open setup db: %v", err)
	}
	t.Cleanup(func() { setupDB.Close() }) //nolint:errcheck
	tmpl := name + "tmpl"
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, name, tmpl),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", strings.ToUpper(dbPath), clusterFilePath, strings.ToUpper(name)))
	if err != nil {
		t.Fatalf("open schema db: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db
}

func explainVia(t *testing.T, ctx context.Context, conn *sql.Conn, query string) string {
	t.Helper()
	var plan string
	if err := conn.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	return plan
}

func selectRows(t *testing.T, ctx context.Context, conn *sql.Conn, query string) [][]any {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
