package recordlayer

import (
	"errors"
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
}
