package sqldriver_test

// Shared assertions for plans that may contain a COVERING index scan.
//
// # The rule: interface dispatch is safe, concrete type assertion is blind
//
// RFC-220 made coveringness a plan TYPE that HOLDS its index plan as a field
// rather than a flag stamped on the scan. By criterion C1 the held plan is not
// a child: `GetChildren()` returns nil, and `plans.Walk` recurses through
// `GetChildren()`. So:
//
//	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
//	    if idx, ok := n.(*plans.RecordQueryIndexPlan); ok { ... }   // BLIND
//	})
//
// never fires for a scan sitting inside a covering wrapper. It does not error
// and it does not skip — it silently observes nothing, so a reachability guard
// written this way reports "the query did not use the index" about a plan that
// visibly scans it. Two guards in this package failed exactly that way and both
// were false negatives, not planner regressions.
//
// Dispatching on an INTERFACE the covering plan implements by delegation
// (GetIndexName, GetScanComparisons, IsReverse, DistinctProofStamped, …) is
// safe, because the wrapper forwards to the inner. Asserting the concrete
// *RecordQueryIndexPlan is not. When a walker needs the concrete scan, it must
// see through the wrapper — that is what indexScanOfNode is for.
//
// # The other retired proxy: `Fetch(`
//
// A bare `IndexScan(…)` IS a fetching scan — it resolves every entry to its
// record by primary key, matching Java's executeIndexScan. A separate `Fetch(`
// node renders only when one survives above a COVERING scan, so `Fetch(`
// disappearing does not mean the base record stopped being read; usually it
// means two nodes collapsed into one. Asserting on `Fetch(` therefore tests the
// rendering, not the behaviour. The property that actually matters is "this
// scan does not answer from the index entry alone", i.e. it carries no COVERING
// marker on its own label.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// indexScanOfNode returns the concrete index scan a node denotes, seeing
// through a covering wrapper. A thin alias for plans.IndexPlanOf, kept only so
// this package's call sites read unchanged.
//
// It was a hand-written copy of the identical helper in package embedded, in a
// different Go package so neither could import the other. The shared symbol is
// exported now; the copies are not, because a structural guard maintained in
// two places stops being one guard the first time only one copy is updated.
//
// Use it for questions about the SCAN — index name, direction, scan ranges,
// uniqueness. Do NOT use it to ask whether a plan answers from the index entry:
// that question is about the wrapper, and unwrapping erases the answer.
func indexScanOfNode(node plans.RecordQueryPlan) (*plans.RecordQueryIndexPlan, bool) {
	return plans.IndexPlanOf(node)
}

// walkIndexScans visits every index scan in the plan, including those held
// inside covering wrappers. It exists so a reachability guard cannot be written
// with the blind concrete assertion described above.
func walkIndexScans(p plans.RecordQueryPlan, visit func(*plans.RecordQueryIndexPlan)) {
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if idx, ok := indexScanOfNode(n); ok {
			visit(idx)
		}
		return true
	})
}

// planBindsBoundedScanOn reports whether the plan scans indexName with at least
// one non-empty comparison range — the property "the predicate reached the
// access path" as distinct from "a Fetch node is rendered".
func planBindsBoundedScanOn(p plans.RecordQueryPlan, indexName string) (found, bounded bool) {
	walkIndexScans(p, func(idx *plans.RecordQueryIndexPlan) {
		if idx.GetIndexName() != indexName {
			return
		}
		found = true
		for _, cr := range idx.GetScanComparisons() {
			if cr != nil && !cr.IsEmpty() {
				bounded = true
			}
		}
	})
	return found, bounded
}

// planUsesIndex reports whether the plan scans indexName at all, seeing through
// covering wrappers.
func planUsesIndex(p plans.RecordQueryPlan, indexName string) bool {
	used := false
	walkIndexScans(p, func(idx *plans.RecordQueryIndexPlan) {
		if idx.GetIndexName() == indexName {
			used = true
		}
	})
	return used
}

// assertScanReadsBaseRecords asserts that the scan introduced by scanPrefix
// reads BASE RECORDS rather than answering from the index entry.
//
// Checked on the scan's own label, not the whole plan, so a covering scan
// elsewhere in the tree cannot mask a regression here.
func assertScanReadsBaseRecords(t *testing.T, plan, scanPrefix string) {
	t.Helper()
	label, ok := scanLabel(plan, scanPrefix)
	if !ok {
		t.Errorf("plan does not contain the scan %q:\n  %s", scanPrefix, plan)
		return
	}
	if strings.Contains(label, "COVERING") {
		t.Errorf("the scan %q answers from the INDEX ENTRY (covering), so the base record "+
			"is never read — any column outside the entry would read NULL:\n  %s\n  scan: %s",
			scanPrefix, plan, label)
	}
}

// assertScanAnswersFromIndexEntry is the mirror: the scan must be COVERING.
func assertScanAnswersFromIndexEntry(t *testing.T, plan, scanPrefix string) {
	t.Helper()
	label, ok := scanLabel(plan, scanPrefix)
	if !ok {
		t.Errorf("plan does not contain the scan %q:\n  %s", scanPrefix, plan)
		return
	}
	if !strings.Contains(label, "COVERING") {
		t.Errorf("the scan %q reads base records, but every projected column is in the "+
			"index entry so it should answer from the entry alone:\n  %s\n  scan: %s",
			scanPrefix, plan, label)
	}
}

// scanLabel extracts the single scan label beginning at scanPrefix, up to its
// closing paren, so an assertion applies to that scan and not the whole plan.
func scanLabel(plan, scanPrefix string) (string, bool) {
	i := strings.Index(plan, scanPrefix)
	if i < 0 {
		return "", false
	}
	label := plan[i:]
	if j := strings.Index(label, ")"); j >= 0 {
		label = label[:j+1]
	}
	return label, true
}
