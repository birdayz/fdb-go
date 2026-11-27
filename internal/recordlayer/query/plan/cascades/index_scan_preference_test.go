package cascades

import "testing"

// TestPrimaryVsIndexRank_HonorsIndexScanPreference pins RFC-189 F3
// (finding 3-followup): the cost model's primary-scan-vs-index-scan rung
// consults the IndexScanPreference config (mirroring Java's
// comparePrimaryScanToIndexScan config branch) instead of hardcoding
// PREFER_SCAN. With no type filter on the primary, PREFER_SCAN prefers the
// primary (-1) and the non-scan preferences prefer the index (+1).
func TestPrimaryVsIndexRank_HonorsIndexScanPreference(t *testing.T) {
	t.Parallel()
	primaryOps := expressionCounts{scanCount: 1}
	indexOps := expressionCounts{indexScanCount: 1}

	cases := []struct {
		pref IndexScanPreference
		want int
	}{
		{PreferScan, -1},
		{PreferIndex, 1},
		{PreferPrimaryKeyIndex, 1},
	}
	for _, tc := range cases {
		got := comparePrimaryVsIndexRank(
			primaryVsIndexRankOf(nil, primaryOps, tc.pref),
			primaryVsIndexRankOf(nil, indexOps, tc.pref),
		)
		if got != tc.want {
			t.Fatalf("primary-vs-index rung (pref=%v) = %d, want %d", tc.pref, got, tc.want)
		}
	}
}

// TestIndexScanPreferenceOf pins the config-read path (nil ctx → default; a
// PlannerConfiguration carrying a non-default preference is honored end-to-end).
func TestIndexScanPreferenceOf(t *testing.T) {
	t.Parallel()
	if got := indexScanPreferenceOf(nil); got != PreferScan {
		t.Fatalf("nil ctx → PreferScan (default), got %v", got)
	}
	if got := DefaultPlannerConfiguration().IndexScanPreference; got != PreferScan {
		t.Fatalf("Cascades default must be PreferScan, got %v", got)
	}
	ctx := &prefTestCtx{pref: PreferIndex}
	if got := indexScanPreferenceOf(ctx); got != PreferIndex {
		t.Fatalf("config PreferIndex must be read through the ctx, got %v", got)
	}
}

// prefTestCtx is a full PlanContext whose planner configuration carries a
// chosen IndexScanPreference (indexTestPlanContext supplies the rest of the
// interface and hardcodes the default config).
type prefTestCtx struct {
	indexTestPlanContext
	pref IndexScanPreference
}

func (c *prefTestCtx) GetPlannerConfiguration() PlannerConfiguration {
	return PlannerConfiguration{IndexScanPreference: c.pref}
}
