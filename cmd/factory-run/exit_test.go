package main

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
)

// healthyManifest is a run that should exit 0: it committed something and its
// second-plan oracle demonstrably bit.
func healthyManifest() factory.Manifest {
	return factory.Manifest{
		Committed: 12,
		Oracles: factory.OracleStats{
			TLPChecked:        12,
			SecondPlanKept:    30,
			SecondPlanSkipped: 18,
		},
	}
}

// TestBatchExitPinsEveryOutcome is the factory runner's decision contract.
//
// These four codes are the entire interface between a factory run and whatever
// automation schedules it: 0 means the corpus grew, 1 means an oracle
// disagreed and a human must look, 2 means the run itself is untrustworthy, 3
// means the pipeline produced nothing. Getting 1 and 3 backwards would file a
// broken pipeline as a bug report; getting 2 and 0 backwards would publish a
// batch blessed by an oracle that was switched off. None of it was reachable
// by a test before, because it lived inside a function that needs a container
// and a full sweep to call.
func TestBatchExitPinsEveryOutcome(t *testing.T) {
	t.Parallel()

	finding := &factory.Finding{Oracle: "tlp", Seed: 7, Candidate: "fc_0000000007_q0_p0"}

	for _, tc := range []struct {
		name     string
		manifest func() factory.Manifest
		findings []*factory.Finding
		want     int
		wantSays string
	}{
		{
			name:     "a clean batch that committed tests",
			manifest: healthyManifest,
			want:     exitOK,
		},
		{
			name:     "an oracle disagreed",
			manifest: healthyManifest,
			findings: []*factory.Finding{finding},
			want:     exitFindings,
			wantSays: "ORACLE DISAGREEMENTS",
		},
		{
			name: "the batch committed nothing",
			manifest: func() factory.Manifest {
				m := healthyManifest()
				m.Committed = 0
				return m
			},
			want:     exitEmpty,
			wantSays: "committed nothing",
		},
		{
			name: "the second-plan oracle went dark",
			manifest: func() factory.Manifest {
				m := healthyManifest()
				m.Oracles.SecondPlanKept = 0
				m.Oracles.SecondPlanSkipped = 4812
				return m
			},
			want:     exitInfra,
			wantSays: "never saw two different plans",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := batchExit(tc.manifest(), tc.findings, "factory-findings")
			if got != tc.want {
				t.Fatalf("exit %d, want %d (%s)", got, tc.want, why)
			}
			if tc.wantSays != "" && !strings.Contains(why, tc.wantSays) {
				t.Errorf("message %q does not say %q; an exit code with no explanation is a pager nobody can act on",
					why, tc.wantSays)
			}
			if tc.want == exitOK && why != "" {
				t.Errorf("a healthy run produced the message %q", why)
			}
		})
	}
}

// TestWentDarkOutranksFindings pins the precedence between the two failure
// signals, which is not arbitrary.
//
// A run whose second-plan oracle never bit cannot be trusted to have classified
// its findings either — the same broken instrument produced both. Reporting
// exit 1 there sends someone to triage readings from an instrument that was
// off. The floor therefore comes first, and this is the only test that can tell
// the two orderings apart, because both inputs are present at once.
func TestWentDarkOutranksFindings(t *testing.T) {
	t.Parallel()
	m := healthyManifest()
	m.Oracles.SecondPlanKept = 0
	m.Oracles.SecondPlanSkipped = 100

	got, why := batchExit(m, []*factory.Finding{{Oracle: "second-plan"}}, "factory-findings")
	if got != exitInfra {
		t.Fatalf("exit %d with BOTH a dark oracle and a finding, want %d (infra): a broken instrument's "+
			"readings are not a bug report (%s)", got, exitInfra, why)
	}
}

// TestWentDarkFloorDoesNotFireOnAHealthyMix guards the other direction.
//
// Skips are NORMAL: a query whose plan the disabled rule does not change has no
// second plan to compare, and that is a counted, expected state. A floor that
// tripped on any skip at all would fail every real run, get muted, and stop
// protecting anything. It fires only on kept == 0, which is the shape of the
// option being accepted and ignored.
func TestWentDarkFloorDoesNotFireOnAHealthyMix(t *testing.T) {
	t.Parallel()
	m := healthyManifest()
	m.Oracles.SecondPlanKept = 1
	m.Oracles.SecondPlanSkipped = 99999
	if got, why := batchExit(m, nil, "factory-findings"); got != exitOK {
		t.Fatalf("exit %d for a run with 99999 skips and ONE kept comparison, want %d: skips are the normal "+
			"case and a floor that fires on them is a floor that gets muted (%s)", got, exitOK, why)
	}

	// Neither may it fire on a run that executed no second plans at all — the
	// counters are 0/0 there, which is "nothing happened", not "the instrument
	// is dark". That run is caught by the committed-nothing arm instead.
	m.Oracles.SecondPlanKept, m.Oracles.SecondPlanSkipped = 0, 0
	if got, _ := batchExit(m, nil, "factory-findings"); got != exitOK {
		t.Errorf("exit %d for a run with no second-plan executions at all, want %d", got, exitOK)
	}
}

// TestASecondPlanViolationIsReportedAsAFinding pins the path from the strongest
// signal this harness produces to a red exit code.
//
// A second-plan violation means two plans for ONE query returned different
// rows: one of them is wrong, and there is no interpretation under which the
// engine is fine. It must never be absorbed as a skip, a counted-and-ignored
// statistic, or a green run with a note. The oracle name is asserted in the
// message so a triage that greps for "second-plan" finds it.
func TestASecondPlanViolationIsReportedAsAFinding(t *testing.T) {
	t.Parallel()
	m := healthyManifest()
	m.Oracles.SecondPlanViolations = 1
	f := &factory.Finding{
		Oracle:    "second-plan",
		Seed:      41,
		Candidate: "fc_0000000041_q2_p1",
		Detail:    "two plans for one query returned different rows as sequences: at position 3",
	}

	got, why := batchExit(m, []*factory.Finding{f}, "/tmp/findings")
	if got != exitFindings {
		t.Fatalf("exit %d for a run holding a second-plan violation, want %d: the strongest planner-bug "+
			"signal the factory has must fail the run (%s)", got, exitFindings, why)
	}
	if !strings.Contains(why, "/tmp/findings") {
		t.Errorf("the message %q does not name where the reproduction was persisted", why)
	}
}
