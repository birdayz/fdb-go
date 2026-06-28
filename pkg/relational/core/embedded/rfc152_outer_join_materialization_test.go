package embedded

// RFC-152 — cost-model materialization for the LEFT-OUTER rewrite.
//
// A preserved-only ON predicate (`a LEFT JOIN b ON a.flag = 1`, the ON pred
// references ONLY the preserved leg `a`, not the null-supplying leg `b`) used to
// plan as a FlatMap whose inner RE-SCANS `b` from FDB once per `a` row (O(N)
// scans) — RewriteOuterJoinRule always fires (Java-faithful, no cross-leg guard)
// and the cost model could not tell the re-scan FlatMap from Go's materialized
// NLJ (which scans `b` ONCE), so it picked the re-scan. RFC-152 models
// materialization in the cost (nestedLoopJoinCost charges the inner scanned once)
// and ranks same-Reference join candidates by WORK, so the materialized NLJ wins
// for a NON-PROBE inner while a card-1 PROBE inner keeps the FlatMap cheapest.
//
// These are TYPED plan-tree assertions (plans.Walk over the concrete physical
// plan), NOT EXPLAIN string matches.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

const rfc152Schema = `
CREATE TABLE A (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id))
CREATE TABLE B (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id))
CREATE INDEX b_a_id ON B(a_id)
`

// planRFC152 plans a SQL query against the RFC-152 schema with large uniform
// table stats and returns the typed physical RecordQueryPlan.
func planRFC152(t *testing.T, sql string) plans.RecordQueryPlan {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(rfc152Schema)
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

// countPlanNodes walks the typed plan tree and counts NLJ vs FlatMap nodes, plus
// whether any FlatMap inner contains a re-scanning full Scan (the regression
// shape) vs an index point-probe.
type rfc152Shape struct {
	nlj         int // materialized RecordQueryNestedLoopJoinPlan
	flatMap     int // correlated RecordQueryFlatMapPlan
	flatMapScan int // a FlatMap whose subtree contains a full primary Scan (re-scan inner)
	indexScan   int // index scans anywhere (probe inners)
}

func rfc152Classify(p plans.RecordQueryPlan) rfc152Shape {
	var s rfc152Shape
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		switch n.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			s.nlj++
		case *plans.RecordQueryFlatMapPlan:
			s.flatMap++
		case *plans.RecordQueryIndexPlan:
			s.indexScan++
		}
		return true
	})
	// A re-scan FlatMap is a FlatMap whose inner subtree reads a full primary Scan
	// of the null-supplying table (no index probe). Detect by: there is a FlatMap
	// AND a full primary Scan of B is reachable under it.
	if s.flatMap > 0 {
		plans.Walk(p, func(n plans.RecordQueryPlan) bool {
			fm, ok := n.(*plans.RecordQueryFlatMapPlan)
			if !ok {
				return true
			}
			plans.Walk(fm.GetInner(), func(inner plans.RecordQueryPlan) bool {
				if scan, ok := inner.(*plans.RecordQueryScanPlan); ok {
					// A full scan (no equality SARG) re-scans the whole table.
					full := true
					for _, cr := range scan.GetScanComparisons() {
						if cr != nil && !cr.IsEmpty() {
							full = false
						}
					}
					if full {
						s.flatMapScan++
					}
				}
				return true
			})
			return true
		})
	}
	return s
}

// TestRFC152_PreservedOnlyOnPredicate_MaterializedNLJ proves a preserved-only ON
// predicate plans to the MATERIALIZED NestedLoopJoin (inner scanned once), NOT the
// re-scan FlatMap. This is the codex P2 regression fix (typed plan-tree assertion).
func TestRFC152_PreservedOnlyOnPredicate_MaterializedNLJ(t *testing.T) {
	t.Parallel()
	plan := planRFC152(t, "SELECT A.id FROM A LEFT JOIN B ON A.flag = 1")
	s := rfc152Classify(plan)
	if s.nlj != 1 {
		t.Errorf("preserved-only LEFT JOIN: want exactly 1 materialized NestedLoopJoin, got %d (plan: %s)", s.nlj, plan.Explain())
	}
	if s.flatMap != 0 {
		t.Errorf("preserved-only LEFT JOIN: want 0 FlatMap (the re-scan shape), got %d (plan: %s)", s.flatMap, plan.Explain())
	}
	if s.flatMapScan != 0 {
		t.Errorf("preserved-only LEFT JOIN: a FlatMap re-scans B per outer row — the RFC-152 regression (plan: %s)", plan.Explain())
	}
}

// TestRFC152_CrossLegProbe_StaysFlatMap proves a cross-leg (probe) ON predicate
// keeps the index-probe FlatMap — the materialization fix must NOT regress the
// case where the FlatMap genuinely enables a cheap per-outer index point-probe.
func TestRFC152_CrossLegProbe_StaysFlatMap(t *testing.T) {
	t.Parallel()
	plan := planRFC152(t, "SELECT A.id FROM A LEFT JOIN B ON B.a_id = A.id")
	s := rfc152Classify(plan)
	if s.flatMap != 1 {
		t.Errorf("cross-leg probe LEFT JOIN: want exactly 1 probe FlatMap, got %d (plan: %s)", s.flatMap, plan.Explain())
	}
	if s.nlj != 0 {
		t.Errorf("cross-leg probe LEFT JOIN: want 0 materialized NLJ, got %d (plan: %s)", s.nlj, plan.Explain())
	}
	if s.indexScan == 0 {
		t.Errorf("cross-leg probe LEFT JOIN: want an index point-probe inner, got none (plan: %s)", plan.Explain())
	}
	// The probe FlatMap must NOT re-scan B's full table — it point-probes the index.
	if s.flatMapScan != 0 {
		t.Errorf("cross-leg probe LEFT JOIN: FlatMap inner re-scans B full instead of index-probing (plan: %s)", plan.Explain())
	}
}
