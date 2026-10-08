package recordlayer

import "strings"

// VectorEngineKind is Java's VectorIndexEngineKind.
type VectorEngineKind int

const (
	VectorEngineHNSW VectorEngineKind = iota
	VectorEngineGuardiann
)

func (k VectorEngineKind) String() string {
	if k == VectorEngineGuardiann {
		return "GUARDIANN"
	}
	return "HNSW"
}

// VectorEngineOf is VectorIndexEngineKind.fromOptionValue over the index's
// vectorEngine option: case-insensitive, unknown values refused.
func VectorEngineOf(index *Index) (VectorEngineKind, error) {
	v, ok := index.Options[IndexOptionVectorEngine]
	if !ok {
		return VectorEngineHNSW, nil
	}
	switch strings.ToUpper(v) {
	case "HNSW":
		return VectorEngineHNSW, nil
	case "GUARDIANN":
		return VectorEngineGuardiann, nil
	}
	return VectorEngineHNSW, &MetaDataError{Message: "unknown vector index engine: " + v}
}
