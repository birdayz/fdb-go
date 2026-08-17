package factory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/factory"
)

// TestFDB_SecondPlanPreconditionIgnoresGeneratedAliases pins the second-plan
// oracle's precondition against a process-global counter and against Explain
// changing where that counter is hidden.
//
// The precondition is `altPlan == basePlan`, a STRING equality over EXPLAIN
// text. Historically that text carried planner-generated correlation
// identifiers whose numeric suffix comes from a process-global counter. The
// query below was the measured reproducer: with no indexable predicate,
// MatchLeafRule has nothing to do, so the two connections produce the SAME
// PLAN — and the raw text came back as
// `Project([(SCALAR_SUBQUERY q$11)], Scan(T))` against
// `Project([(SCALAR_SUBQUERY q$38)], Scan(T))`.
//
// Compared raw, that is a failure with no visible symptom. The oracle would
// conclude the plans differ, count SecondPlanKept, and then compare a plan's
// rows against ITS OWN — a tautology that passes for any engine, correct or
// not. Worse, it would bank that as evidence: the run-level went-dark floor
// fires only when kept == 0, so a harness whose precondition was satisfied by
// nothing but counter drift would report a healthy, well-exercised oracle.
//
// Exact result-owner display may instead suppress an ownership-only scalar
// alias, rendering `(SCALAR_SUBQUERY)`. That is also safe, but only when the
// two raw plans are then identical. This test accepts those two safety
// mechanisms and no middle state: either both raw plans expose different
// generated aliases which normalization removes, or neither exposes one and
// the raw plans already compare equal.
//
// A scalar subquery is not an exotic shape here. rowdiff draws one on roughly a
// fifth of its plain queries, so this reaches the factory on its own.
func TestFDB_SecondPlanPreconditionIgnoresGeneratedAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_a ON t (a)"
	db := openFactorySchema(t, ctx, "zzalias", ddl)

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
	if _, err := defaultConn.ExecContext(ctx, "INSERT INTO t VALUES (1,5,10),(2,3,20)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const query = "SELECT (SELECT MIN(b) FROM t) FROM t"

	rawBase := explainVia(t, ctx, defaultConn, query)
	rawAlt := explainVia(t, ctx, altConn, query)
	if !strings.Contains(rawBase, "SCALAR_SUBQUERY") || !strings.Contains(rawAlt, "SCALAR_SUBQUERY") {
		t.Fatalf("query stopped producing the scalar-subquery shape whose second-plan precondition this test pins:\n  baseline: %s\n  second:   %s", rawBase, rawAlt)
	}
	baseHasGeneratedAlias := strings.Contains(rawBase, "$")
	altHasGeneratedAlias := strings.Contains(rawAlt, "$")
	if baseHasGeneratedAlias != altHasGeneratedAlias {
		t.Fatalf("Explain exposed a generated alias on only one planning of the same scalar-subquery shape:\n  baseline: %s\n  second:   %s", rawBase, rawAlt)
	}
	if baseHasGeneratedAlias && rawBase == rawAlt {
		t.Fatalf("both plans expose a generated alias but the independent plannings rendered identically, so this run cannot prove normalization removes process-global counter drift: %s", rawBase)
	}
	if !baseHasGeneratedAlias && rawBase != rawAlt {
		t.Fatalf("Explain suppressed generated aliases but the otherwise identical plans still drifted:\n  baseline: %s\n  second:   %s", rawBase, rawAlt)
	}
	t.Logf("raw baseline: %s", rawBase)
	t.Logf("raw second:   %s", rawAlt)

	base, err := factory.ExplainOnForTest(ctx, defaultConn, query)
	if err != nil {
		t.Fatalf("explainOn baseline: %v", err)
	}
	alt, err := factory.ExplainOnForTest(ctx, altConn, query)
	if err != nil {
		t.Fatalf("explainOn second: %v", err)
	}

	if base != alt {
		t.Fatalf("two connections produced plan texts the oracle treats as DIFFERENT PLANS:\n  baseline: %s\n"+
			"  second:   %s\nThe plans are structurally identical — only the process-global alias counter moved. "+
			"The oracle would count this as a kept comparison and then compare a plan's rows against its own, "+
			"which passes for every engine; and because the went-dark floor only fires when kept == 0, banking "+
			"tautologies here also switches the floor off.", base, alt)
	}

	// The erasure must not be total. Two genuinely different plans still have to
	// compare unequal, or the precondition never holds and every case skips —
	// the opposite failure, and the one the floor was built for.
	const indexed = "SELECT id, a FROM t WHERE a = 5"
	sharp, err := factory.ExplainOnForTest(ctx, defaultConn, indexed)
	if err != nil {
		t.Fatal(err)
	}
	dull, err := factory.ExplainOnForTest(ctx, altConn, indexed)
	if err != nil {
		t.Fatal(err)
	}
	if sharp == dull {
		t.Fatalf("normalizing the plan text also erased a REAL difference: %q. An index scan and a filtered "+
			"full scan must not compare equal, or the oracle skips everything.", sharp)
	}
}
