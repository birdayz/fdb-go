// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBRecordStoreBase.java, FormatVersion.java, IndexingCommon.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

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
	context                    *FDBRecordContext
	metaData                   *RecordMetaData
	subspace                   subspace.Subspace
	indexRebuildPolicy         IndexRebuildPolicy
	bypassFullStoreLockReason  *string                      // nil = no bypass; Java's @Nullable String
	storeStateCache            FDBRecordStoreStateCache     // per-store override; nil = use db cache
	database                   *FDBDatabase                 // for inheriting cache
	skipPossiblyRebuild        bool                         // skip checkPossiblyRebuild on open
	cachedSSKeys               *storeSubspaceKeys           // cached from getCachedSubspaceKeys; avoids sync.Map lookup per Open
	assumeAllIndexesReadable   bool                         // pre-populate empty indexStates so ensureStoreStateLoaded is a no-op
	formatVersion              *int32                       // nil = not pinned; see SetFormatVersion
	maintenanceFilter          IndexMaintenanceFilter       // nil = IndexMaintenanceFilterNormal
	serializer                 *TransformedRecordSerializer // nil = write the bare union message
	goOnlyCollation            bool                         // see SetGoOnlyCollation
	allowLegacyKeySpaceCutover bool
	pipelineSizer              PipelineSizer // nil = DefaultPipelineSizer; see SetPipelineSizer
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

// SetSerializer is Java's Builder.setSerializer for a TransformedRecordSerializer:
// the store writes every record through it (compressed, encrypted, prefixed as
// configured). Unset, a store writes the bare union message, as Java's default
// DynamicMessageRecordSerializer does. Every store reads a transformed record
// whatever it writes with, decrypting with this serializer's key manager
// (FDBRecordStore.readStoredRecord).
func (b *StoreBuilder) SetSerializer(s *TransformedRecordSerializer) *StoreBuilder {
	b.serializer = s
	return b
}

// SetMetaDataProvider sets the metadata
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder {
	b.metaData = metaData
	return b
}

// SetAllowLegacyKeySpaceCutover acknowledges that this store intentionally uses
// Java-flat path keys while data remains at the old Go nested path. No data is copied.
func (b *StoreBuilder) SetAllowLegacyKeySpaceCutover(allow bool) *StoreBuilder {
	b.allowLegacyKeySpaceCutover = allow
	return b
}

// SetSubspace sets the subspace for this store
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder {
	b.subspace = subspace
	// The subspace keys are derived from the subspace (subspaceKeys); a set
	// subspace invalidates them, or a store would take its records subspace
	// from the old one and everything else from the new.
	b.cachedSSKeys = nil
	return b
}

// copyBuilder is Java's Builder(Builder other), Builder.copyFrom(Builder)
// (FDBRecordStore.java:5723-5738), over the settings Go has. It copies the
// configuration only: the Go-only derived subspace keys and open shortcuts
// (assumeAllIndexesReadable, skipPossiblyRebuild) belong to the builder they
// were set on.
func (b *StoreBuilder) copyBuilder() *StoreBuilder {
	c := &StoreBuilder{
		context:                    b.context,
		metaData:                   b.metaData,
		subspace:                   b.subspace,
		indexRebuildPolicy:         b.indexRebuildPolicy,
		bypassFullStoreLockReason:  b.bypassFullStoreLockReason,
		storeStateCache:            b.storeStateCache,
		database:                   b.database,
		maintenanceFilter:          b.maintenanceFilter,
		serializer:                 b.serializer,
		goOnlyCollation:            b.goOnlyCollation,
		allowLegacyKeySpaceCutover: b.allowLegacyKeySpaceCutover,
		pipelineSizer:              b.pipelineSizer,
	}
	if b.formatVersion != nil {
		v := *b.formatVersion
		c.formatVersion = &v
	}
	return c
}

// effectiveFormatVersion is the builder-side twin of the store accessor: the
// pinned format version, or the newest this binary knows.
func (b *StoreBuilder) effectiveFormatVersion() int32 {
	if b.formatVersion != nil {
		return *b.formatVersion
	}
	return int32(formatVersionDefault)
}

// SetFormatVersion pins the format version this store opens at, instead of
// defaulting to the newest version this binary knows. An explicit 0 is an ERROR,
// not a request for the default -- see validateBuilder.
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
// layout. Values outside [formatVersionMinimum, formatVersionMaxSupported] are
// rejected when the store is opened.
func (b *StoreBuilder) SetFormatVersion(version int32) *StoreBuilder {
	b.formatVersion = &version
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

// SetIndexMaintenanceFilter sets the store's IndexMaintenanceFilter, Java's
// FDBRecordStore.Builder.setIndexMaintenanceFilter: which index entries of each
// record the store's indexes maintain. Nil is IndexMaintenanceFilterNormal.
func (b *StoreBuilder) SetIndexMaintenanceFilter(filter IndexMaintenanceFilter) *StoreBuilder {
	b.maintenanceFilter = filter
	return b
}

// GetIndexMaintenanceFilter is Java's FDBRecordStore.Builder.getIndexMaintenanceFilter.
func (b *StoreBuilder) GetIndexMaintenanceFilter() IndexMaintenanceFilter {
	if b.maintenanceFilter == nil {
		return IndexMaintenanceFilterNormal
	}
	return b.maintenanceFilter
}

// SetGoOnlyCollation opts into x/text collation keys for a store never shared
// with Java. Without it, collated index access and evaluation of collated primary
// or record-count keys return GoOnlyCollationError. Metadata and raw record reads
// remain available. This option is not persisted and does not migrate existing keys.
func (b *StoreBuilder) SetGoOnlyCollation(goOnly bool) *StoreBuilder {
	b.goOnlyCollation = goOnly
	return b
}

// SetSkipPossiblyRebuild disables automatic index rebuild checks during Open/CreateOrOpen.
// When set, the store will not call checkPossiblyRebuild even if the metadata version changed.
// This is used by OnlineIndexer which manages index states independently.
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

		targetFormatVersion:        b.effectiveFormatVersion(),
		maintenanceFilter:          b.maintenanceFilter,
		serializer:                 b.serializer,
		goOnlyCollation:            b.goOnlyCollation,
		allowLegacyKeySpaceCutover: b.allowLegacyKeySpaceCutover,
		pipelineSizer:              b.pipelineSizer,
	}
	if b.assumeAllIndexesReadable {
		store.indexStates = make(map[string]IndexState)
	}
	return store
}
