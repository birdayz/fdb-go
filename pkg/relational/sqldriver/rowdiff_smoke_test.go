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
	"os"
	"strconv"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

const rowdiffSmokeSeeds = 25

// rowdiffSeedRange returns the seed range to sweep. It defaults to the fast
// smoke slice (25 seeds, ROTATED daily — see below) and is widened by
// environment for the deep runs the RFCs call for — RFC-183's exit criteria
// ask for >=50k seeds, which at roughly 2.7 seeds/s is a multi-hour sweep
// that must not sit in `just test`.
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
// The DEFAULT start (no env override — the PR gate's actual path) rotates by
// day-of-year, mirroring nightly-rowdiff.yml's window rotation
// (1 + dayOfYear*budget): a fixed "seeds 1..25 forever" window spends the
// PR gate's entire lifetime re-verifying the SAME 25 cases that already
// passed, proving nothing new after day one. Rotating keeps the budget (and
// therefore the gate's runtime) identical while walking a fresh, disjoint
// 25-seed window every calendar day — free additional coverage at zero extra
// PR latency.
func rowdiffSeedRange(t *testing.T) (start, count uint64) {
	t.Helper()
	count = rowdiffSmokeSeeds
	start = 1 + uint64(time.Now().UTC().YearDay())*count
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
		for i := uint64(0); i < seedCount; i++ {
			res := rowdiff.RunSeed(ctx, setup, dbPath, clusterFilePath, seedStart+i)
			reportRowdiff(t, res)
			for fam, n := range res.Histogram {
				histogram[fam] += n
			}
			executed += res.Executed
		}
		elapsed := time.Since(start)
		t.Logf("rowdiff: seeds %d..%d (%d), %d comparisons in %s (%.1f seeds/s); plan-family histogram: %v",
			seedStart, seedStart+seedCount-1, seedCount, executed, elapsed.Round(time.Millisecond),
			float64(seedCount)/elapsed.Seconds(), histogram)
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
	for i := uint64(0); i < seedCount; i++ {
		res := rowdiff.RunSeedPaged(ctx, setup, dbPath, clusterFilePath, seedStart+i, scanLimit)
		reportRowdiff(t, res)
		for fam, n := range res.Histogram {
			histogram[fam] += n
		}
		executed += res.Executed
	}
	t.Logf("rowdiff PAGED(scanLimit=%d): seeds %d..%d (%d), %d comparisons in %s; plan-family histogram: %v",
		scanLimit, seedStart, seedStart+seedCount-1, seedCount, executed,
		time.Since(start).Round(time.Millisecond), histogram)
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
