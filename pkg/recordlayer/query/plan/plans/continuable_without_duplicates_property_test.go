package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// scanFixture builds a trivial leaf plan to sit under the operators below.
func cwdScanFixture() RecordQueryPlan {
	return NewRecordQueryScanPlan([]string{"Orders"}, values.UnknownType, false)
}

// TestContinuableWithoutDuplicates_PerPlanType pins the property's verdict for
// each plan type the visitor names explicitly, plus the default arm.
//
// THE TWO DISTINCT PLANS ARE THE POINT. Java's property returns FALSE for both
// (ContinuableWithoutDuplicatesProperty.java: visitUnorderedPrimaryKeyDistinctPlan
// and visitUnorderedDistinctPlan) because Java mints their dedup set fresh per
// execution and re-admits a duplicate spanning a resume. Go carries the set
// across pages by reference through the ExecutionScratch, so the premise is
// gone and the verdict inverts. If either of these ever reads false, Java's
// CONCLUSION has been imported without its premise — and GROUP BY over a
// DISTINCT input will silently stop planning, because Go has no hash
// aggregation to fall back to.
func TestContinuableWithoutDuplicates_PerPlanType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		plan RecordQueryPlan
		want bool
		why  string
	}{
		{
			name: "unordered primary-key distinct",
			plan: NewRecordQueryUnorderedPrimaryKeyDistinctPlan(cwdScanFixture()),
			want: true,
			why: "Java returns FALSE here (per-execution HashSet lost across the continuation). " +
				"Go carries the seen-set across pages by reference through the ExecutionScratch, " +
				"so the duplicate is not re-admitted and the plan is admissible",
		},
		{
			name: "unordered by-row distinct",
			plan: NewRecordQueryDistinctPlan(cwdScanFixture()),
			want: true,
			why: "Java's RecordQueryUnorderedDistinctPlan counterpart, FALSE in Java for the same " +
				"reason. Go is resume-clean on BOTH executors — the streaming form rides its last " +
				"key in the continuation, the hash form its whole set in the ExecutionScratch",
		},
		{
			name: "bare scan (default arm)",
			plan: cwdScanFixture(),
			want: true,
			why:  "a leaf that resumes positionally",
		},
	} {
		if got := EvaluateContinuableWithoutDuplicates(tc.plan); got != tc.want {
			t.Errorf("%s: EvaluateContinuableWithoutDuplicates = %v, want %v — %s",
				tc.name, got, tc.want, tc.why)
		}
	}
}

// TestContinuableWithoutDuplicates_NilIsSafe pins the nil guard: Visit(nil) must
// not panic, because the rule's admission runs over members that can be absent.
func TestContinuableWithoutDuplicates_NilIsSafe(t *testing.T) {
	t.Parallel()
	if !EvaluateContinuableWithoutDuplicates(nil) {
		t.Fatal("EvaluateContinuableWithoutDuplicates(nil) = false, want true — a missing plan " +
			"imposes no constraint, and the traversal must not treat absence as unsafe")
	}
}

// TestContinuableWithoutDuplicates_FoldsChildren pins Java's visitDefault →
// fromChildren composition: the verdict is over the whole TREE, not the root.
// This is what makes the empty false set safe to extend — the day a plan is
// added to the false set, every operator stacked above it inherits the verdict
// without anyone editing those operators.
//
// It is pinned through the visitor's own fold rather than through a false plan,
// because no plan is false today — see TestContinuableWithoutDuplicates_FalseSetIsEmpty.
func TestContinuableWithoutDuplicates_FoldsChildren(t *testing.T) {
	t.Parallel()

	v := ContinuableWithoutDuplicatesVisitor{}

	if !v.fromChildren(nil) {
		t.Fatal("fromChildren(nil) = false, want true — a childless plan is decided by its own arm")
	}
	if !v.fromChildren([]RecordQueryPlan{cwdScanFixture(), cwdScanFixture()}) {
		t.Fatal("fromChildren over two safe children = false, want true")
	}

	// A stack of operators over a safe leaf stays safe, and the fold really does
	// descend: the distinct wrapper is the node whose Java twin would have
	// stopped the walk with false.
	stacked := NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
		NewRecordQueryDistinctPlan(cwdScanFixture()))
	if !EvaluateContinuableWithoutDuplicates(stacked) {
		t.Fatal("a distinct over a distinct over a scan is not continuable-without-duplicates — " +
			"under Java's false set this shape is doubly false, and reading false here means " +
			"Go imported that conclusion")
	}
	if n := Size(stacked); n != 3 {
		t.Fatalf("the stacked fixture has %d nodes, want 3 — if the tree collapsed, the fold "+
			"assertion above stopped proving that the walk descends", n)
	}
}

// TestContinuableWithoutDuplicates_FalseSetIsEmpty is a NEGATIVE RESULT, pinned
// deliberately rather than left as prose.
//
// The claim: no plan type in this package is currently continuation-unsafe, so
// the streaming-aggregation rule's admission filter is VACUOUSLY TRUE. That is
// load-bearing in an unobvious direction — it is exactly why removing the
// filter cannot be caught by any behavioural test today, and why the filter is
// justified as correct-by-construction rather than by a red test.
//
// WHAT RE-ARMS THIS: a cursor whose resume can re-emit an already-emitted row —
// one that restarts an inner instead of resuming it, or holds emission state in
// memory without serializing it or parking it in the ExecutionScratch. When
// that lands, this test fails, and the failure is the reminder that adding the
// plan to the false set is NOT enough on its own: Go has no hash aggregation,
// so GROUP BY over the declined shape would fail to plan rather than fall back.
func TestContinuableWithoutDuplicates_FalseSetIsEmpty(t *testing.T) {
	t.Parallel()

	scan := cwdScanFixture()
	for _, p := range []RecordQueryPlan{
		scan,
		NewRecordQueryDistinctPlan(scan),
		NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan),
	} {
		if !EvaluateContinuableWithoutDuplicates(p) {
			t.Fatalf("%T is continuation-UNSAFE, so the false set is no longer empty. Adding it to "+
				"ContinuableWithoutDuplicatesVisitor.selfContinuableWithoutDuplicates is only half "+
				"the work: streaming aggregation is Go's ONLY aggregation strategy and the "+
				"in-memory-sort path buffers a re-emitting inner rather than laundering it, so "+
				"GROUP BY over this shape will FAIL TO PLAN until a hash aggregation exists", p)
		}
	}
}
