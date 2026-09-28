package recordlayer

import "strings"

// IndexOptionVectorEngine selects a VECTOR index's engine (Java
// IndexOptions.VECTOR_ENGINE); absent means HNSW.
const IndexOptionVectorEngine = "vectorEngine"

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

// GuardiANN option names (Java IndexOptions.GUARDIANN_*).
const (
	IndexOptionGuardiannPrimaryClusterMin                = "guardiannPrimaryClusterMin"
	IndexOptionGuardiannPrimaryClusterMax                = "guardiannPrimaryClusterMax"
	IndexOptionGuardiannPrimaryClusterHardMax            = "guardiannPrimaryClusterHardMax"
	IndexOptionGuardiannUnderreplicatedPrimaryClusterMax = "guardiannUnderreplicatedPrimaryClusterMax"
	IndexOptionGuardiannReplicatedClusterMaxWrites       = "guardiannReplicatedClusterMaxWrites"
	IndexOptionGuardiannReplicatedClusterTarget          = "guardiannReplicatedClusterTarget"
	IndexOptionGuardiannReplicationPriorityMin           = "guardiannReplicationPriorityMin"
	IndexOptionGuardiannInsertMaxCandidateClusters       = "guardiannInsertMaxCandidateClusters"
	IndexOptionGuardiannDeleteMaxCandidateClusters       = "guardiannDeleteMaxCandidateClusters"
	IndexOptionGuardiannSplitNumNearestClusters          = "guardiannSplitNumNearestClusters"
	IndexOptionGuardiannMergeNumNearestClusters          = "guardiannMergeNumNearestClusters"
	IndexOptionGuardiannReassignNumNeighboringClusters   = "guardiannReassignNumNeighboringClusters"
	IndexOptionGuardiannCollapseMinDuplicates            = "guardiannCollapseMinDuplicates"
)

// UnsupportedVectorEngineError refuses to maintain or read a vector index
// whose engine Go does not implement.
type UnsupportedVectorEngineError struct {
	Index  string
	Engine VectorEngineKind
}

func (e *UnsupportedVectorEngineError) Error() string {
	return "vector index " + e.Index + ": the " + e.Engine.String() + " vector engine is not supported"
}
