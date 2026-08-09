package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// End-to-end plan shape for the primary-key dedup the DML implementation rules
// interpose, through the real SQL → Cascades → physical path rather than the
// rule harness. The rule-level arms live in
// pkg/recordlayer/query/plan/cascades/dml_pk_dedup_test.go; what this file adds
// is that the shape survives actual planning, where cost chooses among the
// access-path alternatives the rules now enumerate.
//
// WHY THE plan_shape GOLDEN MOVED, class by class. Anyone reading the golden
// diff and grepping UnorderedPrimaryKeyDistinct lands here. Measured over the
// explaindiff corpus (350 files, 2516 queries + 157 DML) by dumping the golden
// before and after and pairing stanzas by key:
//
//	99 DML stanzas changed, 0 non-DML plan lines.
//
//	95 WRAPPER-ONLY — the stanza is byte-identical once the
//	   UnorderedPrimaryKeyDistinct(...) wrapper is stripped. This is the port
//	   itself: Java inserts the dedup in every UPDATE (ImplementUpdateRule.java:
//	   79-80) and in every DELETE whose access path does not already prove
//	   DistinctRecordsProperty (ImplementDeleteRule.java:79-82). Of these, 7 are
//	   DELETEs that GAINED a dedup, all over a FlatMap whose distinctness the
//	   property cannot establish; every other DELETE correctly kept none.
//
//	 4 ACCESS-PATH CHANGES, from replacing the rules' single-winner pick with
//	   enumeration. Each is justified by the SAME fact — the DML now agrees with
//	   the SELECT that has the identical predicate, which is precisely what the
//	   pick was suppressing:
//	     composite_secondary_index_prefix_pushdown #7 (UPDATE) and #9 (DELETE):
//	       IDX_REGION_PLAN_TIER[=,*,*] -> IDX_REGION_PLAN[=,*], the same index
//	       `SELECT id FROM rp WHERE region = 'eu'` already picks. It also moves
//	       the UPDATE off the one index whose key contains the column it writes.
//	     in_list_pushdown #28 (UPDATE) and #31 (DELETE): gain an InJoin, the
//	       shape `SELECT a, b, c FROM kvw WHERE a = 1 AND b IN (10, 20)` already
//	       plans. Verified by planning that SELECT directly, not inferred.
//
// The classifier was NOT re-run for the final tree: the yamsql work that
// followed changed only plan_contains/plan_not_contains claims — zero query,
// rows or rowcount edits — so the planned corpus is identical and a re-run
// re-derives the same split. Confirmed by dumping the golden again afterwards:
// zero delta.

func planDMLShape(t *testing.T, sql, ddl string) string {
	t.Helper()
	p, err := PlanPhysicalDMLForTest(sql, ddl, nil)
	if err != nil {
		t.Fatalf("planning %q: %v", sql, err)
	}
	return p.Explain()
}

const dedupDDL = `CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX t_a ON t (a)
CREATE INDEX t_ab ON t (a, b)`

// TestDMLPlan_UpdateAlwaysCarriesPrimaryKeyDedup pins Java's unconditional
// arm (ImplementUpdateRule.java:79-80) at the SQL level, across access paths.
// Every one of these inners reports DistinctRecords, which is exactly why the
// DELETE twin below has none of them wrapped — the asymmetry is Java's and the
// two tests only mean something together.
func TestDMLPlan_UpdateAlwaysCarriesPrimaryKeyDedup(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"UPDATE t SET v = v + 1 WHERE id = 1",
		"UPDATE t SET v = v + 1 WHERE a > 1",
		"UPDATE t SET v = v + 1 WHERE b > 1",
		"UPDATE t SET v = v + 1 WHERE a IN (10, 20)",
	} {
		shape := planDMLShape(t, sql, dedupDDL)
		if !strings.HasPrefix(shape, "Update(T, [1 transforms], UnorderedPrimaryKeyDistinct(") {
			t.Errorf("%s\n  plan = %s\n  want the mutation to sit directly on a "+
				"primary-key dedup; Java's ImplementUpdateRule inserts one for "+
				"EVERY update regardless of the access path's distinctness "+
				"(ImplementUpdateRule.java:79-80)", sql, shape)
		}
	}
}

// TestDMLPlan_DeleteElidesDedupOverDistinctAccessPaths is the other half. A
// DELETE consults DistinctRecordsProperty (ImplementDeleteRule.java:79-82), and
// every access path the SQL surface builds for these statements is distinct, so
// none may carry a dedup. Unifying the two rules on the update's unconditional
// form would turn every one of these into a wasted operator, and unifying them
// on the delete's conditional form would strip the update's guarantee.
func TestDMLPlan_DeleteElidesDedupOverDistinctAccessPaths(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"DELETE FROM t WHERE id = 1",
		"DELETE FROM t WHERE a > 1",
		"DELETE FROM t WHERE b > 1",
		"DELETE FROM t WHERE a IN (10, 20)",
		"DELETE FROM t",
	} {
		shape := planDMLShape(t, sql, dedupDDL)
		if strings.Contains(shape, "UnorderedPrimaryKeyDistinct(") {
			t.Errorf("%s\n  plan = %s\n  want NO primary-key dedup: this access "+
				"path proves DistinctRecords, which is the short-circuit "+
				"ImplementDeleteRule.java:79-82 applies", sql, shape)
		}
	}
}

// TestDMLPlan_InListCarriesNoDuplicateValue is a NEGATIVE result, pinned
// because a decision rests on it.
//
// An IN-join runs its inner scan once per value in its list, so a list holding
// the same value twice would present one stored record twice — the one shape on
// today's SQL surface that could reach a DML mutation with a duplicate primary
// key. It does not, because a repeated literal is collapsed before the InJoin is
// built. That is what makes the missing dedup a LATENT divergence on this branch
// rather than a shipped double-mutation bug, and the classification is only as
// good as this fact.
//
// Nothing else pins it. If a future change lets a repeated literal survive into
// the binding list, the double-mutation path is re-armed and the primary-key
// dedup the DML rules now insert becomes load-bearing for row correctness rather
// than for Java parity — so this test failing is a signal to go re-read the
// dedup, not to update the expectation.
func TestDMLPlan_InListCarriesNoDuplicateValue(t *testing.T) {
	t.Parallel()
	p, err := PlanPhysicalDMLForTest(
		"UPDATE t SET v = v + 1 WHERE a IN (10, 10, 20)", dedupDDL, nil)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}

	var found []*plans.RecordQueryInJoinPlan
	var walk func(plans.RecordQueryPlan)
	walk = func(n plans.RecordQueryPlan) {
		if n == nil {
			return
		}
		if ij, ok := n.(*plans.RecordQueryInJoinPlan); ok {
			found = append(found, ij)
		}
		for _, c := range n.GetChildren() {
			walk(c)
		}
	}
	walk(p)

	if len(found) == 0 {
		t.Fatalf("no IN-join in %s — the duplicate-value question is not being "+
			"asked of anything, so a green here proves nothing about it",
			p.Explain())
	}
	for _, ij := range found {
		vals := ij.GetInValues()
		if len(vals) == 0 {
			t.Fatalf("IN-join carries no values; the duplicate check is vacuous")
		}
		seen := map[any]bool{}
		for _, v := range vals {
			if seen[v] {
				t.Fatalf("IN-join value list %v repeats %v. The access path now "+
					"presents one stored record on two iterations, so an UPDATE "+
					"applying a relative transform (v = v + 1) would apply it "+
					"twice. The primary-key dedup in ImplementUpdateRule is what "+
					"stops that — verify it is still there and still above this "+
					"IN-join before changing this test", vals, v)
			}
			seen[v] = true
		}
	}
}
