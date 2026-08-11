package sqldriver_test

import (
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestRowdiffSweepStopsWhenTheClusterDies drives every arm of the sweep's
// stopping policy from explicit state.
//
// The live sweep reaches NONE of these arms on a healthy cluster, which is
// exactly why they need driving here: the breaker's first real firing would
// otherwise be read as a finding rather than as an untested branch. Both
// DISABLED arms are pinned too — a bound of zero must mean "no bound", and
// reading it as "a bound of zero" would abort every sweep on its first seed,
// which fails in the loud-but-opposite direction.
func TestRowdiffSweepStopsWhenTheClusterDies(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		lim        rowdiffLimits
		consecutve int
		elapsed    time.Duration
		wantToken  string // "" = must not stop
		wantFatal  bool   // does this stop RED the night?
	}{
		{
			name:       "the wedge: enough consecutive INFRA that the cluster is gone",
			lim:        rowdiffLimits{maxConsecutiveInfra: 10},
			consecutve: 10,
			wantFatal:  true,
			wantToken:  "ROWDIFF_CLUSTER_DEAD",
		},
		{
			name:       "one short of the breaker keeps sweeping",
			lim:        rowdiffLimits{maxConsecutiveInfra: 10},
			consecutve: 9,
		},
		{
			// The load-bearing negative. A sweep on a healthy cluster that hits
			// scattered INFRA seeds must run to completion; the counter resets in
			// the loop on any seed that measures something, so a high TOTAL infra
			// count never reaches this arm.
			name:       "scattered INFRA never accumulates — a healthy sweep is untouched",
			lim:        rowdiffLimits{maxConsecutiveInfra: 10},
			consecutve: 0,
			elapsed:    9 * time.Hour,
		},
		{
			name:       "breaker disabled by a non-positive bound stays silent forever",
			lim:        rowdiffLimits{maxConsecutiveInfra: 0},
			consecutve: 100000,
		},
		{
			name:      "wall-clock budget exhausted",
			lim:       rowdiffLimits{maxConsecutiveInfra: 10, budget: time.Hour},
			elapsed:   time.Hour,
			wantToken: "ROWDIFF_BUDGET_EXHAUSTED",
		},
		{
			name:    "inside the budget keeps sweeping",
			lim:     rowdiffLimits{maxConsecutiveInfra: 10, budget: time.Hour},
			elapsed: 59 * time.Minute,
		},
		{
			// A zero budget means UNBOUNDED, not "a bound of zero". Getting this
			// backwards would stop every sweep after its first seed while still
			// looking like a working guard.
			name:    "zero budget means unbounded, not instant abort",
			lim:     rowdiffLimits{maxConsecutiveInfra: 10},
			elapsed: 1000 * time.Hour,
		},
		{
			name:       "the breaker outranks the budget so the message names the real cause",
			lim:        rowdiffLimits{maxConsecutiveInfra: 10, budget: time.Hour},
			consecutve: 50,
			elapsed:    5 * time.Hour,
			wantFatal:  true,
			wantToken:  "ROWDIFF_CLUSTER_DEAD",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, fatal := tc.lim.stop(tc.consecutve, tc.elapsed)
			if tc.wantToken == "" {
				if got != "" {
					t.Fatalf("stop() aborted a sweep that must continue: %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("stop() did not fire; want %s", tc.wantToken)
			}
			if !strings.Contains(got, tc.wantToken) {
				t.Errorf("stop() = %q, want the greppable token %s — the workflow summary leads with it",
					got, tc.wantToken)
			}
			// Severity is the load-bearing half: a cluster that vanished must red the
			// night, and running out of clock must not, or the nightly is permanently
			// failing and nobody reads it.
			if fatal != tc.wantFatal {
				t.Errorf("stop() fatal=%v, want %v for %q", fatal, tc.wantFatal, tc.wantToken)
			}
		})
	}
}

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

// TestRowdiffCoverageFloor pins the guard that keeps a NON-fatal budget
// exhaustion honest.
//
// Budget exhaustion is deliberately not an error, so this floor is the only
// thing standing between "the clock sized the night" and "per-seed cost
// regressed tenfold and the nightly went quietly green on 200 seeds forever".
// Both directions matter and the negatives are the load-bearing ones: a
// developer running the 25-seed smoke slice, and a nightly that walked its full
// request, must never trip a thousands-scale floor.
func TestRowdiffCoverageFloor(t *testing.T) {
	t.Parallel()

	const floor = 2000
	for _, tc := range []struct {
		name              string
		walked, requested uint64
		wantFire          bool
	}{
		{
			name: "the collapse this floor exists for: a truncated night that measured almost nothing",
			// A tenfold per-seed regression against a 3h30m budget looks exactly
			// like this, and without the floor it is a green.
			walked: 200, requested: 10000, wantFire: true,
		},
		{
			name:   "a normally truncated night is fine — the clock is meant to size the sweep",
			walked: 8000, requested: 10000, wantFire: false,
		},
		{
			// THE negative that makes the floor safe to ship: `requested` is a
			// ceiling, so completing a small request is complete coverage. A floor
			// that ignored this would fail every PR smoke run and every local run.
			name:   "the 25-seed PR smoke slice walked everything it was asked for",
			walked: 25, requested: 25, wantFire: false,
		},
		{
			name:   "exactly at the floor is enough",
			walked: floor, requested: 10000, wantFire: false,
		},
		{
			name:   "one below the floor is not",
			walked: floor - 1, requested: 10000, wantFire: true,
		},
		{
			// Raising the seed ceiling must never raise the bar the night clears.
			// Expressing the floor as a FRACTION of the request would invert this:
			// the same 3000 walked seeds would pass at request=10000 and fail at
			// request=100000, punishing a config change that measured nothing new.
			name:   "raising the ceiling does not raise the bar",
			walked: 3000, requested: 1000000, wantFire: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := coverageFloor("test", tc.walked, tc.requested, floor)
			if fired := msg != ""; fired != tc.wantFire {
				t.Fatalf("coverageFloor(walked=%d, requested=%d) fired=%v, want %v (msg=%q)",
					tc.walked, tc.requested, fired, tc.wantFire, msg)
			}
			if tc.wantFire && !strings.Contains(msg, "ROWDIFF_COVERAGE_FLOOR") {
				t.Errorf("the complaint must carry its greppable token; got %q", msg)
			}
		})
	}
}
