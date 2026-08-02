package factorycorpus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fdb.dev/pkg/relational/conformance/javacorpus"
)

// RunScenario executes one committed scenario against the cluster and returns
// the javayamsql runner's ledger entry.
//
// Execution is NOT re-implemented: the scenario's document triple goes through
// javacorpus.RunParsed, so a committed factory scenario is executed by exactly
// the machinery that executes the vendored Java corpus — same schema_template
// lifecycle, same option layering, same result matching. That is what makes
// the shared format worth having. Only the LOADING is this package's own,
// because provenance is a factory concept the vendored corpus does not carry.
//
// The IDPrefix is derived from the scenario name, which is unique by
// construction (seed, query index, projection), so parallel execution cannot
// collide on a database path or a template name.
func RunScenario(ctx context.Context, clusterFile string, s *Scenario) javacorpus.FileResult {
	return javacorpus.RunParsed(ctx, s.SubFile(), javacorpus.Config{
		ClusterFile: clusterFile,
		IDPrefix:    "FC_" + sanitize(s.Header.Name),
	})
}

// CheckResult folds a scenario's ledger entry into pass/fail: every committed
// test must have RUN and ASSERTED. A skip class that suppressed an assertion,
// or a query count below the committed stanza count, is a failure — zero
// failures out of zero executed tests is the loudest possible instrument
// failure wearing the quietest possible output.
func CheckResult(s *Scenario, res javacorpus.FileResult) error {
	if res.Status == javacorpus.StatusFail {
		return fmt.Errorf("committed expectation no longer holds:\n%s", Describe(s, res))
	}
	if res.Status == javacorpus.StatusSkip {
		return fmt.Errorf("scenario was skipped (%s), but a committed scenario must always be executable:\n%s",
			res.SkipClass, Describe(s, res))
	}
	if res.QueriesRun != len(s.Doc.Tests) {
		return fmt.Errorf("%d of %d committed tests asserted — the difference ran without being checked:\n%s",
			res.QueriesRun, len(s.Doc.Tests), Describe(s, res))
	}
	for _, sk := range res.Skips {
		if sk.Class.SuppressesAssertion() {
			return fmt.Errorf("skip %s removed an assertion (%s):\n%s", sk.Class, sk.Detail, Describe(s, res))
		}
	}
	return nil
}

// ScenarioTimeout bounds one scenario. A committed scenario runs four queries
// over at most a couple of dozen rows; anything approaching this bound is a
// hang, not slow work, and failing at the bound keeps a hung scenario from
// consuming the whole target's budget and reporting as a timeout with no name
// attached.
const ScenarioTimeout = 2 * time.Minute

// Describe renders a scenario's result for a test log: where it lives, its
// provenance, and what happened.
func Describe(s *Scenario, res javacorpus.FileResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (scenario %s: seed %d, query %d, projection %d, blessing %s, oracles %s)\n",
		s.Path, s.Header.Name, s.Header.Seed, s.Header.QueryIndex, s.Header.Projection,
		s.Header.Blessing, strings.Join(s.Header.Oracles, "+"))
	if res.Err != nil {
		fmt.Fprintf(&b, "  %v\n", res.Err)
	}
	for _, sk := range res.Skips {
		fmt.Fprintf(&b, "  skip %s at %s: %s\n", sk.Class, sk.Where, sk.Detail)
	}
	fmt.Fprintf(&b, "  regenerate: go run ./cmd/factory-run -seed-start %d -seeds 1 -date %s\n",
		s.Header.Seed, s.Header.Date)
	return b.String()
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
