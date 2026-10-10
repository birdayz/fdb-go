// Portions derived from FoundationDB Record Layer (
// VectorIndexMaintainerFactory.java, IndexMaintainerState.java,
// IndexMaintainer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import "fdb.dev/pkg/recordlayer"

func init() {
	recordlayer.RegisterIndexMaintainerFactory(vectorIndexMaintainerFactory{})
	recordlayer.RegisterIndexMaintainerFactory(spfreshIndexMaintainerFactory{})
}

// vectorIndexMaintainerFactory is Java's VectorIndexMaintainerFactory.
type vectorIndexMaintainerFactory struct{}

func (vectorIndexMaintainerFactory) IndexTypes() []string {
	return []string{recordlayer.IndexTypeVector}
}

// NewIndexMaintainer stores the HNSW graph under the primary index subspace
// (Java's getIndexSubspace()), not the secondary subspace, matching Java's layout.
func (vectorIndexMaintainerFactory) NewIndexMaintainer(state recordlayer.IndexMaintainerState) (recordlayer.IndexMaintainer, error) {
	m, err := newVectorIndexMaintainer(state.Index, state.IndexSubspace, state.IndexSubspace,
		state.Store.IndexSecondarySubspace(state.Index), state.Transaction, state.Store)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (vectorIndexMaintainerFactory) ValidateIndexOptions(index *recordlayer.Index) error {
	return validateVectorIndexOptionsAtBuild(index)
}

func (vectorIndexMaintainerFactory) ValidateChangedOptions(oldIndex, newIndex *recordlayer.Index, changed map[string]bool) error {
	return validateVectorIndexOptions(oldIndex, newIndex, changed)
}

// spfreshIndexMaintainerFactory maintains Go's FDB-native SPFresh vector index
// (RFC-094), all data under the primary index subspace, generation-prefixed.
type spfreshIndexMaintainerFactory struct{}

func (spfreshIndexMaintainerFactory) IndexTypes() []string {
	return []string{recordlayer.IndexTypeVectorSPFresh}
}

func (spfreshIndexMaintainerFactory) NewIndexMaintainer(state recordlayer.IndexMaintainerState) (recordlayer.IndexMaintainer, error) {
	m, err := newSPFreshIndexMaintainer(state.Index, state.IndexSubspace, state.Transaction, state.Store, state.Context, state.Timer)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateIndexOptions has nothing to check at build: SPFresh's options are
// read, and refused, when its maintainer is created.
func (spfreshIndexMaintainerFactory) ValidateIndexOptions(*recordlayer.Index) error { return nil }

func (spfreshIndexMaintainerFactory) ValidateChangedOptions(oldIndex, newIndex *recordlayer.Index, changed map[string]bool) error {
	return validateSPFreshIndexOptions(oldIndex, newIndex, changed)
}

var (
	_ recordlayer.ByDistanceScanner    = (*vectorIndexMaintainer)(nil)
	_ recordlayer.ByDistanceScanner    = (*spfreshIndexMaintainer)(nil)
	_ recordlayer.OrderedStreamScanner = (*spfreshIndexMaintainer)(nil)
	_ recordlayer.VectorIndexSearcher  = (*vectorIndexMaintainer)(nil)
)

// unwrapVectorMaintainer peels any decorators off a maintainer and returns the
// vector maintainer underneath, if there is one.
func unwrapVectorMaintainer(m recordlayer.IndexMaintainer) (*vectorIndexMaintainer, bool) {
	return recordlayer.IndexMaintainerAs[*vectorIndexMaintainer](m)
}
