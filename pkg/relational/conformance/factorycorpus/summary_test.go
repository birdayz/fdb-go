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

	// Half the budget: the other half is headroom for the failure detail the
	// framing exists to surround. A floor that eats the whole budget leaves no
	// room for the thing anyone actually reads.
	if framing*2 > capBytes {
		t.Errorf("the corpus's -test.v framing floor is %d bytes, over half the %d-byte console "+
			"budget in .bazelrc. Raise --experimental_ui_max_stdouterr_bytes: past the budget Bazel "+
			"discards the stream entirely and a corpus red names no scenario.", framing, capBytes)
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
