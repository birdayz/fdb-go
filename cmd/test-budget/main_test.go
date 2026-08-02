package main

import (
	"strings"
	"testing"
	"time"
)

func slots(t *testing.T) map[string]time.Duration {
	t.Helper()
	// Mirrors .bazelrc's `--test_timeout=60,300,900,3600`.
	b, err := parseSlots("60,300,900,3600")
	if err != nil {
		t.Fatalf("parseSlots: %v", err)
	}
	return b
}

// The timeout slot a target is judged against must be the one Bazel would
// enforce: an explicit `timeout` wins, and a target that only sets `size`
// still gets the slot that size implies. Judging //pkg/b:b_test (size=large)
// against the medium 300s default would call a healthy 880s run a 293%
// breach, and judging //pkg/a:a_test (timeout=long over a medium size)
// against 300s would do the same — both would make the gate cry wolf until
// someone turned it off.
func TestParseTargets_TimeoutSlotResolution(t *testing.T) {
	t.Parallel()
	targets, err := parseTargets("testdata/targets.xml", slots(t))
	if err != nil {
		t.Fatalf("parseTargets: %v", err)
	}
	for _, tc := range []struct {
		label string
		want  time.Duration
		slot  string
	}{
		{"//pkg/a:a_test", 900 * time.Second, "long"},
		{"//pkg/b:b_test", 900 * time.Second, "long"},
		{"//pkg/c:c_test", 300 * time.Second, "moderate"},
		{"//pkg/d:d_test", 3600 * time.Second, "eternal"},
	} {
		got, ok := targets[tc.label]
		if !ok {
			t.Fatalf("%s missing from parsed targets", tc.label)
		}
		if got.Budget != tc.want || got.BudgetName != tc.slot {
			t.Errorf("%s budget = %v (%s), want %v (%s)", tc.label, got.Budget, got.BudgetName, tc.want, tc.slot)
		}
	}
}

// A budget gate over zero resolvable targets would pass every run vacuously —
// the loudest instrument failure wearing the quietest output. It must be an
// error, not an empty report.
func TestParseTargets_EmptyIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := parseTargets("testdata/empty.xml", slots(t)); err == nil {
		t.Fatal("parseTargets over a query with no test rules returned nil error")
	}
}

// The gate's whole reason to exist: a CACHED result carries the duration of
// whatever run first populated the cache, on whatever machine, under whatever
// contention — it is not a measurement of this run. //pkg/b:b_test sits at
// 880s of a 900s budget and MUST NOT be reported, because nothing here
// observed it; //pkg/a:a_test at 700s of 900s executed and MUST be. If this
// test goes green with cached results included, the gate is reporting stale
// numbers as fresh ones and will fire on branches that changed nothing.
func TestEvaluate_CachedResultsAreNotMeasurements(t *testing.T) {
	t.Parallel()
	runs, err := parseBEP("testdata/run.jsonl")
	if err != nil {
		t.Fatalf("parseBEP: %v", err)
	}
	all, warned, failed := Evaluate(runs, mustTargets(t), 0.6, 0.95)
	over := append(append([]Result{}, failed...), warned...)

	seen := map[string]float64{}
	for _, r := range all {
		seen[r.Label] = r.Fraction()
	}
	if _, ok := seen["//pkg/b:b_test"]; ok {
		t.Errorf("cached //pkg/b:b_test appeared in the report at %.2f — a cache hit is not a runtime measurement", seen["//pkg/b:b_test"])
	}
	if got, ok := seen["//pkg/a:a_test"]; !ok {
		t.Errorf("executed //pkg/a:a_test missing from the report")
	} else if got < 0.77 || got > 0.79 {
		t.Errorf("//pkg/a:a_test fraction = %.4f, want 700/900", got)
	}
	// A target whose attempts were PARTLY cached still executed for real on
	// the attempt that was not — dropping it would lose the one shard that
	// actually ran.
	if got, ok := seen["//pkg/c:c_test"]; !ok {
		t.Errorf("partially-cached //pkg/c:c_test missing from the report")
	} else if got < 0.82 || got > 0.84 {
		t.Errorf("//pkg/c:c_test fraction = %.4f, want 250/300", got)
	}

	var overLabels []string
	for _, r := range over {
		overLabels = append(overLabels, r.Label)
	}
	want := "//pkg/c:c_test,//pkg/a:a_test" // sorted by descending utilisation
	if strings.Join(overLabels, ",") != want {
		t.Errorf("over threshold = %v, want %s", overLabels, want)
	}
}

// A target that is big but healthy must warn, not fail. Measured on master,
// //pkg/relational/conformance/rowdiff runs 659s of a 900s budget — 73%, every
// run, passing. A gate that failed there would block PRs that changed nothing,
// which is the disease this whole change is treating. //pkg/a:a_test at 78%
// stands in for it: over the warn line, under the fail line, exit 0.
// //pkg/c:c_test at 83% is over both and is what a genuine "fix this now" looks
// like.
func TestEvaluate_WarnsBeforeItFails(t *testing.T) {
	t.Parallel()
	runs, err := parseBEP("testdata/run.jsonl")
	if err != nil {
		t.Fatalf("parseBEP: %v", err)
	}
	_, warned, failed := Evaluate(runs, mustTargets(t), 0.70, 0.80)

	names := func(rs []Result) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Label)
		}
		return out
	}
	if got := strings.Join(names(warned), ","); got != "//pkg/a:a_test" {
		t.Errorf("warned = %v, want just //pkg/a:a_test (78%%: past warn, short of fail)", names(warned))
	}
	if got := strings.Join(names(failed), ","); got != "//pkg/c:c_test" {
		t.Errorf("failed = %v, want just //pkg/c:c_test (83%%: past both)", names(failed))
	}
	// The two sets must be disjoint: a target reported twice reads as two
	// separate problems in the CI annotations.
	for _, w := range warned {
		for _, f := range failed {
			if w.Label == f.Label {
				t.Errorf("%s is reported as both warned and failed", w.Label)
			}
		}
	}
}

// The gate must fire while the target still has headroom, not at the breach.
// //pkg/a:a_test at 700s of 900s is passing today and is the exact case the
// tool exists to catch; if the threshold logic were `>= 1.0` it would report
// nothing until the run that already blocked the merge queue.
func TestEvaluate_FiresBeforeTheBreach(t *testing.T) {
	t.Parallel()
	runs, err := parseBEP("testdata/run.jsonl")
	if err != nil {
		t.Fatalf("parseBEP: %v", err)
	}
	_, warned, failed := Evaluate(runs, mustTargets(t), 0.6, 0.95)
	over := append(append([]Result{}, failed...), warned...)
	if len(over) == 0 {
		t.Fatal("no target reported over 60% though //pkg/a:a_test ran 700s of a 900s budget")
	}
	for _, r := range over {
		if r.Duration >= r.Budget {
			t.Errorf("%s only reported at/after its budget (%v of %v) — the gate must fire with headroom left", r.Label, r.Duration, r.Budget)
		}
	}
	// //pkg/d:d_test at 600s of an eternal 3600s budget is healthy and must
	// not be dragged in by an absolute-seconds rule.
	for _, r := range over {
		if r.Label == "//pkg/d:d_test" {
			t.Errorf("//pkg/d:d_test (600s of 3600s) reported over threshold — the gate compares against the BUDGET, not against a fixed duration")
		}
	}
}

// A run in which every target was a cache hit is normal and healthy; it must
// report that it measured nothing rather than fail or claim a clean bill.
func TestRenderReport_AllCachedSaysSo(t *testing.T) {
	t.Parallel()
	got := RenderReport(nil, "race", 0.7, 0.9)
	if !strings.Contains(got, "cache hit") {
		t.Errorf("report over zero executed targets does not say the run measured nothing:\n%s", got)
	}
}

func mustTargets(t *testing.T) map[string]Target {
	t.Helper()
	targets, err := parseTargets("testdata/targets.xml", slots(t))
	if err != nil {
		t.Fatalf("parseTargets: %v", err)
	}
	return targets
}
