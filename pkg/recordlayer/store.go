// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb/subspace"

	"fdb.dev/pkg/recordlayer/metadatapb"
)

// Record Layer format versions - should match Java FormatVersion enum.
// See FormatVersion.java for the full list.
const (
	formatVersionInfoAdded = 1 // Minimum version (INFO_ADDED in Java)
	// formatVersionRecordCountAdded (2, RECORD_COUNT_ADDED) is the version at which
	// record counts exist on disk at all. An existing store below it has no counts
	// worth trusting, so reconciliation rebuilds them unconditionally — Java's first
	// rebuildRecordCounts arm (FDBRecordStore.java:5116, via
	// RECORD_COUNT_ADDED_FORMAT_VERSION at :208).
	formatVersionRecordCountAdded = 2
	// formatVersionRecordCountKeyAdded (3, RECORD_COUNT_KEY_ADDED) is the version at
	// which the store header can carry a record-count key at all.
	formatVersionRecordCountKeyAdded = 3
	// formatVersionSaveUnsplitWithSuffix (5, SAVE_UNSPLIT_WITH_SUFFIX) is the version at
	// which every record key carries a split suffix even when split_long_records is false —
	// unless DataStoreInfo.omit_unsplit_record_suffix is true (set when a store created at an
	// earlier format version is upgraded). Below this version, unsplit records are stored at
	// the bare key recordsSubspace.pack(primaryKey) with no suffix.
	formatVersionSaveUnsplitWithSuffix = 5
	// formatVersionSaveVersionWithRecord (6, SAVE_VERSION_WITH_RECORD) is the version at which
	// record versions are stored inline adjacent to the record (recordsSubspace.pack(pk, -1))
	// rather than in the separate RecordVersionKey(8) subspace. See useOldVersionFormat().
	formatVersionSaveVersionWithRecord = 6
	// (7 = CACHEABLE_STATE; declared separately as formatVersionCacheableState below.)
	formatVersionHeaderUserFields      = 8  // User-defined key→bytes map in store header
	formatVersionReadableUniquePending = 9  // READABLE_UNIQUE_PENDING index state
	formatVersionCheckIndexBuildType   = 10 // Non-idempotent index build-from-source validation
	formatVersionRecordCountState      = 11 // RecordCountState enum (READABLE/WRITE_ONLY/DISABLED)
	formatVersionStoreLockState        = 12 // StoreLockState with FORBID_RECORD_UPDATE + FULL_STORE
	formatVersionIncarnation           = 13 // Incarnation counter for cross-cluster migration
	formatVersionFullStoreLock         = 14 // Unknown lock states prevent store opening
	formatVersionPendingWrites         = 15 // Pending index write queues
	// formatVersionMaxSupported is the highest format this binary can OPEN, and
	// it is a different question from the version a new store is BORN at.
	//
	// Java keeps the two apart deliberately — getMaximumSupportedVersion()
	// (FormatVersion.java:209) against getDefaultFormatVersion() (`:221-223`) —
	// and Go had them fused into one constant, which made the default impossible
	// to move without also moving the ceiling. Lowering a fused constant would
	// have REJECTED every store this binary had already created at 14, so "just
	// change the default" was a change that could not be made safely. Splitting
	// them is what makes it a free decision.
	//
	// Matches Java's MAX_SUPPORTED_VERSION, computed as the max enum value
	// (FormatVersion.java:188), WRITE_ONLY_WITH_QUEUE(15) at 4.14.2.0.
	formatVersionMaxSupported = formatVersionPendingWrites

	// formatVersionDefault is the version a NEW store is created at, and the
	// version an existing store is upgraded to, when the caller pins nothing.
	//
	// Java defaults to CACHEABLE_STATE(7); Go retains FULL_STORE_LOCK(14).
	// Supporting explicit queued-write format 15 must not silently enable it for
	// existing deployments. See RFC-257 and DIVERGENCES.md for this default policy.
	formatVersionDefault = formatVersionFullStoreLock

	formatVersionMinimum = formatVersionInfoAdded // Matches Java's FormatVersion.getMinimumVersion()
)

// FDBRecordStore provides record storage operations within a transaction context.
// This is the main struct for storing and retrieving records.
type FDBRecordStore struct {
	deferredMaintenance IndexDeferredMaintenanceControl
	// maintenanceFilter is Java's FDBRecordStore.indexMaintenanceFilter; nil
	// is IndexMaintenanceFilterNormal.
	maintenanceFilter IndexMaintenanceFilter
	// serializer is the TransformedRecordSerializer records are written
	// through; nil writes the bare union message (StoreBuilder.SetSerializer).
	serializer *TransformedRecordSerializer
	// goOnlyCollation is StoreBuilder.SetGoOnlyCollation: the store may hold
	// Go collation keys (GoOnlyCollationError).
	goOnlyCollation            bool
	allowLegacyKeySpaceCutover bool
	// pipelineSizer is StoreBuilder.SetPipelineSizer; nil is DefaultPipelineSizer.
	pipelineSizer      PipelineSizer
	context            *FDBRecordContext
	metaData           *RecordMetaData
	subspace           subspace.Subspace
	recordsSubspace    subspace.Subspace         // Cached subspace.Sub(RecordKey) — avoids alloc per method call
	storeHeader        *metadatapb.DataStoreInfo // Cached store header, loaded on Open/Create or lazily
	indexStates        map[string]IndexState     // Cached index states, loaded on Open/Create or lazily
	indexRebuildPolicy IndexRebuildPolicy        // Policy for rebuilding indexes on metadata version change
	// targetFormatVersion is the format version this store opens AT — the
	// ceiling maybeUpgradeFormatVersion upgrades toward, not necessarily the
	// newest the binary knows. 0 means formatVersionDefault.
	//
	// Java's equivalent is a builder property, not a constant
	// (FDBRecordStoreBase.BaseBuilder.setFormatVersion, :2245/:2266), and its
	// javadoc spells out the reason it must be settable: during a rolling
	// upgrade you arrange "for setFormatVersion to be called with the OLD format
	// version" everywhere, so that no instance starts writing a format the
	// not-yet-upgraded instances cannot read. A store that always jumped to the
	// newest version its binary knew would make that impossible.
	targetFormatVersion int32
	storeStateCache     FDBRecordStoreStateCache // Cache for store state across transactions
	stateMu             sync.RWMutex             // protects storeHeader + indexStates
	stateLoadOnce       sync.Once                // ensures lazy store state load happens exactly once (Build() path)
	stateLoadErr        error                    // error from lazy load (nil if loaded successfully or not yet attempted)
	indexStateView      *transactionIndexStateView
	versionChanged      bool     // true if checkPossiblyRebuild detected a version change
	maintainerCache     sync.Map // string → IndexMaintainer, cached per-transaction
}

// IsVersionChanged returns true if the metadata version changed during
// the most recent Open/CreateOrOpen (i.e., checkPossiblyRebuild detected
// that the stored version < current metadata version).
// Matches Java's FDBRecordStore.isVersionChanged().
func (store *FDBRecordStore) IsVersionChanged() bool {
	return store.versionChanged
}

// AsBuilder creates a new StoreBuilder with this store's configuration, in
// the same context. Matches Java's FDBRecordStore.asBuilder() (new
// Builder(this), Builder.copyFrom(FDBRecordStore)).
func (store *FDBRecordStore) AsBuilder() *StoreBuilder {
	return store.builderFrom(store.context)
}

// CopyBuilder creates a new StoreBuilder with this store's configuration
// but for a different context (transaction). Used to open the same store
// in a new transaction; the configuration copied is AsBuilder's.
func (store *FDBRecordStore) CopyBuilder(newContext *FDBRecordContext) *StoreBuilder {
	return store.builderFrom(newContext)
}

// builderFrom is Java's Builder.copyFrom(FDBRecordStore)
// (FDBRecordStore.java:5744-5757) over the settings Go has: the serializer,
// the format version, the metadata, the subspace, the index rebuild policy,
// the maintenance filter and the state cache, in ctx. The format version is
// the store's own, Java's formatVersion field: the one it was opened at,
// raised to the header's by the open's upgrade check (maybeUpgradeFormatVersion,
// as checkPossiblyRebuild sets it, :4845-4847); a store built without that
// check keeps the builder's, as Java's does. A store re-opened from this one
// therefore opens at that version, and never upgrades a header its opener
// pinned lower (which an unpinned builder, opening at the default, would).
func (store *FDBRecordStore) builderFrom(ctx *FDBRecordContext) *StoreBuilder {
	formatVersion := store.effectiveFormatVersion()
	return &StoreBuilder{
		context:                    ctx,
		metaData:                   store.metaData,
		subspace:                   store.subspace,
		indexRebuildPolicy:         store.indexRebuildPolicy,
		storeStateCache:            store.storeStateCache,
		maintenanceFilter:          store.maintenanceFilter,
		serializer:                 store.serializer,
		goOnlyCollation:            store.goOnlyCollation,
		allowLegacyKeySpaceCutover: store.allowLegacyKeySpaceCutover,
		pipelineSizer:              store.pipelineSizer,
		formatVersion:              &formatVersion,
	}
}

// validateRecordUpdateAllowed checks if the store allows record mutations.
// Returns StoreIsLockedForRecordUpdatesError if the store header has
// StoreLockState set to FORBID_RECORD_UPDATE.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.validateRecordUpdateAllowed().
func (store *FDBRecordStore) validateRecordUpdateAllowed() error {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return fmt.Errorf("load store state: %w", err)
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.validateRecordUpdateAllowedLocked()
}

// validateRecordUpdateAllowedLocked is the same check but caller must hold stateMu.
func (store *FDBRecordStore) validateRecordUpdateAllowedLocked() error {
	if store.storeHeader == nil {
		return nil
	}
	lockState := store.storeHeader.GetStoreLockState()
	if lockState == nil {
		return nil
	}
	if lockState.GetLockState() == metadatapb.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE {
		return &StoreIsLockedForRecordUpdatesError{
			Reason:    lockState.GetReason(),
			Timestamp: lockState.GetTimestamp(),
		}
	}
	return nil
}

// validateStoreLockState checks lock state during store open.
// FULL_STORE prevents opening unless bypass reason matches exactly.
// At FormatVersion >= FULL_STORE_LOCK (14), unknown/unspecified states also prevent opening.
// Matches Java's FDBRecordStore.validateStoreLockState(): the bypass is
// nullable (nil = none), so a lock stored with an empty reason is
// bypassable by supplying "" — non-nil empty compares equal, exactly as
// Java's `bypassFullStoreLockReason.equals(storeLockState.getReason())`.
func validateStoreLockState(storeHeader *metadatapb.DataStoreInfo, bypassFullStoreLockReason *string) error {
	lockState := storeHeader.GetStoreLockState()
	if lockState == nil {
		return nil
	}

	state := lockState.GetLockState()

	// FULL_STORE: blocked unless bypass reason matches exactly
	if state == metadatapb.DataStoreInfo_StoreLockState_FULL_STORE {
		if bypassFullStoreLockReason != nil && *bypassFullStoreLockReason == lockState.GetReason() {
			return nil // bypass successful
		}
		return &StoreIsFullyLockedError{
			Reason:    lockState.GetReason(),
			Timestamp: lockState.GetTimestamp(),
		}
	}

	// At FormatVersion >= FULL_STORE_LOCK, reject unknown/unspecified states.
	// FORBID_RECORD_UPDATE is known and handled at mutation time, so skip it here.
	if storeHeader.GetFormatVersion() >= formatVersionFullStoreLock {
		if state != metadatapb.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE {
			return &UnknownStoreLockStateError{LockStateValue: int32(state), UnknownFields: lockState.ProtoReflect().GetUnknown()}
		}
	}

	return nil
}

// Context returns the record context this store is using
func (store *FDBRecordStore) Context() *FDBRecordContext {
	return store.context
}

// Subspace returns the subspace this store is using
func (store *FDBRecordStore) Subspace() subspace.Subspace {
	return store.subspace
}

// GetMetaData returns the metadata for this store.
// Matches Java's FDBRecordStore.getRecordMetaData().
func (store *FDBRecordStore) GetMetaData() *RecordMetaData {
	return store.metaData
}

// Env exposes the record context's simulation environment to index maintainers.
// Nil-safe (a nil *Env means production); index maintainers draw persisted
// nonces through it so a simulation run is byte-reproducible.
func (store *FDBRecordStore) Env() *Env { return store.context.Env() }

// lockRegistry delegation — matches Java's LockRegistry on FDBRecordContext.

// AcquireWriteLock takes the context's exclusive lock for key.
func (store *FDBRecordStore) AcquireWriteLock(key string) { store.context.locks.WriteLock(key) }

// ReleaseWriteLock releases the exclusive lock AcquireWriteLock took for key.
func (store *FDBRecordStore) ReleaseWriteLock(key string) { store.context.locks.WriteUnlock(key) }

// AcquireReadLock takes the context's shared lock for key.
func (store *FDBRecordStore) AcquireReadLock(key string) { store.context.locks.ReadLock(key) }

// ReleaseReadLock releases the shared lock AcquireReadLock took for key.
func (store *FDBRecordStore) ReleaseReadLock(key string) { store.context.locks.ReadUnlock(key) }

// GetFormatVersion returns the store format version.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getFormatVersion().
func (store *FDBRecordStore) GetFormatVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.getFormatVersionLocked()
}

func (store *FDBRecordStore) getFormatVersionLocked() int32 {
	if store.storeHeader != nil && store.storeHeader.FormatVersion != nil {
		return *store.storeHeader.FormatVersion
	}
	return 0
}

// GetUserVersion returns the user-defined store version.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getUserVersion().
func (store *FDBRecordStore) GetUserVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader != nil && store.storeHeader.UserVersion != nil {
		return *store.storeHeader.UserVersion
	}
	return 0
}

// SetUserVersion updates the user-defined store version and writes it to FDB.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setUserVersion().
func (store *FDBRecordStore) SetUserVersion(version int32) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	store.storeHeader.UserVersion = &version
	return store.writeStoreHeader(store.storeHeader)
}

// GetMetaDataVersion returns the metadata version stored in the header.
// Goroutine-safe via stateMu (read lock).
func (store *FDBRecordStore) GetMetaDataVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader != nil && store.storeHeader.MetaDataversion != nil {
		return *store.storeHeader.MetaDataversion
	}
	return 0
}

// requireFormatVersion is Java's per-feature
// `if (!getFormatVersionEnum().isAtLeast(V)) throw` guard, applied at the WRITE
// site of every version-gated store-header feature.
//
// It reads the ACTUAL header version, not the version this process would like to
// open at, because the header is what an older instance opening the same store
// will see. Writing a v12 store lock into a header that still says 11 produces a
// lock a correctly-behaved older reader ignores — the feature silently does
// nothing, which is worse than refusing to set it.
//
// Java sites ported: FDBRecordStore.java:3222 (header user fields), :3443
// (record count state), :3478/:3494 (store lock state), :3503/:3517
// (incarnation).
//
// Callers must hold stateMu (or be inside a path that does); this only reads
// storeHeader.
func (store *FDBRecordStore) requireFormatVersion(feature string, required int32) error {
	if got := store.storeHeader.GetFormatVersion(); got < required {
		return &UnsupportedFeatureForFormatVersionError{
			Feature: feature, Version: got, RequiredVersion: required,
		}
	}
	return nil
}

// GetIncarnation returns the incarnation counter from the store header.
// Used for cross-cluster data migration versioning.
// Goroutine-safe via stateMu (read lock).
//
// GATED like Java. FDBRecordStore.getIncarnation() (`:3502-3506`) throws when the
// format version is below INCARNATION(13), and Go returned 0 — which a caller
// cannot tell apart from a store whose incarnation genuinely is 0. That is not a
// leniency, it is an AMBIGUITY: "this store has no incarnation yet" and "this
// store's format is too old to HAVE one" are different facts that were returning
// the same value. A migration keyed off incarnation reads a too-old store as a
// never-migrated one.
//
// The error is the write gate's twin (UnsupportedFeatureForFormatVersionError),
// not a new type: read and write are gated on the same version by the same
// predicate, so a caller that can write the field can read it back.
func (store *FDBRecordStore) GetIncarnation() (int32, error) {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.requireFormatVersion("incarnation", formatVersionIncarnation); err != nil {
		return 0, err
	}
	if store.storeHeader != nil {
		return store.storeHeader.GetIncarnation(), nil
	}
	return 0, nil
}

// UpdateIncarnation atomically updates the incarnation counter using the provided
// function. The new value must be strictly greater than the current value.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.updateIncarnation().
func (store *FDBRecordStore) UpdateIncarnation(updater func(current int32) int32) error {
	if updater == nil {
		return fmt.Errorf("updater must not be nil")
	}
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	if err := store.requireFormatVersion("incarnation", formatVersionIncarnation); err != nil {
		return err
	}
	current := store.storeHeader.GetIncarnation()
	newVal := updater(current)
	if newVal <= current {
		return fmt.Errorf("incarnation must increase: current %d, new %d", current, newVal)
	}
	store.storeHeader.Incarnation = &newVal
	return store.writeStoreHeader(store.storeHeader)
}

// GetHeaderUserField returns a user-defined field from the store header, or nil
// if the field is not set.
// Goroutine-safe via stateMu (read lock).
//
// GATED like Java. getHeaderUserField() routes through
// validateCanAccessHeaderUserFields() — the SHARED validator at `:3221-3226`,
// reached from getHeaderUserField at `:3249` — which throws below
// HEADER_USER_FIELDS(8); Go returned nil, which a caller cannot tell apart from
// an unset field. Same ambiguity as GetIncarnation, and the same fix: "not set"
// and "this format cannot hold user fields at all" are different answers.
//
// SetHeaderUserField already gated on this exact version, so the pair was
// ASYMMETRIC — a write that refused loudly beside a read that answered "absent",
// which is the shape that makes a caller conclude its write silently failed to
// persist.
func (store *FDBRecordStore) GetHeaderUserField(key string) ([]byte, error) {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.requireFormatVersion("header user fields", formatVersionHeaderUserFields); err != nil {
		return nil, err
	}
	if store.storeHeader == nil {
		return nil, nil
	}
	for _, entry := range store.storeHeader.GetUserField() {
		if entry.GetKey() == key {
			return entry.GetValue(), nil
		}
	}
	return nil, nil
}

// SetHeaderUserField sets a user-defined field in the store header.
// The value is persisted immediately. Keep values small since the entire
// header is loaded on every store open.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setHeaderUserField().
func (store *FDBRecordStore) SetHeaderUserField(key string, value []byte) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	if err := store.requireFormatVersion("header user fields", formatVersionHeaderUserFields); err != nil {
		return err
	}
	// Update existing entry or append new one
	for _, entry := range store.storeHeader.UserField {
		if entry.GetKey() == key {
			entry.Value = value
			return store.writeStoreHeader(store.storeHeader)
		}
	}
	store.storeHeader.UserField = append(store.storeHeader.UserField, &metadatapb.DataStoreInfo_UserFieldEntry{
		Key:   &key,
		Value: value,
	})
	return store.writeStoreHeader(store.storeHeader)
}

// ClearHeaderUserField removes a user-defined field from the store header.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.clearHeaderUserField().
func (store *FDBRecordStore) ClearHeaderUserField(key string) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	if err := store.requireFormatVersion("header user fields", formatVersionHeaderUserFields); err != nil {
		return err
	}
	fields := store.storeHeader.UserField
	for i, entry := range fields {
		if entry.GetKey() == key {
			store.storeHeader.UserField = append(fields[:i], fields[i+1:]...)
			return store.writeStoreHeader(store.storeHeader)
		}
	}
	return nil // not found, no-op
}

// SetStoreLockState sets the store lock state in the header and persists it.
// Use FORBID_RECORD_UPDATE to prevent record mutations, or FULL_STORE to
// prevent the store from being opened entirely.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setStoreLockStateAsync().
func (store *FDBRecordStore) SetStoreLockState(state metadatapb.DataStoreInfo_StoreLockState_State, reason string) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	if err := store.requireFormatVersion("store lock state", formatVersionStoreLockState); err != nil {
		return err
	}
	ts := store.context.Env().Now().UnixMilli()
	store.storeHeader.StoreLockState = &metadatapb.DataStoreInfo_StoreLockState{
		LockState: &state,
		Reason:    &reason,
		Timestamp: &ts,
	}
	return store.writeStoreHeader(store.storeHeader)
}

// ClearStoreLockState removes the lock state from the store header.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.clearStoreLockStateAsync().
func (store *FDBRecordStore) ClearStoreLockState() error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	if err := store.requireFormatVersion("store lock state", formatVersionStoreLockState); err != nil {
		return err
	}
	store.storeHeader.StoreLockState = nil
	return store.writeStoreHeader(store.storeHeader)
}
