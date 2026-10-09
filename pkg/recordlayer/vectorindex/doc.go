// Package vectorindex implements the record layer's vector indexes: Java's
// VECTOR index (VectorIndexMaintainer) with its HNSW and GuardiANN engines, and
// Go's SPFresh index (RFC-094).
//
// Importing the package registers their index maintainer factories with
// package recordlayer, as Java's service loader finds VectorIndexMaintainerFactory.
// A binary that opens a store holding a vector index must link it:
//
//	import _ "fdb.dev/pkg/recordlayer/vectorindex"
//
// Without it, using such an index fails with
// recordlayer.IndexMaintainerNotLinkedError.
package vectorindex
