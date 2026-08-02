package factorycorpus_test

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// A corpus red is only useful if it names its scenarios, and it can fail to do
// that in two independent ways: the summary itself can be unbounded (so it is
// the thing that blows the budget), or the log budget can be too small to hold
// the target's output at all (so the build tool discards the whole stream and
// the summary goes with it). Each direction is pinned separately below.

func failures(n int) []factorycorpus.ScenarioFailure {
	out := make([]factorycorpus.ScenarioFailure, 0, n)
	for i := n - 1; i >= 0; i-- { // reversed: the summary must sort, not echo
		out = append(out, factorycorpus.ScenarioFailure{
			Name: fmt.Sprintf("fc_%010d_q0_p0", i),
			Path: "testdata/join2_comma__and_arith-cmp__none.yamsql",
			Seed: uint64(i),
			Date: "2026-07-31",
			Err: fmt.Errorf("committed expectation no longer holds:\n%s",
				strings.Repeat("expected row [1, 2, 3] actual row [1, 2, 4]\n", 200)),
		})
	}
	return out
}

// TestFailureSummaryIsBounded is the whole point of the summary: a systemic
// break reddens hundreds of scenarios with megabyte-scale diffs, and the
// summary must stay small enough that it survives whatever budget the log has.
func TestFailureSummaryIsBounded(t *testing.T) {
	t.Parallel()

	huge := failures(400)
	s := factorycorpus.FailureSummary(huge, 5000, 0)

	// 400 failures at ~9 KB of diff each is ~3.6 MB of raw error text. The
	// summary of it has to stay in the kilobytes.
	const budget = 4096
	if len(s) > budget {
		t.Errorf("summary of 400 failures is %d bytes, over the %d-byte budget — an unbounded "+
			"summary is the failure it exists to prevent:\n%s", len(s), budget, s)
	}
	if lines := strings.Count(s, "\n"); lines > 3*factorycorpus.SummaryLimit+3 {
		t.Errorf("summary rendered %d lines for 400 failures; it must name at most %d",
			lines, factorycorpus.SummaryLimit)
	}
	if !strings.Contains(s, "400 of 5000 scenarios failed") {
		t.Errorf("summary must carry the exact counts; got:\n%s", s)
	}
	if !strings.Contains(s, "and 390 further failing scenarios") {
		t.Errorf("summary must say how many it omitted; got:\n%s", s)
	}
}

// TestFailureSummaryNamesAndReproduces pins what a triager actually needs out
// of a red: the scenario name and a command that regenerates it.
func TestFailureSummaryNamesAndReproduces(t *testing.T) {
	t.Parallel()

	s := factorycorpus.FailureSummary([]factorycorpus.ScenarioFailure{{
		Name: "fc_0000000112_q0_p0",
		Path: "testdata/join2_comma__and_arith-cmp__none.yamsql",
		Seed: 112,
		Date: "2026-07-31",
		Err:  errors.New("committed expectation no longer holds:\nrow 3 differs"),
	}}, 5000, 0)

	for _, want := range []string{
		"fc_0000000112_q0_p0",
		"testdata/join2_comma__and_arith-cmp__none.yamsql",
		"committed expectation no longer holds:",
		"-seed-start 112 -seeds 1 -date 2026-07-31",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary is missing %q — a red that omits it cannot be acted on; got:\n%s", want, s)
		}
	}
	// Only the classifying first line survives; the evidence stays at the
	// failure site rather than being duplicated into the summary.
	if strings.Contains(s, "row 3 differs") {
		t.Errorf("summary carried the error's body, not just its first line; got:\n%s", s)
	}
}

// TestFailureSummaryIsDeterministic pins that the same break names the same
// scenarios every run. Parallel subtests finish in arrival order, so an
// unsorted summary would name a different ten each time and two CI reds of one
// bug would not look like the same bug.
func TestFailureSummaryIsDeterministic(t *testing.T) {
	t.Parallel()

	s := factorycorpus.FailureSummary(failures(40), 5000, 3)
	want := []string{"fc_0000000000_q0_p0", "fc_0000000001_q0_p0", "fc_0000000002_q0_p0"}
	for i, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("summary must name the %d lowest scenario names in order; %q missing:\n%s", len(want), w, s)
		}
		if i > 0 {
			if strings.Index(s, want[i-1]) > strings.Index(s, w) {
				t.Errorf("summary is not sorted by scenario name: %q came after %q", want[i-1], w)
			}
		}
	}
	if strings.Contains(s, "fc_0000000039_q0_p0") {
		t.Errorf("summary named a scenario past its limit:\n%s", s)
	}
}

// TestFailureSummaryRanksChangedAnswersAboveTimeouts pins the shape a
// contended run actually produces, because that is the only shape in which
// this can go wrong.
//
// CheckResult classifies a scenario that ran out of wall-clock separately from
// one whose answer changed, on the grounds that they mean opposite things. A
// summary ordered by name alone throws that away: it draws its ten names
// uniformly from the failures, and on a loaded box the failures are
// overwhelmingly timing.
//
// The numbers below are measured, not invented — one contended run of the full
// corpus produced 58 failures of which 53 were deadline expiries. Ordered by
// name, such a run names about one changed answer in ten lines; the reader
// sees "timeout, timeout, timeout", concludes load, and the regression the
// corpus exists to catch is in the omitted tail.
//
// A single-class fixture cannot express this: with only timeouts or only
// mismatches every ordering agrees, and the test passes with the defect fully
// present.
func TestFailureSummaryRanksChangedAnswersAboveTimeouts(t *testing.T) {
	t.Parallel()

	var mixed []factorycorpus.ScenarioFailure
	// Named so that EVERY timing failure sorts before EVERY genuine one by
	// name — so name order alone would bury all five.
	for i := 0; i < 53; i++ {
		mixed = append(mixed, factorycorpus.ScenarioFailure{
			Name: fmt.Sprintf("fc_00000%05d_q0_p0", i),
			Path: "testdata/join3_comma__and_cmp__none.yamsql", Seed: uint64(i), Date: "2026-08-01",
			Err: &factorycorpus.IncompleteError{
				Reason: "the per-scenario ScenarioTimeout deadline expired mid-query",
				Detail: "…",
			},
		})
	}
	for i := 0; i < 5; i++ {
		mixed = append(mixed, factorycorpus.ScenarioFailure{
			Name: fmt.Sprintf("fc_99999%05d_q0_p0", i),
			Path: "testdata/join2_comma__and_arith-cmp__none.yamsql", Seed: uint64(900 + i), Date: "2026-08-01",
			Err: errors.New("committed expectation no longer holds:\nrow 3 differs"),
		})
	}

	s := factorycorpus.FailureSummary(mixed, 5000, 0)

	// All five changed answers must be named, ahead of any timeout.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("fc_99999%05d_q0_p0", i)
		if !strings.Contains(s, want) {
			t.Errorf("changed answer %q was ranked out of the summary by timing artifacts — this is "+
				"the regression the corpus exists to catch, omitted in favour of failures that say "+
				"nothing about the engine. Got:\n%s", want, s)
		}
	}
	if first, last := strings.Index(s, "fc_9999900000_q0_p0"), strings.Index(s, "fc_0000000000_q0_p0"); first > last && last >= 0 {
		t.Errorf("a wall-clock timeout was ranked above a changed answer:\n%s", s)
	}
	// The counts must say what the mix was, so a reader knows before reading
	// the names whether this red is about the engine or about the box.
	if !strings.Contains(s, "58 of 5000 scenarios failed (5 changed answers, 53 ran out of wall-clock)") {
		t.Errorf("summary must break the count down by class; got:\n%s", s)
	}
	// And the bound still holds with a mixed population.
	if len(s) > 4096 {
		t.Errorf("summary is %d bytes, over budget", len(s))
	}
}

// TestFailureSummaryOmitsTheBreakdownWhenThereIsNoMix keeps the header honest
// for the ordinary case: a red that is entirely one class must not grow a
// parenthetical restating its own total.
func TestFailureSummaryOmitsTheBreakdownWhenThereIsNoMix(t *testing.T) {
	t.Parallel()
	s := factorycorpus.FailureSummary(failures(3), 5000, 0)
	if strings.Contains(s, "changed answers,") {
		t.Errorf("a single-class summary must not carry a breakdown; got:\n%s", s)
	}
	if !strings.Contains(s, "3 of 5000 scenarios failed\n") {
		t.Errorf("summary must carry the plain count; got:\n%s", s)
	}
}

func TestFailureSummaryEmpty(t *testing.T) {
	t.Parallel()
	if s := factorycorpus.FailureSummary(nil, 5000, 0); s != "" {
		t.Errorf("a run with no failures must render nothing, got %q", s)
	}
}

var capRE = regexp.MustCompile(`(?m)^test --experimental_ui_max_stdouterr_bytes=(\d+)\s*$`)

// TestSubtestFramingFitsLogCap is the negative-result pin, and it is the one
// that re-arms if it is deleted.
//
// Bazel does not truncate an over-budget console stream — it DISCARDS it. The
// full corpus target runs one parallel subtest per committed scenario and
// .bazelrc forces -test.v, so the framing lines alone (RUN/PAUSE/CONT/result)
// are a fixed cost paid on a PASSING run. At 5000 scenarios that floor is
// ~1.18 MB, past Bazel's 1 MiB default, which is exactly how a corpus red once
// arrived with its scenario name unrecoverable.
//
// The floor grows with every scenario the factory commits. This test fails
// when it grows into the configured budget, so the answer is to raise
// --experimental_ui_max_stdouterr_bytes in .bazelrc (or shrink the target's
// output) — never to delete this check.
func TestSubtestFramingFitsLogCap(t *testing.T) {
	t.Parallel()

	rc, err := os.ReadFile("../../../../.bazelrc")
	if err != nil {
		t.Fatalf("read .bazelrc: %v", err)
	}
	m := capRE.FindSubmatch(rc)
	if m == nil {
		t.Fatalf(".bazelrc does not set --experimental_ui_max_stdouterr_bytes. Without it Bazel's " +
			"1 MiB default silently DISCARDS this corpus target's output, and every failure of it " +
			"becomes a red with no scenario name attached.")
	}
	capBytes, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse cap: %v", err)
	}

	scenarios, err := factorycorpus.LoadDir("testdata")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	framing := 0
	for _, s := range scenarios {
		framing += factorycorpus.SubtestFramingBytes("TestFDB_FactoryCorpusFull/" + s.Header.Name)
	}
	t.Logf("%d scenarios cost %d bytes of -test.v framing against a %d-byte console budget",
		len(scenarios), framing, capBytes)

	// The framing floor is not the quantity that has to fit — the log a RED
	// produces is, and that is the only log anyone ever needs to read. A run in
	// which every scenario fails carries each one's failure detail on top of
	// its framing, and that whole log is measured against the same budget.
	//
	// Measured on a run where all 5000 committed scenarios fail: 4985971 bytes
	// against a 1180000-byte framing floor, i.e. the detail costs a little over
	// three times the framing that surrounds it. Budgeting for the framing
	// alone, with the rest nominally "headroom", is what leaves a corpus red
	// discarded while this check is still green: at a floor of half the current
	// 16 MiB budget the all-fail log would be ~35 MB, over the cap by 2x.
	//
	// So require room for the amplified log, not for the floor. The multiplier
	// is the measured ratio rounded UP — an all-fail run is the worst case this
	// budget exists to survive, and rounding it down is how the check goes
	// quietly under-specified again.
	const allFailAmplification = 5 // measured 4985971/1180000 = 4.22x, rounded up
	if framing*allFailAmplification > capBytes {
		t.Errorf("the corpus's -test.v framing floor is %d bytes; a run in which every scenario "+
			"fails costs about %dx that (%d bytes), over the %d-byte console budget in .bazelrc. "+
			"Raise --experimental_ui_max_stdouterr_bytes: past the budget Bazel discards the stream "+
			"entirely and a corpus red names no scenario.",
			framing, allFailAmplification, framing*allFailAmplification, capBytes)
	}
	// The default is what bit; assert we are actually clear of it, so a future
	// edit that "restores the default" is caught here rather than in a CI red
	// with no name.
	if const1MiB := 1 << 20; framing <= const1MiB {
		t.Logf("note: framing is still under Bazel's %d-byte default", const1MiB)
	} else if capBytes <= const1MiB {
		t.Errorf("framing %d exceeds Bazel's default %d but .bazelrc's cap is only %d",
			framing, const1MiB, capBytes)
	}
}
