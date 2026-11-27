package plans

import (
	"testing"
)

// scanFixture builds a trivial leaf plan to sit under the operators below.
func cwdScanFixture(t testing.TB) RecordQueryPlan {
	return mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Orders"}, exactTestRecordType(), false)
	})
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
			plan: mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
				return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(cwdScanFixture(t))
			}),
			want: true,
			why: "Java returns FALSE here (per-execution HashSet lost across the continuation). " +
				"Go carries the seen-set across pages by reference through the ExecutionScratch, " +
				"so the duplicate is not re-admitted and the plan is admissible",
		},
		{
			name: "unordered by-row distinct",
			plan: mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
				return NewRecordQueryDistinctPlan(cwdScanFixture(t))
			}),
			want: true,
			why: "Java's RecordQueryUnorderedDistinctPlan counterpart, FALSE in Java for the same " +
				"reason. Go is resume-clean on BOTH executors — the streaming form rides its last " +
				"key in the continuation, the hash form its whole set in the ExecutionScratch",
		},
		{
			name: "bare scan (default arm)",
			plan: cwdScanFixture(t),
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

// TestContinuableWithoutDuplicates_FoldsChildren pins Java's fromChildren
// helper, and is deliberately explicit about the ONE thing it cannot pin.
//
// WHAT IT PINS: fromChildren itself — the all-children conjunction, including
// the empty case. Mutating the visitor's default arm to false reds this.
//
// WHAT IT DOES NOT PIN, measured rather than assumed: that Visit actually CALLS
// fromChildren. Replacing `return v.fromChildren(p.GetChildren())` in Visit with
// a bare `return true` leaves this whole file GREEN. That is not a hole in the
// test — it is the empty false set again. With no node anywhere answering false,
// a fold and a no-op are behaviourally identical, so no test over real plans can
// separate them. The day the false set gains a member, the fold becomes
// observable and this is the test that should grow the case; until then, saying
// so is more useful than an assertion that would pass either way.
//
// The composition still matters, because it is what makes the empty set safe to
// extend: adding one plan to the false set must make every operator stacked
// above it unsafe without anyone editing those operators.
func TestContinuableWithoutDuplicates_FoldsChildren(t *testing.T) {
	t.Parallel()

	v := ContinuableWithoutDuplicatesVisitor{}

	if !v.fromChildren(nil) {
		t.Fatal("fromChildren(nil) = false, want true — a childless plan is decided by its own arm")
	}
	if !v.fromChildren([]RecordQueryPlan{cwdScanFixture(t), cwdScanFixture(t)}) {
		t.Fatal("fromChildren over two safe children = false, want true")
	}

	// A stack of operators over a safe leaf stays safe. Under Java's false set
	// this exact shape is doubly false, so reading false here means Go imported
	// that conclusion.
	stacked := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
			return NewRecordQueryDistinctPlan(cwdScanFixture(t))
		}))
	})
	if !EvaluateContinuableWithoutDuplicates(stacked) {
		t.Fatal("a distinct over a distinct over a scan is not continuable-without-duplicates — " +
			"under Java's false set this shape is doubly false, and reading false here means " +
			"Go imported that conclusion")
	}
	if n := Size(stacked); n != 3 {
		t.Fatalf("the stacked fixture has %d nodes, want 3 — the shape under test collapsed", n)
	}
}

// TestContinuableWithoutDuplicates_FalseSetIsEmpty is a NEGATIVE RESULT, pinned
// deliberately rather than left as prose.
//
// The claim: no plan type in this package is currently continuation-unsafe, so
// the streaming-aggregation rule's admission filter is VACUOUSLY TRUE. That is
// load-bearing in an unobvious direction, and both consequences were MEASURED
// rather than reasoned about:
//
//   - Removing the rule's admission filter entirely leaves the suite GREEN.
//   - Removing the child fold from ContinuableWithoutDuplicatesVisitor.Visit
//     also leaves it GREEN.
//
// Neither is a gap in coverage; both are the emptiness showing through. With no
// node answering false, a filter and a pass-through are the same function. This
// is why the filter is justified as correct-by-construction rather than by a red
// test — and why the mutation that DOES bite is inverting the two DISTINCT arms,
// which is the ruling this property exists to record.
//
// WHAT RE-ARMS THIS: a cursor whose resume can re-emit an already-emitted row —
// one that restarts an inner instead of resuming it, or holds emission state in
// memory without serializing it or parking it in the ExecutionScratch. When
// that lands, this test fails, and the failure is the reminder that adding the
// plan to the false set is NOT enough on its own: Go has no hash aggregation,
// so GROUP BY over the declined shape would fail to plan rather than fall back.
func TestContinuableWithoutDuplicates_FalseSetIsEmpty(t *testing.T) {
	t.Parallel()

	scan := cwdScanFixture(t)
	for _, p := range []RecordQueryPlan{
		scan, mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
			return NewRecordQueryDistinctPlan(scan)
		}), mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
			return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan)
		}),
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
