package factorycorpus_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

// A corpus scenario can fail for two reasons that mean opposite things, and
// the suite reported them with the same sentence.
//
// "Committed expectation no longer holds" is a claim about what the engine
// ANSWERED. It is the corpus's whole product: an oracle froze those rows, and
// a change in them is a behaviour change worth a triager's day. A scenario
// whose query never returned has answered nothing, so the sentence is simply
// untrue of it — and it is the expensive kind of untrue, because it sends
// someone looking for a planner regression that does not exist.
//
// This distinction is not theoretical. At four-way parallelism under -race the
// full corpus loses scenarios to wall-clock budgets (measured: 37 deadline
// expiries and 5 execution-limit errors across three runs) while losing none
// at 24-way on the same commit. The engine did not change between those runs;
// the core count did.
//
// Both remain FAILURES. The corpus must not learn to shrug off a scenario that
// cannot finish.

func scenario() *factorycorpus.Scenario {
	return &factorycorpus.Scenario{
		Path: "testdata/join3_inner__and_cmp__none.yamsql",
		Header: factorycorpus.Header{
			Name: "fc_0000000231_q5_p0", Seed: 231, Date: "2026-07-31",
			Blessing: factorycorpus.BlessingMetamorphic,
		},
		Doc: &yamsql.Scenario{Tests: []yamsql.Test{{}}},
	}
}

func failWith(err error) javacorpus.FileResult {
	return javacorpus.FileResult{Status: javacorpus.StatusFail, Err: err, QueriesRun: 1}
}

// TestCheckResultNamesATimeoutAsATimeout pins the deadline direction: the
// per-scenario ScenarioTimeout expiring must not be reported as a changed
// answer.
func TestCheckResultNamesATimeoutAsATimeout(t *testing.T) {
	t.Parallel()

	// The shape the runner actually produces: the deadline is wrapped through
	// several layers of %w before it reaches CheckResult.
	err := fmt.Errorf("test_block line 2512: %q: %w",
		"SELECT l.id FROM t_rd AS l JOIN t_rd AS m ON l.b = m.b", context.DeadlineExceeded)

	got := factorycorpus.CheckResult(scenario(), failWith(err))
	if got == nil {
		t.Fatal("a scenario that never finished must still FAIL — a corpus that shrugs one off is " +
			"worse than one that misnames it")
	}
	if strings.Contains(got.Error(), "committed expectation no longer holds") {
		t.Errorf("a deadline expiry was reported as a changed answer. The engine answered nothing, "+
			"so the claim is false, and it sends a triager hunting a regression that is not "+
			"there. Got:\n%v", got)
	}
	if !strings.Contains(got.Error(), "did not COMPLETE within its budget") {
		t.Errorf("failure must name the budget it ran out of; got:\n%v", got)
	}
}

// TestCheckResultNamesAnExecutionLimitAsATimeout pins the second, independent
// direction: the per-page execution-time budget escaping as a 54F01 query
// error is also a clock outcome, not a disagreement about rows.
func TestCheckResultNamesAnExecutionLimitAsATimeout(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("test_block line 691: %q: %w", "SELECT id FROM t_rd",
		api.NewError(api.ErrCodeExecutionLimitReached,
			"leaf cursor scan limit exceeded: scan limit reached: time limit exceeded"))

	got := factorycorpus.CheckResult(scenario(), failWith(err))
	if got == nil {
		t.Fatal("an execution-limit failure must still FAIL")
	}
	if strings.Contains(got.Error(), "committed expectation no longer holds") {
		t.Errorf("a 54F01 execution-time limit was reported as a changed answer; got:\n%v", got)
	}
	if !strings.Contains(got.Error(), string(api.ErrCodeExecutionLimitReached)) {
		t.Errorf("failure must name the 54F01 limit that ended the query; got:\n%v", got)
	}
}

// TestCheckResultStillReportsARealMismatch is the direction that must NOT
// move. Widening the timeout classification until it swallows genuine row
// disagreements would delete the corpus's entire reason to exist, and it would
// do it quietly.
func TestCheckResultStillReportsARealMismatch(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("test_block line 2512: %q: expected row [1, 2] but got [1, 3]",
		"SELECT id, a FROM t_rd")

	got := factorycorpus.CheckResult(scenario(), failWith(err))
	if got == nil {
		t.Fatal("a row mismatch must FAIL")
	}
	if !strings.Contains(got.Error(), "committed expectation no longer holds") {
		t.Errorf("a genuine row disagreement must still be reported as one — this is the corpus's "+
			"whole product, and misfiling it as a timing artifact is how a real regression gets "+
			"waved off as load. Got:\n%v", got)
	}
}
