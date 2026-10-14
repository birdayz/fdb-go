// Portions derived from FoundationDB Record Layer (FDBRecordStoreBase.java,
// FDBRecordStore.java, StandardIndexMaintainer.java, IndexMaintainer.java,
// IndexPrefetchRangeKeyValueCursor.java), Copyright Apple Inc. and the
// FoundationDB project authors. Licensed under the Apache License, Version 2.0;
// translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// remoteFetchMinAPIVersion is APIVersion.API_VERSION_7_1.
const remoteFetchMinAPIVersion = 710

// FDBIndexedRawRecord is an index entry with the record rows the storage server
// returned for it. Matches Java's FDBIndexedRawRecord.
type FDBIndexedRawRecord struct {
	IndexEntry *IndexEntry
	RawRecord  fdb.MappedKeyValue
}

// RemoteFetchIndexMaintainer is the optional form of Java's
// IndexMaintainer.scanRemoteFetch. The maintainer checks the scan and supplies
// the mapper, as StandardIndexMaintainer.scanRemoteFetchByValue does before it
// builds its cursor; the store issues the mapped read. A maintainer without it
// has Java's default, which throws UnsupportedRemoteFetchIndexException.
type RemoteFetchIndexMaintainer interface {
	RemoteFetchMapper(scanType IndexScanType, commonPrimaryKeyLength int) (tuple.Tuple, error)
}

var (
	_ RemoteFetchIndexMaintainer = (*StandardIndexMaintainer)(nil)
	_ RemoteFetchIndexMaintainer = (*versionIndexMaintainer)(nil)
)

func unsupportedRemoteFetchIndex(index *Index) error {
	return &UnsupportedRemoteFetchIndexError{
		Message:   "scanRemoteFetch operation is not supported by this index maintainer for Index " + index.Name,
		IndexName: index.Name,
	}
}

// RemoteFetchMapper is Java's ValueIndexMaintainer.scanRemoteFetch. Go shares
// one maintainer type between VALUE and the types built on it, which keep
// Java's unsupported default.
func (m *StandardIndexMaintainer) RemoteFetchMapper(scanType IndexScanType, commonPrimaryKeyLength int) (tuple.Tuple, error) {
	store, ok := m.store.(*FDBRecordStore)
	if !ok || canonicalIndexType(m.index.Type) != IndexTypeValue {
		return nil, unsupportedRemoteFetchIndex(m.index)
	}
	return remoteFetchMapperByValue(m.index, m.indexSubspace, store.RecordsSubspace(), scanType, commonPrimaryKeyLength)
}

// RemoteFetchMapper is Java's VersionIndexMaintainer.scanRemoteFetch.
func (m *versionIndexMaintainer) RemoteFetchMapper(scanType IndexScanType, commonPrimaryKeyLength int) (tuple.Tuple, error) {
	store, ok := m.store.(*FDBRecordStore)
	if !ok {
		return nil, unsupportedRemoteFetchIndex(m.index)
	}
	return remoteFetchMapperByValue(m.index, m.indexSubspace, store.RecordsSubspace(), scanType, commonPrimaryKeyLength)
}

// remoteFetchMapperByValue is the checks and mapper of
// StandardIndexMaintainer.scanRemoteFetchByValue.
func remoteFetchMapperByValue(index *Index, indexSubspace, recordsSubspace subspace.Subspace, scanType IndexScanType, commonPrimaryKeyLength int) (tuple.Tuple, error) {
	if commonPrimaryKeyLength <= 0 {
		return nil, &RecordCoreArgumentError{Message: "scanRemoteFetch requires a positive commonPrimaryKeyLength"}
	}
	if scanType != IndexScanByValue {
		return nil, &RecordCoreArgumentError{Message: "scanRemoteFetch can only be used with VALUE index scan type and Range Scan"}
	}
	return createRemoteFetchMapper(index, indexSubspace, recordsSubspace, commonPrimaryKeyLength)
}

// remoteFetchReader returns the transaction's mapped read. Java fails a
// snapshot getMappedRange with UnsupportedOperationException; a client without
// mapped reads (the libfdb_c backend) reports Java's unsupported remote fetch.
func remoteFetchReader(tx fdb.WritableTransaction, index *Index, props ScanProperties) (fdb.MappedRangeReader, error) {
	if props.ExecuteProperties.IsolationLevel.IsSnapshot() {
		return nil, &UnsupportedOperationError{Message: "getMappedRange is only supported in serializable"}
	}
	reader, ok := tx.(fdb.MappedRangeReader)
	if !ok {
		return nil, &UnsupportedRemoteFetchIndexError{
			Message:   "remote fetch is not supported by this transaction's client for Index " + index.Name,
			IndexName: index.Name,
		}
	}
	return reader, nil
}

// createRemoteFetchMapper is StandardIndexMaintainer.createRemoteFetchMapper:
// the records subspace, then {K[i]} for each primary-key position in the
// index key counted from the start of the packed key, then {...}.
func createRemoteFetchMapper(index *Index, indexSubspace, recordsSubspace subspace.Subspace, commonPrimaryKeyLength int) (tuple.Tuple, error) {
	prefix, err := tuple.Unpack(indexSubspace.Bytes())
	if err != nil {
		return nil, err
	}
	mapper, err := tuple.Unpack(recordsSubspace.Bytes())
	if err != nil {
		return nil, err
	}
	for _, position := range index.EntryPrimaryKeyPositions(commonPrimaryKeyLength) {
		mapper = append(mapper, fmt.Sprintf("{K[%d]}", position+len(prefix)))
	}
	return append(mapper, "{...}"), nil
}

// CreateRemoteFetchMapper returns the mapper this store's index sends with a
// remote-fetch scan.
func (store *FDBRecordStore) CreateRemoteFetchMapper(index *Index, commonPrimaryKeyLength int) (tuple.Tuple, error) {
	return createRemoteFetchMapper(index, store.IndexSubspace(index), store.RecordsSubspace(), commonPrimaryKeyLength)
}

// mappedIndexRows feeds a mapped read's primary rows to the ordinary index
// cursor, so limits, continuations and index entries are shared with
// SCAN_AND_FETCH, as Java's cursors share KeyValueCursorBase. current is valid
// because the index cursor calls Get once for each entry it returns, and
// indexPrefetchCursor reads it before advancing again.
type mappedIndexRows struct {
	rows    fdb.MappedRangeIterator
	current fdb.MappedKeyValue
}

func (r *mappedIndexRows) Advance() bool { return r.rows.Advance() }

func (r *mappedIndexRows) Get() (fdb.KeyValue, error) {
	row, err := r.rows.Get()
	if err != nil {
		return fdb.KeyValue{}, err
	}
	r.current = row
	return fdb.KeyValue{Key: fdb.Key(row.Key), Value: row.Value}, nil
}

// indexPrefetchCursor is IndexPrefetchRangeKeyValueCursor mapped through
// StandardIndexMaintainer.unpackRemoteFetchRecord.
type indexPrefetchCursor struct {
	entries *indexCursor
	rows    *mappedIndexRows
}

func (c *indexPrefetchCursor) OnNext(ctx context.Context) (RecordCursorResult[FDBIndexedRawRecord], error) {
	result, err := c.entries.OnNext(ctx)
	if err != nil {
		return RecordCursorResult[FDBIndexedRawRecord]{}, err
	}
	if !result.HasNext() {
		return NewResultNoNext[FDBIndexedRawRecord](result.GetNoNextReason(), result.GetContinuation()), nil
	}
	raw := FDBIndexedRawRecord{IndexEntry: result.GetValue(), RawRecord: c.rows.current}
	return NewResultWithValue(raw, result.GetContinuation()), nil
}

func (c *indexPrefetchCursor) Close() error   { return c.entries.Close() }
func (c *indexPrefetchCursor) IsClosed() bool { return c.entries.IsClosed() }

// GetCommonPrimaryKeyLength is the primary-key length shared by every record
// type of the index, or -1. Matches Java's FDBRecordStoreBase.getCommonPrimaryKeyLength.
func (store *FDBRecordStore) GetCommonPrimaryKeyLength(index *Index) int {
	common := -1
	for i, recordType := range store.metaData.RecordTypesForIndex(index) {
		size := recordType.PrimaryKey.ColumnSize()
		if i == 0 {
			common = size
		} else if size != common {
			return -1
		}
	}
	return common
}

// ScanIndexRemoteFetch scans an index and has the storage server return each
// entry's record with it. Matches Java's FDBRecordStore.scanIndexRemoteFetch.
func (store *FDBRecordStore) ScanIndexRemoteFetch(index *Index, scanType IndexScanType, scanRange TupleRange, commonPrimaryKeyLength int, continuation []byte, props ScanProperties, orphan IndexOrphanBehavior) RecordCursor[*FDBIndexedRecord] {
	cursor, err := store.scanIndexRemoteFetch(index, scanType, scanRange, commonPrimaryKeyLength, continuation, props, orphan)
	if err != nil {
		return NewErrorCursor[*FDBIndexedRecord](err)
	}
	return cursor
}

// scanIndexRemoteFetch is FDBRecordStore.scanIndexRemoteFetchInternal. Its
// errors are the exceptions Java throws before returning a cursor.
func (store *FDBRecordStore) scanIndexRemoteFetch(index *Index, scanType IndexScanType, scanRange TupleRange, commonPrimaryKeyLength int, continuation []byte, props ScanProperties, orphan IndexOrphanBehavior) (RecordCursor[*FDBIndexedRecord], error) {
	if commonPrimaryKeyLength <= 0 {
		return nil, &RecordCoreArgumentError{Message: "commonPrimaryKeyLength has to be a positive number", IndexName: index.Name}
	}
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return nil, err
	}
	if !state.IsScannable() {
		return nil, &IndexNotReadableError{IndexName: index.Name, CurrentState: state}
	}
	if scanType != IndexScanByValue {
		return nil, &RecordCoreArgumentError{Message: "Index remote fetch can only be used with VALUE index scan.", IndexName: index.Name}
	}
	if err := remoteFetchAPIVersionCheck(selectedAPIVersion()); err != nil {
		return nil, err
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	remote, ok := IndexMaintainerAs[RemoteFetchIndexMaintainer](maintainer)
	if !ok {
		return nil, unsupportedRemoteFetchIndex(index)
	}
	mapper, err := remote.RemoteFetchMapper(scanType, commonPrimaryKeyLength)
	if err != nil {
		return nil, err
	}
	if props.ExecuteProperties.ScanState == nil {
		props.ExecuteProperties.ScanState = NewScanLimiterState()
	}
	// Like Java's IndexPrefetchRangeKeyValueCursor.Builder.build: resolve the
	// continuation, then start the mapped read before returning.
	tx := store.context.Transaction()
	entries := newIndexCursor(index, store.IndexSubspace(index), tx, scanRange, continuation, props).withContext(store.context)
	rng, err := entries.bounds.keyRange(continuation, props.IsReverse())
	if err != nil {
		return nil, err
	}
	if props.ExecuteProperties.Skip > 0 {
		// A skip would be sent as a selector offset on the mapped read, a request
		// shape the server does not support for every range. Having checked
		// everything remote fetch requires, scan the index and load the records.
		if _, err := remoteFetchReader(tx, index, props); err != nil {
			return nil, err
		}
		return store.scanIndexRecordsByType(index, scanType, scanRange, continuation, orphan, props), nil
	}
	// Java's InstrumentedReadTransaction counts the call before a snapshot
	// transaction refuses it.
	store.context.Timer().Increment(CountRemoteFetch)
	reader, err := remoteFetchReader(tx, index, props)
	if err != nil {
		return nil, err
	}
	rows := &mappedIndexRows{rows: reader.GetMappedRange(rng, mapper.Pack(), kvRangeOptions(props)).Iterator()}
	entries.iterator = rows
	var raw RecordCursor[FDBIndexedRawRecord] = &indexPrefetchCursor{entries: entries, rows: rows}
	loader := &remoteIndexRecordLoader{store: store, scanState: props.ExecuteProperties.ScanState, orphan: orphan}
	cursor := MapPipelined(raw, loader.load, 1)
	if orphan == IndexOrphanSkip {
		cursor = FilterCursor(cursor, func(record *FDBIndexedRecord) bool { return record != nil })
	}
	return instrumentCursor(store.context.Timer(), EventScanRemoteFetchEntry, cursor), nil
}

// selectedAPIVersion is the facade's selected API version, which every
// transaction the record layer opens uses; 0 if none was selected.
func selectedAPIVersion() int {
	version, err := fdb.GetAPIVersion()
	if err != nil {
		return 0
	}
	return version
}

func remoteFetchAPIVersionCheck(version int) error {
	if version < remoteFetchMinAPIVersion {
		return &UnsupportedMethodError{Message: "Index Remote Fetch can only be used with API_VERSION of at least 7.1."}
	}
	return nil
}

// ScanIndexRecordsWithFetchMethod scans the records an index points to with
// the given fetch method. WITH_FALLBACK falls back to SCAN_AND_FETCH when
// remote fetch cannot start, or fails later from the last result it returned.
// Matches Java's FDBRecordStoreBase.scanIndexRecords(Index, IndexFetchMethod,
// IndexScanBounds, int, byte[], IndexOrphanBehavior, ScanProperties).
func (store *FDBRecordStore) ScanIndexRecordsWithFetchMethod(index *Index, method IndexFetchMethod, scanType IndexScanType, scanRange TupleRange, commonPrimaryKeyLength int, continuation []byte, orphan IndexOrphanBehavior, props ScanProperties) RecordCursor[*FDBIndexedRecord] {
	if props.ExecuteProperties.ScanState == nil {
		// The remote cursor and its fallback charge one budget, as Java's share ExecuteState.
		props.ExecuteProperties.ScanState = NewScanLimiterState()
	}
	switch method {
	case IndexFetchScanAndFetch:
		return store.scanIndexRecordsByType(index, scanType, scanRange, continuation, orphan, props)
	case IndexFetchUseRemoteFetch:
		return store.ScanIndexRemoteFetch(index, scanType, scanRange, commonPrimaryKeyLength, continuation, props, orphan)
	case IndexFetchUseRemoteFetchWithFallback:
		remote, err := store.scanIndexRemoteFetch(index, scanType, scanRange, commonPrimaryKeyLength, continuation, props, orphan)
		if err != nil {
			return store.scanIndexRecordsByType(index, scanType, scanRange, continuation, orphan, props)
		}
		return Fallback(remote, func(last *RecordCursorResult[*FDBIndexedRecord]) RecordCursor[*FDBIndexedRecord] {
			// Java's remoteFetchFallbackFrom resumes after the last result returned.
			resume := continuation
			if last != nil {
				var err error
				if resume, err = last.GetContinuation().ToBytes(); err != nil {
					return NewErrorCursor[*FDBIndexedRecord](err)
				}
			}
			return store.scanIndexRecordsByType(index, scanType, scanRange, resume, orphan, props)
		})
	default:
		return NewErrorCursor[*FDBIndexedRecord](&RecordCoreError{Message: fmt.Sprintf("Unknown fetchMethod option: %v", method)})
	}
}

// scanIndexRecordsByType is SCAN_AND_FETCH: Java's scanIndexRecords(Index,
// IndexScanType, TupleRange, byte[], IndexOrphanBehavior, ScanProperties).
func (store *FDBRecordStore) scanIndexRecordsByType(index *Index, scanType IndexScanType, scanRange TupleRange, continuation []byte, orphan IndexOrphanBehavior, props ScanProperties) RecordCursor[*FDBIndexedRecord] {
	if props.ExecuteProperties.ScanState == nil {
		props.ExecuteProperties.ScanState = NewScanLimiterState()
	}
	entries := store.ScanIndexByType(index, scanType, scanRange, continuation, props)
	loader := &indexRecordLoader{store: store, scanState: props.ExecuteProperties.ScanState, orphan: orphan}
	cursor := MapPipelined(entries, loader.load, DefaultPipelineSize)
	if orphan == IndexOrphanSkip {
		cursor = FilterCursor(cursor, func(record *FDBIndexedRecord) bool { return record != nil })
	}
	return cursor
}

// handleOrphanEntry applies the orphan behavior to an entry without a record.
// Java words the error differently for the two fetch methods.
func (store *FDBRecordStore) handleOrphanEntry(entry *IndexEntry, primaryKey tuple.Tuple, orphan IndexOrphanBehavior, prefetched bool) (*FDBIndexedRecord, error) {
	switch orphan {
	case IndexOrphanSkip:
		return nil, nil
	case IndexOrphanReturn:
		return &FDBIndexedRecord{IndexEntry: entry}, nil
	case IndexOrphanError:
		store.context.Timer().Increment(CountBadIndexEntry)
		message := "record not found from index entry"
		if prefetched {
			message = "record not found for prefetched index entry"
		}
		indexName := ""
		if entry.Index != nil {
			indexName = entry.Index.Name
		}
		return nil, &RecordCoreStorageError{Message: message, IndexName: indexName, PrimaryKey: primaryKey, IndexKey: entry.Key}
	default:
		return nil, &RecordCoreError{Message: fmt.Sprintf("Unexpected index orphan behavior: %v", orphan)}
	}
}
