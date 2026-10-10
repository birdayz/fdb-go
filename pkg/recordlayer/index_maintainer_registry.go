// Portions derived from FoundationDB Record Layer (IndexMaintainerFactory.java,
// IndexMaintainerState.java, IndexMaintainer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// IndexMaintainerFactory is Java's IndexMaintainerFactory for an index type
// implemented outside this package. Java finds its factories with a service
// loader; Go's register themselves from an init function, so a binary maintains
// such a type only when it links the implementing package. The built-in types
// are dispatched directly by createIndexMaintainer.
type IndexMaintainerFactory interface {
	// IndexTypes is getIndexTypes: the types this factory maintains.
	IndexTypes() []string
	// NewIndexMaintainer is getIndexMaintainer(IndexMaintainerState).
	NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error)
	// ValidateIndexOptions is the option half of the type's
	// IndexValidator.validate, run at meta-data build.
	ValidateIndexOptions(index *Index) error
	// ValidateChangedOptions is IndexValidator.validateChangedOptions: it
	// checks the changed options the type owns and removes them from changed.
	ValidateChangedOptions(oldIndex, newIndex *Index, changed map[string]bool) error
}

// idempotentIndexMaintainerFactory is implemented by a factory whose
// maintainers are idempotent (Java's IndexMaintainer.isIdempotent), which Go
// asks per index type because the online indexer decides without a maintainer.
// A factory that does not implement it is not idempotent.
type idempotentIndexMaintainerFactory interface {
	IsIdempotent(index *Index) bool
}

// IndexMaintainerState is Java's IndexMaintainerState: what a factory builds a
// maintainer from.
type IndexMaintainerState struct {
	Store         *FDBRecordStore
	Context       *FDBRecordContext
	Index         *Index
	IndexSubspace subspace.Subspace
	Transaction   fdb.WritableTransaction
	Timer         *StoreTimer
}

// linkedIndexTypePackages names the package implementing each index type this
// module knows but does not maintain itself. A store meeting one of these types
// in a binary that does not link its package must fail, never fall through to
// "unknown type" handling that tolerates meta-data written by another program.
var linkedIndexTypePackages = map[string]string{
	IndexTypeVector:        "fdb.dev/pkg/recordlayer/vectorindex",
	IndexTypeVectorSPFresh: "fdb.dev/pkg/recordlayer/vectorindex",
}

var (
	indexMaintainerFactoriesMu sync.RWMutex
	indexMaintainerFactories   = map[string]IndexMaintainerFactory{}
)

// RegisterIndexMaintainerFactory makes f the factory for each of its index
// types. It is called from the implementing package's init function and panics
// on a type registered twice, as database/sql.Register does: two
// implementations of one wire format in a binary is a build error.
func RegisterIndexMaintainerFactory(f IndexMaintainerFactory) {
	indexMaintainerFactoriesMu.Lock()
	defer indexMaintainerFactoriesMu.Unlock()
	for _, t := range f.IndexTypes() {
		t = canonicalIndexType(t)
		if _, dup := indexMaintainerFactories[t]; dup {
			panic(fmt.Sprintf("recordlayer: index maintainer factory registered twice for index type %q", t))
		}
		indexMaintainerFactories[t] = f
	}
}

// lookupIndexMaintainerFactory returns the registered factory for index's
// type, nil when the type is not one implemented outside this package, and an
// IndexMaintainerNotLinkedError when it is but this binary does not link it.
func lookupIndexMaintainerFactory(index *Index) (IndexMaintainerFactory, error) {
	t := canonicalIndexType(index.Type)
	indexMaintainerFactoriesMu.RLock()
	f, ok := indexMaintainerFactories[t]
	indexMaintainerFactoriesMu.RUnlock()
	if ok {
		return f, nil
	}
	if pkg, known := linkedIndexTypePackages[t]; known {
		return nil, &IndexMaintainerNotLinkedError{IndexName: index.Name, IndexType: index.Type, Package: pkg}
	}
	return nil, nil
}

// IndexMaintainerNotLinkedError is raised when an index's type is implemented
// by a package this binary does not link (see linkedIndexTypePackages). Like
// UnknownIndexTypeError it unwraps to *MetaDataError, Java's registry miss.
type IndexMaintainerNotLinkedError struct {
	IndexName string
	IndexType string
	Package   string
}

func (e *IndexMaintainerNotLinkedError) Error() string {
	return fmt.Sprintf("index %q has type %q, implemented by package %s, which this binary does not link: add import _ %q",
		e.IndexName, e.IndexType, e.Package, e.Package)
}

func (*IndexMaintainerNotLinkedError) JavaRecordCoreException() {}

// Unwrap reports this as a metadata failure.
func (e *IndexMaintainerNotLinkedError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
}
