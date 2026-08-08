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
// THIS TEST IS GREEN AND THE INVARIANT IT NAMES IS FALSE. Read RFC-224 before
// trusting a pass here, and do not cite this test as evidence for P5.
//
// Two independent reasons, both measured:
//
//  1. The walk is BLIND. VerifyOneFinalPlanPerReference descends only through
//     FINAL members' quantifiers, so a reference holding an empty final set is
//     simultaneously a non-violation and a walk terminator. Across the 20
//     subtests below: 43 reference visits, 21 of them dead ends, and for 18 of
//     the 20 queries the root is the only non-empty-final reference the walk
//     ever reaches. On the `join` case the verifier visits 2 references where
//     plan extraction visits 5, and the memo holds three groups with multiple
//     physical finals and no winner. Repointing the walk at the graph
//     extraction actually traverses turns `join` red on a group with four
//     physical finals.
//
//  2. The property is not Go's. Java prunes to exactly one member
//     (Reference.pruneWith clears then inserts), while Go's OptimizeGroupTask
//     prunes to a KEEP SET of best-plus-one-per-requested-ordering. Multi-final
//     groups are Go's DESIGN — a group retaining one winner per required
//     physical property is textbook Cascades — so the invariant as stated
//     contradicts the planner rather than describing it.
//
// So the fix is not "make the walk honest and let this go red": that would
// assert a property Go deliberately does not have. RFC-224 decides what Go
// should assert instead (one final per REQUIRED PHYSICAL PROPERTY) and this
// test changes with it. No wrong plan has been demonstrated from either half.
//
// The original claim, kept because it is what a reader will otherwise assume:
// "zero violations across all 2407 yamsql corpus queries and the whole Go test
// suite." That number is real and means less than it sounds — it counts
// violations the walk could see.
//
// A NOTE ON WHERE IT IS MEASURED, because getting this wrong inverts the
// answer. Counting final members while RULES ARE FIRING finds plenty of
// references holding many (1186, max 52) — mid-planning groups legitimately
// hold alternatives, and that number says nothing about P5. What matters is
// the state at EXTRACTION, after the task stack drains and OptimizeGroup has
// pruned each group to its winner. This test walks final members only,
// descending through their quantifiers — which it does INSTEAD of walking the
// graph extraction walks, and that gap is defect 1 above.
//
// If this test goes red, P5 is blocked and any plan-holds-a-quantifier work
// must stop until the group that grew a second final is understood. If it
// stays green, nothing follows: it is green today with the invariant false.
func TestOneFinalPlanPerReference(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON t (a)
CREATE INDEX idx_b ON t (b)
CREATE INDEX idx_ab ON t (a, b)
CREATE TABLE u (id BIGINT, t_id BIGINT, v BIGINT, PRIMARY KEY (id))
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
