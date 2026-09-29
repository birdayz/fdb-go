package recordlayer

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

func TestVectorEngineIdentity(t *testing.T) {
	t.Parallel()
	idx := func(engine string) *Index {
		i := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{IndexOptionVectorNumDimensions: "3"}}
		if engine != "" {
			i.Options[IndexOptionVectorEngine] = engine
		}
		return i
	}
	for engine, want := range map[string]VectorEngineKind{"": VectorEngineHNSW, "hnsw": VectorEngineHNSW, "GuardiANN": VectorEngineGuardiann} {
		if got, err := VectorEngineOf(idx(engine)); err != nil || got != want {
			t.Errorf("%q: %v %v", engine, got, err)
		}
	}
	var mde *MetaDataError
	if _, err := VectorEngineOf(idx("spann")); !errors.As(err, &mde) {
		t.Errorf("unknown engine: %v", err)
	}
	if err := validateVectorIndexOptions(idx(""), idx("GUARDIANN"), map[string]bool{IndexOptionVectorEngine: true}); !errors.As(err, &mde) {
		t.Errorf("engine change: %v", err)
	}
	sub := subspace.Sub("v")
	m, err := newVectorIndexMaintainer(idx("GUARDIANN"), sub, sub, sub, nil, nil)
	if err != nil || m.engine != VectorEngineGuardiann || m.guardiannConfig.primaryClusterMax != 1000 {
		t.Errorf("GuardiANN maintainer: %v", err)
	}
	// Config's own checks refuse what Java's parseConfig refuses.
	bad := idx("GUARDIANN")
	bad.Options[IndexOptionGuardiannCollapseMinDuplicates] = "1000"
	var iae *IllegalArgumentError
	if _, err := newVectorIndexMaintainer(bad, sub, sub, sub, nil, nil); !errors.As(err, &iae) {
		t.Errorf("collapseMinDuplicates >= primaryClusterMax: %v", err)
	}
	if err := validateVectorIndexOptionsAtBuild(bad); !errors.As(err, &mde) || mde.Message != "incorrect index options" {
		t.Errorf("build-time GuardiANN config check: %v", err)
	}
}

func TestGuardiannChangedOptions(t *testing.T) {
	t.Parallel()
	with := func(kv ...string) *Index {
		i := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{
			IndexOptionVectorNumDimensions: "3", IndexOptionVectorEngine: "GUARDIANN",
		}}
		for n := 0; n < len(kv); n += 2 {
			i.Options[kv[n]] = kv[n+1]
		}
		return i
	}
	old := with()
	var mee *MetaDataEvolutionError
	for _, c := range []struct {
		key, val string
		ok       bool
	}{
		{IndexOptionGuardiannPrimaryClusterMax, "500", false},
		{IndexOptionGuardiannPrimaryClusterMin, "100", true}, // its default: no effective change
		{IndexOptionGuardiannConstructionCentroidEfRingSearch, "7", false},
		{IndexOptionVectorMetric, "COSINE_METRIC", false},
		{IndexOptionGuardiannPrimaryClusterHardMax, "3000", true},
		{IndexOptionGuardiannBounceConcurrency, "3", true},
		{IndexOptionHNSWStatsThreshold, "9", true},
	} {
		changed := map[string]bool{c.key: true}
		err := validateVectorIndexOptions(old, with(c.key, c.val), changed)
		if c.ok {
			if err != nil || len(changed) != 0 {
				t.Errorf("%s: %v, left %v", c.key, err, changed)
			}
		} else if !errors.As(err, &mee) {
			t.Errorf("%s: want immutable-option refusal, got %v", c.key, err)
		}
	}
}

func TestGuardiannNaNOptions(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		IndexOptionGuardiannMergeMaxEverFraction, IndexOptionGuardiannMinChildFraction,
		IndexOptionGuardiannMaxRelativeImbalance, IndexOptionGuardiannSplitImbalancePenalty,
	} {
		idx := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{
			IndexOptionVectorNumDimensions: "3", IndexOptionVectorEngine: "GUARDIANN", key: "NaN",
		}}
		var md *MetaDataError
		var argument *IllegalArgumentError
		err := validateVectorIndexOptionsAtBuild(idx)
		if !errors.As(err, &md) || !errors.As(err, &argument) {
			t.Errorf("%s: want wrapped IllegalArgumentError, got %v", key, err)
		}
	}
	nan := math.NaN()
	for _, opts := range []VectorIndexScanOptions{
		{GuardiannCandidatePoolFactor: &nan}, {GuardiannSearchDistanceRatioCutoff: &nan},
	} {
		_, err := opts.guardiannSearchConfig()
		var argument *IllegalArgumentError
		if !errors.As(err, &argument) {
			t.Errorf("NaN scan option accepted: %v", err)
		}
	}
}
