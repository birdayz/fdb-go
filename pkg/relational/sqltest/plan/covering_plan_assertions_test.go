package sqltest

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

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// assertScanAnswersFromIndexEntry is the mirror: the scan must be COVERING.
func assertScanAnswersFromIndexEntry(t *testing.T, plan, scanPrefix string) {
	t.Helper()
	label, ok := testkit.ScanLabel(plan, scanPrefix)
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
