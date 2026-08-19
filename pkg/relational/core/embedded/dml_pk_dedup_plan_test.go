package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
// diff and grepping UnorderedPrimaryKeyDistinct lands here.
//
// TWO of the three claims below are CORPUS measurements and no test in this
// package can re-derive them; they are stated with the command that does, so a
// reader can check rather than trust. The third — the one that JUSTIFIES the
// access-path movement — is a property of the planner, not of the corpus, and
// it is asserted in TestDMLPlan_AccessPathAgreesWithTheEquivalentSelect below
// rather than argued here.
//
// TO RE-DERIVE THE TWO COUNTS: dump the explaindiff golden (350 files, 2516
// queries + 157 DML) on this branch and on its merge-base, pair stanzas by key,
// and diff. As measured when the change landed:
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
//	   enumeration:
//	     composite_secondary_index_prefix_pushdown #7 (UPDATE) and #9 (DELETE):
//	       IDX_REGION_PLAN_TIER[=,*,*] -> IDX_REGION_PLAN[=,*].
//	     in_list_pushdown #28 (UPDATE) and #31 (DELETE): gain an InJoin.
//
// Those four are all justified by ONE fact — a DML statement now reaches the
// same access path as the SELECT with the identical predicate, which is exactly
// what the single-winner pick was suppressing. That fact is the load-bearing
// one, it is a planner property rather than a corpus reading, and it is
// asserted directly on this file's own schema below. The corpus numbers say how
// MANY stanzas moved; the assertion says why moving was right.
//
// The classifier has NOT been re-run since: the yamsql work that followed
// changed only plan_contains/plan_not_contains claims — no query, rows or
// rowcount edits — so the planned corpus is unchanged and a re-run re-derives
// the same split. Re-running is the dump-and-pair procedure above; nothing
// automated checks it.
//
// A SECOND, LATER GOLDEN MOVE — 7 stanzas, measured on a full dump rather than
// argued. When PushDistinctThroughFetchRule was retargeted from the full-row
// distinct to the primary-key one (its Java matcher had always been
// unorderedPrimaryKeyDistinctPlan(fetchFromPartialRecordPlan(anyPlan()))), the
// DML dedup became pushable for the first time, and 7 UPDATE stanzas moved:
//
//	-  Update(X, …, UnorderedPrimaryKeyDistinct(IndexScan(I, […])))
//	+  Update(X, …, Fetch(UnorderedPrimaryKeyDistinct(IndexScan(I, […] COVERING))))
//
// All 7 are that one class and nothing else moved: diffing the dumps by line
// kind gives 7 `plan:` lines each way and 21-vs-14 `shape:` lines — the +7 is
// one Fetch node per stanza — with no `sql:` line and no stanza added or
// removed, so no SELECT plan and no query population changed. The rewrite
// cannot change rows either: a primary-key dedup is indifferent to whether it
// sits above or below a fetch, because every partial row for a record carries
// the same primary key. It is a straight improvement — dedup the covering
// entries, fetch only the survivors — which is the reason Java's rule exists.
//
// To re-derive: dump the golden before and after and diff it.
//
//	go run ./cmd/explain-differ dump -out /tmp/plan_shape.new
//	diff testdata/plan_shape.golden /tmp/plan_shape.new

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
		// Two positions are legal for the dedup, and both put it BEFORE the
		// mutation consumes rows, which is the property this test is about:
		//
		//	Update(T, …, UnorderedPrimaryKeyDistinct(accessPath))
		//	Update(T, …, Fetch(UnorderedPrimaryKeyDistinct(coveringAccessPath)))
		//
		// The second is the first with PushDistinctThroughFetchRule applied —
		// dedup the cheap covering entries, then fetch only the survivors,
		// instead of fetching every record and discarding duplicates after.
		// Java's rule pushes it the same way (its matcher is
		// unorderedPrimaryKeyDistinctPlan(fetchFromPartialRecordPlan(anyPlan()))),
		// so accepting only the unpushed spelling would pin a shape Java does
		// not commit to. What must NOT change is that a primary-key dedup is
		// there at all.
		if !strings.HasPrefix(shape, "Update(T, [1 transforms], UnorderedPrimaryKeyDistinct(") &&
			!strings.HasPrefix(shape, "Update(T, [1 transforms], Fetch(UnorderedPrimaryKeyDistinct(") {
			t.Errorf("%s\n  plan = %s\n  want the mutation to be fed by a primary-key dedup, either "+
				"directly or through the fetch it was pushed below; Java's ImplementUpdateRule "+
				"inserts one for EVERY update regardless of the access path's distinctness "+
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

// accessPathOf renders every scan a plan reaches, in traversal order, as
// "<source>[bounded]" or "<source>[unbounded]" — an index by name, the base
// table as "<primary>".
//
// BOUNDEDNESS IS RECORDED FOR THE PRIMARY SCAN TOO, not only for index scans.
// A primary-key equality plans as a base-table scan with a bound comparison
// range, so a renderer that labelled every RecordQueryScanPlan "full table scan"
// would report the same string for `WHERE id = 1` and for no predicate at all —
// collapsing a point lookup and a full table read into one answer, on the one
// access path where the difference is largest.
//
// It sees THROUGH covering wrappers (indexScanOf), because coveringness is a
// row-shaping decision — which columns the row is built from — and not part of
// which physical range is read. A DML statement writes whole records and can
// never use a covering path, so comparing it against a SELECT that can would
// otherwise report a difference that is not one.
func accessPathOf(p plans.RecordQueryPlan) []string {
	bound := func(ranges []*predicates.ComparisonRange) string {
		for _, cr := range ranges {
			if cr != nil && !cr.IsEmpty() {
				return "bounded"
			}
		}
		return "unbounded"
	}
	var out []string
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if idx, ok := indexScanOf(n); ok {
			out = append(out, idx.GetIndexName()+"["+bound(idx.GetScanComparisons())+"]")
			return true
		}
		if s, ok := n.(*plans.RecordQueryScanPlan); ok {
			out = append(out, "<primary>["+bound(s.GetScanComparisons())+"]")
		}
		return true
	})
	return out
}

// TestDMLPlan_AccessPathAgreesWithTheEquivalentSelect asserts the fact the four
// access-path stanzas in the header rest on, instead of leaving it as prose.
//
// The DML implementation rules used to pick a single access-path winner
// themselves; they now ENUMERATE and let cost choose, which is why four corpus
// stanzas moved. The justification for every one of them is the same and is not
// a corpus reading: an UPDATE or DELETE with a given predicate must reach the
// same access path as a SELECT with that predicate, because the two are asking
// the storage engine the identical question. A rule that picks its own winner
// can disagree with cost, and did.
//
// Coveringness is deliberately not compared — see accessPathOf. What is compared
// is the index and whether the predicate reached it as a scan bound, which is
// what "the same access path" means and what the four stanzas changed.
//
// If this fails, the DML rules have gone back to deciding an access path
// independently of the SELECT path. That is the defect the enumeration removed;
// do not reconcile it by changing the expectation.
func TestDMLPlan_AccessPathAgreesWithTheEquivalentSelect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pred string
	}{
		{"pk_equality", "id = 1"},
		{"single_column_range", "a > 1"},
		{"unindexed_column_range", "b > 1"},
		{"in_list", "a IN (10, 20)"},
		{"composite_prefix_equality", "a = 1"},
		{"composite_two_column_equality", "a = 1 AND b = 2"},
		{"composite_prefix_plus_in_list", "a = 1 AND b IN (10, 20)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The SELECT counterpart reads whole records, as the DML does. A
			// narrower projection could win a covering path the DML cannot use,
			// which would be a difference in the projection and not in the access
			// path.
			sel, err := PlanPhysicalForTest("SELECT * FROM t WHERE "+tc.pred, dedupDDL, nil)
			if err != nil {
				t.Fatalf("planning the SELECT counterpart: %v", err)
			}
			want := accessPathOf(sel)
			t.Logf("WHERE %s -> SELECT access path %v", tc.pred, want)
			if len(want) == 0 {
				t.Fatalf("the SELECT counterpart reached no scan at all:\n  %s\n"+
					"there is then no access path to agree with and this case is vacuous",
					sel.Explain())
			}

			for _, dml := range []string{
				"UPDATE t SET v = v + 1 WHERE " + tc.pred,
				"DELETE FROM t WHERE " + tc.pred,
			} {
				p, err := PlanPhysicalDMLForTest(dml, dedupDDL, nil)
				if err != nil {
					t.Fatalf("planning %q: %v", dml, err)
				}
				got := accessPathOf(p)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s\n  DML access path:    %v\n    %s\n"+
						"  SELECT access path: %v\n    %s\n"+
						"The two ask storage the same question and must reach the same "+
						"answer. A DML rule that picks its own single winner instead of "+
						"enumerating and letting cost choose is how they diverge.",
						dml, got, p.Explain(), want, sel.Explain())
				}
			}
		})
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
