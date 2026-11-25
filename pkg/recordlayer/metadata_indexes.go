// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"maps"
	"slices"
)

// GetIndexesForRecordType returns the indexes defined for a specific record type,
// including both single-type and multi-type indexes.
// Does NOT include universal indexes — use GetUniversalIndexes() for those.
// Matches Java's RecordType.getAllIndexes().
//
// Resolves through GetRecordType rather than indexing the map, so a caller
// holding the SQL identifier for a type stored under its escaped protobuf name
// gets that type's indexes instead of nil. A raw lookup here returns "this type
// has no indexes", which is a silent wrong answer wherever the result feeds an
// index-selection decision.
func (m *RecordMetaData) GetIndexesForRecordType(name string) []*Index {
	rt := m.GetRecordType(name)
	if rt == nil {
		return nil
	}
	if len(rt.multiTypeIndexes) == 0 {
		return rt.indexes
	}
	all := make([]*Index, 0, len(rt.indexes)+len(rt.multiTypeIndexes))
	all = append(all, rt.indexes...)
	all = append(all, rt.multiTypeIndexes...)
	return all
}

// GetUniversalIndexes returns indexes that apply to all record types.
func (m *RecordMetaData) GetUniversalIndexes() []*Index {
	return m.universalIndexes
}

// HasIndexes returns true if any indexes are defined.
func (m *RecordMetaData) HasIndexes() bool {
	return len(m.indexes) > 0
}

// GetIndex returns the index with the given name, or nil if not found.
// Matches Java's RecordMetaData.getIndex(String).
func (m *RecordMetaData) GetIndex(name string) *Index {
	return m.indexes[name]
}

// GetAllIndexes returns all indexes by name.
func (m *RecordMetaData) GetAllIndexes() map[string]*Index {
	return m.indexes
}

// RecordTypesForIndex returns the record types that the given index covers.
// Universal indexes cover all record types. Type-specific indexes cover only
// the record types they are associated with.
// Matches Java's RecordMetaData.recordTypesForIndex(Index).
//
// The types come in name order, so a caller reporting the first of them that
// violates something names the same one on every run.
func (m *RecordMetaData) RecordTypesForIndex(idx *Index) []*RecordType {
	names := slices.Sorted(maps.Keys(m.recordTypes))
	// Check if it's a universal index.
	for _, ui := range m.universalIndexes {
		if ui.Name == idx.Name {
			result := make([]*RecordType, 0, len(m.recordTypes))
			for _, name := range names {
				result = append(result, m.recordTypes[name])
			}
			return result
		}
	}
	// Type-specific: find which types have this index.
	var result []*RecordType
	for _, name := range names {
		rt := m.recordTypes[name]
		for _, i := range m.GetIndexesForRecordType(rt.Name) {
			if i.Name == idx.Name {
				result = append(result, rt)
				break
			}
		}
	}
	return result
}

// GetFormerIndexes returns all former (deleted) indexes.
// Matches Java's RecordMetaData.getFormerIndexes().
func (m *RecordMetaData) GetFormerIndexes() []*FormerIndex {
	return m.formerIndexes
}

// GetFormerIndexesSince returns former indexes removed since the given version.
// Matches Java's RecordMetaData.getFormerIndexesSince(int).
func (m *RecordMetaData) GetFormerIndexesSince(version int) []*FormerIndex {
	var result []*FormerIndex
	for _, fi := range m.formerIndexes {
		if fi.RemovedVersion > version {
			result = append(result, fi)
		}
	}
	return result
}

// GetIndexFromSubspaceKey returns the index with the given subspace key, or nil.
// Java's RecordMetaData.getIndexFromSubspaceKey (RecordMetaData.java:329-336)
// compares each index's normalized key with the ARGUMENT AS GIVEN by equals. Go
// normalizes the argument too (subspaceKeyIdentity), so two arguments differ
// from Java's: an int argument finds an index whose key is int64, where Java's
// Integer argument misses a Long key, and a tuple argument finds an index whose
// key is the same list, where Java's Tuple argument never equals a List key.
// The second is reached by the one non-test caller, savedSourceIndex, which
// passes a decoded stamp item as Java's OnlineIndexer passes
// Index.decodeSubspaceKey's (OnlineIndexer.java:205-208): for a source index
// keyed by a nested tuple Java fails the BY_INDEX resume with an unknown key and
// Go resumes from the index the stamp names (DIVERGENCES.md, "BY_INDEX resume
// over a nested-tuple source key"). Java throws MetaDataException on a miss; Go
// returns nil.
func (m *RecordMetaData) GetIndexFromSubspaceKey(key any) *Index {
	want := subspaceKeyIdentity(key)
	for _, idx := range m.indexes {
		if subspaceKeyIdentity(idx.SubspaceTupleKey()) == want {
			return idx
		}
	}
	return nil
}

// GetIndexesSince returns all indexes modified since the given metadata version,
// including replaced originals whose state still needs reconciliation.
// Matches Java's RecordMetaData.getIndexesSince(int).
func (m *RecordMetaData) GetIndexesSince(version int) []*Index {
	var result []*Index
	for _, idx := range m.indexes {
		if idx.LastModifiedVersion > version {
			result = append(result, idx)
		}
	}
	return result
}

// GetIndexesToBuildSince excludes replaced originals from changed indexes.
// Matches Java's RecordMetaData.getIndexesToBuildSince(int).
func (m *RecordMetaData) GetIndexesToBuildSince(version int) []*Index {
	var result []*Index
	for _, index := range m.GetIndexesSince(version) {
		if len(index.GetReplacedByIndexNames()) == 0 {
			result = append(result, index)
		}
	}
	return result
}

// formerIndexNameSuffix is Java's rendering of a former index's name after
// "Former index" and "former index": a space and the name, or nothing for an
// unnamed one (MetaDataValidator.java:110-112, :163-178).
func formerIndexNameSuffix(fi *FormerIndex) string {
	if fi.FormerName == "" {
		return ""
	}
	return " " + fi.FormerName
}

// formerIndexNameOrUnknown is how MetaDataValidator names a former index that
// may have no name.
func formerIndexNameOrUnknown(fi *FormerIndex) string {
	if fi.FormerName == "" {
		return "<unknown>"
	}
	return fi.FormerName
}
