// Portions derived from FoundationDB Record Layer (
// VectorIndexOptionsHelper.java, VectorIndexMaintainerFactory.java,
// VectorIndexHelper.java, GuardiannVectorIndexEngine.java),
// Copyright 2023 Apple Inc. and the FoundationDB project authors
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer"
)

// validateVectorIndexOptionsAtBuild is the option half of Java's VectorIndexValidator
// (VectorIndexMaintainerFactory.java:96-111): VectorIndexHelper.validate, which
// refuses an option given under both its name and its alias (a MetaDataError,
// VectorIndexOptionsHelper.validateNoAliasConflicts) and then parses the HNSW
// configuration as HnswVectorIndexEngine.parseConfig does, Config's checks
// included (readHNSWOptions, without Go's dimension default; a plain HNSW
// index's metric only); any IllegalArgumentException
// of the parse, a NumberFormatException included, is rethrown as
// MetaDataException("incorrect index options", cause), and a missing dimension
// count is the parse's own MetaDataException. So the windowed index Go builds is
// the one Java builds, and its maintainer (parseHNSWConfig, the same reader)
// reads the numbers Java reads. A GuardiANN index is parsed by
// parseGuardiannConfig, GuardiannVectorIndexEngine.parseConfig.
func validateVectorIndexOptionsAtBuild(idx *recordlayer.Index) error {
	engine, err := VectorEngineOf(idx)
	if err != nil {
		return err
	}
	if err := hnswAliasConflict(idx); err != nil {
		return err
	}
	switch {
	case engine == VectorEngineGuardiann:
		_, err = parseGuardiannConfig(idx)
	case !idx.HasRowNumberWindowPredicate():
		// A plain HNSW index's other options reach only its maintainer: Go
		// fixtures still carry configurations Java's Config refuses (DIVERGENCES.md).
		_, err = hnswMetric(idx)
	default:
		_, err = readHNSWOptions(idx, false)
	}
	if err != nil {
		var iae *recordlayer.IllegalArgumentError
		if errors.As(err, &iae) {
			return &recordlayer.MetaDataError{Message: "incorrect index options", Cause: err}
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
func validateSPFreshIndexOptions(oldIdx, newIdx *recordlayer.Index, changed map[string]bool) error {
	structural := []string{
		recordlayer.IndexOptionSPFreshNumDimensions,
		recordlayer.IndexOptionSPFreshMetric,
		recordlayer.IndexOptionSPFreshLmax,
		recordlayer.IndexOptionSPFreshLminRatio,
		recordlayer.IndexOptionSPFreshCellTarget,
		recordlayer.IndexOptionSPFreshCellMax,
		recordlayer.IndexOptionSPFreshReplication,
		recordlayer.IndexOptionSPFreshAlpha,
		recordlayer.IndexOptionSPFreshKn,
		recordlayer.IndexOptionSPFreshCooldownSec,
		recordlayer.IndexOptionSPFreshRaBitQNumExBits,
		recordlayer.IndexOptionSPFreshSidecar,
	}
	for _, key := range structural {
		if !changed[key] {
			continue
		}
		oldVal := optionValueOrDefault(oldIdx.Options, key, "")
		newVal := optionValueOrDefault(newIdx.Options, key, "")
		if oldVal != newVal {
			return &recordlayer.MetaDataEvolutionError{
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
func validateVectorIndexOptions(oldIdx, newIdx *recordlayer.Index, changed map[string]bool) error {
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
		return &recordlayer.MetaDataError{Message: fmt.Sprintf("attempted to change immutable vector index option (index=%q, option=%q)",
			newIdx.Name, recordlayer.IndexOptionVectorEngine)}
	}
	delete(changed, recordlayer.IndexOptionVectorEngine)
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
		{recordlayer.IndexOptionVectorMetric, oldOpts.metric == newOpts.metric},
		{recordlayer.IndexOptionVectorNumDimensions, oldConfig.NumDimensions == newConfig.NumDimensions},
		{recordlayer.IndexOptionHNSWUseInlining, oldConfig.UseInlining == newConfig.UseInlining},
		{recordlayer.IndexOptionHNSWM, oldConfig.M == newConfig.M},
		{recordlayer.IndexOptionHNSWMMax, oldConfig.MMax == newConfig.MMax},
		{recordlayer.IndexOptionHNSWMMax0, oldConfig.MMax0 == newConfig.MMax0},
		{recordlayer.IndexOptionHNSWEfConstruction, oldConfig.EfConstruction == newConfig.EfConstruction},
		{recordlayer.IndexOptionHNSWEfRepair, oldConfig.EfRepair == newConfig.EfRepair},
		{recordlayer.IndexOptionVectorExtendCandidates, oldConfig.ExtendCandidates == newConfig.ExtendCandidates},
		{recordlayer.IndexOptionVectorKeepPrunedConnections, oldConfig.KeepPrunedConnections == newConfig.KeepPrunedConnections},
		{recordlayer.IndexOptionHNSWUseRaBitQ, oldOpts.useRaBitQ == newOpts.useRaBitQ},
		{recordlayer.IndexOptionHNSWRaBitQNumExBits, oldOpts.raBitQNumExBits == newOpts.raBitQNumExBits},
	} {
		if err := disallowVectorOptionChange(newIdx, changed, o.key, o.same); err != nil {
			return err
		}
	}

	// Runtime-only options: always safe to change, just remove from changed.
	runtime := []string{
		recordlayer.IndexOptionHNSWSampleVectorStatsProbability,
		recordlayer.IndexOptionHNSWMaintainStatsProbability,
		recordlayer.IndexOptionHNSWStatsThreshold,
		recordlayer.IndexOptionHNSWMaxNumConcurrentNodeFetches,
		recordlayer.IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches,
		recordlayer.IndexOptionHNSWMaxNumConcurrentDeleteFromLayer,
	}
	for _, key := range runtime {
		delete(changed, key)
		delete(changed, hnswOptionAliases[key])
	}

	return nil
}

// disallowVectorOptionChange is VectorIndexOptionsHelper.disallowChange for one
// key over its effective values; a handled key leaves changed.
func disallowVectorOptionChange(newIdx *recordlayer.Index, changed map[string]bool, key string, same bool) error {
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
		return &recordlayer.MetaDataEvolutionError{
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
func validateGuardiannIndexOptions(oldIdx, newIdx *recordlayer.Index, changed map[string]bool) error {
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
		{recordlayer.IndexOptionVectorMetric, o.metric == n.metric},
		{recordlayer.IndexOptionVectorNumDimensions, o.numDimensions == n.numDimensions},
		{recordlayer.IndexOptionHNSWUseRaBitQ, o.useRaBitQ == n.useRaBitQ},
		{recordlayer.IndexOptionHNSWRaBitQNumExBits, o.raBitQNumExBits == n.raBitQNumExBits},
		{recordlayer.IndexOptionGuardiannPrimaryClusterMin, o.primaryClusterMin == n.primaryClusterMin},
		{recordlayer.IndexOptionGuardiannMergeMaxEverFraction, o.mergeMaxEverFraction == n.mergeMaxEverFraction},
		{recordlayer.IndexOptionGuardiannMinChildFraction, o.minChildFraction == n.minChildFraction},
		{recordlayer.IndexOptionGuardiannMaxRelativeImbalance, o.maxRelativeImbalance == n.maxRelativeImbalance},
		{recordlayer.IndexOptionGuardiannSplitImbalancePenalty, o.splitImbalancePenalty == n.splitImbalancePenalty},
		{recordlayer.IndexOptionGuardiannPrimaryClusterMax, o.primaryClusterMax == n.primaryClusterMax},
		{recordlayer.IndexOptionGuardiannUnderreplicatedPrimaryClusterMax, o.underreplicatedPrimaryClusterMax == n.underreplicatedPrimaryClusterMax},
		{recordlayer.IndexOptionGuardiannReplicatedClusterMaxWrites, o.replicatedClusterMaxWrites == n.replicatedClusterMaxWrites},
		{recordlayer.IndexOptionGuardiannReplicatedClusterTarget, o.replicatedClusterTarget == n.replicatedClusterTarget},
		{recordlayer.IndexOptionGuardiannReplicationPriorityMin, o.replicationPriorityMin == n.replicationPriorityMin},
		{recordlayer.IndexOptionGuardiannReplicationDistanceRatioWeight, o.replicationDistanceRatioWeight == n.replicationDistanceRatioWeight},
		{recordlayer.IndexOptionGuardiannReplicationZScoreWeight, o.replicationZScoreWeight == n.replicationZScoreWeight},
		{recordlayer.IndexOptionGuardiannReplicationStatsMinSampleSize, o.replicationStatsMinSampleSize == n.replicationStatsMinSampleSize},
		{recordlayer.IndexOptionGuardiannDeterministicRandomness, o.deterministicRandomness == n.deterministicRandomness},
		{recordlayer.IndexOptionGuardiannInsertMaxCandidateClusters, o.insertMaxCandidateClusters == n.insertMaxCandidateClusters},
		{recordlayer.IndexOptionGuardiannDeleteMaxCandidateClusters, o.deleteMaxCandidateClusters == n.deleteMaxCandidateClusters},
		{recordlayer.IndexOptionGuardiannSplitNumNearestClusters, o.splitNumNearestClusters == n.splitNumNearestClusters},
		{recordlayer.IndexOptionGuardiannMergeNumNearestClusters, o.mergeNumNearestClusters == n.mergeNumNearestClusters},
		{recordlayer.IndexOptionGuardiannKMeansMaxIterations, o.kMeansMaxIterations == n.kMeansMaxIterations},
		{recordlayer.IndexOptionGuardiannKMeansMaxRestarts, o.kMeansMaxRestarts == n.kMeansMaxRestarts},
		{recordlayer.IndexOptionGuardiannReassignNumNeighboringClusters, o.reassignNumNeighboringClusters == n.reassignNumNeighboringClusters},
		{recordlayer.IndexOptionGuardiannCollapseMinDuplicates, o.collapseMinDuplicates == n.collapseMinDuplicates},
		{recordlayer.IndexOptionGuardiannConstructionCentroidEfRingSearch, o.constructionSearchConfig.centroidEfRingSearch == n.constructionSearchConfig.centroidEfRingSearch},
		{recordlayer.IndexOptionGuardiannConstructionCentroidEfOutwardSearch, o.constructionSearchConfig.centroidEfOutwardSearch == n.constructionSearchConfig.centroidEfOutwardSearch},
	} {
		if err := disallowVectorOptionChange(newIdx, changed, c.key, c.same); err != nil {
			return err
		}
	}
	for _, key := range []string{
		recordlayer.IndexOptionHNSWSampleVectorStatsProbability,
		recordlayer.IndexOptionHNSWMaintainStatsProbability,
		recordlayer.IndexOptionHNSWStatsThreshold,
		recordlayer.IndexOptionGuardiannSampleBatchSize,
		recordlayer.IndexOptionGuardiannDeleteConcurrency,
		recordlayer.IndexOptionGuardiannSplitMergeConcurrency,
		recordlayer.IndexOptionGuardiannReassignConcurrency,
		recordlayer.IndexOptionGuardiannCollapseConcurrency,
		recordlayer.IndexOptionGuardiannBounceConcurrency,
		recordlayer.IndexOptionGuardiannPrimaryClusterHardMax,
	} {
		delete(changed, key)
		delete(changed, hnswOptionAliases[key])
	}
	return nil
}
