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
	return store.buildSingleRecord(raw, nil).Get()
}

// buildSingleRecord is buildSingleRecordInternal. Only a legacy-format version
// read can remain pending; its error surfaces after deserialization succeeds,
// as Java chains it in deserializeRecord.
func (store *FDBRecordStore) buildSingleRecord(raw *FDBIndexedRawRecord, scanState *ScanLimiterState) PendingRead[*FDBIndexedRecord] {
	if len(raw.RawRecord.GetRange.Rows) == 0 {
		return NewPendingRead(func() (*FDBIndexedRecord, error) { return nil, nil }, nil)
	}
	var sizes sizeInfo
	primaryKey, value, version, err := store.reconstructSingleRecord(raw.RawRecord, &sizes)
	if err != nil {
		return NewPendingRead(func() (*FDBIndexedRecord, error) { return nil, err }, nil)
	}
	var oldVersion PendingRead[*FDBRecordVersion]
	if store.useOldVersionFormat() && version == nil {
		oldVersion = store.loadRecordVersionAsync(raw.IndexEntry.PrimaryKey(), false)
	}
	if scanState != nil {
		// The index entry's own bytes were charged by the index cursor.
		scanState.AddBytesScanned(int64(sizes.KeySize + sizes.ValueSize))
	}
	recordType, message, wire, err := store.deserializeAndDiscover(value)
	if err != nil {
		err = &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
		return NewPendingRead(func() (*FDBIndexedRecord, error) { return nil, err }, nil)
	}
	record := &FDBIndexedRecord{IndexEntry: raw.IndexEntry, Record: &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey, RecordType: recordType, Record: message, wire: wire, Store: store,
		Version: version, KeyCount: sizes.KeyCount, KeySize: sizes.KeySize, ValueSize: sizes.ValueSize, Split: sizes.IsSplit,
	}}
	if oldVersion == nil {
		return NewPendingRead(func() (*FDBIndexedRecord, error) { return record, nil }, nil)
	}
	return NewPendingRead(func() (*FDBIndexedRecord, error) {
		version, err := oldVersion.Get()
		if err != nil {
			return nil, fmt.Errorf("load record version for %v: %w", primaryKey, err)
		}
		record.Record.Version = version
		return record, nil
	}, oldVersion.IsReady)
}

// reconstructSingleRecord is FDBRecordStore.reconstructSingleRecord: the first
// record of the secondary rows, unsplit as SplitHelper.KeyValueUnsplitter does
// in the forward direction (the rows are forward whatever the scan direction).
// The caller handles a result without rows.
func (store *FDBRecordStore) reconstructSingleRecord(raw fdb.MappedKeyValue, sizes *sizeInfo) (tuple.Tuple, []byte, *FDBRecordVersion, error) {
	rows := raw.GetRange.Rows
	first := rows[0]
	key, err := store.recordsSubspace.Unpack(fdb.Key(first.Key))
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
			case index == recordVersionSuffix && (next == unsplitRecord || next == startSplitRecord):
				value = bytes.Clone(row.Value)
				sizes.IsSplit = next == startSplitRecord
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
	if value == nil && version != nil {
		return nil, nil, nil, &FoundSplitWithoutStartError{NextIndex: recordVersionSuffix, KeyTuple: primaryKey}
	}
	if !store.useOldVersionFormat() {
		// Java's unsplitter reports a version this transaction saved as incomplete.
		versionKey := store.recordsSubspace.Pack(appendToTuple(primaryKey, recordVersionSuffix))
		if local, found := store.context.GetLocalVersion(versionKey); found {
			if version, err = IncompleteVersion(local); err != nil {
				return nil, nil, nil, err
			}
			sizes.KeyCount++
			sizes.KeySize += len(versionKey)
			sizes.ValueSize += VersionBytes + 1
		}
	}
	sizes.VersionedInline = version != nil
	return primaryKey, value, version, nil
}
