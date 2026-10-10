// Portions derived from FoundationDB Record Layer (IndexOptions.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

// Vector index option names. They live in the core package, as Java's
// IndexOptions does, so meta-data code can name them without linking the
// vector index implementation (package vectorindex).

// IndexOptionVectorNumDimensions specifies the number of vector dimensions.
// Matches Java's IndexOptions.HNSW_NUM_DIMENSIONS.
const IndexOptionVectorNumDimensions = "hnswNumDimensions"

// IndexOptionVectorMetric specifies the distance metric.
// Matches Java's IndexOptions.HNSW_METRIC.
const IndexOptionVectorMetric = "hnswMetric"

// IndexOptionVectorExtendCandidates controls whether the candidate set is extended
// with neighbors-of-neighbors during neighbor selection (2nd-degree exploration).
// Matches Java's IndexOptions.HNSW_EXTEND_CANDIDATES.
const IndexOptionVectorExtendCandidates = "hnswExtendCandidates"

// IndexOptionVectorKeepPrunedConnections controls whether pruned candidates are
// added back to fill up to M neighbors when the heuristic selection produces too few.
// Matches Java's IndexOptions.HNSW_KEEP_PRUNED_CONNECTIONS.
const IndexOptionVectorKeepPrunedConnections = "hnswKeepPrunedConnections"

// IndexOptionHNSWMaxNumConcurrentNodeFetches controls the maximum number of
// concurrent node fetches during search and modification operations.
// In Go's synchronous model this is not used for concurrency control, but is
// stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_NODE_FETCHES.
const IndexOptionHNSWMaxNumConcurrentNodeFetches = "hnswMaxNumConcurrentNodeFetches"

// IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches controls the maximum number of
// concurrent neighborhood fetches during insert when neighbors are pruned.
// Stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_NEIGHBORHOOD_FETCHES.
const IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches = "hnswMaxNumConcurrentNeighborhoodFetches"

// IndexOptionHNSWMaxNumConcurrentDeleteFromLayer controls the maximum number of
// concurrent layer deletions during deletion of a record.
// Stored for Java round-trip compatibility.
// Matches Java's IndexOptions.HNSW_MAX_NUM_CONCURRENT_DELETE_FROM_LAYER.
const IndexOptionHNSWMaxNumConcurrentDeleteFromLayer = "hnswMaxNumConcurrentDeleteFromLayer"

// IndexOptionHNSWM specifies the connectivity factor M for the HNSW graph.
// Matches Java's IndexOptions.HNSW_M.
const IndexOptionHNSWM = "hnswM"

// IndexOptionHNSWMMax specifies the maximum number of connections for non-zero layers.
// Matches Java's IndexOptions.HNSW_M_MAX.
const IndexOptionHNSWMMax = "hnswMMax"

// IndexOptionHNSWMMax0 specifies the maximum number of connections for layer 0.
// Matches Java's IndexOptions.HNSW_M_MAX_0.
const IndexOptionHNSWMMax0 = "hnswMMax0"

// IndexOptionHNSWEfConstruction specifies the search factor used during index construction.
// Matches Java's IndexOptions.HNSW_EF_CONSTRUCTION.
const IndexOptionHNSWEfConstruction = "hnswEfConstruction"

// IndexOptionHNSWUseInlining controls whether vector data is inlined into the HNSW node.
// Matches Java's IndexOptions.HNSW_USE_INLINING.
const IndexOptionHNSWUseInlining = "hnswUseInlining"

// IndexOptionHNSWEfRepair specifies the search factor used during repair operations.
// Matches Java's IndexOptions.HNSW_EF_REPAIR.
const IndexOptionHNSWEfRepair = "hnswEfRepair"

// IndexOptionHNSWUseRaBitQ enables RaBitQ quantization for approximate nearest neighbor.
// Matches Java's IndexOptions.HNSW_USE_RABITQ.
const IndexOptionHNSWUseRaBitQ = "hnswUseRaBitQ"

// IndexOptionHNSWRaBitQNumExBits specifies the number of extra bits for RaBitQ.
// Matches Java's IndexOptions.HNSW_RABITQ_NUM_EX_BITS.
const IndexOptionHNSWRaBitQNumExBits = "hnswRaBitQNumExBits"

// IndexOptionHNSWSampleVectorStatsProbability controls the probability of sampling vector stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_SAMPLE_VECTOR_STATS_PROBABILITY.
const IndexOptionHNSWSampleVectorStatsProbability = "hnswSampleVectorStatsProbability"

// IndexOptionHNSWMaintainStatsProbability controls the probability of maintaining stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_MAINTAIN_STATS_PROBABILITY.
const IndexOptionHNSWMaintainStatsProbability = "hnswMaintainStatsProbability"

// IndexOptionHNSWStatsThreshold specifies the minimum number of vectors for stats.
// Runtime-only option, safe to change without rebuild.
// Matches Java's IndexOptions.HNSW_STATS_THRESHOLD.
const IndexOptionHNSWStatsThreshold = "hnswStatsThreshold"

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

// IndexOptionVectorEngine selects a VECTOR index's engine (Java
// IndexOptions.VECTOR_ENGINE); absent means HNSW.
const IndexOptionVectorEngine = "vectorEngine"

// GuardiANN-only option names beyond those in vector_engine.go.
const (
	IndexOptionGuardiannMergeMaxEverFraction                = "guardiannMergeMaxEverFraction"
	IndexOptionGuardiannMinChildFraction                    = "guardiannMinChildFraction"
	IndexOptionGuardiannMaxRelativeImbalance                = "guardiannMaxRelativeImbalance"
	IndexOptionGuardiannSplitImbalancePenalty               = "guardiannSplitImbalancePenalty"
	IndexOptionGuardiannReplicationDistanceRatioWeight      = "guardiannReplicationDistanceRatioWeight"
	IndexOptionGuardiannReplicationZScoreWeight             = "guardiannReplicationZScoreWeight"
	IndexOptionGuardiannReplicationStatsMinSampleSize       = "guardiannReplicationStatsMinSampleSize"
	IndexOptionGuardiannDeterministicRandomness             = "guardiannDeterministicRandomness"
	IndexOptionGuardiannSampleBatchSize                     = "guardiannSampleBatchSize"
	IndexOptionGuardiannDeleteConcurrency                   = "guardiannDeleteConcurrency"
	IndexOptionGuardiannKMeansMaxIterations                 = "guardiannKMeansMaxIterations"
	IndexOptionGuardiannKMeansMaxRestarts                   = "guardiannKMeansMaxRestarts"
	IndexOptionGuardiannSplitMergeConcurrency               = "guardiannSplitMergeConcurrency"
	IndexOptionGuardiannReassignConcurrency                 = "guardiannReassignConcurrency"
	IndexOptionGuardiannCollapseConcurrency                 = "guardiannCollapseConcurrency"
	IndexOptionGuardiannBounceConcurrency                   = "guardiannBounceConcurrency"
	IndexOptionGuardiannConstructionCentroidEfRingSearch    = "guardiannConstructionCentroidEfRingSearch"
	IndexOptionGuardiannConstructionCentroidEfOutwardSearch = "guardiannConstructionCentroidEfOutwardSearch"
)

// SPFresh index options (RFC-094 §10). All structural options are immutable for
// an existing index — enforced by the metadata-evolution validator — because
// the lifecycle invariants (topology, posting sizes, closure replication) are
// derived from them. Runtime knobs (probe width w, k_c, ε, re-rank C, refresh
// interval, rebalancer pacing) are deliberately NOT index options: they are
// query/maintenance-time parameters and are never stored.
const (
	// IndexOptionSPFreshNumDimensions is the vector dimensionality. Required.
	IndexOptionSPFreshNumDimensions = "spfreshNumDimensions"
	// IndexOptionSPFreshMetric is the distance metric (EUCLIDEAN_METRIC,
	// COSINE_METRIC, DOT_PRODUCT_METRIC — same names the HNSW index accepts).
	IndexOptionSPFreshMetric = "spfreshMetric"
	// IndexOptionSPFreshLmax is the posting-list split threshold in entries.
	// Sized so one posting fits a single range-reply (REPLY_BYTE_LIMIT = 80 KB).
	IndexOptionSPFreshLmax = "spfreshLmax"
	// IndexOptionSPFreshLminRatio divides Lmax to produce the merge threshold.
	IndexOptionSPFreshLminRatio = "spfreshLminRatio"
	// IndexOptionSPFreshCellTarget is the fine-centroids-per-cell build target;
	// sized so one L2 cell load fits a single range-reply.
	IndexOptionSPFreshCellTarget = "spfreshCellTarget"
	// IndexOptionSPFreshCellMax is the coarse-split threshold in fine centroids.
	IndexOptionSPFreshCellMax = "spfreshCellMax"
	// IndexOptionSPFreshReplication is the closure replication cap r.
	IndexOptionSPFreshReplication = "spfreshReplication"
	// IndexOptionSPFreshAlpha is the RNG closure threshold: keep centroid c_i of
	// the r nearest iff dist(v,c_i) <= alpha * dist(v,c_1). Must be > 1.0 or
	// only the nearest centroid is ever admitted (effective r=1).
	IndexOptionSPFreshAlpha = "spfreshAlpha"
	// IndexOptionSPFreshKn is the NPA reassignment neighborhood (centroids).
	IndexOptionSPFreshKn = "spfreshKn"
	// IndexOptionSPFreshBuildAssignCells is the bulk-build wave-B assignment
	// width w_b (RFC-099): how many nearest coarse cells supply candidate fine
	// centroids when assigning an imported vector. Build-time only — it changes
	// which fine a vector is assigned to, never the on-disk format. Must be ≥
	// the query probe width so build assignments are query-reachable.
	IndexOptionSPFreshBuildAssignCells = "spfreshBuildAssignCells"
	// IndexOptionSPFreshCooldownSec is the post-split merge cooldown.
	IndexOptionSPFreshCooldownSec = "spfreshCooldownSec"
	// IndexOptionSPFreshRaBitQNumExBits is the RaBitQ extended-bits parameter
	// for posting residual codes.
	IndexOptionSPFreshRaBitQNumExBits = "spfreshRaBitQNumExBits"
	// IndexOptionSPFreshSidecar enables the fp16 SIDECAR subspace. Default
	// true — and currently REQUIRED: the sidecar is not just the query
	// re-rank source, every rebalancer lifecycle reads it (split 2-means,
	// chunked drain, merge drain, GC re-home), so disabling it would brick
	// maintenance permanently. ValidateSPFreshConfig rejects false until a
	// source-record fallback exists for all of those paths. The option stays
	// (the wire layout reserves the choice); only the value is constrained.
	IndexOptionSPFreshSidecar = "spfreshSidecar"
)
