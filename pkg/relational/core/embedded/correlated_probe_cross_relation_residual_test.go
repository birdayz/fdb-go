package embedded_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// crossRelationResidualDDL is the corpus table shape reduced to the two columns
// the defect needs: an indexed column the inner side is probed on (C, IDX_C)
// and a second column the residual can reference (D). ID/A/S ride along so the
// projection can be varied between a covered and an uncovered column.
const crossRelationResidualDDL = `CREATE TABLE T_RD (ID BIGINT, A BIGINT, C BIGINT, S STRING, F BOOLEAN, D DOUBLE, PRIMARY KEY (ID))
CREATE INDEX IDX_C ON T_RD (C)`

// TestCorrelatedInnerKeepsEqualityProbeUnderCrossRelationResidual pins that a
// correlated equality index probe on the inner side of a join SURVIVES an
// additional predicate that references BOTH sides of the join.
//
// The join key `R.C = L.A` is sargable against IDX_C, so the inner leg must
// plan as a per-outer-row point probe — FlatMap(outer, IndexScan(IDX_C, [=])),
// O(log M) per outer row. The regression this pins replaced it with a
// materialized NestedLoopJoin over two full scans: O(M) per outer row, with
// IDENTICAL ROWS. No row-based test can see that, which is why this class
// regressed across 183 corpus scenarios with the suite green.
//
// MECHANISM. The data-access match for the inner leg produces a covering scan
// plus a residual-filter compensation. That compensation is only routed for
// re-optimization (yieldUnknown → ImplementFilterRule) when
// compensationResidualCorrelationSafe judges its outer-correlated residual
// safe, and the safety EXCEPTION is "the compensation's own probe is already
// correlated to that same outer alias". Recovering the probe's correlations
// walks the plan through GetChildren; a covering scan holds its index scan as a
// FIELD, so the walk stopped short, the probe correlations read as EMPTY, the
// residual was judged unsafe, and the compensation went to InsertFinal with NO
// exploration task. ImplementFilterRule then never fired on it and the probe
// alternative was NEVER CONSTRUCTED — not out-costed. Java answers the same
// question by delegation (RecordQueryCoveringIndexPlan implements
// RecordQueryPlanWithNoChildren and returns indexPlan.getCorrelatedTo(),
// RecordQueryCoveringIndexPlan.java:224).
//
// THE CONTROLS ARE THE POINT. Only the CROSS-RELATION arms exercise the defect:
// an outer-only or inner-only extra predicate never reaches the outer-
// correlation guard at all, and both kept the probe throughout. A test built
// from those arms alone passes with the bug fully present. They are kept here
// as the invariance statement — the variable is cross-relation-ness, not the
// presence of a second predicate — and they are what makes a future failure
// readable: if only the cross-relation arms go red, the guard is back.
func TestCorrelatedInnerKeepsEqualityProbeUnderCrossRelationResidual(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
		why  string
	}{
		{
			name: "no_extra_predicate",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A`,
			why:  "control: the bare correlated probe, never affected",
		},
		{
			name: "outer_only_extra_predicate",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND L.F = FALSE`,
			why:  "control: an outer-leg-local residual never reaches the correlation guard",
		},
		{
			name: "inner_only_extra_predicate",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND R.A > 5`,
			why:  "control: an inner-leg-local residual is not outer-correlated",
		},
		{
			name: "both_legs_but_separately",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND L.F = FALSE AND R.A > 5`,
			why:  "control: two single-leg residuals are still not CROSS-relation",
		},
		{
			name: "cross_relation_residual_same_column",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND L.D <> R.C`,
			why:  "the defect: residual reads both legs, and its outer alias IS fed by the probe",
		},
		{
			name: "cross_relation_residual_other_column",
			sql:  `SELECT R.ID FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND L.D <> R.A`,
			why:  "the defect does not depend on the residual naming the indexed column",
		},
		{
			// A projection of S — a column in no index entry — so no covering
			// plan can serve the output. Pinned because it separates "the probe
			// survives" from "the plan happens to be covering": the defect is in
			// the probe's CONSTRUCTION, not in whether the winner covers.
			name: "cross_relation_residual_uncovered_projection",
			sql:  `SELECT R.S FROM T_RD AS L, T_RD AS R WHERE R.C = L.A AND L.D <> R.C`,
			why:  "the probe must survive even when the projection forces a record read",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := embedded.PlanQueryForTest(tc.sql, crossRelationResidualDDL, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			// Asserted as two independent halves on purpose. "Uses IDX_C" alone
			// would pass on an unbounded index scan, and "no NestedLoopJoin"
			// alone would pass on a FlatMap over a full scan — which is the
			// regression one step less obvious.
			if !strings.Contains(plan, "IndexScan(IDX_C, [=])") {
				t.Errorf("correlated inner lost its EQUALITY probe on IDX_C (%s)\n  sql:  %s\n  plan: %s",
					tc.why, tc.sql, plan)
			}
			if strings.Contains(plan, "NestedLoopJoin") {
				t.Errorf("join degraded to a materialized NestedLoopJoin — O(N) per outer row with identical rows (%s)\n  sql:  %s\n  plan: %s",
					tc.why, tc.sql, plan)
			}
		})
	}
}
