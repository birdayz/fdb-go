package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestRowdiffTallyCountsEveryOutcome pins each arm of the classifier tally from
// EXPLICIT state.
//
// It is deliberately not left to the live sweep: a corpus run only exercises
// the arms the corpus happens to reach, and on a healthy cluster it reaches
// OutcomeOK and nothing else. The mismatch arm and the infra arm are exactly
// the ones that matter and exactly the ones a green run never touches.
func TestRowdiffTallyCountsEveryOutcome(t *testing.T) {
	t.Parallel()

	var a rowdiffTally
	a.add(&rowdiff.SeedResult{Seed: 1, Kind: rowdiff.OutcomeOK, Executed: 12})
	a.add(&rowdiff.SeedResult{Seed: 2, Kind: rowdiff.OutcomeInfra})
	a.add(&rowdiff.SeedResult{Seed: 3, Kind: rowdiff.OutcomeInfra})
	a.add(&rowdiff.SeedResult{
		Seed: 4, Kind: rowdiff.OutcomeMismatch,
		Mismatches: []*rowdiff.Mismatch{{Detail: "a"}, {Detail: "b"}},
	})
	a.add(&rowdiff.SeedResult{Seed: 5, Kind: rowdiff.OutcomeOK, Declines: []string{"known gap"}})

	if a.seeds != 5 {
		t.Errorf("seeds = %d, want 5", a.seeds)
	}
	if a.ok != 2 {
		t.Errorf("ok = %d, want 2", a.ok)
	}
	if a.infra != 2 {
		t.Errorf("infra = %d, want 2", a.infra)
	}
	if a.mismatch != 1 {
		t.Errorf("mismatch = %d, want 1", a.mismatch)
	}
	// The ROW count is separate from the SEED count on purpose: the 2026-08-07
	// nightly had 16 mismatch rows across 4 seeds, and reporting only one of
	// those two numbers makes the finding look four times bigger or four times
	// smaller than it is.
	if a.mismatchRows != 2 {
		t.Errorf("mismatchRows = %d, want 2", a.mismatchRows)
	}
	if a.declines != 1 {
		t.Errorf("declines = %d, want 1", a.declines)
	}
	if len(a.mismatchSeeds) != 1 || a.mismatchSeeds[0] != 4 {
		t.Errorf("mismatchSeeds = %v, want [4] — the seed number is what makes a finding reproducible", a.mismatchSeeds)
	}
}

// TestRowdiffVacuityFloor pins the floor in BOTH directions. The negative
// direction is the load-bearing one: a floor that fires on a healthy sweep is
// worse than no floor, because it retrains people to ignore the very signal it
// exists to raise.
func TestRowdiffVacuityFloor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		tally    rowdiffTally
		executed int
		wantFire bool
	}{
		{
			name:     "all seeds infra, nothing compared — the instrument is dark",
			tally:    rowdiffTally{seeds: 200, infra: 200},
			executed: 0,
			wantFire: true,
		},
		{
			name: "cluster died late, but real comparisons happened first",
			// This is the 2026-08-07 shape: heavy infra AND real findings in
			// the same run. It must NOT be called vacuous — the sweep measured
			// the engine and caught it.
			tally:    rowdiffTally{seeds: 200, ok: 150, infra: 46, mismatch: 4, mismatchRows: 16},
			executed: 4211,
			wantFire: false,
		},
		{
			name:     "healthy sweep",
			tally:    rowdiffTally{seeds: 200, ok: 200},
			executed: 9000,
			wantFire: false,
		},
		{
			name:     "no seeds requested at all is not vacuity, it is configuration",
			tally:    rowdiffTally{},
			executed: 0,
			wantFire: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := tc.tally.vacuous("test", tc.executed)
			if fired := msg != ""; fired != tc.wantFire {
				t.Fatalf("vacuous() fired=%v, want %v (msg=%q)", fired, tc.wantFire, msg)
			}
			if tc.wantFire && !strings.Contains(msg, "ROWDIFF_VACUOUS") {
				t.Errorf("the complaint must carry the greppable token the workflow summary leads with; got %q", msg)
			}
		})
	}
}
