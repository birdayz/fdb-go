package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The census is the instrument a corpus re-bless argument rests on, so its
// classification must be driven from EXPLICIT state rather than from whatever
// the corpus happens to contain. A full-corpus run exercises only the arms the
// corpus reaches; an arm that is rare today — or that a pending change is about
// to make reachable — otherwise ships untested, and its first real firing is
// read as a FINDING instead of as an untested branch.
//
// Four classification arms, driven positively AND cross-negatively below:
//
//	lostEqProbe    — regression, exit 1
//	newPlanError   — regression, exit 1
//	lostAnyIndex   — representation, exit 0
//	gainedFullScan — representation, exit 0

// armCase is one crafted movement plus the EXACT set of arms it must fire.
// Naming the full set is what makes the case cross-negative: an arm that fires
// on everything passes a positive-only assertion.
type armCase struct {
	name     string
	before   string
	after    string
	arms     []string // exactly these arms fire, no others
	exitCode int
}

var armCases = []armCase{
	{
		// Equality bound degraded to a one-sided range: the probe is gone
		// while index access remains, so ONLY the equality arm may fire.
		name:     "eq_probe_degraded_to_range",
		before:   "Fetch(IndexScan(IDX_A, [=5]))",
		after:    "Fetch(IndexScan(IDX_A, [>5]))",
		arms:     []string{"lostEqProbe"},
		exitCode: 1,
	},
	{
		// Index access lost entirely, but the BEFORE plan never had an
		// equality probe and the AFTER plan is a bounded PK probe, not an
		// unbounded scan. Only the all-index arm may fire.
		name:     "range_index_replaced_by_pk_probe",
		before:   "Fetch(IndexScan(IDX_A, [>5]))",
		after:    "Fetch(Scan(T, [=1]))",
		arms:     []string{"lostAnyIndex"},
		exitCode: 0,
	},
	{
		// A bounded PK probe becomes an unbounded full scan. No index scan
		// exists on either side, so neither index arm may fire.
		name:     "pk_probe_becomes_full_scan",
		before:   "Fetch(Scan(T, [=1]))",
		after:    "Fetch(Scan(T))",
		arms:     []string{"gainedFullScan"},
		exitCode: 0, // representation-only
	},
	{
		// Newly unplannable from a plan that ALREADY had a full scan, so the
		// full-scan arm cannot fire (it requires absence before).
		name:     "became_unplannable",
		before:   "Fetch(Scan(T))",
		after:    planError,
		arms:     []string{"newPlanError"},
		exitCode: 1,
	},
}

func armsOf(c *census) map[string][]string {
	return map[string][]string{
		"lostEqProbe":    c.lostEqProbe,
		"lostAnyIndex":   c.lostAnyIndex,
		"gainedFullScan": c.gainedFullScan,
		"newPlanError":   c.newPlanError,
	}
}

// TestCensusArmsFireCrossNegatively drives each of the four classification arms
// from crafted state and asserts the other three stay silent on that same
// input.
func TestCensusArmsFireCrossNegatively(t *testing.T) {
	t.Parallel()
	for _, tc := range armCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := map[string]bool{}
			for _, a := range tc.arms {
				want[a] = true
			}
			c, err := takeCensus(
				map[string]string{"s": tc.before},
				map[string]string{"s": tc.after})
			if err != nil {
				t.Fatalf("takeCensus refused a well-formed pair: %v", err)
			}
			if c.compared != 1 || c.moved != 1 {
				t.Fatalf("compared=%d moved=%d, want 1/1 — the case did not reach classification",
					c.compared, c.moved)
			}
			for arm, hits := range armsOf(c) {
				if want[arm] && len(hits) != 1 {
					t.Errorf("arm %s did NOT fire on %q -> %q; an arm that cannot fire is an unpinned branch, not a clean corpus",
						arm, tc.before, tc.after)
				}
				if !want[arm] && len(hits) != 0 {
					t.Errorf("arm %s fired on %q -> %q but this movement belongs to %v; an arm that fires on everything classifies nothing",
						arm, tc.before, tc.after, tc.arms)
				}
			}
			if got := c.exitCode(); got != tc.exitCode {
				t.Errorf("exit code %d, want %d for arms %v", got, tc.exitCode, tc.arms)
			}
		})
	}
}

// TestCensusExitCodeIsExactlyTheRegressionClasses drives every class through
// the verdict, including the unmoved case. Only the two regression classes may
// exit 1.
func TestCensusExitCodeIsExactlyTheRegressionClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		before map[string]string
		after  map[string]string
		want   int
	}{
		{
			"unchanged",
			map[string]string{"s": "Fetch(IndexScan(IDX_A, [=5]))"},
			map[string]string{"s": "Fetch(IndexScan(IDX_A, [=5]))"},
			0,
		},
		{
			"lostEqProbe",
			map[string]string{"s": "Fetch(IndexScan(IDX_A, [=5]))"},
			map[string]string{"s": "Fetch(IndexScan(IDX_A, [>5]))"},
			1,
		},
		{
			"newPlanError",
			map[string]string{"s": "Fetch(Scan(T))"},
			map[string]string{"s": planError},
			1,
		},
		{
			"lostAnyIndex",
			map[string]string{"s": "Fetch(IndexScan(IDX_A, [>5]))"},
			map[string]string{"s": "Fetch(Scan(T, [=1]))"},
			0,
		},
		{
			"gainedFullScan",
			map[string]string{"s": "Fetch(Scan(T, [=1]))"},
			map[string]string{"s": "Fetch(Scan(T))"},
			0,
		},
		{"regression alongside representation", map[string]string{
			"a": "Fetch(IndexScan(IDX_A, [=5]))",
			"b": "Fetch(Scan(T, [=1]))",
		}, map[string]string{
			"a": "Fetch(IndexScan(IDX_A, [>5]))",
			"b": "Fetch(Scan(T))",
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := takeCensus(tc.before, tc.after)
			if err != nil {
				t.Fatalf("takeCensus refused a well-formed pair: %v", err)
			}
			if got := c.exitCode(); got != tc.want {
				t.Errorf("exit code %d, want %d — the verdict must be exactly the two regression classes", got, tc.want)
			}
		})
	}
}

// TestCensusRefusesAnUnusablePopulation pins the three-state separation:
// passed, failed, and NEVER RAN. Every input here would otherwise be reported
// as "no regression class present" — a green produced by an empty set.
func TestCensusRefusesAnUnusablePopulation(t *testing.T) {
	t.Parallel()
	full := map[string]string{"a": "Fetch(Scan(T))", "b": "Fetch(Scan(U))"}
	cases := []struct {
		name        string
		before      map[string]string
		after       map[string]string
		wantMention string
	}{
		{"empty before", map[string]string{}, full, "BEFORE dump is empty"},
		{"empty after", full, map[string]string{}, "AFTER dump is empty"},
		{"both empty", map[string]string{}, map[string]string{}, "BEFORE dump is empty"},
		{
			"disjoint names — zero compared",
			map[string]string{"a": "Fetch(Scan(T))"},
			map[string]string{"z": "Fetch(Scan(T))"},
			"share NO scenario name",
		},
		// The two truncation directions are INDEPENDENT: a guard that catches
		// the after-side truncation is not a guard that catches the before-side
		// one, and a fix satisfying only one direction is how this survives a
		// round of review.
		{
			"truncated after", full,
			map[string]string{"a": "Fetch(Scan(T))"},
			"AFTER dump is missing 1 of 2",
		},
		{
			"truncated before",
			map[string]string{"a": "Fetch(Scan(T))"},
			full,
			"BEFORE dump is missing 1 of 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := takeCensus(tc.before, tc.after)
			if err == nil {
				t.Fatalf("takeCensus returned a VERDICT (exit %d) for %s; an unusable population must refuse, "+
					"because a green from an empty set is indistinguishable from a clean corpus",
					c.exitCode(), tc.name)
			}
			var r *refusal
			if !errors.As(err, &r) {
				t.Fatalf("error %v is not a *refusal; the caller distinguishes refusal (exit 2) from verdict", err)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("refusal %q does not name the cause %q", err.Error(), tc.wantMention)
			}
			// The population's expected value is the FULL corpus, so its alarm
			// direction is the opposite of the regression classes'. A message
			// that does not say which direction is the alarm leaves the next
			// reader to guess which way the instrument broke.
			if !strings.Contains(err.Error(), "SHRINKAGE") {
				t.Errorf("refusal %q does not state that the alarm direction is SHRINKAGE", err.Error())
			}
		})
	}
}

// TestCensusTruncationIsNotSilentlyClassified is the specific regression: a
// scenario missing from the AFTER dump used to be skipped, so a dump truncated
// at any point reported a smaller population as clean and exited 0. The count
// printed was len(before), which made the truncation invisible in the report
// as well.
func TestCensusTruncationIsNotSilentlyClassified(t *testing.T) {
	t.Parallel()
	before := map[string]string{
		"kept": "Fetch(Scan(T))",
		// This one loses an equality probe; truncating it away must not turn
		// the run green.
		"dropped": "Fetch(IndexScan(IDX_A, [=5]))",
	}
	after := map[string]string{"kept": "Fetch(Scan(T))"}
	if c, err := takeCensus(before, after); err == nil {
		t.Fatalf("a truncated AFTER dump produced a verdict (exit %d, compared=%d); the dropped scenario "+
			"was the one carrying the regression, so this is a green from a population that never ran",
			c.exitCode(), c.compared)
	}
}

// TestCensusComparedCountIsTheExaminedPopulation pins that the reported count
// is DRIVEN BY THE COMPARISON LOOP rather than assumed from an input size. The
// population guard now makes the two coincide, so the printed number cannot
// exceed what ran — but only as long as the counter is what is printed. The
// tool previously printed len(before) while the loop skipped every name absent
// from after, so a truncated dump advertised the full corpus size.
func TestCensusComparedCountIsTheExaminedPopulation(t *testing.T) {
	t.Parallel()
	before := map[string]string{"a": "Fetch(Scan(T))", "b": "Fetch(Scan(U))"}
	after := map[string]string{"a": "Fetch(Scan(T))", "b": "Fetch(Scan(U))"}
	c, err := takeCensus(before, after)
	if err != nil {
		t.Fatalf("takeCensus refused an identical, complete pair: %v", err)
	}
	if c.compared != 2 {
		t.Errorf("compared=%d, want 2 (the examined population)", c.compared)
	}
	var buf bytes.Buffer
	c.report(&buf, before, after)
	if !strings.Contains(buf.String(), "scenarios compared: 2") {
		t.Errorf("report does not state the examined population:\n%s", buf.String())
	}
}

// TestCensusReportStatesTheAlarmDirection pins that the regression report says
// growth above zero is the alarm. The two regression classes sit at zero in
// steady state, which is the same reading an instrument that died produces.
func TestCensusReportStatesTheAlarmDirection(t *testing.T) {
	t.Parallel()
	before := map[string]string{"s": "Fetch(IndexScan(IDX_A, [=5]))"}
	after := map[string]string{"s": "Fetch(IndexScan(IDX_A, [>5]))"}
	c, err := takeCensus(before, after)
	if err != nil {
		t.Fatalf("takeCensus: %v", err)
	}
	var buf bytes.Buffer
	c.report(&buf, before, after)
	out := buf.String()
	for _, want := range []string{"GROWTH above zero", "lost an EQUALITY index probe:   1", "before: Fetch(IndexScan(IDX_A, [=5]))"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// TestCensusCleanReportSaysRepresentationOnly pins the passing report, so that
// "no regression class present" cannot be reached with a regression counted.
func TestCensusCleanReportSaysRepresentationOnly(t *testing.T) {
	t.Parallel()
	before := map[string]string{"s": "Fetch(Scan(T, [=1]))"}
	after := map[string]string{"s": "Fetch(Scan(T))"}
	c, err := takeCensus(before, after)
	if err != nil {
		t.Fatalf("takeCensus: %v", err)
	}
	var buf bytes.Buffer
	c.report(&buf, before, after)
	out := buf.String()
	if !strings.Contains(out, "no regression class present") {
		t.Errorf("a representation-only movement must report as such:\n%s", out)
	}
	if strings.Contains(out, "REGRESSION:") {
		t.Errorf("a representation-only movement must not report a regression:\n%s", out)
	}
}

// TestCensusRegexArmsDoNotOvermatch pins the two deliberate exclusions the
// classification rests on: a bounded primary probe Scan(T, [=1]) is NOT a full
// scan, and IndexScan(...) does not read as one either. Both keep the
// full-scan arm from firing on improvements.
func TestCensusRegexArmsDoNotOvermatch(t *testing.T) {
	t.Parallel()
	if fullScan.MatchString("Fetch(Scan(T, [=1]))") {
		t.Error("a bounded PK probe matched the UNBOUNDED full-scan pattern; " +
			"replacing a secondary-index probe with a PK probe would then read as a regression")
	}
	if fullScan.MatchString("Fetch(IndexScan(IDX_A, [>5]))") {
		t.Error("an index scan matched the primary full-scan pattern")
	}
	if !fullScan.MatchString("Fetch(Scan(T))") {
		t.Error("an unbounded primary scan did NOT match the full-scan pattern; the arm is dead")
	}
	if eqIndexScan.MatchString("Fetch(IndexScan(IDX_A, [>5]))") {
		t.Error("a one-sided range matched the equality-probe pattern")
	}
	if !eqIndexScan.MatchString("Fetch(IndexScan(IDX_A, [=5]))") {
		t.Error("an equality probe did NOT match the equality-probe pattern; the regression arm is dead")
	}
}
