// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBRecordStoreBase.java, FormatVersion.java, IndexingCommon.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// LegacyKeySpacePrefixError refuses a silent switch from an earlier Go path.
type LegacyKeySpacePrefixError struct {
	LegacyPrefix []byte
	FlatPrefix   []byte
}

func (e *LegacyKeySpacePrefixError) Error() string {
	return fmt.Sprintf("legacy nested KeySpace prefix %x still contains data; explicit legacy access or cutover to %x required", e.LegacyPrefix, e.FlatPrefix)
}

func (b *StoreBuilder) checkLegacyKeySpacePrefix() error {
	guarded, ok := b.subspace.(interface{ LegacyKeySpaceSubspace() subspace.Subspace })
	if !ok {
		return nil
	}
	legacy := guarded.LegacyKeySpaceSubspace()
	r, err := fdb.PrefixRange(legacy.Bytes())
	if err != nil {
		return &RecordCoreError{Message: "invalid legacy KeySpace range", Cause: err}
	}
	// A serializable range read also orders an empty-prefix cutover against
	// a concurrent writer still using the old layout.
	rows, err := b.context.Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return err
	}
	if len(rows) > 0 && !b.allowLegacyKeySpaceCutover {
		return &LegacyKeySpacePrefixError{LegacyPrefix: legacy.Bytes(), FlatPrefix: b.subspace.Bytes()}
	}
	return nil
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
	// An EXPLICIT SetFormatVersion(0) is rejected, not silently read as "unset".
	// Java validates the value it was handed (FormatVersion.validateFormatVersion,
	// FormatVersion.java:225-229) and 0 is below the minimum, so it throws there
	// too. Treating it as unset would quietly open at 14 -- the opposite of what a
	// caller asking for 0 could possibly want, and unnoticeable until it had
	// already written a newer format than intended.
	if b.formatVersion != nil &&
		(*b.formatVersion < formatVersionMinimum || *b.formatVersion > int32(formatVersionMaxSupported)) {
		return &UnsupportedFormatVersionError{Version: *b.formatVersion, MaxVersion: int32(formatVersionMaxSupported)}
	}
	return nil
}

// initializeNewIndexStates applies Java's new-store rebuild policy: replaced
// originals are disabled without consulting policy, while ordinary indexes
// have no records to build and remain readable unless explicitly disabled.
func (store *FDBRecordStore) initializeNewIndexStates() error {
	store.indexStates = make(map[string]IndexState)
	for _, index := range store.metaData.GetAllIndexes() {
		desired := IndexStateDisabled
		if len(index.GetReplacedByIndexNames()) == 0 {
			desired = store.indexRebuildPolicy(index, 0, true)
		}
		if desired == IndexStateDisabled {
			if _, err := store.MarkIndexDisabled(index.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Create creates a new record store, fails if store already exists
func (b *StoreBuilder) Create() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}
	if err := b.checkLegacyKeySpacePrefix(); err != nil {
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
	if err := store.initializeNewIndexStates(); err != nil {
		return nil, err
	}

	store.rememberRetirementMetadata()
	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist.
// When the current metadata version is higher than the stored version,
// new indexes are automatically rebuilt inline (matching Java's checkVersion flow).
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	return b.openWithPreflight(nil)
}

// openWithPreflight admits an indexing session against persisted state before
// metadata reconciliation can erase the heartbeat evidence used for admission.
func (b *StoreBuilder) openWithPreflight(preflight func(*FDBRecordStore) error) (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}
	if err := b.checkLegacyKeySpacePrefix(); err != nil {
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
	if preflight != nil {
		if err := preflight(store); err != nil {
			return nil, err
		}
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if !b.skipPossiblyRebuild {
		if err := store.checkPossiblyRebuild(store.storeHeader); err != nil {
			return nil, err
		}
	}

	store.rememberRetirementMetadata()
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
	if err := b.checkLegacyKeySpacePrefix(); err != nil {
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
		if err := store.initializeNewIndexStates(); err != nil {
			return nil, err
		}
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

	store.rememberRetirementMetadata()
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
