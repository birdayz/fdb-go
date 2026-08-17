package embedded

import (
	"strings"
	"testing"
)

// TestExtractionIsUnambiguous pins RFC-224's actual extraction precondition.
// Go does not require Java's one-final-member mechanism: OptimizeGroup retains
// one physical member per required property, while extraction resolves a live
// Reference through its stamped winner or a cheapest compatible physical
// fallback. Several physical finals can therefore be both intentional and
// necessary.
//
// The invariant is instead totality and coherence along the path extraction
// really selects. Every reached Reference must resolve to a physical member;
// positional ordinal-layout requirements must select within their compatible
// subset; a stamped winner must still be final; and every retained alternative
// must be the winner for a legitimate requested/interesting physical property.
// The explicit reach and dead-end assertions keep the gate from passing by
// stopping at an empty final set, which made the retired one-final walk
// vacuous.
func TestExtractionIsUnambiguous(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON t (a)
CREATE INDEX idx_b ON t (b)
CREATE INDEX idx_ab ON t (a, b)
CREATE TABLE u (id BIGINT, t_id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_u_t ON u (t_id)`

	// Spread across the plan families that retain live child References.
	for _, tc := range []struct {
		name string
		sql  string
	}{
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
			report, err := planAndVerifyExtraction(tc.sql, schema)
			if err != nil {
				// A query that cannot plan says nothing about the invariant; fail
				// rather than silently reducing the exercised shape population.
				t.Fatalf("plan: %v", err)
			}
			if report.VisitedReferences == 0 {
				t.Fatal("extraction verifier visited no References")
			}
			if report.DeadEnds != 0 {
				t.Errorf("extraction verifier reached %d dead end(s)", report.DeadEnds)
			}
			if len(report.Violations) > 0 {
				t.Errorf("RFC-224 extraction coherence violated (%d issue(s)):\n  %s",
					len(report.Violations), strings.Join(report.Violations, "\n  "))
			}
			t.Logf("visited=%d deadEnds=%d multiFinal=%d",
				report.VisitedReferences, report.DeadEnds, report.MultiFinalReferences)
		})
	}
}
