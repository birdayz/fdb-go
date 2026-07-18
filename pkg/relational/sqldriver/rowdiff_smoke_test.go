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
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

const rowdiffSmokeSeeds = 25

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
			res := rowdiff.RunCase(ctx, setup, dbPath, clusterFilePath, tpl.Case, "tpl"+tpl.Name)
			reportRowdiff(t, res)
		})
	}

	// Layer 2: random seeds. Fixed base so the smoke slice is stable run to
	// run (nightly covers fresh ranges via -seed-start).
	t.Run("random_seeds", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		histogram := map[string]int{}
		executed := 0
		for seed := uint64(1); seed <= rowdiffSmokeSeeds; seed++ {
			res := rowdiff.RunSeed(ctx, setup, dbPath, clusterFilePath, seed)
			reportRowdiff(t, res)
			for fam, n := range res.Histogram {
				histogram[fam] += n
			}
			executed += res.Executed
		}
		elapsed := time.Since(start)
		t.Logf("rowdiff smoke: %d seeds, %d comparisons in %s (%.1f seeds/s); plan-family histogram: %v",
			rowdiffSmokeSeeds, executed, elapsed.Round(time.Millisecond),
			float64(rowdiffSmokeSeeds)/elapsed.Seconds(), histogram)
	})
}

func reportRowdiff(t *testing.T, res *rowdiff.SeedResult) {
	t.Helper()
	switch res.Kind {
	case rowdiff.OutcomeInfra:
		// INFRA is not a soundness finding, but the smoke shares the suite's
		// healthy container — treat it as a hard failure here so it cannot
		// silently zero out coverage.
		t.Errorf("seed %d INFRA: %v", res.Seed, res.InfraErr)
	case rowdiff.OutcomeMismatch:
		for _, m := range res.Mismatches {
			t.Errorf("%s", m.String())
		}
	}
}
