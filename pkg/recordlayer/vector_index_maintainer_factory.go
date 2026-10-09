package recordlayer

func init() {
	RegisterIndexMaintainerFactory(vectorIndexMaintainerFactory{})
	RegisterIndexMaintainerFactory(spfreshIndexMaintainerFactory{})
}

// vectorIndexMaintainerFactory is Java's VectorIndexMaintainerFactory.
type vectorIndexMaintainerFactory struct{}

func (vectorIndexMaintainerFactory) IndexTypes() []string { return []string{IndexTypeVector} }

// NewIndexMaintainer stores the HNSW graph under the primary index subspace
// (Java's getIndexSubspace()), not the secondary subspace, matching Java's layout.
func (vectorIndexMaintainerFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	m, err := newVectorIndexMaintainer(state.Index, state.IndexSubspace, state.IndexSubspace,
		state.Store.IndexSecondarySubspace(state.Index), state.Transaction, state.Store)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (vectorIndexMaintainerFactory) ValidateIndexOptions(index *Index) error {
	return validateVectorIndexOptionsAtBuild(index)
}

func (vectorIndexMaintainerFactory) ValidateChangedOptions(oldIndex, newIndex *Index, changed map[string]bool) error {
	return validateVectorIndexOptions(oldIndex, newIndex, changed)
}

// spfreshIndexMaintainerFactory maintains Go's FDB-native SPFresh vector index
// (RFC-094), all data under the primary index subspace, generation-prefixed.
type spfreshIndexMaintainerFactory struct{}

func (spfreshIndexMaintainerFactory) IndexTypes() []string { return []string{IndexTypeVectorSPFresh} }

func (spfreshIndexMaintainerFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	m, err := newSPFreshIndexMaintainer(state.Index, state.IndexSubspace, state.Transaction, state.Store, state.Context, state.Timer)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateIndexOptions has nothing to check at build: SPFresh's options are
// read, and refused, when its maintainer is created.
func (spfreshIndexMaintainerFactory) ValidateIndexOptions(*Index) error { return nil }

func (spfreshIndexMaintainerFactory) ValidateChangedOptions(oldIndex, newIndex *Index, changed map[string]bool) error {
	return validateSPFreshIndexOptions(oldIndex, newIndex, changed)
}

var (
	_ byDistanceScanner    = (*vectorIndexMaintainer)(nil)
	_ byDistanceScanner    = (*spfreshIndexMaintainer)(nil)
	_ orderedStreamScanner = (*spfreshIndexMaintainer)(nil)
	_ VectorIndexSearcher  = (*vectorIndexMaintainer)(nil)
)

// unwrapVectorMaintainer peels any decorators off a maintainer and returns the
// vector maintainer underneath, if there is one.
func unwrapVectorMaintainer(m IndexMaintainer) (*vectorIndexMaintainer, bool) {
	return maintainerAs[*vectorIndexMaintainer](m)
}
