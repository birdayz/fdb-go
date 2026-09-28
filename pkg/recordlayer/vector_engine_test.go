package recordlayer

import (
	"errors"
	"testing"
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
	var unsupported *UnsupportedVectorEngineError
	if _, err := newVectorIndexMaintainer(idx("GUARDIANN"), nil, nil, nil, nil); !errors.As(err, &unsupported) {
		t.Errorf("GuardiANN maintainer: %v", err)
	}
}
