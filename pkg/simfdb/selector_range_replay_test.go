package simfdb_test

import (
	"fmt"
	mrand "math/rand/v2"
	"os"
	"testing"

	"fdb.dev/pkg/simfdb"
)

// The selector-range conflict divergences the differential found once the client began
// resolving selectors inside the range read (RFC-262 D10). SimFDB still modelled the old
// client, which resolved each selector with a getKey and took that getKey's span plus the
// resolved range; the client now records C++'s addConflictRange(GetRangeReq)
// (ReadYourWrites.actor.cpp:245-319), pinned against libfdb_c by
// //pkg/fdbgo/client:selector_range_conformance_test. Each entry is a seed, the scenario that
// diverged and the outcome the real cluster gave.
var selectorRangeDivergences = []struct {
	arm      string // "diff" for runDifferentialConflictOutcome, else the runArm name
	seed     uint64
	scenario int
	real     bool // committed on the real cluster
}{
	// forward (k02,-2) .. lastLessThan(k19), limit 5: C++ keeps the span to the end anchor
	// k19 because end.offset <= 0 (:257), so a write at k14 past the 5 rows read conflicts.
	{"diff", 1793635174985733078, 1, false},
	// reverse (k19,3) .. (k20,-2), limit 2: begin resolves above end, nothing is read, and
	// C++'s span is [k19, k20) (:295); the old getKey span [k15, k20) of the end selector
	// conflicted on a write at k17.
	{"diff", 1, 63, true},
	{"atomicsel", 7, 139, true},
	{"atomicsel", 1793635174996401581, 9, true},
}

// TestSelectorRangeDivergences_SimReplay replays each diverging sweep on SimFDB alone, through
// the diverging scenario, and requires the outcome the real cluster gave. The earlier scenarios
// run too, so the SimDB holds the same history the sweep built.
func TestSelectorRangeDivergences_SimReplay(t *testing.T) {
	t.Parallel()
	for _, d := range selectorRangeDivergences {
		t.Run(fmt.Sprintf("%s/seed=%d/scenario=%d", d.arm, d.seed, d.scenario), func(t *testing.T) {
			t.Parallel()
			sim := simfdb.New(nil)
			rng := mrand.New(mrand.NewPCG(d.seed, 0))
			var got bool
			for i := 0; i <= d.scenario; i++ {
				if d.arm == "diff" {
					s := nextConflictScenario(rng, d.seed, i)
					if i == d.scenario && s.readMode != 3 {
						t.Fatalf("scenario %d is not a selector range read (%+v): the replay drifted", i, s)
					}
					got = runConflictScenario(t, sim, s)
					continue
				}
				prog := atomicSelectorArm(rng)
				got, _ = prog(t, sim, fmt.Sprintf("arm/%s/%d/%d/%d/", d.arm, os.Getpid(), d.seed, i))
			}
			if got != d.real {
				t.Fatalf("SimFDB committed=%v, real FDB committed=%v", got, d.real)
			}
		})
	}
}

// TestSelectorRangeDivergences_Differential replays the two rotating-seed sweeps against the
// real cluster through their diverging scenario. The fixed seeds (1 and 7) already run in
// TestSimFDB_DifferentialConflictOutcome_FixedSeed and TestDifferentialArm_AtomicSelector/fixed.
func TestSelectorRangeDivergences_Differential(t *testing.T) {
	t.Parallel()
	t.Run("diff", func(t *testing.T) {
		t.Parallel()
		runDifferentialConflictOutcome(t, 1793635174985733078, 2)
	})
	t.Run("atomicsel", func(t *testing.T) {
		t.Parallel()
		runArm(t, "atomicsel", 1793635174996401581, 10, atomicSelectorArm)
	})
}
