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

// TestFDB_SecondPlanKeepsCorrelatedIndexProbe is the counterexample that
// retired the second-plan oracle's index-free precondition.
//
// The oracle used to demand that the MatchLeafRule-disabled plan contain no
// index scan, on the reasoning that MatchLeafRule is the sole seed of
// PartialMatch objects — which it is (rule_match_leaf.go:76 is the only
// unconditional NewPartialMatch call) — so disabling it starves the whole
// match/data-access pipeline. The step that does not follow is that the match
// pipeline is the only thing that builds an index scan. It is not:
// ImplementNestedLoopJoinRule.tryExistsFlatMap reads GetMatchCandidates()
// directly and constructs a RecordQueryIndexPlan from a single
// ComparisonEquals range (rule_implement_nested_loop_join.go:4328), which is
// exactly the `[=]`-bound probe below. It never touches a PartialMatch, so
// MatchLeafRule cannot take it away.
//
// The precondition would therefore have reported a CORRECT engine as a broken
// planner option on every correlated-EXISTS query. This test exists so that
// claim stops being prose: it plans the shape on a live engine, through the
// same PinConn the oracle uses, and fails if the probe ever stops surviving.
//
// If it goes RED, the reasoning behind dropping the precondition has changed
// and the precondition is worth reconsidering — that is a real finding, not a
// test to relax.
func TestFDB_SecondPlanKeepsCorrelatedIndexProbe(t *testing.T) {
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
	const ddl = "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
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

	// The counterexample itself: an equality-bound index scan survives.
	if !strings.Contains(altPlan, "IndexScan") {
		t.Fatalf("the MatchLeafRule-disabled plan has NO index scan:\n  %s\nThe oracle's retired index-free "+
			"precondition would now hold for this shape, so the reasoning that retired it needs re-deriving "+
			"before anyone relies on it again.", altPlan)
	}
	if !strings.Contains(altPlan, "IndexScan(IDX_B, [=])") {
		t.Fatalf("the MatchLeafRule-disabled plan keeps an index scan but NOT the equality-bound correlated "+
			"probe:\n  %s\nOnly tryExistsFlatMap can produce a `[=]` bound without a PartialMatch; the other "+
			"ungated rules pass an empty comparison prefix and emit full-range scans.", altPlan)
	}
}

// TestFDB_SecondPlanIsBlindToCorrelatedExists records a MEASURED hole in the
// oracle, so nobody reads the corpus as covering a shape it does not.
//
// The probe leg is not the only part of a correlated-EXISTS plan that
// MatchLeafRule cannot touch: the OUTER leg comes out as a plain filtered scan
// in the baseline too, even with the filtered column indexed. So the two plans
// are byte-identical, the oracle's plan-inequality precondition never holds,
// and every correlated-EXISTS candidate is counted as a second-plan SKIP.
//
// That has a consequence the skip counter alone does not show. Metamorphic
// blessing requires BOTH oracles, so an EXISTS candidate can never be blessed
// without a Java leg — and the committed corpus contains, at the time of
// writing, zero scenarios whose feature vector carries `exists=`. The
// generator's "rich wrong-rows surface" for semi-joins, decorrelation and
// correlation binding reaches the factory and is dropped at the last gate.
//
// This is pinned rather than fixed because the fix is a ruling about what
// blessing MEANS (a third metamorphic oracle, or a perturbation the EXISTS plan
// responds to), not a bug in this file. If it goes GREEN-to-RED — the two plans
// start differing — the hole has closed and the blessing rule should be
// revisited to let these candidates through.
func TestFDB_SecondPlanIsBlindToCorrelatedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const ddl = "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
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

	for _, q := range []string{
		"SELECT id, a FROM t WHERE a = 5 AND NOT EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b)",
		"SELECT id, a FROM t WHERE a = 5 AND EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b)",
		"SELECT id, a FROM t WHERE a > 2 AND NOT EXISTS (SELECT 1 FROM t AS r WHERE r.b = t.b AND r.a > 1)",
	} {
		base, alt := explainVia(t, ctx, defaultConn, q), explainVia(t, ctx, altConn, q)
		if base != alt {
			t.Errorf("the two plans now DIFFER for %s\n  base: %s\n  alt:  %s\nThe second-plan oracle can now "+
				"bite on correlated EXISTS, which is a strengthening: revisit the metamorphic blessing rule, "+
				"which currently drops every one of these candidates.", q, base, alt)
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

	const ddl = "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
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
	setupDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFilePath))
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
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, name))
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
