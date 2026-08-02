package factorycorpus

import (
	"fmt"
	"sort"
	"strings"
)

// A corpus red must NAME its scenarios inside whatever budget the log has.
//
// The full corpus runs one Go subtest per committed scenario, and the test
// runner prints four framing lines per subtest (RUN / PAUSE / CONT / result).
// At 5000 scenarios that framing alone is ~1.18 MB of output that says nothing
// — and a build tool that drops an over-budget stream drops the failure with
// it, leaving a red whose scenario is unrecoverable once the run is retried.
// That is not hypothetical: it is how a corpus red arrived with no name
// attached, and the framing cost grows with every scenario the factory adds.
//
// Two things keep a red legible, and they are independent. The log budget must
// be large enough to hold the framing (pinned by TestSubtestFramingFitsLogCap,
// which fails when the corpus outgrows the configured cap). And the failure
// itself must be summarised compactly, at the very end of the stream, so that
// naming the scenarios never depends on how much noise preceded it.

// ScenarioFailure is one failing scenario, reduced to what identifies it and
// what reproduces it. It deliberately does NOT carry the full result diff:
// the diff is already in the log at the point of failure, and a summary that
// repeats it is a summary that can itself blow the budget.
type ScenarioFailure struct {
	// Name is the scenario's header name, e.g. "fc_0000000112_q0_p0".
	Name string
	// Path is the family file it was loaded from.
	Path string
	// Seed and Date are the generator inputs — together the reproduce recipe.
	Seed uint64
	Date string
	// Err is why it failed. Only its first line survives into the summary.
	Err error
}

// SummaryLimit is how many failing scenarios a summary names in full. A corpus
// break is usually systemic — one engine change reddens hundreds of scenarios
// at once — so naming every one of them would reproduce the very unbounded
// output the summary exists to avoid. The first few names plus an exact count
// is what a triager actually acts on.
const SummaryLimit = 10

// summaryDetailBudget caps the per-failure error excerpt. A result mismatch
// renders every expected and actual row; the summary wants the sentence, not
// the table.
const summaryDetailBudget = 160

// FailureSummary renders a bounded report of a corpus run's failures: how many
// of how many, then up to limit named scenarios with their reproduce command.
//
// The result is bounded by limit regardless of how many scenarios failed or how
// large their diffs are, and it is deterministic: failures are sorted by name,
// so the same break names the same scenarios on every run and across runners.
// A limit <= 0 means SummaryLimit.
func FailureSummary(failures []ScenarioFailure, total, limit int) string {
	if len(failures) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = SummaryLimit
	}

	sorted := make([]ScenarioFailure, len(failures))
	copy(sorted, failures)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	fmt.Fprintf(&b, "FACTORY CORPUS FAILURE SUMMARY: %d of %d scenarios failed\n", len(sorted), total)
	shown := sorted
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for i, f := range shown {
		fmt.Fprintf(&b, "  [%d] %s (%s, seed %d, date %s)\n", i+1, f.Name, f.Path, f.Seed, f.Date)
		fmt.Fprintf(&b, "      %s\n", firstLine(f.Err))
		fmt.Fprintf(&b, "      reproduce: bazelisk run //cmd/factory-run -- -seed-start %d -seeds 1 -date %s -out <dir>\n",
			f.Seed, f.Date)
	}
	if n := len(sorted) - len(shown); n > 0 {
		fmt.Fprintf(&b, "  ... and %d further failing scenarios (each named at its own failure above)\n", n)
	}
	return b.String()
}

// firstLine reduces an error to a single bounded line. Corpus errors lead with
// the sentence that classifies the break and follow it with the evidence, so
// the first line is the part worth carrying into a summary.
func firstLine(err error) string {
	if err == nil {
		return "(no error recorded)"
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > summaryDetailBudget {
		s = s[:summaryDetailBudget] + "..."
	}
	if s == "" {
		return "(empty error message)"
	}
	return s
}

// SubtestFramingBytes is what the Go test runner spends on framing lines alone
// for a parallel subtest of the given full name — RUN, PAUSE, CONT and the
// result line. It is the floor of the corpus target's output: it is paid on a
// fully passing run, before a single byte of failure detail.
//
//	=== RUN   <name>\n            10 + len + 1
//	=== PAUSE <name>\n            10 + len + 1
//	=== CONT  <name>\n            10 + len + 1
//	    --- PASS: <name> (0.00s)  4 + 10 + len + 9
func SubtestFramingBytes(fullName string) int {
	n := len(fullName)
	return 3*(10+n+1) + (4 + 10 + n + 9)
}
