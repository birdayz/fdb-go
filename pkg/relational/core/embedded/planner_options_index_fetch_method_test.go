package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/relational/api"
)

// TestPlannerOptions_IndexFetchMethod pins OptionsUtils.getIndexFetchMethod's
// mapping for an option that is set, on the connection's options or a parent,
// and Go's SCAN_AND_FETCH where it is not: Java's default would be
// USE_REMOTE_FETCH_WITH_FALLBACK (DIVERGENCES.md).
func TestPlannerOptions_IndexFetchMethod(t *testing.T) {
	t.Parallel()
	set := func(v any) *api.Options { return api.NewOptionsBuilder().Set(api.OptIndexFetchMethod, v).Build() }
	parent, err := set(api.IndexFetchUseRemoteFetch).WithChild(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		opts *api.Options
		want indexfetch.Method
	}{
		"nil":                  {nil, indexfetch.ScanAndFetch},
		"no options":           {api.NoOptions(), indexfetch.ScanAndFetch},
		"other option only":    {api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build(), indexfetch.ScanAndFetch},
		"explicit nil":         {set(nil), indexfetch.ScanAndFetch},
		"SCAN_AND_FETCH":       {set(api.IndexFetchScanAndFetch), indexfetch.ScanAndFetch},
		"USE_REMOTE_FETCH":     {set(api.IndexFetchUseRemoteFetch), indexfetch.UseRemoteFetch},
		"WITH_FALLBACK":        {set(api.IndexFetchUseRemoteFetchWithFallback), indexfetch.UseRemoteFetchWithFallback},
		"set on parent":        {parent, indexfetch.UseRemoteFetch},
		"default value object": {api.NewOptionsBuilder().Build(), indexfetch.ScanAndFetch},
	} {
		if got := plannerOptionsFrom(c.opts).config.IndexFetchMethod; got != c.want {
			t.Errorf("%s: planner fetch method = %v, want %v", name, got, c.want)
		}
	}
	if def := api.DefaultOptionValues()[api.OptIndexFetchMethod]; def != api.IndexFetchUseRemoteFetchWithFallback {
		t.Fatalf("option default = %v; Java's Options default map must stay byte-identical", def)
	}
}

// TestPlannerOptions_CacheKeyPart_IndexFetchMethod pins Java's QueryCacheKey,
// which carries the whole PlannerConfiguration, fetch method included: each
// method keys its own plans, so a connection never runs a plan stamped with
// another connection's method. SCAN_AND_FETCH, set or not, keeps the default
// key unchanged.
func TestPlannerOptions_CacheKeyPart_IndexFetchMethod(t *testing.T) {
	t.Parallel()
	part := func(b *api.OptionsBuilder) string { return plannerOptionsFrom(b.Build()).cacheKeyPart() }
	method := func(m api.IndexFetchMethod) *api.OptionsBuilder {
		return api.NewOptionsBuilder().Set(api.OptIndexFetchMethod, m)
	}
	if got := part(method(api.IndexFetchScanAndFetch)); got != "" {
		t.Fatalf("explicit SCAN_AND_FETCH keys %q, want the default's empty part", got)
	}
	seen := map[string]string{}
	for label, got := range map[string]string{
		"default":                     part(api.NewOptionsBuilder()),
		"remote":                      part(method(api.IndexFetchUseRemoteFetch)),
		"fallback":                    part(method(api.IndexFetchUseRemoteFetchWithFallback)),
		"right-deep":                  part(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true)),
		"right-deep remote":           part(method(api.IndexFetchUseRemoteFetch).Set(api.OptPlanRightDeep, true)),
		"right-deep fallback":         part(method(api.IndexFetchUseRemoteFetchWithFallback).Set(api.OptPlanRightDeep, true)),
		"hnsw remote":                 part(method(api.IndexFetchUseRemoteFetch).Set(api.OptVectorIndexEnginePreference, api.VectorIndexPreferHNSW)),
		"hnsw":                        part(api.NewOptionsBuilder().Set(api.OptVectorIndexEnginePreference, api.VectorIndexPreferHNSW)),
		"rule named like the marker":  part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"fr"})),
		"remote and a rule":           part(method(api.IndexFetchUseRemoteFetch).Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"})),
		"rule only":                   part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"})),
		"fallback and a rule":         part(method(api.IndexFetchUseRemoteFetchWithFallback).Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"})),
		"rule named like both marker": part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"fw"})),
	} {
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q share cache key part %q — one would serve the other's plan", label, prev, got)
		}
		seen[got] = label
	}
}
