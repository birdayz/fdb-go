// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// ScanRecords scans all records in the store, resuming after continuation's key.
// Matches Java's FDBRecordStoreBase.scanRecords(continuation, scanProperties).
func (store *FDBRecordStore) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	// EventScanRecords is recorded by ScanRecordsInRange, not here — see its
	// doc-comment for why the innermost method is the only recording site.
	return store.ScanRecordsInRange(nil, nil, EndpointTypeTreeStart, EndpointTypeTreeEnd, continuation, scanProperties)
}

// ScanRecordsByType scans records filtered to a specific record type.
// When the primary key uses RecordTypeKey() as its first component (common pattern),
// this does a prefix scan on just that type's range — O(records of this type) instead
// of O(all records). Falls back to full scan + filter if RecordTypeKey is not used.
// Matches Java's RecordQuery with RecordTypeFilter.
func (store *FDBRecordStore) ScanRecordsByType(recordTypeName string, continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType != nil && recordType.PrimaryKey != nil && primaryKeyHasRecordTypePrefix(recordType.PrimaryKey) {
		// Fast path: prefix scan on the record type key range.
		// RecordTypeKey is the first component, so all records of this type
		// have PK starting with (recordTypeKey, ...).
		// Use GetRecordTypeKey() to respect explicit keys from SetRecordTypeKey().
		// Matches Java's RecordType.getRecordTypeKey().
		rtk := recordType.GetRecordTypeKey()
		return store.ScanRecordsInRange(
			tuple.Tuple{rtk}, tuple.Tuple{rtk},
			EndpointTypeRangeInclusive, EndpointTypeRangeInclusive,
			continuation, scanProperties,
		)
	}
	// Slow path: full scan + filter (no RecordTypeKey prefix).
	//
	// Filter on the RESOLVED name, never on the caller's string. GetRecordType
	// accepts a SQL identifier and maps it through the protobuf escaping, so
	// `MY$TABLE` and `MY__1TABLE` both reach here for one type -- but only the
	// stored spelling is what FDBStoredRecord.RecordType.Name holds. Comparing
	// the caller's string made the fast path answer and the slow path return
	// zero rows and a nil error, which is indistinguishable from "this type has
	// no records". An unresolvable name keeps matching nothing, which is what
	// the caller-string fallback preserves.
	wantName := recordTypeName
	if recordType != nil {
		wantName = recordType.Name
	}
	inner := store.ScanRecords(continuation, scanProperties)
	return &filterCursor[*FDBStoredRecord[proto.Message]]{
		inner: inner,
		predicate: func(rec *FDBStoredRecord[proto.Message]) bool {
			return rec.RecordType.Name == wantName
		},
	}
}

// primaryKeyHasRecordTypePrefix checks if the key expression starts with RecordTypeKey().
func primaryKeyHasRecordTypePrefix(expr KeyExpression) bool {
	if _, ok := expr.(*RecordTypeKeyExpression); ok {
		return true
	}
	if concat, ok := expr.(*CompositeKeyExpression); ok && len(concat.expressions) > 0 {
		_, ok := concat.expressions[0].(*RecordTypeKeyExpression)
		return ok
	}
	return false
}

// KeyExpressionHasRecordTypePrefix is the exported form of the per-expression
// prefix check. Used by the SQL layer's covering-index pushdown to decide
// how to slice a primary-key tuple (first element is the record-type key
// when true).
func KeyExpressionHasRecordTypePrefix(expr KeyExpression) bool {
	return primaryKeyHasRecordTypePrefix(expr)
}

// ScanRecordsInRange scans records in a key range.
//
// This is the ONE place EventScanRecords is recorded, because it is the one
// method every record scan funnels through — ScanRecords, ScanRecordsByType and
// the query executor's range scans all end here. Java makes the same choice for
// the same reason: its single recording site is the innermost private
// scanTypedRecords (FDBRecordStore.java:1441), never the outer convenience
// wrappers, so no entry point can scan records without the event and none can
// record it twice. Instrumenting the outer wrapper instead left the executor's
// own scans — which enter here directly — entirely uncounted.
func (store *FDBRecordStore) ScanRecordsInRange(
	low, high tuple.Tuple,
	lowEndpoint, highEndpoint EndpointType,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*FDBStoredRecord[proto.Message]] {
	cursor := store.newKeyValueCursor(low, high, lowEndpoint, highEndpoint, continuation, scanProperties)
	return instrumentCursor(store.context.Timer(), EventScanRecords, cursor)
}

func (store *FDBRecordStore) newKeyValueCursor(
	low, high tuple.Tuple,
	lowEndpoint, highEndpoint EndpointType,
	continuation []byte,
	scanProperties ScanProperties,
) *keyValueCursor {
	recordsSubspace := store.recordsSubspace
	bounds := newKVScanBounds(recordsSubspace, low, high, lowEndpoint, highEndpoint)
	props := scanProperties.ExecuteProperties
	split := store.metaData.IsSplitLongRecords()

	cursor := &keyValueCursor{
		store:           store,
		continuation:    continuation,
		scanProperties:  scanProperties,
		reverse:         scanProperties.IsReverse(),
		keyPrefixLength: len(recordsSubspace.Bytes()),
		bounds:          bounds,
		prefixLength:    bounds.prefixLength,
		scanState:       resolveScanLimiterState(props),
		// Bare-key layout applies ONLY when the store does not split long records;
		// a split-capable store always suffixes its keys even at format < 5. Matches
		// Java's scan logic, which gates the bare-key path on !isSplitLongRecords().
		omitUnsplitRecordSuffix: store.omitUnsplitRecordSuffix() && !split,
		useOldVersionFormat:     store.useOldVersionFormat(),
	}
	cursor.clock = transactionClock(store.context, props, cursor.scanState)
	if cursor.omitUnsplitRecordSuffix {
		cursor.selectorSkip = max(props.Skip, 0)
	}
	if limit := props.ReturnedRowLimit; limit > 0 && !split {
		if cursor.omitUnsplitRecordSuffix {
			cursor.innerLimit = limit
		} else {
			// Java doubles the unsplit inner limit in case records carry versions.
			cursor.innerLimit = saturatingAdd(limit, max(props.Skip, 0))
			cursor.innerLimit = saturatingAdd(cursor.innerLimit, cursor.innerLimit)
		}
	}
	return cursor
}

// CountRecords is Java's FDBRecordStore.countRecords: skip moves the range's
// selector and row and time limits are cleared. An unsplit store counts its
// key-values, inline versions included, without charging the scan limits; a
// split store counts the unsplitter's records, which charge them. A count cut
// short by a scan limit also returns ScanLimitReachedError (RFC-106a).
func (store *FDBRecordStore) CountRecords(
	ctx context.Context,
	low, high tuple.Tuple,
	lowEndpoint, highEndpoint EndpointType,
	continuation []byte,
	scanProperties ScanProperties,
) (int, error) {
	scanProperties.ExecuteProperties = scanProperties.ExecuteProperties.ClearRowAndTimeLimits()
	cursor := store.newKeyValueCursor(low, high, lowEndpoint, highEndpoint, continuation, scanProperties)
	cursor.selectorSkip = max(scanProperties.ExecuteProperties.Skip, 0)
	if err := cursor.initIterator(); err != nil {
		return 0, err
	}
	split := store.metaData.IsSplitLongRecords()
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if !split {
			_, ok, err := cursor.nextKV()
			if err != nil || !ok {
				return count, err
			}
			count++
			continue
		}
		ok, reason, cont, err := cursor.unsplit(ctx)
		if err != nil {
			return count, err
		}
		if !ok {
			return count, errIfDrainTruncated(NewResultNoNext[*FDBStoredRecord[proto.Message]](reason, cont))
		}
		count++
	}
}

// FDBStoredRecord represents a record that has been stored in or loaded from FDB
// This is generic to match Java's FDBStoredRecord<M extends Message>
type FDBStoredRecord[M proto.Message] struct {
	// PrimaryKey is the record's primary key
	PrimaryKey tuple.Tuple

	// RecordType is the type of this record
	RecordType *RecordType

	// Record is the actual record data
	Record M

	// Version is the record's version, if loaded.
	// Matches Java's FDBStoredRecord.getVersion().
	Version *FDBRecordVersion

	// Store is the record store this record belongs to.
	// Matches Java's FDBRecord.getStore().
	Store *FDBRecordStore

	// Storage size information
	KeyCount  int
	KeySize   int
	ValueSize int

	// Whether the record is split across multiple keys
	Split bool

	// VersionedInline reports a version stored with the record and counted in its sizes.
	VersionedInline bool

	// wire is the stored bytes the record was decoded from, for a type that
	// reaches a map field (internal/recordlayer/rlwire/map_order.go); nil otherwise.
	wire *recordWire
}

// HasVersion returns whether this stored record has a version.
// Matches Java's FDBStoredRecord.hasVersion().
func (r *FDBStoredRecord[M]) HasVersion() bool {
	return r.Version != nil
}

// omitUnsplitRecordSuffix reports whether records are stored WITHOUT the unsplit
// record suffix (legacy layout: the value lives at the bare key
// recordsSubspace.pack(primaryKey) with no trailing suffix). This only takes
// effect when split_long_records is false; split stores always suffix their keys.
//
// Matches Java's FDBRecordStore.omitUnsplitRecordSuffix field, as initialized in
// checkVersion(): the persisted DataStoreInfo flag is authoritative once the store
// is at format version >= SAVE_UNSPLIT_WITH_SUFFIX (5); below that the records were
// necessarily saved without a suffix, so the flag is implicitly true.
func (store *FDBRecordStore) omitUnsplitRecordSuffix() bool {
	h := store.storeHeader
	if h == nil {
		// No header yet (new store being created) — current layout.
		return false
	}
	if h.GetFormatVersion() >= formatVersionSaveUnsplitWithSuffix {
		// "Note that this depends on the property that calling get on an unset
		// boolean field results in getting back false." (Java checkVersion.)
		return h.GetOmitUnsplitRecordSuffix()
	}
	return true
}

// useOldVersionFormat reports whether record versions are stored in the legacy
// separate RecordVersionKey(8) subspace (keyed by the bare primary key) rather
// than inline adjacent to the record at recordsSubspace.pack(pk, -1).
//
// Matches Java's FDBRecordStore.useOldVersionFormat():
//
//	formatVersion < SAVE_VERSION_WITH_RECORD (6) || omitUnsplitRecordSuffix
//
// The omit clause is required because a store that omits the unsplit suffix has
// no place to put the inline version key, so versions stay in the old subspace
// even after the format version is bumped past 6.
func (store *FDBRecordStore) useOldVersionFormat() bool {
	h := store.storeHeader
	if h == nil {
		return false
	}
	return h.GetFormatVersion() < formatVersionSaveVersionWithRecord || store.omitUnsplitRecordSuffix()
}

// EstimateStoreSize returns the estimated byte size of the entire store subspace.
// Uses FDB's GetEstimatedRangeSizeBytes which provides an approximation.
// Matches Java's FDBRecordStore.estimateStoreSizeAsync().
func (store *FDBRecordStore) EstimateStoreSize() (int64, error) {
	begin, end := store.subspace.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	return store.context.Transaction().GetEstimatedRangeSizeBytes(kr).Get()
}

// EstimateRecordsSize returns the estimated byte size of the records subspace only.
// Matches Java's FDBRecordStore.estimateRecordsSizeAsync().
func (store *FDBRecordStore) EstimateRecordsSize() (int64, error) {
	recordsSub := store.recordsSubspace
	begin, end := recordsSub.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	return store.context.Transaction().GetEstimatedRangeSizeBytes(kr).Get()
}
