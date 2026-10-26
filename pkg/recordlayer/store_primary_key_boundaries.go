// Portions derived from FoundationDB Record Layer (FDBRecordStore.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// GetPrimaryKeyBoundaries returns, for each shard boundary in r, the primary key of the first
// record at or after it, sorted and without duplicates. Java's FDBRecordStore.getPrimaryKeyBoundaries.
func (store *FDBRecordStore) GetPrimaryKeyBoundaries(r TupleRange) ([]tuple.Tuple, error) {
	kr := r.ToFDBRange(store.recordsSubspace)
	return store.primaryKeyBoundaries(kr.Begin.FDBKey(), kr.End.FDBKey())
}

// primaryKeyBoundaries is GetPrimaryKeyBoundaries over the raw key range [begin, end). It reads
// the shard map at the transaction's read version and looks keys up with snapshot reads.
func (store *FDBRecordStore) primaryKeyBoundaries(begin, end fdb.Key) ([]tuple.Tuple, error) {
	db := store.context.GetDatabase()
	if db == nil { // a context made without a database has no shard map
		return nil, &BackendCapabilityError{Op: "LocalityGetBoundaryKeys"}
	}
	readVersion, err := store.context.GetReadVersion()
	if err != nil {
		return nil, err
	}
	shardKeys, err := db.LocalityGetBoundaryKeys(fdb.KeyRange{Begin: begin, End: end}, 0, readVersion)
	if err != nil {
		return nil, err
	}
	suffixed := store.metaData.IsSplitLongRecords() || !store.omitUnsplitRecordSuffix()
	snapshot := store.context.Transaction().Snapshot()
	var boundaries []tuple.Tuple
	var previous []byte
	collect := func(pending fdb.RangeResult) error {
		kvs, err := pending.GetSliceWithError()
		if err != nil || len(kvs) == 0 {
			return err
		}
		primaryKey, err := store.recordsSubspace.Unpack(kvs[0].Key)
		if err != nil {
			return err
		}
		if suffixed && len(primaryKey) > 0 {
			primaryKey = primaryKey[:len(primaryKey)-1]
		}
		packed := primaryKey.Pack()
		if previous != nil && bytes.Equal(packed, previous) {
			return nil
		}
		previous = packed
		boundaries = append(boundaries, primaryKey)
		return nil
	}
	// Up to DefaultPipelineSize lookups in flight, as Java's flatMapPipelined.
	inFlight := make([]fdb.RangeResult, 0, DefaultPipelineSize)
	for _, key := range shardKeys {
		if len(inFlight) == DefaultPipelineSize {
			if err := collect(inFlight[0]); err != nil {
				return nil, err
			}
			inFlight = inFlight[1:]
		}
		inFlight = append(inFlight, snapshot.GetRange(fdb.KeyRange{Begin: key, End: end}, fdb.RangeOptions{Limit: 1}))
	}
	for _, pending := range inFlight {
		if err := collect(pending); err != nil {
			return nil, err
		}
	}
	return boundaries, nil
}
