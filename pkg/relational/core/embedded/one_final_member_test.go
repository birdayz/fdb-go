package embedded

import (
	"strings"
	"testing"
)

// TestOneFinalPlanPerReference pins RFC-183 P5's precondition.
//
// Java lets a plan hold a Quantifier instead of a raw child pointer because
// dereferencing one is unambiguous:
//
//	Quantifier.Physical.getRangesOverPlan() =
//	    Iterables.getOnlyElement(getRangesOver().getFinalExpressions())
//
// `getOnlyElement` THROWS on a group holding two. That single assertion is
// what makes the nil-inner shell state UNREPRESENTABLE in Java rather than
// merely absent — there is no second storage location to disagree with the
// first. Go can only adopt that shape while the same property holds here.
//
// It does hold, and this test is what keeps it holding. Measured at the time
// it was written: zero violations across all 2407 yamsql corpus queries and
// the whole Go test suite.
//
// A NOTE ON WHERE IT IS MEASURED, because getting this wrong inverts the
// answer. Counting final members while RULES ARE FIRING finds plenty of
// references holding many (1186, max 52) — mid-planning groups legitimately
// hold alternatives, and that number says nothing about P5. What matters is
// the state at EXTRACTION, after the task stack drains and OptimizeGroup has
// pruned each group to its winner. This test walks the reference graph the
// way extraction does: final members only, descending through their
// quantifiers.
//
// If this test goes red, P5 is blocked and any plan-holds-a-quantifier work
// must stop until the group that grew a second final is understood.
func TestOneFinalPlanPerReference(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON t (a)
CREATE INDEX idx_b ON t (b)
CREATE INDEX idx_ab ON t (a, b)
CREATE TABLE u (id BIGINT NOT NULL, t_id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_u_t ON u (t_id)`

	// Spread across the plan families the wrappers cover, since the property
	// is per-reference and a single shape would prove almost nothing.
	for _, tc := range []struct{ name, sql string }{
		{"full_scan", "SELECT id FROM t"},
		{"index_equality", "SELECT id FROM t WHERE a = 1"},
		{"index_range", "SELECT id FROM t WHERE a > 1 AND a < 9"},
		{"residual_over_index", "SELECT id FROM t WHERE a = 1 AND c > 2"},
		{"covering", "SELECT a, b FROM t WHERE a = 1"},
		{"intersection", "SELECT id FROM t WHERE a = 1 AND b = 2"},
		{"in_list", "SELECT id FROM t WHERE a IN (1, 2, 3)"},
		{"in_plus_residual", "SELECT id FROM t WHERE a IN (1, 2) AND c > 0"},
		{"sort", "SELECT id FROM t ORDER BY a"},
		{"sort_desc", "SELECT id, a FROM t WHERE a = 1 ORDER BY a DESC"},
		{"limit", "SELECT id FROM t WHERE a = 1 LIMIT 5"},
		{"distinct", "SELECT DISTINCT a FROM t"},
		{"group_by", "SELECT a, COUNT(*) FROM t GROUP BY a"},
		{"having", "SELECT a, SUM(b) FROM t GROUP BY a HAVING SUM(b) > 2"},
		{"join", "SELECT t.id FROM t, u WHERE u.t_id = t.id"},
		{"join_with_filters", "SELECT t.id FROM t, u WHERE u.t_id = t.id AND t.a = 1 AND u.v > 3"},
		{"union_all", "SELECT id FROM t WHERE a = 1 UNION ALL SELECT id FROM t WHERE b = 2"},
		{"or_predicate", "SELECT id FROM t WHERE a = 1 OR b = 2"},
		{"projection_expr", "SELECT id + 100 FROM t WHERE a = 1"},
		{"exists", "SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.t_id = t.id)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			violations, err := planAndVerifyOneFinal(tc.sql, schema)
			if err != nil {
				// A query that cannot plan tells us nothing about the
				// invariant; fail rather than skip, so a shape that silently
				// stops planning is not mistaken for coverage.
				t.Fatalf("plan: %v", err)
			}
			if len(violations) > 0 {
				t.Errorf("P5 precondition broken — %d reference(s) hold more than one physical final, "+
					"so Java's getOnlyElement(getFinalExpressions()) could not be adopted here:\n  %s",
					len(violations), strings.Join(violations, "\n  "))
			}
		})
	}
}
