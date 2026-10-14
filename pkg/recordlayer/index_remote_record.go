// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// SplitHelper.java), Copyright Apple Inc. and the FoundationDB project authors.
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/internal/tuplefast"
	"google.golang.org/protobuf/proto"
)

// BuildSingleRecord rebuilds the record of a remote-fetch result. It returns
// nil when the server found no record rows: the orphan behavior belongs to the
// scan. Matches Java's FDBRecordStore.buildSingleRecord.
func (store *FDBRecordStore) BuildSingleRecord(raw *FDBIndexedRawRecord) (*FDBIndexedRecord, error) {
	var load remoteRecordLoad
	load.start(store, *raw, nil)
	return load.Get()
}

// Only a legacy-format version read can remain pending; its error surfaces
// after deserialization succeeds, as Java chains it in deserializeRecord.
type remoteRecordLoad struct {
	record     *FDBIndexedRecord
	oldVersion PendingRead[*FDBRecordVersion]
	err        error
}

func (load *remoteRecordLoad) start(store *FDBRecordStore, raw FDBIndexedRawRecord, scanState *ScanLimiterState) {
	*load = remoteRecordLoad{}
	if len(raw.RawRecord.GetRange.Rows) == 0 {
		return
	}
	var sizes sizeInfo
	primaryKey, value, version, err := store.reconstructSingleRecord(raw.RawRecord, &sizes)
	if err != nil {
		load.err = err
		return
	}
	if store.useOldVersionFormat() && version == nil {
		load.oldVersion = store.loadRecordVersionAsync(raw.IndexEntry.PrimaryKey(), false)
	}
	if scanState != nil {
		// The index entry's own bytes were charged by the index cursor.
		scanState.AddBytesScanned(int64(sizes.KeySize + sizes.ValueSize))
	}
	recordType, message, wire, err := store.deserializeAndDiscover(value)
	if err != nil {
		load.err = &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
		load.oldVersion = nil
		return
	}
	load.record = &FDBIndexedRecord{IndexEntry: raw.IndexEntry, Record: &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey, RecordType: recordType, Record: message, wire: wire, Store: store,
		Version: version, KeyCount: sizes.KeyCount, KeySize: sizes.KeySize, ValueSize: sizes.ValueSize, Split: sizes.IsSplit,
	}}
}

func (load *remoteRecordLoad) Get() (*FDBIndexedRecord, error) {
	if load.oldVersion != nil {
		version, err := load.oldVersion.Get()
		load.oldVersion = nil
		if err != nil {
			load.err = fmt.Errorf("load record version for %v: %w", load.record.Record.PrimaryKey, err)
			load.record = nil
		} else {
			load.record.Record.Version = version
		}
	}
	return load.record, load.err
}

func (load *remoteRecordLoad) IsReady() bool {
	return load.oldVersion == nil || load.oldVersion.IsReady()
}

// MapPipelined's size-one queue resolves its only load before issuing another.
// Reuse that pending state, never the records or index entries it returns.
type remoteIndexRecordLoader struct {
	store     *FDBRecordStore
	scanState *ScanLimiterState
	orphan    IndexOrphanBehavior
	pending   remoteRecordLoad
}

func (loader *remoteIndexRecordLoader) load(raw FDBIndexedRawRecord) PendingRead[*FDBIndexedRecord] {
	loader.pending.start(loader.store, raw, loader.scanState)
	if loader.pending.record == nil && loader.pending.err == nil {
		loader.pending.record, loader.pending.err = loader.store.handleOrphanEntry(raw.IndexEntry, raw.IndexEntry.PrimaryKey(), loader.orphan, true)
	}
	return &loader.pending
}

// reconstructSingleRecord is FDBRecordStore.reconstructSingleRecord: the first
// record of the secondary rows, unsplit as SplitHelper.KeyValueUnsplitter does
// in the forward direction (the rows are forward whatever the scan direction).
// The caller handles a result without rows. Like Java, it ignores GetRange.More:
// a record's own rows stay far below the server's secondary row limit.
func (store *FDBRecordStore) reconstructSingleRecord(raw fdb.MappedKeyValue, sizes *sizeInfo) (tuple.Tuple, []byte, *FDBRecordVersion, error) {
	rows := raw.GetRange.Rows
	first := rows[0]
	if !store.recordsSubspace.Contains(fdb.Key(first.Key)) {
		return nil, nil, nil, &RecordCoreArgumentError{Message: "unable to unpack key: key is not in subspace"}
	}
	key, err := tuplefast.SubspaceUnpack(first.Key, len(store.recordsSubspace.Bytes()))
	if err != nil {
		return nil, nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("unable to unpack key: %v", err)}
	}
	*sizes = sizeInfo{KeyCount: 1, KeySize: len(first.Key), ValueSize: len(first.Value)}
	if !store.metaData.IsSplitLongRecords() && store.omitUnsplitRecordSuffix() {
		return key, first.Value, nil, nil
	}
	if len(key) == 0 {
		return nil, nil, nil, &RecordCoreArgumentError{Message: "record key has no split suffix"}
	}
	primaryKey := key[:len(key)-1]
	index, ok := key[len(key)-1].(int64)
	if !ok {
		return nil, nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("split record suffix is not an integer: %v", key)}
	}
	var value []byte
	var version *FDBRecordVersion
	switch index {
	case unsplitRecord:
		value = first.Value
	case recordVersionSuffix:
		if store.useOldVersionFormat() {
			return nil, nil, nil, &RecordCoreError{Message: "Found record version when old format specified"}
		}
		if version, err = unpackVersion(first.Value); err != nil {
			return nil, nil, nil, err
		}
	case startSplitRecord:
		value = bytes.Clone(first.Value)
		sizes.IsSplit = true
	default:
		return nil, nil, nil, &FoundSplitWithoutStartError{NextIndex: index, KeyTuple: key}
	}
	if index != unsplitRecord {
		prefix := store.recordsSubspace.Pack(primaryKey)
		for _, row := range rows[1:] {
			if !bytes.HasPrefix(row.Key, prefix) {
				break
			}
			suffix, err := tuplefast.SubspaceUnpack(row.Key, len(prefix))
			if err != nil || len(suffix) == 0 {
				return nil, nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("unable to unpack split key for %v: %v", primaryKey, err)}
			}
			next, ok := suffix[0].(int64)
			if !ok {
				return nil, nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("split record suffix is not an integer: %v", suffix)}
			}
			sizes.KeyCount++
			sizes.KeySize += len(row.Key)
			sizes.ValueSize += len(row.Value)
			switch {
			case index == recordVersionSuffix && next == unsplitRecord:
				value = row.Value
			case index == recordVersionSuffix && next == startSplitRecord:
				value = bytes.Clone(row.Value)
				sizes.IsSplit = true
			case next == index+1:
				value = append(value, row.Value...)
			case index == recordVersionSuffix:
				return nil, nil, nil, &FoundSplitWithoutStartError{NextIndex: next, KeyTuple: primaryKey}
			default:
				return nil, nil, nil, &RecordCoreError{Message: "Split record segments out of order"}
			}
			index = next
			if index == unsplitRecord {
				break
			}
		}
	}
	if index == recordVersionSuffix {
		// A version row with no data row, however empty the data would be.
		return nil, nil, nil, &FoundSplitWithoutStartError{NextIndex: recordVersionSuffix, KeyTuple: primaryKey}
	}
	if !store.useOldVersionFormat() {
		// Avoid repacking a version key on the read-only unsplit fast path.
		// Lookup and packing share the lock, so a cached local version is never lost.
		store.context.versionMu.Lock()
		if len(store.context.localVersionCache) != 0 {
			versionKey := store.recordsSubspace.Pack(appendToTuple(primaryKey, recordVersionSuffix))
			if local, found := store.context.localVersionCache[string(versionKey)]; found {
				version, err = IncompleteVersion(local)
				if err == nil {
					sizes.KeyCount++
					sizes.KeySize += len(versionKey)
					sizes.ValueSize += VersionBytes + 1
				}
			}
		}
		store.context.versionMu.Unlock()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	sizes.VersionedInline = version != nil
	return primaryKey, value, version, nil
}
