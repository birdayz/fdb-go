package recordlayer

import (
	"errors"
	"fmt"
)

// validateVectorIndexOptionsAtBuild is the option half of Java's VectorIndexValidator
// (VectorIndexMaintainerFactory.java:96-111): VectorIndexHelper.validate, which
// refuses an option given under both its name and its alias (a MetaDataError,
// VectorIndexOptionsHelper.validateNoAliasConflicts) and then parses the HNSW
// configuration as HnswVectorIndexEngine.parseConfig does, Config's checks
// included (readHNSWOptions, without Go's forms); any IllegalArgumentException
// of the parse, a NumberFormatException included, is rethrown as
// MetaDataException("incorrect index options", cause), and a missing dimension
// count is the parse's own MetaDataException. So the windowed index Go builds is
// the one Java builds, and its maintainer (parseHNSWConfig, the same reader)
// reads the numbers Java reads. A GuardiANN index is parsed by
// parseGuardiannConfig, GuardiannVectorIndexEngine.parseConfig.
func validateVectorIndexOptionsAtBuild(idx *Index) error {
	engine, err := VectorEngineOf(idx)
	if err != nil {
		return err
	}
	if err := hnswAliasConflict(idx); err != nil {
		return err
	}
	if engine == VectorEngineGuardiann {
		_, err = parseGuardiannConfig(idx)
	} else {
		_, err = readHNSWOptions(idx, false)
	}
	if err != nil {
		var iae *IllegalArgumentError
		if errors.As(err, &iae) {
			return &MetaDataError{Message: "incorrect index options", Cause: err}
		}
		return err
	}
	return nil
}

// validateSPFreshIndexOptions enforces RFC-094 §10: every structural SPFresh
// option is immutable for an existing index — the lifecycle invariants
// (topology, posting sizes, closure replication, single-tx split budget) are
// derived from them, and a changed value would silently invalidate data
// written under the old one (the PR #278 lesson: immutability is what makes
// config-derived invariants sound). There are deliberately NO runtime-mutable
// SPFresh options: query/maintenance knobs are never stored.
func validateSPFreshIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	structural := []string{
		IndexOptionSPFreshNumDimensions,
		IndexOptionSPFreshMetric,
		IndexOptionSPFreshLmax,
		IndexOptionSPFreshLminRatio,
		IndexOptionSPFreshCellTarget,
		IndexOptionSPFreshCellMax,
		IndexOptionSPFreshReplication,
		IndexOptionSPFreshAlpha,
		IndexOptionSPFreshKn,
		IndexOptionSPFreshCooldownSec,
		IndexOptionSPFreshRaBitQNumExBits,
		IndexOptionSPFreshSidecar,
	}
	for _, key := range structural {
		if !changed[key] {
			continue
		}
		oldVal := optionValueOrDefault(oldIdx.Options, key, "")
		newVal := optionValueOrDefault(newIdx.Options, key, "")
		if oldVal != newVal {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("SPFresh option %q changed for index %q", key, newIdx.Name),
			}
		}
		delete(changed, key)
	}
	return nil
}

// validateVectorIndexOptions validates VECTOR (HNSW) index option changes.
// Structural options (metric, dimensions, graph parameters) cannot change.
// Runtime-only options (concurrency limits, stats) are safe to change.
// Matches Java's VectorIndexValidator.validateChangedOptions().
func validateVectorIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	// The engine never changes: it would reinterpret the stored layout.
	oldEngine, err := VectorEngineOf(oldIdx)
	if err != nil {
		return err
	}
	newEngine, err := VectorEngineOf(newIdx)
	if err != nil {
		return err
	}
	if oldEngine != newEngine {
		return &MetaDataError{Message: fmt.Sprintf("attempted to change immutable vector index option (index=%q, option=%q)",
			newIdx.Name, IndexOptionVectorEngine)}
	}
	delete(changed, IndexOptionVectorEngine)
	if newEngine == VectorEngineGuardiann {
		return validateGuardiannIndexOptions(oldIdx, newIdx, changed)
	}
	// Structural options: disallow EFFECTIVE value changes, as Java's
	// VectorIndexOptionsHelper.disallowChange compares the parsed and defaulted
	// values (VectorIndexOptionsHelper.java:120-147), so an option set to its
	// default beside one left unset is no change; with Java's message.
	oldOpts, err := readHNSWOptions(oldIdx, true)
	if err != nil {
		return err
	}
	newOpts, err := readHNSWOptions(newIdx, true)
	if err != nil {
		return err
	}
	oldConfig, newConfig := oldOpts.config, newOpts.config
	for _, o := range []struct {
		key  string
		same bool
	}{
		{IndexOptionVectorMetric, oldOpts.metric == newOpts.metric},
		{IndexOptionVectorNumDimensions, oldConfig.NumDimensions == newConfig.NumDimensions},
		{IndexOptionHNSWUseInlining, oldConfig.UseInlining == newConfig.UseInlining},
		{IndexOptionHNSWM, oldConfig.M == newConfig.M},
		{IndexOptionHNSWMMax, oldConfig.MMax == newConfig.MMax},
		{IndexOptionHNSWMMax0, oldConfig.MMax0 == newConfig.MMax0},
		{IndexOptionHNSWEfConstruction, oldConfig.EfConstruction == newConfig.EfConstruction},
		{IndexOptionHNSWEfRepair, oldConfig.EfRepair == newConfig.EfRepair},
		{IndexOptionVectorExtendCandidates, oldConfig.ExtendCandidates == newConfig.ExtendCandidates},
		{IndexOptionVectorKeepPrunedConnections, oldConfig.KeepPrunedConnections == newConfig.KeepPrunedConnections},
		{IndexOptionHNSWUseRaBitQ, oldOpts.useRaBitQ == newOpts.useRaBitQ},
		{IndexOptionHNSWRaBitQNumExBits, oldOpts.raBitQNumExBits == newOpts.raBitQNumExBits},
	} {
		if err := disallowVectorOptionChange(newIdx, changed, o.key, o.same); err != nil {
			return err
		}
	}

	// Runtime-only options: always safe to change, just remove from changed.
	runtime := []string{
		IndexOptionHNSWSampleVectorStatsProbability,
		IndexOptionHNSWMaintainStatsProbability,
		IndexOptionHNSWStatsThreshold,
		IndexOptionHNSWMaxNumConcurrentNodeFetches,
		IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches,
		IndexOptionHNSWMaxNumConcurrentDeleteFromLayer,
	}
	for _, key := range runtime {
		delete(changed, key)
		delete(changed, hnswOptionAliases[key])
	}

	return nil
}

// disallowVectorOptionChange is VectorIndexOptionsHelper.disallowChange for one
// key over its effective values; a handled key leaves changed.
func disallowVectorOptionChange(newIdx *Index, changed map[string]bool, key string, same bool) error {
	// A key covers its alias (VectorOptionKey's names): disallowChange visits
	// the hnsw* name then the alias, and a refusal names the first that changed.
	alias := hnswOptionAliases[key]
	name := key
	switch {
	case changed[key]:
	case alias != "" && changed[alias]:
		name = alias
	default:
		return nil
	}
	if !same {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("attempted to change immutable vector index option (index=%q, option=%q)", newIdx.Name, name),
		}
	}
	delete(changed, key)
	delete(changed, alias)
	return nil
}

// validateGuardiannIndexOptions is GuardiannVectorIndexEngine.validateChangedOptions:
// only the stats and concurrency knobs and the primary-cluster hard cap may
// change; everything else would restructure data already on disk.
func validateGuardiannIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	o, err := parseGuardiannConfig(oldIdx)
	if err != nil {
		return err
	}
	n, err := parseGuardiannConfig(newIdx)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		key  string
		same bool
	}{
		{IndexOptionVectorMetric, o.metric == n.metric},
		{IndexOptionVectorNumDimensions, o.numDimensions == n.numDimensions},
		{IndexOptionHNSWUseRaBitQ, o.useRaBitQ == n.useRaBitQ},
		{IndexOptionHNSWRaBitQNumExBits, o.raBitQNumExBits == n.raBitQNumExBits},
		{IndexOptionGuardiannPrimaryClusterMin, o.primaryClusterMin == n.primaryClusterMin},
		{IndexOptionGuardiannMergeMaxEverFraction, o.mergeMaxEverFraction == n.mergeMaxEverFraction},
		{IndexOptionGuardiannMinChildFraction, o.minChildFraction == n.minChildFraction},
		{IndexOptionGuardiannMaxRelativeImbalance, o.maxRelativeImbalance == n.maxRelativeImbalance},
		{IndexOptionGuardiannSplitImbalancePenalty, o.splitImbalancePenalty == n.splitImbalancePenalty},
		{IndexOptionGuardiannPrimaryClusterMax, o.primaryClusterMax == n.primaryClusterMax},
		{IndexOptionGuardiannUnderreplicatedPrimaryClusterMax, o.underreplicatedPrimaryClusterMax == n.underreplicatedPrimaryClusterMax},
		{IndexOptionGuardiannReplicatedClusterMaxWrites, o.replicatedClusterMaxWrites == n.replicatedClusterMaxWrites},
		{IndexOptionGuardiannReplicatedClusterTarget, o.replicatedClusterTarget == n.replicatedClusterTarget},
		{IndexOptionGuardiannReplicationPriorityMin, o.replicationPriorityMin == n.replicationPriorityMin},
		{IndexOptionGuardiannReplicationDistanceRatioWeight, o.replicationDistanceRatioWeight == n.replicationDistanceRatioWeight},
		{IndexOptionGuardiannReplicationZScoreWeight, o.replicationZScoreWeight == n.replicationZScoreWeight},
		{IndexOptionGuardiannReplicationStatsMinSampleSize, o.replicationStatsMinSampleSize == n.replicationStatsMinSampleSize},
		{IndexOptionGuardiannDeterministicRandomness, o.deterministicRandomness == n.deterministicRandomness},
		{IndexOptionGuardiannInsertMaxCandidateClusters, o.insertMaxCandidateClusters == n.insertMaxCandidateClusters},
		{IndexOptionGuardiannDeleteMaxCandidateClusters, o.deleteMaxCandidateClusters == n.deleteMaxCandidateClusters},
		{IndexOptionGuardiannSplitNumNearestClusters, o.splitNumNearestClusters == n.splitNumNearestClusters},
		{IndexOptionGuardiannMergeNumNearestClusters, o.mergeNumNearestClusters == n.mergeNumNearestClusters},
		{IndexOptionGuardiannKMeansMaxIterations, o.kMeansMaxIterations == n.kMeansMaxIterations},
		{IndexOptionGuardiannKMeansMaxRestarts, o.kMeansMaxRestarts == n.kMeansMaxRestarts},
		{IndexOptionGuardiannReassignNumNeighboringClusters, o.reassignNumNeighboringClusters == n.reassignNumNeighboringClusters},
		{IndexOptionGuardiannCollapseMinDuplicates, o.collapseMinDuplicates == n.collapseMinDuplicates},
		{IndexOptionGuardiannConstructionCentroidEfRingSearch, o.constructionSearchConfig.centroidEfRingSearch == n.constructionSearchConfig.centroidEfRingSearch},
		{IndexOptionGuardiannConstructionCentroidEfOutwardSearch, o.constructionSearchConfig.centroidEfOutwardSearch == n.constructionSearchConfig.centroidEfOutwardSearch},
	} {
		if err := disallowVectorOptionChange(newIdx, changed, c.key, c.same); err != nil {
			return err
		}
	}
	for _, key := range []string{
		IndexOptionHNSWSampleVectorStatsProbability,
		IndexOptionHNSWMaintainStatsProbability,
		IndexOptionHNSWStatsThreshold,
		IndexOptionGuardiannSampleBatchSize,
		IndexOptionGuardiannDeleteConcurrency,
		IndexOptionGuardiannSplitMergeConcurrency,
		IndexOptionGuardiannReassignConcurrency,
		IndexOptionGuardiannCollapseConcurrency,
		IndexOptionGuardiannBounceConcurrency,
		IndexOptionGuardiannPrimaryClusterHardMax,
	} {
		delete(changed, key)
		delete(changed, hnswOptionAliases[key])
	}
	return nil
}
