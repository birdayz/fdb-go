package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
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

	// Step 4: Mark the index READABLE. A uniqueness violation FAILS the rebuild.
	//
	// Java's inline rebuild calls the ONE-ARGUMENT markIndexReadable(index)
	// (FDBRecordStore.java:4602), which is markIndexReadable(index,
	// allowUniquePending=FALSE) (:3767-3768). With a violation present,
	// checkAndUpdateBuiltIndexState (:3821) throws RecordIndexUniquenessViolation
	// ("Uniqueness violation when making index readable", :3856-3861) instead of
	// settling on READABLE_UNIQUE_PENDING.
	//
	// READABLE_UNIQUE_PENDING is reachable in Java core through exactly ONE caller,
	// IndexingBase.java:324, and only when
	// OnlineIndexer.IndexingPolicy.shouldAllowUniquePendingState (OnlineIndexer.java:1117)
	// says so — an explicit opt-in defaulting FALSE (:1220, javadoc: "allow=false
	// (default, backward compatible): throw an exception") that additionally requires
	// format version >= READABLE_UNIQUE_PENDING (FormatVersion.java:145).
	//
	// So a store-open rebuild must NOT quietly downgrade to a pending state. Doing
	// that would leave a store whose index the operator believes is fine, carrying a
	// violation nobody was told about; the throw is what surfaces it.
	if _, err := store.MarkIndexReadable(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: mark readable: %w", index.Name, err)
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
	var lowKey, highKey int64
	found := false
	for _, recordType := range store.metaData.RecordTypesForIndex(index) {
		if !recordType.PrimaryKeyHasRecordTypePrefix() || recordType.IsSynthetic() {
			return nil, nil, false
		}
		typeKey, isInt := recordTypeKeyInt64(recordType)
		if !isInt {
			return nil, nil, false
		}
		switch {
		case !found:
			lowKey, highKey, found = typeKey, typeKey, true
		case typeKey < lowKey:
			lowKey = typeKey
		case typeKey > highKey:
			highKey = typeKey
		}
	}
	if !found {
		return nil, nil, false
	}
	return tuple.Tuple{lowKey}, tuple.Tuple{highKey}, true
}

// validateFormatVersion checks that the stored format version is supported.
// Rejects versions below formatVersionMinimum (1) and above formatVersionCurrent.
// Matches Java's FormatVersion.validateFormatVersion().
func (store *FDBRecordStore) validateFormatVersion(storeHeader *gen.DataStoreInfo) error {
	storedVersion := storeHeader.GetFormatVersion()
	if storedVersion < formatVersionMinimum || storedVersion > formatVersionCurrent {
		return &UnsupportedFormatVersionError{Version: storedVersion, MaxVersion: int32(formatVersionCurrent)}
	}
	return nil
}

// effectiveFormatVersion is the format version this store opens at: the pinned
// target when the builder set one, else the newest this binary knows.
func (store *FDBRecordStore) effectiveFormatVersion() int32 {
	if store.targetFormatVersion > 0 {
		return store.targetFormatVersion
	}
	return int32(formatVersionCurrent)
}

// maybeUpgradeFormatVersion upgrades the persisted format version in the store header
// to this store's TARGET version — effectiveFormatVersion(), i.e. whatever the builder
// pinned, and only otherwise the newest this binary knows — performing the same on-disk
// layout migrations Java performs in checkRebuild() before reading any data. A header
// already at or past the target is left alone; this never downgrades. Returns true if
// the header was modified.
//
// The order matters and mirrors Java exactly: the format version is bumped FIRST so that
// useOldVersionFormat()/omitUnsplitRecordSuffix() reflect the new version, then:
//   - upgrading past SAVE_UNSPLIT_WITH_SUFFIX(5) on a non-splitting store sets
//     omit_unsplit_record_suffix=true (records were saved without a suffix and we
//     cannot rewrite them all), keeping them at the bare key forever;
//   - upgrading past SAVE_VERSION_WITH_RECORD(6) with versioning enabled, when the store
//     is NOT staying in the old version layout, moves every version from the legacy
//     RecordVersionKey(8) subspace to its inline location.
//
// storeHeader is the same pointer as store.storeHeader, so mutating it immediately
// updates what useOldVersionFormat() derives.
func (store *FDBRecordStore) maybeUpgradeFormatVersion(storeHeader *gen.DataStoreInfo) (bool, error) {
	oldFormat := storeHeader.GetFormatVersion()
	// The target is the version this store was OPENED at, which is the newest the
	// binary knows only when the caller did not pin one. Java takes the same value
	// off the builder rather than a constant (FDBRecordStoreBase:2245).
	target := store.effectiveFormatVersion()
	// Never DOWNGRADE a store: a header already past the target was written by an
	// instance opening at a higher version, and rewriting it backwards would claim
	// a layout the existing data may not match.
	if oldFormat >= target {
		return false, nil
	}

	newFormat := target
	storeHeader.FormatVersion = &newFormat

	if oldFormat >= formatVersionMinimum &&
		oldFormat < formatVersionSaveUnsplitWithSuffix &&
		!store.metaData.IsSplitLongRecords() {
		// Records were saved without the unsplit suffix; keep omitting it.
		storeHeader.OmitUnsplitRecordSuffix = proto.Bool(true)
	}

	if oldFormat >= formatVersionMinimum &&
		oldFormat < formatVersionSaveVersionWithRecord &&
		store.metaData.IsStoreRecordVersions() &&
		!store.useOldVersionFormat() {
		if err := store.convertRecordVersionsToInline(); err != nil {
			return false, fmt.Errorf("convert record versions to inline format: %w", err)
		}
	}

	return true, nil
}

// convertRecordVersionsToInline moves every record version from the legacy
// RecordVersionKey(8) subspace to its inline location (recordsSubspace.pack(pk, -1))
// and clears the legacy subspace. Matches Java's FDBRecordStore.addConvertRecordVersions().
//
// Like Java, this runs in the current transaction and is therefore subject to FDB's
// 5s / 10MB limits; converting a very large legacy store in one transaction can exceed
// them (an inherent limitation of the format-6 upgrade, shared with Java).
//
// Precondition: store.useOldVersionFormat() is already false (the caller bumped the
// format version), so store.versionKey(pk) returns the inline key.
func (store *FDBRecordStore) convertRecordVersionsToInline() error {
	legacy := store.subspace.Sub(RecordVersionKey)
	begin, end := legacy.FDBRangeKeys()
	tx := store.context.Transaction()

	kvs, err := tx.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{
		Mode: fdb.StreamingModeWantAll,
	}).GetSliceWithError()
	if err != nil {
		return fmt.Errorf("read legacy version subspace: %w", err)
	}

	for _, kv := range kvs {
		pk, err := fastSubspaceUnpack(kv.Key, len(legacy.Bytes()))
		if err != nil {
			return fmt.Errorf("unpack legacy version key: %w", err)
		}
		// Legacy value is the raw 12-byte FDBRecordVersion (always complete/committed),
		// matching Java's FDBRecordVersion.fromBytes(value, false).
		version, err := completeVersionFromBytesUnchecked(kv.Value)
		if err != nil {
			return fmt.Errorf("decode legacy version: %w", err)
		}
		packed, err := packVersion(version)
		if err != nil {
			return fmt.Errorf("pack inline version: %w", err)
		}
		tx.Set(store.versionKey(pk), packed)
	}

	tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})
	return nil
}

// checkPossiblyRebuild compares the stored metadata version with the current
// metadata version. If the current metadata has a higher version, indexes added
// since the old version are rebuilt or marked according to the IndexRebuildPolicy.
// Matches Java's FDBRecordStore.checkPossiblyRebuild() / checkRebuild() /
// getStatesForRebuildIndexes().
func (store *FDBRecordStore) checkPossiblyRebuild(storeHeader *gen.DataStoreInfo) error {
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

	// Upgrade the persisted format version (and migrate the on-disk layout if needed)
	// BEFORE reading/rebuilding anything, matching Java's checkRebuild() which sets the
	// format version and converts record versions at the very start. This runs whether
	// or not the metadata version changed.
	formatUpgraded, err := store.maybeUpgradeFormatVersion(storeHeader)
	if err != nil {
		return err
	}

	// Check record counts BEFORE the version gate — this compares the stored
	// RecordCountKey proto against the current one, independent of version.
	// Matches Java's checkRebuild() which always calls checkPossiblyRebuildRecordCounts().
	rebuildRecordCounts, err := store.checkPossiblyRebuildRecordCounts(storeHeader)
	if err != nil {
		return fmt.Errorf("rebuild record counts: %w", err)
	}
	needHeaderWrite := formatUpgraded || rebuildRecordCounts

	if newMetaDataVersion == oldMetaDataVersion {
		// Even when versions match, the record count check may have modified
		// the header (updated RecordCountKey). Persist if needed.
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

	// Find indexes added since the old version.
	indexesToBuild := store.metaData.GetIndexesToBuildSince(oldMetaDataVersion)
	if len(indexesToBuild) > 0 {
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
			indexOnNewRecordTypes := store.areAllRecordTypesSince(index, oldMetaDataVersion)
			desiredState := store.indexRebuildPolicy(index, recordCount, indexOnNewRecordTypes)

			switch desiredState {
			case IndexStateReadable:
				if err := store.RebuildIndex(index); err != nil {
					return fmt.Errorf("auto-rebuild index %q on metadata version change (%d -> %d): %w",
						index.Name, oldMetaDataVersion, newMetaDataVersion, err)
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
		}
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
	lastUpdateTime := uint64(store.context.Env().Now().UnixMilli())
	storeHeader.LastUpdateTime = &lastUpdateTime
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return fmt.Errorf("update store header after rebuild: %w", err)
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

// checkPossiblyRebuildRecordCounts detects when the record count key expression
// has changed between metadata versions and rebuilds the counts.
// Returns true if the store header was modified (caller must persist).
// Triggers when:
//   - Current metadata has a count key but the store header has a different one (or none)
//   - Current metadata has no count key but the store header still has one
//
// Matches Java's FDBRecordStore.checkPossiblyRebuildRecordCounts().
func (store *FDBRecordStore) checkPossiblyRebuildRecordCounts(storeHeader *gen.DataStoreInfo) (bool, error) {
	currentKey := store.metaData.GetRecordCountKey()

	var needRebuild bool
	if currentKey != nil {
		// Current metadata has a count key — check if header matches.
		currentKeyProto := currentKey.ToKeyExpression()
		storedKeyProto := storeHeader.GetRecordCountKey()
		if storedKeyProto == nil || !proto.Equal(currentKeyProto, storedKeyProto) {
			needRebuild = true
		}
	} else if storeHeader.GetRecordCountKey() != nil {
		// Current metadata removed count key — clear stale data.
		needRebuild = true
	}

	if !needRebuild {
		return false, nil
	}

	// Clear existing count data. Use PrefixRange to include the exact prefix
	// key — ungrouped counts are stored at the subspace prefix itself.
	countSub := store.subspace.Sub(RecordCountKey)
	if pr, err := fdb.PrefixRange(countSub.Bytes()); err == nil {
		store.context.Transaction().ClearRange(pr)
	} else {
		store.context.Transaction().ClearRange(countSub)
	}

	// Update header with the new (or cleared) count key.
	if currentKey != nil {
		storeHeader.RecordCountKey = currentKey.ToKeyExpression()
	} else {
		storeHeader.RecordCountKey = nil
	}

	// Rebuild counts by scanning all records (only if key is set and not disabled).
	if currentKey != nil && !store.isRecordCountDisabled() {
		if err := store.rebuildRecordCounts(currentKey); err != nil {
			return false, err
		}
	}

	return true, nil
}

// rebuildRecordCounts scans all records and repopulates the count subspace.
// Uses direct SET (not atomic ADD) since we're writing from a clean state.
// Matches Java's FDBRecordStore.addRebuildRecordCountsJob().
func (store *FDBRecordStore) rebuildRecordCounts(countKey KeyExpression) error {
	ctx := context.Background()
	counts := make(map[string]int64)       // packed count key → count
	keyMap := make(map[string]tuple.Tuple) // packed → tuple (for FDB writes)

	cursor := store.ScanRecords(nil, ForwardScan())
	defer func() { _ = cursor.Close() }()

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return fmt.Errorf("scan records for count rebuild: %w", err)
		}
		if !result.HasNext() {
			break
		}
		rec := result.GetValue()
		subkeys, err := countKey.Evaluate(rec, rec.Record)
		if err != nil {
			return fmt.Errorf("evaluate count key for record: %w", err)
		}
		if len(subkeys) != 1 {
			return fmt.Errorf("count key should evaluate to single key, got %d", len(subkeys))
		}
		keyTuple := make(tuple.Tuple, len(subkeys[0]))
		for i, v := range subkeys[0] {
			keyTuple[i] = v
		}
		packed := string(keyTuple.Pack())
		counts[packed]++
		keyMap[packed] = keyTuple
	}

	countSubspace := store.subspace.Sub(RecordCountKey)
	for packed, count := range counts {
		fdbKey := countSubspace.Pack(keyMap[packed])
		store.context.Transaction().Set(fdbKey, encodeRecordCount(count))
	}

	return nil
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
// (FDBRecordStore.java:4872): when every index being built is on one type whose
// records live in a contiguous sub-range, only that sub-range decides whether there
// is anything to index. Probing the whole store instead reports "non-empty" for
// records of types the new index will never touch, which routes an index over an
// empty type to DISABLED where Java builds it inline.
//
// Uses a non-snapshot limited range read. This is a DELIBERATE DIVERGENCE, not
// parity: Java's probe is a SNAPSHOT scan (FDBRecordStore.java:4864-4867 builds
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
		typeKey, ok := recordTypeKeyInt64(recordType)
		if !ok {
			return false, fmt.Errorf("record type %q has no integer record type key", recordType.Name)
		}
		recSub = recSub.Sub(typeKey)
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

// recordTypeKeyInt64 returns the record type key as the int64 the tuple encoder
// actually writes into primary keys, and ok=false when the type key is not an
// integer.
//
// Java encodes every record-type-key flavour, but Go's RecordTypeKeyExpression
// only binds INTEGER type keys (metadata.go bindTypeKeys builds map[string]int64)
// and falls back to the message type NAME for any other explicit key at save time
// (key_expression.go RecordTypeKeyExpression.Evaluate). Bounds derived from a
// non-integer key would therefore not match where the records are, so every caller
// treats "not an integer" as "this type's records are not addressable as a range"
// and falls back to the unscoped behaviour. Same restriction, same reason, as
// OnlineIndexer.computeRecordsRange.
func recordTypeKeyInt64(recordType *RecordType) (int64, bool) {
	switch k := recordType.GetRecordTypeKey().(type) {
	case int:
		return int64(k), true
	case int32:
		return int64(k), true
	case int64:
		return k, true
	default:
		return 0, false
	}
}

// singleRecordTypeWithPrefixKey returns the one record type all the indexes being
// built are on, when there is exactly one and its primary key is prefixed by the
// record type key — otherwise nil. Port of Java's
// FDBRecordStore.singleRecordTypeWithPrefixKey (FDBRecordStore.java:4909-4929):
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
	if recordType != nil {
		if _, ok := recordTypeKeyInt64(recordType); !ok {
			return nil
		}
	}
	return recordType
}

// getRecordCountForRebuildPolicy returns the record count the IndexRebuildPolicy
// decides on. Port of Java's getRecordCountForRebuildIndexes
// (FDBRecordStore.java:4836-4898), whose selection chain is, in order:
//
//  1. If all the indexes being built are on ONE record type whose primary key is
//     record-type-prefixed, a count for JUST that type — from a COUNT index on the
//     type, or from a COUNT index grouped by record type (FDBRecordStore.java:4842-4849).
//  2. Otherwise, unless the record counts are being rebuilt in this very
//     transaction, the whole-store count — from the record-count key, or from an
//     ungrouped/roll-uppable COUNT index (FDBRecordStore.java:4850-4861).
//  3. Only when neither yields a count: a scan limited to a SINGLE record, reporting
//     Long.MAX_VALUE the moment the store turns out to be non-empty and 0 only when
//     it is genuinely empty (FDBRecordStore.java:4862-4897). Scoped to the single
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
// (FDBRecordStore.java:4839-4841) — "Do this with the new indexes filtered out to
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
		count, ok, err := store.snapshotRecordCountForRecordType(singleRecordType, excluded)
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

// snapshotRecordCountForRecordType is Java's
// getSnapshotRecordCountForRecordType(name, filter) (FDBRecordStore.java:2326-2348):
// a COUNT index on JUST that record type first, then a COUNT index grouped by record
// type restricted to that type's key. ok=false is Java's terminal
// "Require a COUNT index on <type>" throw, which the caller swallows.
func (store *FDBRecordStore) snapshotRecordCountForRecordType(recordType *RecordType, excluded map[string]bool) (int64, bool, error) {
	// A COUNT index on this record type. Java looks at
	// getIndexableRecordType(name).getIndexes() — the type's OWN indexes, not the
	// multi-type ones, which by definition also cover other types and so cannot count
	// this type alone.
	fn := NewCountAggregateFunction(GroupAll(EmptyKey()))
	count, ok, err := store.evaluateCountIndex(fn, []string{recordType.Name}, TupleRangeAll, excluded)
	if err != nil || ok {
		return count, ok, err
	}

	// A universal COUNT index grouped by record type. In Java's words: "In fact, any
	// COUNT index by record type that applied to this record type would work, no
	// matter what other types it applied to."
	typeKey, hasTypeKey := recordTypeKeyInt64(recordType)
	if !hasTypeKey {
		return 0, false, nil
	}
	fn = NewCountAggregateFunction(GroupAll(RecordTypeKey()))
	return store.evaluateCountIndex(fn, nil, TupleRangeAllOf(tuple.Tuple{typeKey}), excluded)
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
//   - Java's IndexQueryabilityFilter (FDBRecordStore.java:4841,
//     `index -> !indexes.containsKey(index)`), here `excluded` — the indexes being
//     built hold no entries yet, so one of them answering the count would report 0
//     for a full store.
//   - Java's `catch (RecordCoreException ex)` around the count sources
//     (FDBRecordStore.java:4845, 4858), here ok=false — "no appropriate index", which
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

// recordCountStateIsReadable reports whether the stored record counts may be read.
// Java checks the header state directly and notes that "We can always check the state,
// even if the formatVersion is older, because older versions will always have the
// default of READABLE" (FDBRecordStore.java:2290-2293).
func (store *FDBRecordStore) recordCountStateIsReadable() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader == nil {
		return false
	}
	return store.storeHeader.GetRecordCountState() == gen.DataStoreInfo_READABLE
}

// createStoreHeader creates a DataStoreInfo header for a new record store.
// Includes RecordCountKey from metadata if present, matching Java's
// checkPossiblyRebuildRecordCounts which sets it during store creation.
func createStoreHeader(metaDataVersion int32, metaData *RecordMetaData, env *dst.Env) *gen.DataStoreInfo {
	return createStoreHeaderAtFormat(metaDataVersion, metaData, env, int32(formatVersionCurrent))
}

// createStoreHeaderAtFormat is createStoreHeader with the format version the
// builder pinned. A store created by an instance opening at an older format must
// be BORN at that format, not created new and immediately upgraded past it --
// Java threads the builder's formatVersion into store creation the same way.
func createStoreHeaderAtFormat(metaDataVersion int32, metaData *RecordMetaData, env *dst.Env, formatVersionIn int32) *gen.DataStoreInfo {
	formatVersion := formatVersionIn
	userVersion := int32(0) // Default user version
	lastUpdateTime := uint64(env.Now().UnixMilli())

	header := &gen.DataStoreInfo{
		FormatVersion:   &formatVersion,
		MetaDataversion: &metaDataVersion,
		UserVersion:     &userVersion,
		LastUpdateTime:  &lastUpdateTime,
	}

	// Persist RecordCountKey so checkPossiblyRebuildRecordCounts doesn't trigger
	// an unnecessary full rebuild on the first reopen.
	if metaData != nil && metaData.GetRecordCountKey() != nil {
		header.RecordCountKey = metaData.GetRecordCountKey().ToKeyExpression()
		readable := gen.DataStoreInfo_READABLE
		header.RecordCountState = &readable
	}

	return header
}

// checkStoreExists checks if a store exists and returns its state
func (store *FDBRecordStore) checkStoreExists() (bool, *gen.DataStoreInfo, error) {
	// Check if the first key in the subspace exists
	begin, end := store.subspace.FDBRangeKeys()
	storeRange := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(storeRange, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		// %w, not %v: preserve the fdb.Error type so a retryable read error
		// (future_version, transaction_too_old, …) stays retryable in the
		// Transact loop rather than being flattened to a fatal string.
		return false, nil, fmt.Errorf("failed to read store range: %w", err)
	}
	if len(kvs) == 0 {
		// Store is completely empty
		return false, nil, nil
	}

	// Check if the first key is the store info header
	firstKV := kvs[0]
	expectedStoreInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})

	if !bytes.Equal(firstKV.Key, expectedStoreInfoKey) {
		// Store has data but no proper header - matches Java error
		return false, nil, &RecordStoreNoInfoButNotEmptyError{FirstKey: firstKV.Key}
	}

	// Parse the store header
	storeInfo := &gen.DataStoreInfo{}
	if err := storeInfo.UnmarshalVT(firstKV.Value); err != nil {
		return false, nil, fmt.Errorf("failed to parse store header: %v", err)
	}

	return true, storeInfo, nil
}

// writeStoreHeader writes the store header to FDB and handles cache invalidation.
// When the store is cacheable, bumps the metadata version stamp so other transactions
// will see the change. Matches Java's FDBRecordStore.updateStoreHeaderAsync().
// Caller must hold stateMu (write lock) or be in a builder path (pre-concurrent access).
func (store *FDBRecordStore) writeStoreHeader(storeInfo *gen.DataStoreInfo) error {
	oldCacheable := store.storeHeader != nil && store.storeHeader.GetCacheable()

	headerBytes, err := storeInfo.MarshalVT()
	if err != nil {
		return &RecordSerializationError{Cause: err}
	}

	storeInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})
	store.context.Transaction().Set(storeInfoKey, headerBytes)

	// Mark store state as dirty in this transaction.
	// Matches Java: context.setDirtyStoreState(true) in updateStoreHeaderAsync().
	store.context.SetDirtyStoreState(true)

	// Bump metadata version stamp when appropriate.
	// Matches Java's updateStoreHeaderAsync() cache invalidation logic.
	newCacheable := storeInfo.GetCacheable()
	if oldCacheable {
		// Old header was cacheable → always bump to invalidate cached entries.
		store.context.SetMetaDataVersionStamp()
	} else if newCacheable {
		// Transitioning to cacheable → initialize stamp if not yet set.
		stamp, _ := store.context.GetMetaDataVersionStamp()
		if stamp == nil {
			store.context.SetMetaDataVersionStamp()
		}
	}

	return nil
}

// IndexRebuildPolicy determines what state a new/changed index should be put in
// when the store is opened with updated metadata.
// Matches Java's FDBRecordStoreBase.UserVersionChecker.needRebuildIndex().
type IndexRebuildPolicy func(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState

// DefaultIndexRebuildPolicy matches Java's default behavior:
// inline rebuild (READABLE) for stores with ≤200 records or indexes on new record types,
// DISABLED otherwise (requires OnlineIndexer).
// Java constant: FDBRecordStoreBase.MAX_RECORDS_FOR_REBUILD = 200.
func DefaultIndexRebuildPolicy(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState {
	const maxRecordsForRebuild = 200
	if indexOnNewRecordTypes || recordCount <= maxRecordsForRebuild {
		return IndexStateReadable
	}
	return IndexStateDisabled
}

// WriteOnlyIfTooLargePolicy returns READABLE for small stores (inline rebuild)
// and WRITE_ONLY for larger stores. WRITE_ONLY is the production-safe choice:
// new writes maintain the index immediately, and the operator invokes
// OnlineIndexer to backfill historical data. This avoids both:
//   - READABLE: times out on large stores (single-transaction rebuild)
//   - DISABLED: index is completely ignored, new writes don't maintain it
//
// Threshold matches Java's MAX_RECORDS_FOR_REBUILD = 200.
func WriteOnlyIfTooLargePolicy(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState {
	const maxRecordsForRebuild = 200
	if indexOnNewRecordTypes || recordCount <= maxRecordsForRebuild {
		return IndexStateReadable
	}
	return IndexStateWriteOnly
}

// AlwaysRebuildPolicy always rebuilds indexes inline.
// Matches Java's ALWAYS_READABLE_CHECKER behavior.
func AlwaysRebuildPolicy(_ *Index, _ int64, _ bool) IndexState {
	return IndexStateReadable
}

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java exactly.
type StoreBuilder struct {
	context                   *FDBRecordContext
	metaData                  *RecordMetaData
	subspace                  subspace.Subspace
	indexRebuildPolicy        IndexRebuildPolicy
	bypassFullStoreLockReason *string                  // nil = no bypass; Java's @Nullable String
	storeStateCache           FDBRecordStoreStateCache // per-store override; nil = use db cache
	database                  *FDBDatabase             // for inheriting cache
	skipPossiblyRebuild       bool                     // skip checkPossiblyRebuild on open
	cachedSSKeys              *storeSubspaceKeys       // cached from getCachedSubspaceKeys; avoids sync.Map lookup per Open
	assumeAllIndexesReadable  bool                     // pre-populate empty indexStates so ensureStoreStateLoaded is a no-op
	formatVersion             int32                    // 0 = formatVersionCurrent; see SetFormatVersion
}

// NewStoreBuilder creates a new store builder
func NewStoreBuilder() *StoreBuilder {
	return &StoreBuilder{}
}

// SetContext sets the record context
func (b *StoreBuilder) SetContext(ctx *FDBRecordContext) *StoreBuilder {
	b.context = ctx
	return b
}

// SetMetaDataProvider sets the metadata
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder {
	b.metaData = metaData
	return b
}

// SetSubspace sets the subspace for this store
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder {
	b.subspace = subspace
	return b
}

// effectiveFormatVersion is the builder-side twin of the store accessor: the
// pinned format version, or the newest this binary knows.
func (b *StoreBuilder) effectiveFormatVersion() int32 {
	if b.formatVersion > 0 {
		return b.formatVersion
	}
	return int32(formatVersionCurrent)
}

// SetFormatVersion pins the format version this store opens at, instead of
// defaulting to the newest version this binary knows.
//
// Port of Java's FDBRecordStoreBase.BaseBuilder.setFormatVersion
// (FDBRecordStoreBase.java:2245, :2266). Its javadoc gives the reason the value
// has to be a builder property rather than a constant: during a rolling upgrade
// you arrange "for setFormatVersion to be called with the OLD format version"
// on every instance, so no upgraded instance starts writing a layout the
// not-yet-upgraded ones cannot read. A store that always jumped to its binary's
// newest version would make a staged rollout impossible.
//
// The version is a CEILING, never a downgrade: a store whose header is already
// past it keeps what it has, because the data on disk may already be in that
// layout. Values outside [formatVersionMinimum, formatVersionCurrent] are
// rejected when the store is opened.
func (b *StoreBuilder) SetFormatVersion(version int32) *StoreBuilder {
	b.formatVersion = version
	return b
}

// SetIndexRebuildPolicy sets the policy for rebuilding indexes during store open
// when the metadata version changes. If not set, DefaultIndexRebuildPolicy is used
// (inline rebuild for ≤200 records, DISABLED otherwise).
// Matches Java's FDBRecordStore.newBuilder().setUserVersionChecker().
func (b *StoreBuilder) SetIndexRebuildPolicy(policy IndexRebuildPolicy) *StoreBuilder {
	b.indexRebuildPolicy = policy
	return b
}

// SetBypassFullStoreLockReason sets a reason string that, if it matches the
// stored FULL_STORE lock reason exactly, allows the store to be opened despite
// the lock. This is intended for recovery operations. Calling it with ""
// bypasses a lock whose stored reason is empty — Java's bypass is a
// @Nullable String compared with equals(), so the empty string is a valid
// bypass value, distinct from "no bypass" (the field's nil default).
// Matches Java's FDBRecordStore.Builder.setBypassFullStoreLockReason().
func (b *StoreBuilder) SetBypassFullStoreLockReason(reason string) *StoreBuilder {
	b.bypassFullStoreLockReason = &reason
	return b
}

// SetStoreStateCache sets a per-store cache override. If not set, the database's
// cache is used. Matches Java's FDBRecordStore.Builder.setStoreStateCache().
func (b *StoreBuilder) SetStoreStateCache(cache FDBRecordStoreStateCache) *StoreBuilder {
	b.storeStateCache = cache
	return b
}

// SetDatabase sets the database for inheriting the store state cache.
// Matches Java's FDBRecordStore.Builder.setDatabase().
func (b *StoreBuilder) SetDatabase(db *FDBDatabase) *StoreBuilder {
	b.database = db
	return b
}

// SetSkipPossiblyRebuild disables automatic index rebuild checks during Open/CreateOrOpen.
// When set, the store will not call checkPossiblyRebuild even if the metadata version changed.
// This is used by OnlineIndexer which manages index states independently.
// Matches Java's IndexMaintenanceFilter.NONE behavior.
func (b *StoreBuilder) SetSkipPossiblyRebuild(skip bool) *StoreBuilder {
	b.skipPossiblyRebuild = skip
	return b
}

// SetAssumeAllIndexesReadable pre-populates an empty indexStates map during Build(),
// making ensureStoreStateLoaded() a complete no-op (zero FDB reads, zero lazy-load).
// Safe when CreateOrOpen ran at startup and all indexes are known to be READABLE.
// This is an explicit opt-in for maximum performance in the Build() path.
func (b *StoreBuilder) SetAssumeAllIndexesReadable(assume bool) *StoreBuilder {
	b.assumeAllIndexesReadable = assume
	return b
}

// resolveCache returns the cache to use: per-store override > database cache > pass-through.
func (b *StoreBuilder) resolveCache() FDBRecordStoreStateCache {
	if b.storeStateCache != nil {
		return b.storeStateCache
	}
	if b.database != nil && b.database.storeStateCache != nil {
		return b.database.storeStateCache
	}
	return PassThroughStoreStateCache()
}

// subspaceKeys returns the cached subspace keys, computing them lazily.
func (b *StoreBuilder) subspaceKeys() *storeSubspaceKeys {
	if b.cachedSSKeys == nil {
		b.cachedSSKeys = getCachedSubspaceKeys(b.subspace)
	}
	return b.cachedSSKeys
}

// newStore creates an FDBRecordStore from the builder's settings.
func (b *StoreBuilder) newStore() *FDBRecordStore {
	policy := b.indexRebuildPolicy
	if policy == nil {
		policy = DefaultIndexRebuildPolicy
	}
	// Use cached recordsSubspace from subspace key cache.
	recSS := b.subspaceKeys().recordsSubspace
	store := &FDBRecordStore{
		context:            b.context,
		metaData:           b.metaData,
		subspace:           b.subspace,
		recordsSubspace:    recSS,
		indexRebuildPolicy: policy,
		storeStateCache:    b.resolveCache(),

		targetFormatVersion: b.formatVersion,
	}
	if b.assumeAllIndexesReadable {
		store.indexStates = make(map[string]IndexState)
	}
	return store
}

// validateBuilder checks that all required fields are set
func (b *StoreBuilder) validateBuilder() error {
	if b.context == nil {
		return fmt.Errorf("context is required")
	}
	if b.metaData == nil {
		return fmt.Errorf("metadata is required")
	}
	if b.subspace == nil || b.subspace.Bytes() == nil {
		return fmt.Errorf("subspace is required")
	}
	if b.formatVersion != 0 &&
		(b.formatVersion < formatVersionMinimum || b.formatVersion > int32(formatVersionCurrent)) {
		return &UnsupportedFormatVersionError{Version: b.formatVersion, MaxVersion: int32(formatVersionCurrent)}
	}
	return nil
}

// Create creates a new record store, fails if store already exists
func (b *StoreBuilder) Create() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Check if store already exists
	exists, _, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, &RecordStoreAlreadyExistsError{}
	}

	// Create and write store header
	storeHeader := createStoreHeaderAtFormat(int32(b.metaData.Version()), b.metaData, b.context.Env(), b.effectiveFormatVersion())
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return nil, err
	}
	store.storeHeader = storeHeader
	store.indexStates = make(map[string]IndexState)

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist.
// When the current metadata version is higher than the stored version,
// new indexes are automatically rebuilt inline (matching Java's checkVersion flow).
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Load store state via cache (or direct if bypassing locks).
	// Matches Java's checkVersion() which bypasses cache on full store lock bypass.
	if err := store.loadStoreState(ExistenceCheckErrorIfNotExists, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	// Validate format version is supported.
	if err := store.validateFormatVersion(store.storeHeader); err != nil {
		return nil, err
	}

	// Validate store lock state (FULL_STORE blocks open unless bypassed).
	if err := validateStoreLockState(store.storeHeader, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if !b.skipPossiblyRebuild {
		if err := store.checkPossiblyRebuild(store.storeHeader); err != nil {
			return nil, err
		}
	}

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// CreateOrOpen creates store if it doesn't exist, opens if it does (like Java).
// When opening an existing store whose metadata version is older than the
// current metadata, new indexes are automatically rebuilt inline.
// Matches Java's FDBRecordStore.checkPossiblyRebuild().
func (b *StoreBuilder) CreateOrOpen() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Load store state via cache (or direct).
	if err := store.loadStoreState(ExistenceCheckNone, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	exists := store.storeHeader != nil

	if !exists {
		// Create store header if it doesn't exist
		storeHeader := createStoreHeaderAtFormat(int32(b.metaData.Version()), b.metaData, b.context.Env(), b.effectiveFormatVersion())
		if err := store.writeStoreHeader(storeHeader); err != nil {
			return nil, err
		}
		store.storeHeader = storeHeader
		store.indexStates = make(map[string]IndexState)
	} else {
		// Validate format version is supported.
		if err := store.validateFormatVersion(store.storeHeader); err != nil {
			return nil, err
		}
		// Validate store lock state (FULL_STORE blocks open unless bypassed).
		if err := validateStoreLockState(store.storeHeader, b.bypassFullStoreLockReason); err != nil {
			return nil, err
		}
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if exists && !b.skipPossiblyRebuild {
		if err := store.checkPossiblyRebuild(store.storeHeader); err != nil {
			return nil, err
		}
	}

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// Build returns a store without checking database state (advanced use case)
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	return b.newStore(), nil
}
