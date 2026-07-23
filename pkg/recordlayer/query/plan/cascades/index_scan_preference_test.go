package cascades

import "testing"

// TestPrimaryVsIndexVerdict_HonorsIndexScanPreference pins RFC-189 F3
// (finding 3-followup): the cost model's primary-scan-vs-index-scan tie-break
// now consults the IndexScanPreference config (mirroring Java's
// comparePrimaryScanToIndexScan config branch), instead of hardcoding
// PREFER_SCAN. With no type filter on the primary the SARG sub-case is skipped,
// so the config branch decides: PREFER_SCAN → prefer the primary (-1), the
// non-scan preferences → prefer the index (+1).
func TestPrimaryVsIndexVerdict_HonorsIndexScanPreference(t *testing.T) {
	t.Parallel()
	// typeFilterCount 0 on both sides → the SARG sub-case does not fire.
	ops := expressionCounts{}

	cases := []struct {
		pref IndexScanPreference
		want int
	}{
		{PreferScan, -1},
		{PreferIndex, 1},
		{PreferPrimaryKeyIndex, 1},
	}
	for _, tc := range cases {
		if got := primaryVsIndexVerdict(nil, nil, ops, ops, tc.pref); got != tc.want {
			t.Fatalf("primaryVsIndexVerdict(pref=%v) = %d, want %d", tc.pref, got, tc.want)
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
