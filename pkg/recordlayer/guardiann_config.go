package recordlayer

import (
	"math"
	"strings"
)

// guardiannSearchConfig is Java's guardiann.SearchConfig.
type guardiannSearchConfig struct {
	candidatePoolFactor            float64
	searchMaxClusters              int
	searchMinClustersBeforePruning int
	searchDistanceRatioCutoff      float64
	centroidEfRingSearch           int
	centroidEfOutwardSearch        int
	searchConcurrency              int
}

func defaultGuardiannSearchConfig() guardiannSearchConfig {
	return guardiannSearchConfig{
		candidatePoolFactor:            1.40,
		searchMaxClusters:              48,
		searchMinClustersBeforePruning: 16,
		searchDistanceRatioCutoff:      1.5,
		centroidEfRingSearch:           100,
		centroidEfOutwardSearch:        400,
		searchConcurrency:              10,
	}
}

func (s guardiannSearchConfig) validate() error {
	switch {
	case s.candidatePoolFactor < 1:
		return &IllegalArgumentError{Message: "candidatePoolFactor must be >= 1.0"}
	case s.searchMaxClusters < 1:
		return &IllegalArgumentError{Message: "searchMaxClusters must be >= 1"}
	case s.searchMinClustersBeforePruning < 0:
		return &IllegalArgumentError{Message: "searchMinClustersBeforePruning must be >= 0"}
	case s.searchDistanceRatioCutoff < 1:
		return &IllegalArgumentError{Message: "searchDistanceRatioCutoff must be >= 1.0"}
	case s.centroidEfRingSearch < 1:
		return &IllegalArgumentError{Message: "centroidEfRingSearch must be >= 1"}
	case s.centroidEfOutwardSearch < 1:
		return &IllegalArgumentError{Message: "centroidEfOutwardSearch must be >= 1"}
	case s.searchConcurrency < 1:
		return &IllegalArgumentError{Message: "searchConcurrency must be >= 1"}
	}
	return nil
}

// candidatePoolSize is SearchConfig.candidatePoolSize.
func (s guardiannSearchConfig) candidatePoolSize(k int) int {
	return max(k, int(math.Ceil(s.candidatePoolFactor*float64(k))))
}

// guardiannConfig is Java's guardiann.Config.
type guardiannConfig struct {
	metric                           VectorMetric
	numDimensions                    int
	primaryClusterMin                int
	primaryClusterMax                int
	primaryClusterHardMax            int
	underreplicatedPrimaryClusterMax int
	replicatedClusterMaxWrites       int
	replicatedClusterTarget          int
	replicationPriorityMin           float64
	replicationDistanceRatioWeight   float64
	replicationZScoreWeight          float64
	replicationStatsMinSampleSize    int
	sampleVectorStatsProbability     float64
	maintainStatsProbability         float64
	statsThreshold                   int
	useRaBitQ                        bool
	raBitQNumExBits                  int
	deterministicRandomness          bool
	sampleBatchSize                  int
	insertMaxCandidateClusters       int
	deleteMaxCandidateClusters       int
	deleteConcurrency                int
	splitNumNearestClusters          int
	mergeNumNearestClusters          int
	kMeansMaxIterations              int
	kMeansMaxRestarts                int
	reassignNumNeighboringClusters   int
	collapseMinDuplicates            int
	splitMergeConcurrency            int
	reassignConcurrency              int
	collapseConcurrency              int
	bounceConcurrency                int
	constructionSearchConfig         guardiannSearchConfig
	mergeMaxEverFraction             float64
	minChildFraction                 float64
	maxRelativeImbalance             float64
	splitImbalancePenalty            float64
}

// defaultGuardiannConfig is Config.ConfigBuilder's defaults.
func defaultGuardiannConfig(numDimensions int) guardiannConfig {
	const clusterMax = 1000
	return guardiannConfig{
		metric:                           VectorMetricEuclidean,
		numDimensions:                    numDimensions,
		primaryClusterMin:                clusterMax / 10,
		primaryClusterMax:                clusterMax,
		primaryClusterHardMax:            2 * clusterMax,
		underreplicatedPrimaryClusterMax: 50,
		replicatedClusterMaxWrites:       3 * clusterMax / 10,
		replicatedClusterTarget:          clusterMax / 10,
		replicationPriorityMin:           0.89,
		replicationDistanceRatioWeight:   1.0,
		replicationZScoreWeight:          0.0,
		replicationStatsMinSampleSize:    200,
		sampleVectorStatsProbability:     0.5,
		maintainStatsProbability:         0.05,
		statsThreshold:                   1000,
		raBitQNumExBits:                  4,
		sampleBatchSize:                  50,
		insertMaxCandidateClusters:       10,
		deleteMaxCandidateClusters:       10,
		deleteConcurrency:                10,
		splitNumNearestClusters:          32,
		mergeNumNearestClusters:          11,
		kMeansMaxIterations:              8,
		kMeansMaxRestarts:                3,
		reassignNumNeighboringClusters:   31,
		collapseMinDuplicates:            100,
		splitMergeConcurrency:            10,
		reassignConcurrency:              10,
		collapseConcurrency:              10,
		bounceConcurrency:                10,
		constructionSearchConfig:         defaultGuardiannSearchConfig(),
		mergeMaxEverFraction:             1.0 / 5.0,
		minChildFraction:                 float64(clusterMax/10) / float64(clusterMax),
		maxRelativeImbalance:             0.36,
		splitImbalancePenalty:            3.0,
	}
}

// validate is Config's compact-constructor checks.
func (c guardiannConfig) validate() error {
	switch {
	case c.numDimensions < 1:
		return &IllegalArgumentError{Message: "numDimensions must be >= 1"}
	case c.collapseMinDuplicates >= c.primaryClusterMax:
		return &IllegalArgumentError{Message: "collapseMinDuplicates must be < primaryClusterMax"}
	case c.primaryClusterHardMax <= c.primaryClusterMax:
		return &IllegalArgumentError{Message: "primaryClusterHardMax must be > primaryClusterMax"}
	case c.mergeMaxEverFraction < 0 || c.mergeMaxEverFraction >= 1:
		return &IllegalArgumentError{Message: "mergeMaxEverFraction must be in [0, 1)"}
	case c.minChildFraction < 0 || c.minChildFraction >= 0.5:
		return &IllegalArgumentError{Message: "minChildFraction must be in [0, 0.5)"}
	case c.maxRelativeImbalance < 0 || c.maxRelativeImbalance > 1:
		return &IllegalArgumentError{Message: "maxRelativeImbalance must be in [0, 1]"}
	case c.splitImbalancePenalty < 0:
		return &IllegalArgumentError{Message: "splitImbalancePenalty must be >= 0"}
	}
	return c.constructionSearchConfig.validate()
}

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

// parseGuardiannConfig is GuardiannVectorIndexEngine.parseConfig. The shared
// vector options (metric, dimensions, stats, RaBitQ) read their canonical
// name or alias as the HNSW engine does.
func parseGuardiannConfig(index *Index) (guardiannConfig, error) {
	name, err := hnswMetric(index, false)
	if err != nil {
		return guardiannConfig{}, err
	}
	v, ok := hnswOptionValue(index, IndexOptionVectorNumDimensions)
	if !ok {
		return guardiannConfig{}, &MetaDataError{Message: "need to specify the number of dimensions"}
	}
	dims, err := javaParseInt(v)
	if err != nil {
		return guardiannConfig{}, err
	}
	c := defaultGuardiannConfig(int(dims))
	c.metric = vectorMetricNamed(name)
	read := func(canonical string) (string, bool) { return hnswOptionValue(index, canonical) }
	intOpt := func(name string, set *int) {
		if v, ok := read(name); ok && err == nil {
			var n int32
			if n, err = javaParseInt(v); err == nil {
				*set = int(n)
			}
		}
	}
	floatOpt := func(name string, set *float64) {
		if v, ok := read(name); ok && err == nil {
			var f float64
			if f, err = javaParseDouble(v); err == nil {
				*set = f
			}
		}
	}
	boolOpt := func(name string, set *bool) {
		if v, ok := read(name); ok {
			*set = strings.EqualFold(v, "true")
		}
	}
	floatOpt(IndexOptionHNSWSampleVectorStatsProbability, &c.sampleVectorStatsProbability)
	floatOpt(IndexOptionHNSWMaintainStatsProbability, &c.maintainStatsProbability)
	intOpt(IndexOptionHNSWStatsThreshold, &c.statsThreshold)
	boolOpt(IndexOptionHNSWUseRaBitQ, &c.useRaBitQ)
	intOpt(IndexOptionHNSWRaBitQNumExBits, &c.raBitQNumExBits)
	intOpt(IndexOptionGuardiannPrimaryClusterMin, &c.primaryClusterMin)
	floatOpt(IndexOptionGuardiannMergeMaxEverFraction, &c.mergeMaxEverFraction)
	floatOpt(IndexOptionGuardiannMinChildFraction, &c.minChildFraction)
	floatOpt(IndexOptionGuardiannMaxRelativeImbalance, &c.maxRelativeImbalance)
	floatOpt(IndexOptionGuardiannSplitImbalancePenalty, &c.splitImbalancePenalty)
	intOpt(IndexOptionGuardiannPrimaryClusterMax, &c.primaryClusterMax)
	intOpt(IndexOptionGuardiannPrimaryClusterHardMax, &c.primaryClusterHardMax)
	intOpt(IndexOptionGuardiannUnderreplicatedPrimaryClusterMax, &c.underreplicatedPrimaryClusterMax)
	intOpt(IndexOptionGuardiannReplicatedClusterMaxWrites, &c.replicatedClusterMaxWrites)
	intOpt(IndexOptionGuardiannReplicatedClusterTarget, &c.replicatedClusterTarget)
	floatOpt(IndexOptionGuardiannReplicationPriorityMin, &c.replicationPriorityMin)
	floatOpt(IndexOptionGuardiannReplicationDistanceRatioWeight, &c.replicationDistanceRatioWeight)
	floatOpt(IndexOptionGuardiannReplicationZScoreWeight, &c.replicationZScoreWeight)
	intOpt(IndexOptionGuardiannReplicationStatsMinSampleSize, &c.replicationStatsMinSampleSize)
	boolOpt(IndexOptionGuardiannDeterministicRandomness, &c.deterministicRandomness)
	intOpt(IndexOptionGuardiannSampleBatchSize, &c.sampleBatchSize)
	intOpt(IndexOptionGuardiannInsertMaxCandidateClusters, &c.insertMaxCandidateClusters)
	intOpt(IndexOptionGuardiannDeleteMaxCandidateClusters, &c.deleteMaxCandidateClusters)
	intOpt(IndexOptionGuardiannDeleteConcurrency, &c.deleteConcurrency)
	intOpt(IndexOptionGuardiannSplitNumNearestClusters, &c.splitNumNearestClusters)
	intOpt(IndexOptionGuardiannMergeNumNearestClusters, &c.mergeNumNearestClusters)
	intOpt(IndexOptionGuardiannKMeansMaxIterations, &c.kMeansMaxIterations)
	intOpt(IndexOptionGuardiannKMeansMaxRestarts, &c.kMeansMaxRestarts)
	intOpt(IndexOptionGuardiannReassignNumNeighboringClusters, &c.reassignNumNeighboringClusters)
	intOpt(IndexOptionGuardiannCollapseMinDuplicates, &c.collapseMinDuplicates)
	intOpt(IndexOptionGuardiannSplitMergeConcurrency, &c.splitMergeConcurrency)
	intOpt(IndexOptionGuardiannReassignConcurrency, &c.reassignConcurrency)
	intOpt(IndexOptionGuardiannCollapseConcurrency, &c.collapseConcurrency)
	intOpt(IndexOptionGuardiannBounceConcurrency, &c.bounceConcurrency)
	intOpt(IndexOptionGuardiannConstructionCentroidEfRingSearch, &c.constructionSearchConfig.centroidEfRingSearch)
	intOpt(IndexOptionGuardiannConstructionCentroidEfOutwardSearch, &c.constructionSearchConfig.centroidEfOutwardSearch)
	if err != nil {
		return c, err
	}
	return c, c.validate()
}
