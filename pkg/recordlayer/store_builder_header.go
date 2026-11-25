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
	"context"
	"fmt"

	"fdb.dev/internal/recordlayer/tuplefast"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
)

// validateFormatVersion checks that the stored format version is supported.
// Rejects versions below formatVersionMinimum (1) and above formatVersionMaxSupported.
// Matches Java's FormatVersion.validateFormatVersion().
func (store *FDBRecordStore) validateFormatVersion(storeHeader *metadatapb.DataStoreInfo) error {
	storedVersion := storeHeader.GetFormatVersion()
	if storedVersion < formatVersionMinimum || storedVersion > formatVersionMaxSupported {
		return &UnsupportedFormatVersionError{Version: storedVersion, MaxVersion: int32(formatVersionMaxSupported)}
	}
	return nil
}

// effectiveFormatVersion is the format version this store opens at. The builder
// resolves it once (StoreBuilder.effectiveFormatVersion) so a zero here can only
// mean a store constructed outside the builder; those fall back to the newest
// version this binary knows, which is the pre-pinning behaviour.
func (store *FDBRecordStore) effectiveFormatVersion() int32 {
	if store.targetFormatVersion > 0 {
		return store.targetFormatVersion
	}
	return int32(formatVersionDefault)
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
func (store *FDBRecordStore) maybeUpgradeFormatVersion(storeHeader *metadatapb.DataStoreInfo) (bool, error) {
	oldFormat := storeHeader.GetFormatVersion()
	// The target is the version this store was OPENED at, which is the newest the
	// binary knows only when the caller did not pin one. Java takes the same value
	// off the builder rather than a constant (FDBRecordStoreBase:2245).
	target := store.effectiveFormatVersion()
	// Never DOWNGRADE a store: a header already past the target was written by an
	// instance opening at a higher version, and rewriting it backwards would claim
	// a layout the existing data may not match. The store's own format version is
	// then the header's, as Java's checkPossiblyRebuild sets formatVersion to
	// max(header, target) (FDBRecordStore.java:4845-4847).
	if oldFormat >= target {
		store.targetFormatVersion = oldFormat
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
		pk, err := tuplefast.SubspaceUnpack(kv.Key, len(legacy.Bytes()))
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

// checkPossiblyRebuildRecordCounts detects when the record count key expression
// has changed between metadata versions and rebuilds the counts.
// Returns true if the store header was modified (caller must persist).
// Triggers when:
//   - Current metadata has a count key but the store header has a different one (or none)
//   - Current metadata has no count key but the store header still has one
//
// Matches Java's FDBRecordStore.checkPossiblyRebuildRecordCounts().
func (store *FDBRecordStore) checkPossiblyRebuildRecordCounts(storeHeader *metadatapb.DataStoreInfo, oldFormatVersion int32) (bool, error) {
	currentKey := store.metaData.GetRecordCountKey()

	// The header's format version, already reconciled by maybeUpgradeFormatVersion
	// above — the analog of Java's `formatVersion` field, which checkPossiblyRebuild
	// sets to max(oldFormatVersion, requested) before reaching here.
	headerFormatVersion := storeHeader.GetFormatVersion()

	// Java's FIRST rebuildRecordCounts arm (FDBRecordStore.java:5116):
	//
	//	(existingStore && oldFormatVersion < RECORD_COUNT_ADDED_FORMAT_VERSION)
	//
	// An EXISTING store below RECORD_COUNT_ADDED(2) predates record counts on disk
	// entirely, so whatever is in the count subspace cannot be trusted and is
	// rebuilt unconditionally — regardless of whether the count KEY changed, and
	// regardless of the key gate below. `existingStore` is Java's
	// `oldFormatVersion > 0`: a header with no format version at all is a store
	// being created right now, which has nothing to re-derive.
	//
	// This arm was unreachable until stores could be opened below the newest
	// format version; SetFormatVersion makes a format-1 store constructible, so it
	// is live now.
	existingStore := oldFormatVersion > 0
	needRebuild := existingStore && oldFormatVersion < formatVersionRecordCountAdded

	if needRebuild {
		// Already decided; the arms below only ADD reasons, never remove one.
	} else if currentKey != nil {
		// Current metadata has a count key — check if header matches.
		//
		// GATED, and the gate is load-bearing rather than defensive. Below
		// RECORD_COUNT_KEY_ADDED(3) the header CANNOT carry a count key (nothing is
		// allowed to write one — see createStoreHeaderAtFormat and the assignment
		// below), so an ungated comparison reads that permanent, correct absence as
		// "the metadata's count key changed" on EVERY open. The consequences
		// compound: it clears the counters, then scans every record in the store
		// inline inside the store-open transaction — unbounded work that a large
		// store cannot reopen past FDB's 5s/10MB limits — and then writes the
		// v3-only key into a v2 header anyway, undoing the creation gate.
		//
		// Java puts the same isAtLeast INSIDE this OR-arm
		// (FDBRecordStore.java:5117-5118).
		if headerFormatVersion >= formatVersionRecordCountKeyAdded {
			currentKeyProto := currentKey.ToKeyExpression()
			storedKeyProto := storeHeader.GetRecordCountKey()
			if storedKeyProto == nil || !proto.Equal(currentKeyProto, storedKeyProto) {
				needRebuild = true
			}
		}
	} else if storeHeader.GetRecordCountKey() != nil {
		// Current metadata removed count key — clear stale data. Deliberately NOT
		// gated, matching Java's third arm (:5119): a header that HAS a key is by
		// construction at >= 3 already, and clearing stale counts is always safe.
		needRebuild = true
	}

	if !needRebuild {
		return false, nil
	}
	if !store.isRecordCountDisabled() {
		if err := store.checkRecordCountCollation(currentKey); err != nil {
			return false, err
		}
	}

	// Clear existing count data. Use PrefixRange to include the exact prefix
	// key — ungrouped counts are stored at the subspace prefix itself.
	countSub := store.subspace.Sub(RecordCountKey)
	if pr, err := fdb.PrefixRange(countSub.Bytes()); err == nil {
		store.context.Transaction().ClearRange(pr)
	} else {
		store.context.Transaction().ClearRange(countSub)
	}

	// Update header with the new (or cleared) count key — but only at a format
	// version that HAS the field. Java wraps both the set and the clear in the same
	// isAtLeast (FDBRecordStore.java:5130-5136); writing it below 3 would put a
	// field in the header that an instance honouring that version does not expect,
	// which is the same wire hazard createStoreHeaderAtFormat guards at birth.
	//
	// UNREACHABLE-BY-CONSTRUCTION while the comparison gate above holds, and kept
	// anyway — do not delete it as dead code. Below version 3 neither arm can set
	// needRebuild: the first is gated, and the second requires a header that
	// already HAS a count key, which nothing below 3 is allowed to write. So this
	// branch only becomes reachable if the gate above is weakened, and then it is
	// the last thing standing between that weakening and a v3-only field in a v2
	// header. Mutating the two gates separately shows exactly that split: dropping
	// the comparison gate alone reds the rescan assertion, dropping both reds the
	// header assertion that this line is responsible for.
	if headerFormatVersion >= formatVersionRecordCountKeyAdded {
		if currentKey != nil {
			storeHeader.RecordCountKey = currentKey.ToKeyExpression()
		} else {
			storeHeader.RecordCountKey = nil
		}
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
	if err := store.checkRecordCountCollation(countKey); err != nil {
		return err
	}
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
	return store.storeHeader.GetRecordCountState() == metadatapb.DataStoreInfo_READABLE
}

// createStoreHeaderAtFormat creates a DataStoreInfo header for a new record
// store at the format version the builder pinned. Includes RecordCountKey from
// metadata if present, matching Java's checkPossiblyRebuildRecordCounts which
// sets it during store creation.
//
// The format version is a parameter rather than a default because a store
// created by an instance opening at an older format must be BORN at that
// format, not created new and immediately upgraded past it -- Java threads the
// builder's formatVersion into store creation the same way.
func createStoreHeaderAtFormat(metaDataVersion int32, metaData *RecordMetaData, env *Env, formatVersionIn int32) *metadatapb.DataStoreInfo {
	formatVersion := formatVersionIn
	userVersion := int32(0) // Default user version
	lastUpdateTime := uint64(env.Now().UnixMilli())

	header := &metadatapb.DataStoreInfo{
		FormatVersion:   &formatVersion,
		MetaDataversion: &metaDataVersion,
		UserVersion:     &userVersion,
		LastUpdateTime:  &lastUpdateTime,
	}

	// Persist RecordCountKey so checkPossiblyRebuildRecordCounts doesn't trigger
	// an unnecessary full rebuild on the first reopen — but only fields the
	// declared format version actually has. Java gates each independently
	// (FDBRecordStore.java:5950-5957): the key needs RECORD_COUNT_KEY_ADDED(3),
	// the state enum needs RECORD_COUNT_STATE(11). Writing either into a header
	// that declares an older version puts bytes there that an instance honouring
	// that version does not expect.
	if metaData != nil && metaData.GetRecordCountKey() != nil {
		if formatVersion >= formatVersionRecordCountKeyAdded {
			header.RecordCountKey = metaData.GetRecordCountKey().ToKeyExpression()
		}
		if formatVersion >= formatVersionRecordCountState {
			readable := metadatapb.DataStoreInfo_READABLE
			header.RecordCountState = &readable
		}
	}

	return header
}

// checkStoreExists checks if a store exists and returns its state
func (store *FDBRecordStore) checkStoreExists() (bool, *metadatapb.DataStoreInfo, error) {
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
	storeInfo := &metadatapb.DataStoreInfo{}
	if err := UnmarshalVTAsJava(storeInfo, firstKV.Value); err != nil {
		return false, nil, &RecordCoreStorageError{Message: "Error reading version", Cause: err}
	}

	return true, storeInfo, nil
}

// writeStoreHeader writes the store header to FDB and handles cache invalidation.
// When the store is cacheable, bumps the metadata version stamp so other transactions
// will see the change. Matches Java's FDBRecordStore.updateStoreHeaderAsync(), which
// also stamps last_update_time on every update (FDBRecordStore.java:3086): every
// header write goes through here, so the stamp is taken here rather than by each
// mutator.
// Caller must hold stateMu (write lock) or be in a builder path (pre-concurrent access).
func (store *FDBRecordStore) writeStoreHeader(storeInfo *metadatapb.DataStoreInfo) error {
	lastUpdateTime := uint64(store.context.Env().Now().UnixMilli()) //nolint:gosec
	storeInfo.LastUpdateTime = &lastUpdateTime
	oldCacheable := store.storeHeader != nil && store.storeHeader.GetCacheable()
	initializeStamp := false
	if !oldCacheable && storeInfo.GetCacheable() {
		stamp, err := store.context.GetMetaDataVersionStamp()
		if err != nil {
			return fmt.Errorf("read metadata version stamp before header update: %w", err)
		}
		initializeStamp = stamp == nil
	}

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
	if oldCacheable || initializeStamp {
		// Invalidate old cacheable state, or initialize the first stamp.
		store.context.SetMetaDataVersionStamp()
	}

	return nil
}
