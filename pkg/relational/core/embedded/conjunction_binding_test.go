package embedded

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// conjunctionBindingShape is what a plan says about how a conjunction bound
// its index: the scan's comparisons per coordinate (type symbols and rendered
// comparands, so `[>= 2, < 5]` is asserted as such and not as "an
// inequality"), the residual predicate count, and the IN-join / IN-union
// nesting depth.
type conjunctionBindingShape struct {
	scan      string // e.g. "IDX_A[= 1][>= 2 < 5]" — one bracket per key coordinate
	residuals int    // predicates on filters, summed; 0 = none
	inJoins   int    // InJoin nodes
	inUnions  int    // InUnion nodes
}

func conjunctionBindingShapeOf(plan plans.RecordQueryPlan) conjunctionBindingShape {
	var shape conjunctionBindingShape
	var scans []string
	renderComparisons := func(name string, comps []*predicates.ComparisonRange) string {
		var b strings.Builder
		b.WriteString(name)
		for _, cr := range comps {
			b.WriteString("[")
			for i, c := range cr.GetComparisons() {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(c.Type.Symbol())
				if c.Operand != nil {
					if lit, known := c.Operand.Evaluate(nil); known == nil && lit != nil {
						fmt.Fprintf(&b, " %v", lit)
					} else {
						b.WriteString(" $")
					}
				}
			}
			b.WriteString("]")
		}
		return b.String()
	}
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		switch n := p.(type) {
		case *plans.RecordQueryIndexPlan:
			scans = append(scans, renderComparisons(n.GetIndexName(), n.GetScanComparisons()))
		case *plans.RecordQueryCoveringIndexPlan:
			walk(n.GetIndexPlan())
			return
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			walk(n.GetInner())
			return
		case *plans.RecordQueryScanPlan:
			scans = append(scans, renderComparisons("PK", n.GetScanComparisons()))
		case *plans.RecordQueryPredicatesFilterPlan:
			shape.residuals += len(n.GetPredicates())
		case *plans.RecordQueryInJoinPlan:
			shape.inJoins++
		case *plans.RecordQueryInUnionPlan:
			shape.inUnions++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	shape.scan = strings.Join(scans, " + ")
	return shape
}

// TestConjunctionBinding pins how a WHERE conjunction binds an index, on
// the typed plan: every comparison on one column folds into ONE scan range
// (both bounds of a BETWEEN, both of `a > 2 AND a > 3`), an equality beside
// an inequality binds the equality and re-applies the inequality, a
// duplicate equality re-applies nothing, and an IN beside other conjuncts
// still explodes into an IN-join whose inner binds the remaining conjuncts.
// Before this fold every second comparison on a column was a residual over
// a half-bounded scan, and an IN under an AND never reached the explode
// rule at all (a full table scan for `id IN (1, 2) AND b > 5`).
func TestConjunctionBinding(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_a ON T(a)
CREATE INDEX idx_ab ON T(a, b)`

	cases := []struct {
		name string
		sql  string
		want conjunctionBindingShape
	}{
		{"two_sided_range", "SELECT id FROM t WHERE a >= 2 AND a < 5", conjunctionBindingShape{scan: "IDX_A[>= 2 < 5]"}},
		{"two_sided_range_reversed_order", "SELECT id FROM t WHERE a < 5 AND a >= 2", conjunctionBindingShape{scan: "IDX_A[< 5 >= 2]"}},
		{"two_lower_bounds_both_carried", "SELECT id FROM t WHERE a > 2 AND a > 3", conjunctionBindingShape{scan: "IDX_A[> 2 > 3]"}},
		{"between_on_second_column", "SELECT id FROM t WHERE a = 1 AND b BETWEEN 2 AND 5", conjunctionBindingShape{scan: "IDX_AB[= 1][>= 2 <= 5]"}},
		{"range_does_not_extend_the_run", "SELECT id FROM t WHERE a > 2 AND a < 5 AND b = 3", conjunctionBindingShape{scan: "IDX_AB[> 2 < 5][]", residuals: 1}},
		{"equality_beside_inequality", "SELECT id FROM t WHERE a = 1 AND a > 0", conjunctionBindingShape{scan: "IDX_A[= 1]", residuals: 1}},
		{"inequality_then_equality", "SELECT id FROM t WHERE a > 0 AND a = 1", conjunctionBindingShape{scan: "IDX_A[= 1]", residuals: 1}},
		{"contradictory_equalities", "SELECT id FROM t WHERE a = 1 AND a = 2", conjunctionBindingShape{scan: "IDX_A[= 1]", residuals: 1}},
		{"duplicate_equality", "SELECT id FROM t WHERE a = 1 AND a = 1", conjunctionBindingShape{scan: "IDX_A[= 1]"}},
		{"in_with_unindexed_residual", "SELECT id FROM t WHERE a IN (1, 2) AND v = 3", conjunctionBindingShape{scan: "IDX_A[= $]", residuals: 1, inJoins: 1}},
		{"in_with_second_column_bound", "SELECT id FROM t WHERE a IN (1, 2) AND b = 3", conjunctionBindingShape{scan: "IDX_AB[= $][= 3]", inJoins: 1}},
		{"in_on_pk_with_residual", "SELECT id FROM t WHERE id IN (1, 2) AND b > 5", conjunctionBindingShape{scan: "PK[= $]", residuals: 1, inJoins: 1}},
		{"nested_ins", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4)", conjunctionBindingShape{scan: "IDX_AB[= $][= $]", inJoins: 2}},
		{"in_beside_a_range_on_the_same_column", "SELECT id FROM t WHERE a IN (1, 2) AND a > 1", conjunctionBindingShape{scan: "IDX_A[= $]", residuals: 1, inJoins: 1}},
		{"in_on_the_second_column", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4)", conjunctionBindingShape{scan: "IDX_AB[= 1][= $]", inJoins: 1}},
		{"in_with_range_on_second_column_ordered", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 ORDER BY b", conjunctionBindingShape{scan: "IDX_AB[= $][> 3]", inUnions: 1}},
		// An IN over a column no index can bind must NOT become an IN-join:
		// that plan scans the table once per IN element. Criterion #6 of the
		// cost model penalises an unSARGed IN-plan anywhere in the tree, so the
		// single filtered scan wins — alone, beside a conjunct, and below an
		// aggregate.
		{"unindexed_in_alone", "SELECT id FROM t WHERE v IN (1, 2)", conjunctionBindingShape{scan: "PK", residuals: 1}},
		{"unindexed_in_with_conjunct", "SELECT id FROM t WHERE v IN (1, 2) AND c > 3", conjunctionBindingShape{scan: "PK", residuals: 2}},
		{"unindexed_in_under_aggregate", "SELECT COUNT(*) FROM t WHERE s IN ('x', 'y')", conjunctionBindingShape{scan: "PK", residuals: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			got := conjunctionBindingShapeOf(plan)
			if got != c.want {
				t.Fatalf("shape = %+v, want %+v\n  sql:  %s\n  plan: %s", got, c.want, c.sql, plan.Explain())
			}
		})
	}
}
