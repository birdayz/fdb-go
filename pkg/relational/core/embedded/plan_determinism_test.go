package embedded

// RFC-164 NONDETERMINISM: ranging Go maps in the planner made equal-cost ties
// resolve by Go's randomised map-iteration order → the SAME query produced
// distinct plans across runs. Java uses insertion-ordered LinkedHashMultimap and
// a stable index order. This pins the two DOCUMENTED map-iteration sources, now
// fixed:
//   1. Reference.partialMatchMap was ranged directly (GetAllPartialMatches /
//      GetPartialMatchCandidates) → now iterated in first-insertion order
//      (mirrors LinkedHashMultimap).
//   2. metadataPlanContext.GetMatchCandidates ranged RecordMetaData.GetAllIndexes
//      (a Go map) → now iterated in index-name-sorted order.
//
// The test plans one query many times in a single process (Go randomises map
// order per range, so repeated in-process plannings expose order-dependence) and
// requires ONE distinct plan.
//
// RFC-167 Phase 1a (inner-aware shell hash) additionally fixes the multi-equality
// tie over several single-column indexes (`WHERE a=5 AND b=7 AND c=9` with
// idx_a/idx_b/idx_c). Those three competing plans are SAME-TYPE nil-inner shells
// (Fetch(PredicatesFilter(IndexScan))) whose embedded plan has GetChildren()==[],
// so the bare concretePlanHash criterion-#17 tie-break was blind to the buried
// index and the comparator returned a tie → selection fell to member-iteration
// order. costExprHash now resolves the shell's inner STRUCTURALLY through the
// quantifier graph (exprConcreteHash), surfacing the index identity so the
// tie-break is a true total order and the cost-min is deterministic. Pure
// tie-resolution — no plan-shape re-ranking (the cheapest member, a single-index
// shell, still wins, now deterministically).
//
// DECOUPLED FOLLOW-ON (RFC-167 Phase 1b + 4): making shells stop winning over real
// plans (the guard generalization) re-ranks the all-equality case to a 3-way
// Intersection, which requires the primary-key ordering-gate (Phase 4) to be
// correct — and that gate must use the full ordering machinery (a crude
// "all-columns-equality-bound" gate breaks vector/partition-inequality cases).
// Set-op / reverse-scan ties (which the hash fix already covers via the same
// exprConcreteHash) get explicit nets there. Those are NOT in this change.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const multiEqSchema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_a ON T(a)
CREATE INDEX idx_b ON T(b)
CREATE INDEX idx_c ON T(c)`

const multiEqQuery = "SELECT id FROM t WHERE a = 5 AND b = 7 AND c = 9"

// TestPlanDeterminism_MultiEqualityShellTie_CrossProcess pins determinism ACROSS
// PROCESSES (RFC-167 §7 mandatory harness), not just within one. Go re-seeds map
// iteration per process, so the in-process loop above catches map-range bugs; but a
// slice whose order is pointer/seed-stable WITHIN a process yet flips ACROSS
// processes (the dimension Phase 0's bug lived on, and which firstPhysicalChild's
// AllMembers()-order resolution rests on) needs a fresh process per sample. Each
// child process plans once and prints the plan; the parent requires all identical.
func TestPlanDeterminism_MultiEqualityShellTie_CrossProcess(t *testing.T) {
	t.Parallel()
	const marker = "XPROCPLAN:"

	// Child mode: plan once in this fresh process, print the plan, return. A
	// planning error exits NON-ZERO (not a marker) so a deterministic error can't
	// false-green the parent's "all samples identical" check.
	if os.Getenv("FDB_DET_CHILD") == "1" {
		plan, err := PlanQueryForTest(multiEqQuery, multiEqSchema, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "child planning error: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("%s%s\n", marker, plan)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cross-process harness unavailable (os.Executable: %v)", err)
	}
	const procs = 5
	seen := make(map[string]int)
	for i := 0; i < procs; i++ {
		cmd := exec.Command(exe, "-test.run", "^TestPlanDeterminism_MultiEqualityShellTie_CrossProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "FDB_DET_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			// Distinguish "child RAN and FAILED" (planning error / panic / assertion →
			// exit non-zero → *exec.ExitError) from "couldn't START the binary" (restricted
			// sandbox). The former is a real regression and must FAIL the parent; only the
			// latter skips. Skipping a child failure would let CI miss it.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				t.Fatalf("cross-process subprocess %d ran but failed (%v):\n%s", i, err, out)
			}
			t.Skipf("cross-process harness could not start (subprocess %d: %v)", i, err)
		}
		plan := ""
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, marker) {
				plan = strings.TrimPrefix(line, marker)
				break
			}
		}
		if plan == "" {
			t.Fatalf("subprocess %d produced no plan marker:\n%s", i, out)
		}
		seen[plan]++
	}
	if len(seen) != 1 {
		t.Errorf("plan is NONDETERMINISTIC across %d processes — %d distinct: %v", procs, len(seen), seen)
	}
}

func TestPlanDeterminism_EqualCostIndexTie(t *testing.T) {
	t.Parallel()
	// Two indexes on `a`: a `WHERE a = 5` scan costs the same on either, so which
	// is chosen is a pure tie — the partial-match / candidate map-order leak.
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))
CREATE INDEX idx1 ON T(a)
CREATE INDEX idx2 ON T(a)`

	assertSinglePlan(t, "SELECT id FROM t WHERE a = 5", schema, 200)
}

func TestPlanDeterminism_MultiEqualityShellTie(t *testing.T) {
	t.Parallel()
	// Three single-column indexes, three equality predicates: the competing plans
	// are same-type nil-inner shells whose buried index the #17 tie-break couldn't
	// see — the canonical RFC-167 multi-equality leak. The inner-aware shell hash
	// (Phase 1a) makes it deterministic. (Cross-process pin: see the _CrossProcess
	// variant above.)
	assertSinglePlan(t, multiEqQuery, multiEqSchema, 200)
}

func assertSinglePlan(t *testing.T, sql, schema string, runs int) {
	t.Helper()
	seen := make(map[string]int)
	var first string
	for i := 0; i < runs; i++ {
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("run %d: plan: %v", i, err)
		}
		if first == "" {
			first = plan
		}
		seen[plan]++
	}
	if len(seen) != 1 {
		t.Errorf("plan is NONDETERMINISTIC over %d runs — got %d distinct plans:", runs, len(seen))
		for p, n := range seen {
			t.Errorf("  (%d×) %s", n, p)
		}
	}
}
