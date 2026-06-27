package embedded

// RFC-153 — joined/derived-preserved-side LEFT OUTER buried-merge correlation.
//
// When the preserved side of a LEFT OUTER is itself a join, the ON predicate may
// correlate the null-supplying table C to a BURIED preserved source (an alias
// hidden under the preserved join, e.g. `A` in `A JOIN B ... LEFT JOIN C ON
// c.a_id = a.id`, or the OTHER buried leg `B` in `... ON c.bx_ref = b.bx`). The
// fix rebases the correlated key onto the buried source so the LEFT OUTER plans
// as a correlated index-probe FlatMap (no materialized NestedLoopJoin), while the
// RFC-152 preserved-only invariant (no correlation → materialized NLJ, no probe)
// stays intact.
//
// These are TYPED plan-tree assertions (plans.Walk over the concrete physical
// plan), NOT EXPLAIN string matches.

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

const rfc153mxSchema = `
CREATE TABLE a (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bx BIGINT, PRIMARY KEY (id))
CREATE TABLE d (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id))
CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, bx_ref BIGINT, PRIMARY KEY (id))
CREATE INDEX b_a_id ON b(a_id)
CREATE INDEX d_b_id ON d(b_id)
CREATE INDEX c_a_id ON c(a_id)
CREATE INDEX c_bx_ref ON c(bx_ref)
`

// planRFC153mx plans a SQL query against the RFC-153 matrix schema with large
// uniform table stats and returns the typed physical RecordQueryPlan.
func planRFC153mx(t *testing.T, sql string) plans.RecordQueryPlan {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(rfc153mxSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	stats := properties.FixedStatistics{Cardinality: 1_000_000}
	plan, err := PlanRecordQueryWithMetadata(sql, tmpl.Underlying(), stats)
	if err != nil {
		t.Fatalf("plan %q: %v", sql, err)
	}
	if plan == nil {
		t.Fatalf("plan %q: nil plan", sql)
	}
	return plan
}

// indexProbes reports whether the plan tree contains an equality-bound
// RecordQueryIndexPlan on the named index (case-insensitive). "Equality-bound"
// means at least one non-empty ComparisonRange — i.e. a point/range probe, not a
// full index scan.
func indexProbes(plan plans.RecordQueryPlan, indexName string) bool {
	found := false
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		ix, ok := n.(*plans.RecordQueryIndexPlan)
		if !ok {
			return true
		}
		if !strings.EqualFold(ix.GetIndexName(), indexName) {
			return true
		}
		for _, cr := range ix.GetScanComparisons() {
			if cr != nil && !cr.IsEmpty() {
				found = true
			}
		}
		return true
	})
	return found
}

// countNLJ counts materialized RecordQueryNestedLoopJoinPlan nodes in the tree.
func countNLJ(plan plans.RecordQueryPlan) int {
	n := 0
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		if _, ok := node.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			n++
		}
		return true
	})
	return n
}

// TestRFC153_JoinedPreserved_ProbesCViaAId — the canonical buried case: C
// correlates to the buried preserved alias A. The LEFT OUTER must plan as a
// correlated point-probe on C_A_ID, with no materialized NestedLoopJoin.
func TestRFC153_JoinedPreserved_ProbesCViaAId(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id")
	if !indexProbes(plan, "C_A_ID") {
		t.Errorf("joined-preserved LEFT JOIN: want an equality-bound C_A_ID index probe, got none (plan: %s)", plan.Explain())
	}
	if n := countNLJ(plan); n != 0 {
		t.Errorf("joined-preserved LEFT JOIN: want 0 materialized NestedLoopJoin, got %d (plan: %s)", n, plan.Explain())
	}
}

// TestRFC153_BuriedOtherLeg_ProbesCViaBxRef — C correlates to the OTHER buried
// leg B (`c.bx_ref = b.bx`). Proves the rebase targets the correct buried source
// key (B's bx), index-probing C_BX_REF rather than A's key.
func TestRFC153_BuriedOtherLeg_ProbesCViaBxRef(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.bx_ref = b.bx")
	if !indexProbes(plan, "C_BX_REF") {
		t.Errorf("buried-other-leg LEFT JOIN: want an equality-bound C_BX_REF index probe, got none (plan: %s)", plan.Explain())
	}
	if n := countNLJ(plan); n != 0 {
		t.Errorf("buried-other-leg LEFT JOIN: want 0 materialized NestedLoopJoin, got %d (plan: %s)", n, plan.Explain())
	}
}

// TestRFC153_ThreeWayDeeper_ProbesC — three-way preserved join (a⋈b⋈d) with C
// correlated to the deepest buried alias A. The buried correlation must still
// resolve and probe C_A_ID with no materialized NestedLoopJoin.
func TestRFC153_ThreeWayDeeper_ProbesC(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a JOIN b ON b.a_id = a.id JOIN d ON d.b_id = b.id LEFT JOIN c ON c.a_id = a.id")
	if !indexProbes(plan, "C_A_ID") {
		t.Errorf("three-way deeper LEFT JOIN: want an equality-bound C_A_ID index probe, got none (plan: %s)", plan.Explain())
	}
	if n := countNLJ(plan); n != 0 {
		t.Errorf("three-way deeper LEFT JOIN: want 0 materialized NestedLoopJoin, got %d (plan: %s)", n, plan.Explain())
	}
}

// TestRFC153_SimplePreserved_StillProbes — control: a single-table preserved
// side (`a LEFT JOIN c ON c.a_id = a.id`) still plans as a C_A_ID probe FlatMap
// with no materialized NestedLoopJoin (unchanged by the buried-merge fix).
func TestRFC153_SimplePreserved_StillProbes(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a LEFT JOIN c ON c.a_id = a.id")
	if !indexProbes(plan, "C_A_ID") {
		t.Errorf("simple preserved LEFT JOIN: want an equality-bound C_A_ID index probe, got none (plan: %s)", plan.Explain())
	}
	if n := countNLJ(plan); n != 0 {
		t.Errorf("simple preserved LEFT JOIN: want 0 materialized NestedLoopJoin, got %d (plan: %s)", n, plan.Explain())
	}
}

// TestRFC153_PreservedOnly_StillMaterializes — the RFC-152 invariant must hold:
// a preserved-only ON predicate (`a LEFT JOIN c ON a.flag = 1`, no correlation to
// C) plans as exactly one materialized NestedLoopJoin with NO C index probe — it
// does not get rewritten into a (wrong) buried-merge probe.
func TestRFC153_PreservedOnly_StillMaterializes(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a LEFT JOIN c ON a.flag = 1")
	if n := countNLJ(plan); n != 1 {
		t.Errorf("preserved-only LEFT JOIN: want exactly 1 materialized NestedLoopJoin, got %d (plan: %s)", n, plan.Explain())
	}
	if indexProbes(plan, "C_A_ID") {
		t.Errorf("preserved-only LEFT JOIN: must NOT index-probe C_A_ID (RFC-152 invariant) (plan: %s)", plan.Explain())
	}
	if indexProbes(plan, "C_BX_REF") {
		t.Errorf("preserved-only LEFT JOIN: must NOT index-probe C_BX_REF (RFC-152 invariant) (plan: %s)", plan.Explain())
	}
}

// hasLeftOuterNLJ reports whether the tree contains a materialized
// RecordQueryNestedLoopJoinPlan whose join type is LEFT OUTER — i.e. the LEFT OUTER
// was DECLINED into the materialized fallback rather than planned as a correlated
// FlatMap probe.
func hasLeftOuterNLJ(plan plans.RecordQueryPlan) bool {
	found := false
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		if nlj, ok := n.(*plans.RecordQueryNestedLoopJoinPlan); ok && nlj.GetJoinType() == plans.JoinLeftOuter {
			found = true
		}
		return true
	})
	return found
}

// TestRFC153_AggregateInner_DeclinesToMaterializedNLJ — the fail-closed axis (the
// dimension the 3-reviewer NAK flagged). The null-supplying side is an AGGREGATE whose
// grouping correlates to the buried preserved alias A. Its inner carries a node
// (StreamingAgg) the buried-merge rebaser does NOT rewrite, so the verifier fail-CLOSES
// → the LEFT OUTER DECLINES the probe → materialized NestedLoopJoin (LEFT OUTER), NOT a
// correlated probe on the aggregate. (Confirmed via instrumentation that this exercises
// the decline; the unit test pins the verifier logic.)
func TestRFC153_AggregateInner_DeclinesToMaterializedNLJ(t *testing.T) {
	t.Parallel()
	plan := planRFC153mx(t, "SELECT a.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN (SELECT a_id, COUNT(*) cnt FROM c GROUP BY a_id) g ON g.a_id = a.id")
	if !hasLeftOuterNLJ(plan) {
		t.Errorf("aggregate-inner LEFT OUTER must DECLINE to a materialized NLJ (fail-closed on the StreamingAgg inner), got: %s", plan.Explain())
	}
	if indexProbes(plan, "c_a_id") {
		t.Errorf("aggregate-inner LEFT OUTER must NOT correlated-probe the aggregate side via c_a_id — it declined: %s", plan.Explain())
	}
}

const rfc153corrInSchema = `
CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id))
CREATE TABLE c (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id))
CREATE TABLE s (id BIGINT NOT NULL, cv BIGINT, ak BIGINT, PRIMARY KEY (id))
CREATE INDEX b_a_id ON b(a_id)
CREATE INDEX c_x ON c(x)
CREATE INDEX s_ak ON s(ak)
`

// TestRFC153_CorrelatedInInner_DeclinesToMaterializedNLJ — Graefe's correlated-IN shape:
// the null-supplying ON predicate is `c.x IN (SELECT … WHERE s.ak = a.id)`, correlating
// to the buried preserved alias A through an IN-subquery (an InJoin/correlated node the
// rebaser does not rewrite). Asserts ONLY the DECLINE (materialized NLJ, no correlated
// probe on c) — the row correctness of IN-in-ON is governed by a SEPARATE pre-existing
// materialized-NLJ bug (TODO.md), independent of RFC-153, so rows are not asserted here.
func TestRFC153_CorrelatedInInner_DeclinesToMaterializedNLJ(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(rfc153corrInSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	plan, err := PlanRecordQueryWithMetadata(
		"SELECT a.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.x IN (SELECT s.cv FROM s WHERE s.ak = a.id)",
		tmpl.Underlying(), properties.FixedStatistics{Cardinality: 1_000_000})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !hasLeftOuterNLJ(plan) {
		t.Errorf("correlated-IN LEFT OUTER must be a materialized NLJ (never a wrong-rows correlated probe), got: %s", plan.Explain())
	}
	if indexProbes(plan, "c_x") {
		t.Errorf("correlated-IN LEFT OUTER must NOT correlated-probe c via c_x: %s", plan.Explain())
	}
}
