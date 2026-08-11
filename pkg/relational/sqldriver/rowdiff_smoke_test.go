package sqldriver_test

// RFC-182 P1 smoke: the generative row-soundness differential harness
// (pkg/relational/conformance/rowdiff) against the suite's shared FDB
// container. Two layers:
//   - directed template seeds — the deterministic per-family coverage gate
//     (a red template means the family became unreachable, never a flake);
//   - a bounded random-seed slice with Oracle M row-diffing; the histogram
//     is printed as telemetry, with no per-family gate at smoke scale.
// Any mismatch fails with the full ready-to-pin repro in the message.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

const rowdiffSmokeSeeds = 25

// rowdiffSeedRange returns the seed range to sweep. It defaults to the fast
// smoke slice (a FIXED 25 seeds) and is widened by environment for the deep
// runs the RFCs call for — RFC-183's exit criteria ask for >=50k seeds, which
// at roughly 2.7 seeds/s is a multi-hour sweep that must not sit in
// `just test`.
//
//	ROWDIFF_SEEDS=50000 ROWDIFF_SEED_START=1 bazelisk test \
//	  //pkg/relational/sqldriver:sqldriver_test --test_output=streamed \
//	  --test_arg="--test.run=TestFDB_RowDiff_Smoke/random_seeds" \
//	  --test_arg="--test.v" --test_timeout=36000 --nocache_test_results
//
// SEED_START exists so successive deep runs cover FRESH ranges instead of
// re-walking the same seeds; a run that always starts at 1 re-proves what the
// last one already proved.
//
// The DEFAULT start is deliberately CONSTANT, and window rotation belongs to
// nightly-rowdiff.yml alone (which derives the day, passes it as a declared
// --test_env input, runs --nocache_test_results, and sweeps 18000 seeds a
// night). Deriving the default from the wall clock in-process looks like free
// PR-gate coverage but is not:
//
//   - It does not actually rotate. The calendar day is not part of Bazel's
//     action key, so a cached green result from an earlier day is reused and
//     the "fresh window" never executes.
//   - It makes the gate irreproducible. A PR that was green yesterday can go
//     red today from a seed window that has nothing to do with its diff, and
//     the failing seed depends on WHEN the suite ran — the shape of flake
//     this repo treats as a real bug.
//   - The coverage it claims to add is noise next to the nightly: 25 seeds a
//     day against 18000 a night.
//
// A deterministic gate seed and a rotating nightly is the split that gives
// both properties; folding rotation into the gate gave neither.
func rowdiffSeedRange(t *testing.T) (start, count uint64) {
	t.Helper()
	count = rowdiffSmokeSeeds
	start = 1
	if v := os.Getenv("ROWDIFF_SEED_START"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			t.Fatalf("ROWDIFF_SEED_START=%q: want a positive integer", v)
		}
		start = n
	}
	if v := os.Getenv("ROWDIFF_SEEDS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			t.Fatalf("ROWDIFF_SEEDS=%q: want a positive integer", v)
		}
		count = n
	}
	return start, count
}

// defaultMaxConsecutiveInfra bounds how long a sweep will keep paying a full
// context deadline per seed to learn the cluster is still gone.
//
// The value is chosen against the MEASURED cost of being wrong in each
// direction. Too high is what shipped: at the observed 60s-per-dead-seed, the
// 2026-08-10 nightly burned 4h21m of a two-runner fleet after its cluster died,
// and five further nights did the same. Too low would abort a healthy sweep on
// a transient, which is the worse failure — it would retrain readers to ignore
// the abort. Ten consecutive is comfortably outside transient range (a healthy
// sweep reaches zero INFRA, and the counter RESETS on any seed that measures
// something) while capping the wasted runner time at ~10 minutes instead of
// ~5 hours. It is the first-time-it-happens signal the old budget could not give.
const defaultMaxConsecutiveInfra = 10

// rowdiffLimits is the sweep's stopping policy, held as explicit state so
// `stop` is a pure function and every arm can be driven from a unit test. The
// arms only fire when a cluster dies or a range overruns, which is precisely
// what a corpus run cannot be relied on to reach — the same reasoning that
// split `vacuous` out below.
type rowdiffLimits struct {
	maxConsecutiveInfra int           // <=0 disables the breaker
	budget              time.Duration // <=0 disables the wall-clock bound
	minSeeds            uint64        // coverage floor; <=0 disables it
}

// rowdiffSweepLimits reads the policy from the environment.
//
// The breaker is ALWAYS ON — it needs no configuration to protect a run, and a
// knob that must be set to be safe is a knob that will be unset somewhere. The
// budget defaults to unbounded because its correct value is a property of the
// seed range the CALLER chose, which only the caller knows: 25 seeds in the PR
// smoke and 18000 in the nightly do not share a sane bound. nightly-rowdiff.yml
// declares it as a test_env input next to the seed count it belongs to, and
// pkg/docscheck's TestRowdiffSweepBudgetBoundsTheTimeout pins that it stays
// declared and stays below the Bazel timeout.
func rowdiffSweepLimits(t *testing.T) rowdiffLimits {
	t.Helper()
	lim := rowdiffLimits{maxConsecutiveInfra: defaultMaxConsecutiveInfra}
	if v := os.Getenv("ROWDIFF_BUDGET"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("ROWDIFF_BUDGET=%q: want a positive Go duration such as 3h30m", v)
		}
		lim.budget = d
	}
	// The coverage floor is PER LANE because per-seed cost is: the paged lane
	// measured 3.73x the un-paged lane's cost on one tree, and a single shared
	// floor sized for the fast lane would fail the slow one every night for
	// doing exactly what it was asked to do. It only has a default when a budget
	// makes truncation possible at all — an unbounded sweep either finishes its
	// range or dies, and neither needs a floor.
	if lim.budget > 0 {
		lim.minSeeds = defaultMinSeedCoverage
	}
	if v := os.Getenv("ROWDIFF_MIN_SEEDS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("ROWDIFF_MIN_SEEDS=%q: want a non-negative integer (0 disables the floor)", v)
		}
		lim.minSeeds = n
	}
	return lim
}

// stop returns the abort reason and whether it is a DEFECT, or "" to keep
// sweeping.
//
// The two arms differ in severity and conflating them was a design error worth
// naming, because it is the same mistake in a new place. A time-boxed
// generative sweep that runs out of clock has not failed — it has covered fewer
// seeds than its upper bound, which is the normal outcome of asking for "as
// many seeds as fit in 3h30m". Making that red would put a permanent failure on
// a healthy nightly and retrain readers to ignore it, which is precisely the
// disease the vacuity floor below was written to avoid. A cluster that has gone
// away IS a defect: the instrument is dark.
//
// So budget exhaustion is a normal, reported termination and the seed count is
// only an upper bound. What guards against the sweep silently degrading to
// nothing is not this arm but the COVERAGE FLOOR, which is a different question
// asked after the loop — "did this night measure enough to be worth anything?"
// rather than "did it finish?". That split also makes the sizing robust to the
// thing that forced it: per-seed cost moves when the generator changes (it moved
// 3.73x for the paged lane when nested cases landed), and no seed count chosen
// today survives that. A clock does.
//
// Both arms are guarded on being CONFIGURED, not merely on being exceeded: a
// zero budget means "no wall-clock bound", and reading it as "a bound of zero"
// would abort every sweep on its first seed.
func (l rowdiffLimits) stop(consecutiveInfra int, elapsed time.Duration) (reason string, fatal bool) {
	if l.maxConsecutiveInfra > 0 && consecutiveInfra >= l.maxConsecutiveInfra {
		return fmt.Sprintf("ROWDIFF_CLUSTER_DEAD: %d seeds in a row failed with INFRA and nothing in "+
			"between measured anything. That is not a flaky seed, it is the cluster gone — every "+
			"further seed would spend a full context deadline rediscovering that, which is how this "+
			"sweep once consumed a CI runner for 4h21m and reported no tally at all. Stopping here so "+
			"the tally below describes the seeds that DID run. Fix the cluster and re-run; read "+
			"nothing into the engine either way.", consecutiveInfra), true
	}
	if l.budget > 0 && elapsed >= l.budget {
		return fmt.Sprintf("ROWDIFF_BUDGET_EXHAUSTED: the sweep spent its %s wall-clock budget. This "+
			"is a normal termination, not a failure: the seed count is an UPPER bound and the clock "+
			"is what actually sizes the night. The seeds walked are reported below and every one of "+
			"them is valid. The coverage floor, not this line, is what fails a night that measured "+
			"too little.", l.budget), false
	}
	return "", false
}

// defaultMinSeedCoverage is the floor below which a truncated sweep stops being
// a shorter night and starts being a broken one.
//
// It exists because budget exhaustion is deliberately NOT an error: without a
// floor, a sweep whose per-seed cost regressed tenfold would quietly walk 200
// seeds a night and report a clean green forever. The floor is the guard that
// keeps "we ran out of clock" honest, and it is expressed in SEEDS rather than
// as a fraction of the request so that raising ROWDIFF_SEEDS — which only moves
// an upper bound — can never lower the bar the night has to clear.
//
// 2000 is set against measured throughput with room to spare: the un-paged lane
// managed ~1.05 seeds/s on the runner before the nested cases landed, which is
// >13000 seeds in a 3h30m budget, and the slower paged lane still clears 2000
// inside its 70m. A night that cannot reach 2000 has lost most of an order of
// magnitude and is worth a human looking at it.
const defaultMinSeedCoverage = 2000

// coverageFloor returns the complaint for a night that measured too little, or
// "" when it measured enough.
//
// Pure and explicitly parameterised for the same reason as `vacuous` and
// `stop`: a corpus run on a healthy cluster never reaches it, so it has to be
// driveable from a unit test or its first firing is also its first execution.
// `requested` is respected as a ceiling — asking for 25 seeds in the PR smoke
// slice and walking all 25 is complete coverage, and holding that to a
// thousands-scale floor would fail every developer run.
func coverageFloor(lane string, walked, requested uint64, floor uint64) string {
	// floor==0 disables: an unbounded sweep cannot be truncated, so there is
	// nothing for a floor to catch and a default one would only misfire.
	if floor == 0 || walked >= floor || walked >= requested {
		return ""
	}
	return fmt.Sprintf("ROWDIFF_COVERAGE_FLOOR lane=%s: only %d of %d requested seeds were walked, "+
		"below the floor of %d. A truncated night is normally fine — the clock sizes the sweep and "+
		"the seed count is only a ceiling — but this one measured too little to be worth reading as "+
		"evidence about the engine. Either per-seed cost regressed sharply or the sweep lost most of "+
		"its budget to something; both are findings, and neither should pass as a quiet green.",
		lane, walked, requested, floor)
}

func TestFDB_RowDiff_Smoke(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_rowdiff"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)

	// Layer 1: directed template seeds — plan-family hard gate + row diff.
	for _, tpl := range rowdiff.Templates() {
		tpl := tpl
		t.Run("template_"+tpl.Name, func(t *testing.T) {
			t.Parallel()
			if err := rowdiff.CheckTemplateFamily(tpl); err != nil {
				t.Errorf("family gate: %v", err)
			}
			res := rowdiff.RunCase(ctx, setup, dbPath, clusterFilePath, tpl.Case, "tpl"+tpl.Name, 0)
			reportRowdiff(t, res)
		})
	}

	// Layer 2: random seeds. Fixed base so the smoke slice is stable run to
	// run (nightly covers fresh ranges via -seed-start).
	t.Run("random_seeds", func(t *testing.T) {
		t.Parallel()
		seedStart, seedCount := rowdiffSeedRange(t)
		runRowdiffSweep(t, "random_seeds", seedStart, seedCount, rowdiffSweepLimits(t),
			func(seed uint64) *rowdiff.SeedResult {
				return rowdiff.RunSeed(ctx, setup, dbPath, clusterFilePath, seed)
			})
	})
}

// runRowdiffSweep walks the seed range and reports, and it is the ONLY place
// either sweep decides to stop early.
//
// The seed loop used to have exactly one exit condition — seed exhaustion — and
// that is what turned a dead cluster into a wedged CI runner. MEASURED on the
// 2026-08-10 nightly (run 31350088492): the FDB testcontainer became
// unreachable at 03:09:08 UTC, at which point every remaining seed spent a full
// 60s context deadline inside a TCP connect to an address that no longer
// answered, logged one INFRA line, and moved on to the next seed. The goroutine
// dump the Go alarm printed at the 4h50m mark shows the state precisely: the
// only running test is `TestFDB_RowDiff_Smoke/random_seeds`, blocked in
// `client.(*database).getOrDialConn` → `net.(*netFD).connect` in `[IO wait]`.
// Not deadlocked and not merely slow — making perfect forward progress through
// a range it could no longer measure anything with, at 60s a seed. 18000 seeds
// at that rate is 300 hours, so the run could only ever end by timeout, and it
// did, on six consecutive nights.
//
// The damage was not the red run. It is that NOTHING WAS REPORTED. Both
// instruments this harness has for exactly this situation — the tally and the
// vacuity floor below — run AFTER the loop, so the alarm killed the process
// before either could speak. The 2026-08-10 log contains no ROWDIFF_TALLY line
// at all, and the workflow summary greps for one. An instrument that cannot run
// during the failure it was built to diagnose is not an instrument.
//
// So the loop now carries two independent stops, and both leave by the same
// door — `break`, then report — because the reporting is the point:
//
//   - the INFRA circuit breaker, for a cluster that is gone. Consecutive is
//     load-bearing: an isolated INFRA seed is noise and resets the counter,
//     while N in a row is not a cluster that will recover, and every further
//     seed costs a full deadline to learn nothing.
//   - the wall-clock budget, for the other way a sweep can fail to fit —
//     genuinely too slow for the range it was handed. This is the bound that
//     replaces `--test_timeout` as the CONTROL: a budget that fires inside the
//     test prints the tally and the coverage achieved, where the Bazel timeout
//     can only SIGQUIT the process. The timeout stays as the backstop, and must
//     stay comfortably ABOVE the budget or it takes the decision back.
func runRowdiffSweep(t *testing.T, lane string, seedStart, seedCount uint64, lim rowdiffLimits, run func(seed uint64) *rowdiff.SeedResult) {
	t.Helper()
	start := time.Now()
	histogram := map[string]int{}
	executed := 0
	consecutiveInfra := 0
	var tally rowdiffTally
	var stop string
	var fatal bool
	done := uint64(0)
	for i := uint64(0); i < seedCount; i++ {
		res := run(seedStart + i)
		reportRowdiff(t, res)
		tally.add(res)
		for fam, n := range res.Histogram {
			histogram[fam] += n
		}
		executed += res.Executed
		done++
		if res.Kind == rowdiff.OutcomeInfra {
			consecutiveInfra++
		} else {
			consecutiveInfra = 0
		}
		if stop, fatal = lim.stop(consecutiveInfra, time.Since(start)); stop != "" {
			break
		}
	}
	elapsed := time.Since(start)
	t.Logf("rowdiff: lane=%s seeds %d..%d (%d requested, %d walked), %d comparisons in %s (%.1f seeds/s); plan-family histogram: %v",
		lane, seedStart, seedStart+seedCount-1, seedCount, done, executed, elapsed.Round(time.Millisecond),
		float64(done)/elapsed.Seconds(), histogram)
	if stop != "" {
		// Severity is the stop's own, not the fact of stopping. A cluster that
		// went away is a defect; running out of clock is how a time-boxed sweep
		// is supposed to end.
		msg := fmt.Sprintf("ROWDIFF_ABORTED lane=%s after %d of %d seeds in %s: %s",
			lane, done, seedCount, elapsed.Round(time.Second), stop)
		if fatal {
			t.Errorf("%s", msg)
		} else {
			t.Logf("%s", msg)
		}
	}
	// The floor is checked whether or not the sweep aborted: a night can also
	// measure too little by having its seed count set absurdly low, and that
	// should be caught by the same guard rather than by nobody.
	if msg := coverageFloor(lane, done, seedCount, lim.minSeeds); msg != "" {
		t.Errorf("%s", msg)
	}
	tally.report(t, lane, executed)
}

// TestFDB_RowDiff_Paging is the differential sweep in PAGING mode: every
// generated query runs under a small scanned-rows limit, so the engine
// internally pages (a new Execute per page). It tests CONTINUATION SOUNDNESS
// generatively — resume across a scanned-rows page boundary must return exactly
// the rows a single-pass run does — the class BUG C (paginated DISTINCT
// re-admission) belonged to, which the un-paged sweep above cannot reach
// (queries fit in one page). A mismatch here is a resume-clean-continuation
// bug. Cases have 20..120 rows, so scanLimit=4 yields 5..30 pages with real
// mid-result straddles across DISTINCT / GROUP BY / ORDER BY / joins.
func TestFDB_RowDiff_Paging(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_rowdiff_pg"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)

	seedStart, seedCount := rowdiffSeedRange(t)
	scanLimit := 4
	if v := os.Getenv("ROWDIFF_SCAN_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("ROWDIFF_SCAN_LIMIT=%q: want a positive integer", v)
		}
		scanLimit = n
	}
	t.Logf("rowdiff PAGED: scanLimit=%d", scanLimit)
	runRowdiffSweep(t, "paging", seedStart, seedCount, rowdiffSweepLimits(t),
		func(seed uint64) *rowdiff.SeedResult {
			return rowdiff.RunSeedPaged(ctx, setup, dbPath, clusterFilePath, seed, scanLimit)
		})
}

// rowdiffTally counts the sweep's TYPED outcomes, which nothing downstream of
// RunSeed was reading.
//
// rowdiff already classifies every seed as OK / MISMATCH / INFRA
// (rowdiff.OutcomeKind, RFC-182 §4) and cmd/sql-diff-stress already spends that
// distinction as exit 0/1/2. The Go test — which is what the nightly actually
// runs — collapsed all of it into t.Errorf, so the job conclusion could not
// distinguish "the cluster died" from "the engine returned wrong rows", and a
// reader faced with a wall of INFRA lines reasonably concludes the former.
//
// Measured on the 2026-08-07 nightly (run 31143737482): 257 INFRA lines and 16
// MISMATCH lines in the same log. The mismatches were real — four seeds, a
// redundant in-memory sort the ordering oracle caught — and they were invisible
// under the infra noise, in a sweep that had been red every night for a week
// with nobody reading it. This tally is what makes the finding count a number
// somebody can see rather than a grep somebody has to think to run.
type rowdiffTally struct {
	seeds, ok, mismatch, infra int
	mismatchRows, declines     int
	mismatchSeeds              []uint64
}

func (a *rowdiffTally) add(res *rowdiff.SeedResult) {
	a.seeds++
	a.declines += len(res.Declines)
	// Read the TYPED kind rather than re-deriving it from the message text:
	// finalizeResult already made this decision and a second, independent
	// derivation here is how the two drift apart.
	switch res.Kind {
	case rowdiff.OutcomeMismatch:
		a.mismatch++
		a.mismatchRows += len(res.Mismatches)
		a.mismatchSeeds = append(a.mismatchSeeds, res.Seed)
	case rowdiff.OutcomeInfra:
		a.infra++
	default:
		a.ok++
	}
}

// report emits the one greppable line the workflow summary leads with, and
// enforces the VACUITY FLOOR.
//
// The floor is the half that has no equivalent anywhere in this harness today:
// a sweep whose cluster died at seed 3 executes zero comparisons and reports
// red in a way indistinguishable from a sweep that found a soundness bug. Those
// are opposite states — one is the instrument dark, the other is the instrument
// working — and a net that cannot tell them apart trains people to read every
// red as the first. The framing is borrowed rather than invented:
// cmd/factory-run's batchExit already says "a factory run that writes no test
// is a broken pipeline, not a quiet success", and cmd/fuzzrun's summary already
// flips to an error when its benign class swallows the run.
//
// Deliberately NOT an abstain/decline: with real mismatches present in the very
// run that motivated this, a mechanism that downgraded an all-INFRA sweep to
// "declined" would have been the wrong fix and would not have made this red go
// away. The red is CORRECT. What was broken is that it said nothing about why.
func (a *rowdiffTally) report(t *testing.T, lane string, executed int) {
	t.Helper()
	t.Logf("ROWDIFF_TALLY lane=%s seeds=%d ok=%d mismatch=%d infra=%d comparisons=%d "+
		"mismatchRows=%d declines=%d mismatchSeeds=%v",
		lane, a.seeds, a.ok, a.mismatch, a.infra, executed,
		a.mismatchRows, a.declines, a.mismatchSeeds)

	if msg := a.vacuous(lane, executed); msg != "" {
		t.Errorf("%s", msg)
	}
}

// vacuous returns the vacuity complaint, or "" when the sweep measured
// something. Split out as a PURE function of explicit state so the floor can be
// pinned from a unit test rather than only from a live sweep: the arm only
// fires when a cluster dies, which is precisely the condition a corpus run
// cannot be relied on to reach.
func (a *rowdiffTally) vacuous(lane string, executed int) string {
	if a.seeds == 0 || executed > 0 {
		return ""
	}
	return fmt.Sprintf("ROWDIFF_VACUOUS lane=%s: %d seeds ran and NOT ONE comparison was executed "+
		"(%d infra, %d mismatch). The sweep proved nothing about the engine — this is the "+
		"instrument dark, not the engine clean, and it is a different failure from a "+
		"soundness finding even though both land as a red job. Fix the cluster and re-run; "+
		"do not read this run as evidence either way.",
		lane, a.seeds, a.infra, a.mismatch)
}

func reportRowdiff(t *testing.T, res *rowdiff.SeedResult) {
	t.Helper()
	for _, pe := range res.PlanErrors {
		t.Logf("seed %d plan-error: %s", res.Seed, pe)
	}
	// Known-gap declines are logged, never silent: a growing bucket here means
	// the ledger is masking more than it should.
	for _, d := range res.Declines {
		t.Logf("seed %d DECLINE: %s", res.Seed, d)
	}
	// Each dimension reports independently of Kind: a seed can hold
	// confirmed mismatches AND a later infra failure. INFRA is not a
	// soundness finding, but the smoke shares the suite's healthy
	// container — hard-fail it so it cannot silently zero out coverage.
	if res.InfraErr != nil {
		t.Errorf("seed %d INFRA: %v", res.Seed, res.InfraErr)
	}
	for _, m := range res.Mismatches {
		t.Errorf("%s", m.String())
	}
}
