// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBRecordStoreBase.java, FormatVersion.java, IndexingCommon.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
)

// RebuildIndex rebuilds an index within the current transaction.
// Clears existing index data, scans all records, and re-indexes them.
// Upon completion, the index is marked READABLE.
//
// Because this runs in a single transaction, it is limited by FDB's
// 5-second time limit and 10MB transaction size. For large stores,
// use OnlineIndexer.BuildIndex() which splits work across transactions.
//
// Matches Java's FDBRecordStore.rebuildIndex() which delegates to
// IndexingBase.rebuildIndexAsync() for the in-transaction path.
func (store *FDBRecordStore) RebuildIndex(index *Index) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	if err := store.checkIndexCollation(index); err != nil {
		return err
	}
	startTime := time.Now()
	defer func() { store.context.Timer().RecordSince(EventRebuildIndex, startTime) }()

	// Step 1: Clear index data and mark WRITE_ONLY.
	// Matches Java: clearAndMarkIndexWriteOnly(index)
	if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: clear and mark write-only: %w", index.Name, err)
	}

	// Step 2: Pre-mark the full range as built in the RangeSet.
	// Java does this BEFORE scanning records so that even if marking readable
	// fails (e.g. uniqueness violations), the range set records that all data
	// was scanned, preventing re-scanning on future builds.
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	if _, err := rangeSet.InsertRange(store.context.Transaction(), nil, nil, true); err != nil {
		return fmt.Errorf("rebuild index %q: insert full range: %w", index.Name, err)
	}

	// Step 3: Scan the records that can possibly feed this index, and build entries.
	// Java's inline rebuild is NOT a whole-store scan: rebuildIndexInternalAsync starts
	// from common.computeRecordsRange() and "can skip indexing records that are outside
	// this range" (IndexingMultiTargetByRecords.java:187-193). Note the range set above
	// is still marked built over the WHOLE range — the skipped keys cannot hold a record
	// of an indexed type, so there is nothing there left to build.
	scanProps := ForwardScan()
	var cursor RecordCursor[*FDBStoredRecord[proto.Message]]
	if low, high, ok := store.indexedRecordTypesRange(index); ok {
		cursor = store.ScanRecordsInRange(low, high, EndpointTypeRangeInclusive, EndpointTypeRangeInclusive, nil, scanProps)
	} else {
		cursor = store.ScanRecords(nil, scanProps)
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return fmt.Errorf("rebuild index %q: get maintainer: %w", index.Name, err)
	}

	for rec, err := range Seq2(cursor, store.context.ctx) {
		if err != nil {
			return fmt.Errorf("rebuild index %q: scan records: %w", index.Name, err)
		}

		if !store.shouldIndexRecordForIndex(rec, index) {
			continue
		}

		if err := maintainer.Update(nil, rec); err != nil {
			return fmt.Errorf("rebuild index %q: index record pk=%v: %w", index.Name, rec.PrimaryKey, err)
		}
	}

	// Step 4: Try to mark the index READABLE. Failure LEAVES IT WRITE_ONLY and is
	// reported to the log, NOT to the caller.
	//
	// Two Java facts compose here, and using only the first one gets this wrong.
	//
	//  1. The mark itself is strict. Java's inline rebuild calls the ONE-ARGUMENT
	//     markIndexReadable(index) (FDBRecordStore.java:4602), which is
	//     markIndexReadable(index, allowUniquePending=FALSE) (:3767-3768); with a
	//     violation present checkAndUpdateBuiltIndexState (:3821) throws
	//     RecordIndexUniquenessViolation (:3856-3861). It does NOT settle on
	//     READABLE_UNIQUE_PENDING — that state is reachable in Java core only via
	//     IndexingBase.java:324 under IndexingPolicy.shouldAllowUniquePendingState
	//     (OnlineIndexer.java:1117), which defaults FALSE (:1220).
	//
	//  2. The REBUILD SWALLOWS that throw. rebuildIndex wraps the whole
	//     build-then-mark chain in `.handle((b, t) -> { if (t != null)
	//     logExceptionAsWarn(...); indexBuilder.close(); return null; })`
	//     (FDBRecordStore.java:4602-4615). A CompletableFuture handle that returns
	//     normally COMPLETES NORMALLY, so the failure never reaches checkVersion
	//     and the store open commits.
	//
	// The net Java behaviour is pinned by its own test,
	// FDBRecordStoreUniqueIndexTest.addUniqueIndexViaCheckVersion:615-627 — open
	// the store with a unique index over violating data and it asserts
	// REBUILD_INDEX ran, the index is WRITE_ONLY (or READABLE_UNIQUE_PENDING),
	// scanUniquenessViolations holds every conflicting primary key, and
	// commit(context) SUCCEEDS.
	//
	// That combination is the safe one, and propagating instead would be strictly
	// worse: a WRITE_ONLY index is not scannable, so queries fall back to base
	// scans and no read is wrong, whereas a propagated error turns any metadata
	// evolution that meets bad data into a STORE-OPEN OUTAGE — every transaction
	// against the store fails, not just the index. The loud signal is the index
	// being unusable plus the durable violations, not a failed open.
	if _, err := store.MarkIndexReadable(index.Name); err != nil {
		// Java: logExceptionAsWarn("rebuilding index failed", ...) and continue.
		// The index stays WRITE_ONLY, awaiting an explicit OnlineIndexer run.
		slog.Default().Warn("rebuilding index failed",
			"index_name", index.Name,
			"index_version", index.LastModifiedVersion,
			"subspace_key", fmt.Sprintf("%v", index.SubspaceTupleKey()),
			"error", err)
	}

	return nil
}

// indexedRecordTypesRange returns the inclusive record-type-key bounds spanning the
// record types index covers, and ok=false when the whole records space may be
// relevant. Port of Java's IndexingCommon.computeRecordsRange
// (IndexingCommon.java:211-229):
//
//	for (RecordType recordType : getAllRecordTypes()) {
//	    if (!recordType.primaryKeyHasRecordTypePrefix() || recordType.isSynthetic()) {
//	        // If any of the types to build for does not have a prefix, give up.
//	        return null;
//	    }
//	    Tuple prefix = recordType.getRecordTypeKeyTuple();
//	    ...low = min, high = max...
//	}
//
// One type gives that type's range; several give the span from the lexicographically
// first type key to the last, which may include records of types in between — Java
// accepts that, and the per-record type test still filters them.
//
// This is the index's OWN record types, not the caller's single-type narrowing: a
// rebuild is per index and knows exactly what it indexes, whereas
// singleRecordTypeWithPrefixKey answers a different question (do ALL the indexes being
// built agree on one type) that only the shared record-count probe needs.
func (store *FDBRecordStore) indexedRecordTypesRange(index *Index) (low, high tuple.Tuple, ok bool) {
	var lowBytes, highBytes []byte
	for _, recordType := range store.metaData.RecordTypesForIndex(index) {
		if !recordType.PrimaryKeyHasRecordTypePrefix() || recordType.IsSynthetic() {
			return nil, nil, false
		}
		// Java's Tuple.compareTo is the order of the packed bytes, for a type
		// key of any tuple type.
		prefix := tuple.Tuple{recordType.GetRecordTypeKey()}
		packed := prefix.Pack()
		if low == nil || bytes.Compare(packed, lowBytes) < 0 {
			low, lowBytes = prefix, packed
		}
		if high == nil || bytes.Compare(packed, highBytes) > 0 {
			high, highBytes = prefix, packed
		}
	}
	if low == nil {
		return nil, nil, false
	}
	return low, high, true
}

// checkPossiblyRebuild compares the stored metadata version with the current
// metadata version. If the current metadata has a higher version, indexes added
// since the old version are rebuilt or marked according to the IndexRebuildPolicy.
// Matches Java's FDBRecordStore.checkPossiblyRebuild() / checkRebuild() /
// getStatesForRebuildIndexes().
func (store *FDBRecordStore) checkPossiblyRebuild(storeHeader *metadatapb.DataStoreInfo) error {
	oldMetaDataVersion := int(storeHeader.GetMetaDataversion())
	newMetaDataVersion := store.metaData.Version()

	// Stale metadata check: stored version is newer than local version.
	// Matches Java: throws RecordStoreStaleMetaDataVersionException.
	if oldMetaDataVersion > newMetaDataVersion {
		return &StaleMetaDataVersionError{
			LocalVersion:  newMetaDataVersion,
			StoredVersion: oldMetaDataVersion,
		}
	}

	// Captured BEFORE the upgrade below overwrites it. Java keeps the same value
	// under the same name (checkPossiblyRebuild's `oldFormatVersion`,
	// FDBRecordStore.java:5155) and hands it to checkPossiblyRebuildRecordCounts,
	// which needs to know what the store looked like on disk rather than what it is
	// about to become. Zero means the header had no format version at all, i.e. a
	// brand-new store — Java's `newStore = oldFormatVersion == 0`.
	oldFormatVersion := storeHeader.GetFormatVersion()

	// Upgrade the persisted format version (and migrate the on-disk layout if needed)
	// BEFORE reading/rebuilding anything, matching Java's checkRebuild() which sets the
	// format version and converts record versions at the very start. This runs whether
	// or not the metadata version changed.
	formatUpgraded, err := store.maybeUpgradeFormatVersion(storeHeader)
	if err != nil {
		return err
	}

	// THE TRANSITION GATE. Everything below runs only when something actually
	// changed — Java returns early otherwise (FDBRecordStore.java:4646-4648):
	//
	//	if (!formatVersionChanged && !metaDataVersionChanged) {
	//	    return AsyncUtil.DONE;
	//	}
	//
	// checkRebuild — and with it checkPossiblyRebuildRecordCounts (:4728) — is
	// reachable ONLY through that transition. Running the record-count check on
	// every open instead is not a harmless eagerness: the pre-RECORD_COUNT_ADDED
	// arm tests `oldFormatVersion < 2`, which for a store PINNED at format 1 is
	// true on every open, forever. That is a perpetual clear-and-rescan of every
	// record inside the store-open transaction — a large pinned store would stop
	// being openable at all — and even with no count key declared it repeats the
	// range clear and header rewrite on every open. The other arms converge after
	// one pass because they compare against a header they then update; this one
	// never can, because the store's format version is exactly what is pinned.
	//
	// Gating loses no detection. A change to the metadata's record-count key bumps
	// the metadata version (RecordMetaDataBuilder.SetRecordCountKey, metadata.go:
	// "bumps version when value changes", mirroring Java), so any real count-key
	// change arrives here with metaDataVersionChanged already true.
	metaDataVersionChanged := newMetaDataVersion != oldMetaDataVersion
	if !formatUpgraded && !metaDataVersionChanged {
		return nil
	}

	rebuildRecordCounts, err := store.checkPossiblyRebuildRecordCounts(storeHeader, oldFormatVersion)
	if err != nil {
		return fmt.Errorf("rebuild record counts: %w", err)
	}
	needHeaderWrite := formatUpgraded || rebuildRecordCounts

	if !metaDataVersionChanged {
		// Format changed but metadata did not: persist whatever the format upgrade
		// and the record-count check left on the header, then stop — there are no
		// indexes to reconcile.
		if needHeaderWrite {
			if err := store.writeStoreHeader(storeHeader); err != nil {
				return fmt.Errorf("update store header after record count rebuild: %w", err)
			}
		}
		return nil
	}

	// Version changed — set the flag (matches Java's versionChanged field).
	store.versionChanged = true

	// Clean up data for former indexes (dropped since old version).
	// Matches Java's checkRebuild() which calls removeFormerIndex() for each,
	// clearing INDEX_KEY, INDEX_SECONDARY_SPACE_KEY, INDEX_RANGE_SPACE_KEY,
	// INDEX_STATE_SPACE_KEY, and INDEX_UNIQUENESS_VIOLATIONS_KEY subspaces.
	for _, former := range store.metaData.GetFormerIndexes() {
		if former.RemovedVersion > oldMetaDataVersion {
			if err := store.removeFormerIndexData(former); err != nil {
				return err
			}
		}
	}

	if err := store.rebuildNewIndexes(storeHeader, oldMetaDataVersion, newMetaDataVersion, rebuildRecordCounts); err != nil {
		return err
	}

	// Update store header with new metadata version and format version.
	// Matches Java's checkRebuild() which sets info.setFormatVersion(formatVersion).
	newVersion := int32(newMetaDataVersion)
	storeHeader.MetaDataversion = &newVersion
	// The format version this store OPENED at, not the newest the binary knows —
	// otherwise a reconciliation would silently undo a deliberately pinned format
	// (see maybeUpgradeFormatVersion). Never downgrade a header already past it.
	if fmtVersion := store.effectiveFormatVersion(); storeHeader.GetFormatVersion() < fmtVersion {
		storeHeader.FormatVersion = &fmtVersion
	}
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return fmt.Errorf("update store header after rebuild: %w", err)
	}

	return store.removeReplacedIndexes()
}

func (store *FDBRecordStore) rebuildNewIndexes(storeHeader *metadatapb.DataStoreInfo, oldMetaDataVersion, newMetaDataVersion int, rebuildRecordCounts bool) error {
	// Find indexes added since the old version.
	indexesToBuild := store.metaData.GetIndexesSince(oldMetaDataVersion)
	if len(indexesToBuild) == 0 {
		return nil
	}

	// Empty-store unsplit-format upgrade: when a store that still omits the unsplit
	// record suffix gains indexes at format >= SAVE_UNSPLIT_WITH_SUFFIX and is empty,
	// drop the omit flag so future records adopt the modern suffixed layout. This is
	// safe only on an empty store (no records to rewrite). Matches Java's
	// checkRebuildIndexes (FDBRecordStore.java:4728-4754 → clearOmitUnsplitRecordSuffix).
	// recordsSubspaceEmpty does a non-snapshot read so a concurrent insert invalidates
	// this decision rather than racing it — see recordsRangeEmpty for why that
	// deliberately diverges from Java's snapshot probe.
	if store.omitUnsplitRecordSuffix() && storeHeader.GetFormatVersion() >= formatVersionSaveUnsplitWithSuffix {
		empty, emptyErr := store.recordsSubspaceEmpty()
		if emptyErr != nil {
			return fmt.Errorf("check store emptiness for unsplit-format upgrade: %w", emptyErr)
		}
		if empty {
			storeHeader.OmitUnsplitRecordSuffix = nil
		}
	}

	// If all the new indexes are only for a record type whose primary key has a
	// type prefix, then we can scan less. Matches Java's checkRebuildIndexes
	// (FDBRecordStore.java:4747-4754), which computes this once and threads it
	// through both the record count and the record size.
	singleRecordType := store.singleRecordTypeWithPrefixKey(indexesToBuild)

	// Get record count for the policy decision (lazy in Java, eager here).
	recordCount, err := store.getRecordCountForRebuildPolicy(indexesToBuild, singleRecordType, rebuildRecordCounts)
	if err != nil {
		return fmt.Errorf("check record count for rebuild: %w", err)
	}

	for _, index := range indexesToBuild {
		if err := store.applyIndexRebuildPolicy(index, recordCount, oldMetaDataVersion, newMetaDataVersion); err != nil {
			return err
		}
	}
	return nil
}

func (store *FDBRecordStore) applyIndexRebuildPolicy(index *Index, recordCount int64, oldMetaDataVersion, newMetaDataVersion int) error {
	indexOnNewRecordTypes := store.areAllRecordTypesSince(index, oldMetaDataVersion)
	desiredState := IndexStateDisabled
	if len(index.GetReplacedByIndexNames()) == 0 {
		desiredState = store.indexRebuildPolicy(index, recordCount, indexOnNewRecordTypes)
	}

	switch desiredState {
	case IndexStateReadable:
		if err := store.RebuildIndex(index); err != nil {
			return fmt.Errorf("auto-rebuild index %q on metadata version change (%d -> %d): %w",
				index.Name, oldMetaDataVersion, newMetaDataVersion, err)
		}
	case IndexStateWriteOnlyWithQueue:
		if _, err := store.ClearAndMarkIndexWriteOnlyWithQueue(index.Name); err != nil {
			return fmt.Errorf("mark index %q write-only with queue: %w", index.Name, err)
		}
	case IndexStateWriteOnly:
		// Always clear and re-mark, matching Java's rebuildOrMarkIndex().
		// The header version update and clearAndMark are in the same FDB
		// transaction, so crash recovery is atomic — either both happen or
		// neither. No need to check current state.
		if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
			return fmt.Errorf("mark index %q write-only: %w", index.Name, err)
		}
	case IndexStateDisabled:
		if _, err := store.MarkIndexDisabled(index.Name); err != nil {
			return fmt.Errorf("mark index %q disabled: %w", index.Name, err)
		}
	}
	return nil
}

// areAllRecordTypesSince returns true if every record type associated with
// the given index was added after oldMetaDataVersion (i.e. all have
// SinceVersion > oldMetaDataVersion). For universal indexes, checks all
// record types. Matches Java's FDBRecordStore.areAllRecordTypesSince().
func (store *FDBRecordStore) areAllRecordTypesSince(index *Index, oldMetaDataVersion int) bool {
	// Universal indexes apply to all record types.
	isUniversal := false
	for _, uIdx := range store.metaData.GetUniversalIndexes() {
		if uIdx.Name == index.Name {
			isUniversal = true
			break
		}
	}

	if isUniversal {
		for _, rt := range store.metaData.RecordTypes() {
			if rt.SinceVersion == 0 || rt.SinceVersion <= oldMetaDataVersion {
				return false
			}
		}
		return true
	}

	// Type-specific index: find which record types have it.
	found := false
	for _, rt := range store.metaData.RecordTypes() {
		for _, rtIdx := range store.metaData.GetIndexesForRecordType(rt.Name) {
			if rtIdx.Name == index.Name {
				found = true
				if rt.SinceVersion == 0 || rt.SinceVersion <= oldMetaDataVersion {
					return false
				}
			}
		}
	}
	return found
}

// recordsSubspaceEmpty reports whether the records subspace contains no keys.
func (store *FDBRecordStore) recordsSubspaceEmpty() (bool, error) {
	return store.recordsRangeEmpty(nil)
}

// recordsRangeEmpty reports whether the records subspace holds no keys — for the
// WHOLE store when recordType is nil, and for exactly that type's record-type-keyed
// sub-range otherwise. The scoped form is Java's
//
//	records = scanRecords(TupleRange.allOf(singleRecordTypeWithPrefixKey.getRecordTypeKeyTuple()), null, scanProperties)
//
// (FDBRecordStore.java:5098): when every index being built is on one type whose
// records live in a contiguous sub-range, only that sub-range decides whether there
// is anything to index. Probing the whole store instead reports "non-empty" for
// records of types the new index will never touch, which routes an index over an
// empty type to DISABLED where Java builds it inline.
//
// Uses a non-snapshot limited range read. This is a DELIBERATE DIVERGENCE, not
// parity: Java's probe is a SNAPSHOT scan (FDBRecordStore.java:5089-5092 builds
// ExecuteProperties with IsolationLevel.SNAPSHOT) and adds no conflict range of its
// own afterwards, so Java's "the store is empty, build the index inline" decision
// RACES a concurrent insert — the insert commits, the index is marked READABLE, and
// the record it added was never indexed.
//
// A non-snapshot read puts the scanned range in the transaction's read-conflict set,
// so that insert makes this transaction conflict and retry, and the retry sees a
// non-empty store and routes the index to a background build. Losing to a concurrent
// insert is the correct outcome for a decision whose whole premise is emptiness.
//
// Scoping the probe to one record type shrinks that conflict range to the type
// actually being indexed, so an insert of an unrelated type no longer invalidates
// the decision.
//
// This reasoning is specific to a decision predicated on emptiness and does NOT
// generalize to the other reads on this path. In particular the aggregate-index scan
// (countKVCursor.initIterator) must be snapshot when snapshot is requested, because
// a count that adds a conflict range breaks the caller's transaction for a read that
// promised not to. Do not "fix" either of these into the other.
func (store *FDBRecordStore) recordsRangeEmpty(recordType *RecordType) (bool, error) {
	recSub := store.subspace.Sub(RecordKey)
	if recordType != nil {
		recSub = recSub.Sub(recordType.GetRecordTypeKey())
	}
	begin, end := recSub.FDBRangeKeys()
	kvs, err := store.context.Transaction().
		GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: 1}).
		GetSliceWithError()
	if err != nil {
		return false, err
	}
	return len(kvs) == 0, nil
}

// singleRecordTypeWithPrefixKey returns the one record type all the indexes being
// built are on, when there is exactly one and its primary key is prefixed by the
// record type key — otherwise nil. Port of Java's
// FDBRecordStore.singleRecordTypeWithPrefixKey (FDBRecordStore.java:5137-5155):
//
//	RecordType recordType = null;
//	for (List<RecordType> entry : indexes.values()) {
//	    Collection<RecordType> types = entry != null ? entry : getRecordMetaData().getRecordTypes().values();
//	    if (types.size() != 1) {
//	        return null;
//	    }
//	    RecordType type1 = entry != null ? entry.get(0) : types.iterator().next();
//	    if (recordType == null) {
//	        if (!type1.primaryKeyHasRecordTypePrefix()) {
//	            return null;
//	        }
//	        recordType = type1;
//	    } else if (type1 != recordType) {
//	        return null;
//	    }
//	}
//	return recordType;
//
// Java's map value is null for a UNIVERSAL index, and the null case widens to every
// record type in the meta-data — so a universal index qualifies only in a
// single-type store. RecordTypesForIndex reproduces that exactly (universal index →
// all record types), so the len(types) != 1 test covers both.
//
// The record-type-prefix test is written against the FIRST entry only, as in Java;
// every later entry must be the same type, so it applies to all of them.
func (store *FDBRecordStore) singleRecordTypeWithPrefixKey(indexes []*Index) *RecordType {
	var recordType *RecordType
	for _, index := range indexes {
		types := store.metaData.RecordTypesForIndex(index)
		if len(types) != 1 {
			return nil
		}
		type1 := types[0]
		if recordType == nil {
			if !type1.PrimaryKeyHasRecordTypePrefix() {
				return nil
			}
			recordType = type1
		} else if type1 != recordType {
			return nil
		}
	}
	return recordType
}

// getRecordCountForRebuildPolicy returns the record count the IndexRebuildPolicy
// decides on. Port of Java's getRecordCountForRebuildIndexes
// (FDBRecordStore.java:5062-5124), whose selection chain is, in order:
//
//  1. If all the indexes being built are on ONE record type whose primary key is
//     record-type-prefixed, a count for JUST that type — from a COUNT index on the
//     type, or from a COUNT index grouped by record type (FDBRecordStore.java:5068-5075).
//  2. Otherwise, unless the record counts are being rebuilt in this very
//     transaction, the whole-store count — from the record-count key, or from an
//     ungrouped/roll-uppable COUNT index (FDBRecordStore.java:5076-5087).
//  3. Only when neither yields a count: a scan limited to a SINGLE record, reporting
//     Long.MAX_VALUE the moment the store turns out to be non-empty and 0 only when
//     it is genuinely empty (FDBRecordStore.java:5088-5123). Scoped to the single
//     record type's range when there is one.
//
// Steps 1 and 2 are not an optimisation — they change the ANSWER. The probe cannot
// distinguish 1 record from 10^9, so it reports MAX_VALUE for both; a store with a
// usable COUNT index reporting 12 is one Java rebuilds INLINE and marks READABLE,
// and skipping straight to the probe leaves that index DISABLED forever pending a
// background build nobody asked for.
//
// The other direction is the safety property: reporting 0 for an unknown count makes
// DefaultIndexRebuildPolicy answer READABLE unconditionally, so EVERY index added by
// a metadata evolution is built INLINE, inside the transaction that opened the store
// — a full index build against the 5s / 10MB / 100k-key transaction limits on any
// store big enough to matter. MAX_VALUE for a non-empty store instead routes such an
// index to DISABLED (or WRITE_ONLY under WriteOnlyIfTooLargePolicy), and leaves the
// inline path to the small and empty stores where Java also takes it — recordCount <=
// MAX_RECORDS_FOR_REBUILD, FDBRecordStore.java:2471.
//
// The indexes being built are excluded from every count-index lookup: they hold no
// entries yet and have no index state on disk, so an unbuilt COUNT index would answer
// 0 for a full store. Java does this with an IndexQueryabilityFilter
// (FDBRecordStore.java:5065-5067) — "Do this with the new indexes filtered out to
// avoid using one of them when evaluating the snapshot record count. At this point we
// won't have written that any new indexes are disabled".
//
// rebuildRecordCounts is Java's flag of the same name, threaded from
// checkPossiblyRebuildRecordCounts (FDBRecordStore.java:4728): when the counts have
// just been cleared and re-derived, Java does not consult them.
//
// The emptiness probe is non-snapshot (see recordsRangeEmpty) so a concurrent insert
// invalidates an "empty, build inline" decision rather than racing it.
func (store *FDBRecordStore) getRecordCountForRebuildPolicy(indexesToBuild []*Index, singleRecordType *RecordType, rebuildRecordCounts bool) (int64, error) {
	// The queryability filter: exclude the indexes currently being built.
	excluded := make(map[string]bool, len(indexesToBuild))
	for _, index := range indexesToBuild {
		excluded[index.Name] = true
	}

	if singleRecordType != nil {
		count, ok, err := store.snapshotRecordCountForRecordType(singleRecordType.Name, func(index *Index) bool {
			return !excluded[index.Name]
		})
		if err != nil {
			return 0, err
		}
		if ok {
			return count, nil
		}
	}
	if !rebuildRecordCounts {
		count, ok, err := store.snapshotTotalRecordCount(excluded)
		if err != nil {
			return 0, err
		}
		if ok {
			return count, nil
		}
	}

	empty, err := store.recordsRangeEmpty(singleRecordType)
	if err != nil {
		return 0, err
	}
	if empty {
		return 0, nil
	}
	return math.MaxInt64, nil
}

// snapshotTotalRecordCount is Java's
// getSnapshotRecordCount(EmptyKeyExpression.EMPTY, Key.Evaluated.EMPTY, filter)
// (FDBRecordStore.java:2283-2323) specialised to the empty key and value the rebuild
// path always passes. ok=false is Java's RecordCoreException — "no appropriate index
// on count" — which the caller swallows to fall through to the next source.
//
// A read error is NOT ok=false. Java's catch can only see the SYNCHRONOUS throw from
// index selection; a failed range read arrives on the future and propagates. Folding
// one into "unknown count" would silently downgrade a transient FDB error into a
// rebuild-policy decision.
func (store *FDBRecordStore) snapshotTotalRecordCount(excluded map[string]bool) (int64, bool, error) {
	count, fromCountKey, err := store.snapshotRecordCountFromCountKey(tuple.Tuple{})
	if err != nil || fromCountKey {
		return count, fromCountKey, err
	}

	// Java: evaluateAggregateFunction(emptyList(), count(EMPTY), allOf(EMPTY), SNAPSHOT, filter).
	// The empty record-type list means UNIVERSAL indexes only
	// (IndexFunctionHelper.indexesForRecordTypes, IndexFunctionHelper.java:180-181); a
	// COUNT index grouped by anything still qualifies, because count(EMPTY)'s empty
	// grouping is a prefix of it, and rolling it up sums every group.
	fn := NewCountAggregateFunction(GroupAll(EmptyKey()))
	return store.evaluateCountIndex(fn, nil, TupleRangeAll, excluded)
}

// evaluateCountIndex evaluates fn over scanRange using the index that
// findIndexForAggregateFunction picks for recordTypeNames, returning ok=false when no
// index qualifies.
//
// Index selection is the shared port of
// IndexFunctionHelper.indexMaintainerForAggregateFunction
// (IndexFunctionHelper.java:105-122), which is also what the public
// EvaluateAggregateFunction uses; this wrapper only supplies the two things Java's
// rebuild path supplies that the public entry point does not:
//
//   - Java's IndexQueryabilityFilter (FDBRecordStore.java:5067,
//     `index -> !indexes.containsKey(index)`), here `excluded` — the indexes being
//     built hold no entries yet, so one of them answering the count would report 0
//     for a full store.
//   - Java's `catch (RecordCoreException ex)` around the count sources
//     (FDBRecordStore.java:5072, 5084), here ok=false — "no appropriate index", which
//     the caller swallows to fall through to the next source.
//
// A read error is NOT ok=false: only AggregateFunctionNotSupportedError is index
// selection failing. Java's catch can only see the synchronous throw from selection;
// a failed read arrives on the future and propagates. Folding one into "unknown
// count" would silently downgrade a transient FDB error into a rebuild-policy
// decision.
func (store *FDBRecordStore) evaluateCountIndex(fn *IndexAggregateFunction, recordTypeNames []string, scanRange TupleRange, excluded map[string]bool) (int64, bool, error) {
	best, err := store.findIndexForAggregateFunction(fn, recordTypeNames, func(index *Index) bool {
		return !excluded[index.Name]
	})
	if err != nil {
		var unsupported *AggregateFunctionNotSupportedError
		if errors.As(err, &unsupported) {
			return 0, false, nil
		}
		return 0, false, err
	}

	maintainer, err := store.getIndexMaintainer(best)
	if err != nil {
		return 0, false, fmt.Errorf("count index %q: %w", best.Name, err)
	}
	result, err := evaluateAggregate(store.context.ctx, fn, maintainer, scanRange, IsolationLevelSnapshot)
	if err != nil {
		return 0, false, fmt.Errorf("count index %q: %w", best.Name, err)
	}
	if len(result) == 0 {
		return 0, false, fmt.Errorf("count index %q returned an empty aggregate", best.Name)
	}
	count, isInt := result[0].(int64)
	if !isInt {
		return 0, false, fmt.Errorf("count index %q returned %T, want int64", best.Name, result[0])
	}
	return count, true, nil
}
