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
		start := time.Now()
		histogram := map[string]int{}
		executed := 0
		var tally rowdiffTally
		for i := uint64(0); i < seedCount; i++ {
			res := rowdiff.RunSeed(ctx, setup, dbPath, clusterFilePath, seedStart+i)
			reportRowdiff(t, res)
			tally.add(res)
			for fam, n := range res.Histogram {
				histogram[fam] += n
			}
			executed += res.Executed
		}
		elapsed := time.Since(start)
		t.Logf("rowdiff: seeds %d..%d (%d), %d comparisons in %s (%.1f seeds/s); plan-family histogram: %v",
			seedStart, seedStart+seedCount-1, seedCount, executed, elapsed.Round(time.Millisecond),
			float64(seedCount)/elapsed.Seconds(), histogram)
		tally.report(t, "random_seeds", executed)
	})
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
	start := time.Now()
	histogram := map[string]int{}
	executed := 0
	var tally rowdiffTally
	for i := uint64(0); i < seedCount; i++ {
		res := rowdiff.RunSeedPaged(ctx, setup, dbPath, clusterFilePath, seedStart+i, scanLimit)
		reportRowdiff(t, res)
		tally.add(res)
		for fam, n := range res.Histogram {
			histogram[fam] += n
		}
		executed += res.Executed
	}
	t.Logf("rowdiff PAGED(scanLimit=%d): seeds %d..%d (%d), %d comparisons in %s; plan-family histogram: %v",
		scanLimit, seedStart, seedStart+seedCount-1, seedCount, executed,
		time.Since(start).Round(time.Millisecond), histogram)
	tally.report(t, "paging", executed)
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
