// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer/metadatapb"
)

// ensureStoreStateLoaded lazily loads store state (header + index states) from
// FDB on first call. Subsequent calls are no-ops via sync.Once.
//
// Matches Java's build() + lazy preloadRecordStoreStateAsync() pattern:
// Java's Builder.build() returns immediately (zero reads), then the first
// operation that needs index state calls preloadRecordStoreStateAsync().
//
// MUST be called before acquiring stateMu to avoid deadlock.
//
// For Open/CreateOrOpen paths, storeHeader and indexStates are already populated
// during the open call — stateLoadOnce.Do returns immediately.
func (store *FDBRecordStore) ensureStoreStateLoaded() {
	store.stateLoadOnce.Do(func() {
		// Loading and publishing must be atomic with context range clears;
		// otherwise a completed clear can be undone by an older snapshot.
		store.context.indexStateMu.Lock()
		defer store.context.indexStateMu.Unlock()
		if store.indexStateView != nil {
			return // already loaded and bound (Open/CreateOrOpen path)
		}
		if store.indexStates != nil {
			// Preserve the builder's explicit assume-all-readable fast path.
			store.indexStateView = store.context.indexStateViewLocked(store.subspace, store.indexStates)
			return
		}
		state, err := loadRecordStoreState(store, ExistenceCheckNone)
		if err != nil {
			// Capture the error for callers that can propagate it.
			// Default to all readable — safe when CreateOrOpen ran at
			// startup and all indexes are known to be built.
			store.stateLoadErr = err
			store.indexStates = make(map[string]IndexState)
			return
		}
		store.storeHeader = state.StoreHeader
		store.indexStates = state.IndexStates
		store.indexStateView = store.context.indexStateViewLocked(store.subspace, store.indexStates)
	})
}

// ensureStoreStateLoadedErr calls ensureStoreStateLoaded and returns any
// error that occurred during the lazy load. For callers that can propagate
// errors (batch operations, validate, updateSecondaryIndexes).
func (store *FDBRecordStore) ensureStoreStateLoadedErr() error {
	store.ensureStoreStateLoaded()
	return store.stateLoadErr
}

// LoadRecord loads a record by its primary key.
// Handles both unsplit (suffix 0) and split (suffixes 1, 2, ...) records
// via SplitHelper, matching Java's FDBRecordStore.loadRecordAsync().
func (store *FDBRecordStore) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[proto.Message], error) {
	return store.LoadRecordAsync(primaryKey, nil).Get()
}

// LoadRecordAsync sends the record's reads now, like Java's loadRecordAsync;
// Get finishes materialization. A non-nil scanState is charged when the raw
// read completes, even if its issued load is never consumed.
func (store *FDBRecordStore) LoadRecordAsync(primaryKey tuple.Tuple, scanState *ScanLimiterState) PendingRead[*FDBStoredRecord[proto.Message]] {
	issueStart := time.Now()
	load := &recordLoad{store: store, primaryKey: primaryKey}
	load.first = store.startRecordLoad(store.context.Transaction(), primaryKey)
	load.first.raw.startReadinessWithCharges(scanState)
	load.issueCost = time.Since(issueStart)
	return load
}

// pendingStoreRecordLoad adds the separate legacy version read to SplitHelper.
// Modern versions always come from the record range, even if new saves no
// longer store versions. Java loadTypedRecord only issues a separate old-format read.
type pendingStoreRecordLoad struct {
	raw             pendingRecordLoad
	oldVersion      PendingRead[*FDBRecordVersion]
	physical        *recordPhysicalRecord
	stamp           recordPhysicalStamp
	physicalErr     error
	retiredVersions []*recordVersionRead
}

func (store *FDBRecordStore) startRecordLoad(tx fdb.ReadTransaction, primaryKey tuple.Tuple) pendingStoreRecordLoad {
	var load pendingStoreRecordLoad
	if store.useOldVersionFormat() {
		load.oldVersion = store.loadRecordVersionAsync(primaryKey, false)
	}
	load.raw = startLoadWithSplit(tx, store.recordsSubspace, primaryKey, store.metaData.IsSplitLongRecords(), store.omitUnsplitRecordSuffix(), store.context)
	return load
}

// recordLoadTimer ends at the load's completion, before a save/delete continues.
// Deferred completion covers failures; an explicit completion records success once.
type recordLoadTimer struct {
	timer *StoreTimer
	start time.Time
}

func (t *recordLoadTimer) finish() {
	if !t.start.IsZero() {
		t.timer.RecordSince(EventLoadRecord, t.start)
		t.start = time.Time{}
	}
}

// The legacy version future is observed only after successful deserialization,
// preserving Java deserializeRecord's error precedence. An inline version wins.
func (load *pendingStoreRecordLoad) finishRetiredVersionErrors() error {
	for _, read := range load.retiredVersions {
		if err := read.backendFailure(); err != nil {
			return err
		}
	}
	return nil
}

func (load *pendingStoreRecordLoad) finishVersion(inline *FDBRecordVersion) (*FDBRecordVersion, error) {
	if err := load.finishRetiredVersionErrors(); err != nil {
		return nil, err
	}
	if inline == nil && load.oldVersion != nil {
		return load.oldVersion.Get()
	}
	return inline, nil
}

// recordLoad captures layout/local version at issue; later range pages may
// observe later own-writes. Public async loads do not own a physical read lease.
type recordLoad struct {
	store      *FDBRecordStore
	primaryKey tuple.Tuple
	first      pendingStoreRecordLoad
	issueCost  time.Duration
	loadCost   time.Duration
	loaded     bool
	done       bool
	record     *FDBStoredRecord[proto.Message]
	loadErr    error
}

func (l *recordLoad) Get() (*FDBStoredRecord[proto.Message], error) {
	if !l.done {
		record, err := l.finish()
		// Release read and context state while retaining the terminal result.
		*l = recordLoad{done: true, record: record, loadErr: err}
	}
	return l.record, l.loadErr
}

func (l *recordLoad) IsReady() bool { return l.done || l.ready() }

func (l *recordLoad) ready() bool {
	if !l.first.raw.ready() {
		return false
	}
	if l.first.oldVersion == nil || l.first.oldVersion.IsReady() {
		return true
	}
	// A missing/corrupt record, or one with its own inline version, does not
	// depend on the legacy version future. Memoize decoding to check that dependency.
	l.load()
	return l.loadErr != nil || l.record == nil || l.record.Version != nil
}

func (l *recordLoad) load() {
	if !l.loaded {
		start := time.Now()
		l.record, l.loadErr = l.loadRawRecord()
		l.loadCost = time.Since(start)
		l.loaded = true
	}
}

func (l *recordLoad) loadRawRecord() (*FDBStoredRecord[proto.Message], error) {
	store, primaryKey := l.store, l.primaryKey
	var sizeInfo sizeInfo
	value, version, err := l.first.raw.finish(&sizeInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to load record %v: %w", primaryKey, err)
	}
	if value == nil {
		return nil, nil // Record not found
	}

	// Discover which record type is stored by inspecting the UnionDescriptor
	recordType, protoMessage, wire, err := store.deserializeAndDiscover(value)
	if err != nil {
		return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
	}

	stored := &FDBStoredRecord[proto.Message]{
		PrimaryKey:      primaryKey,
		RecordType:      recordType,
		Record:          protoMessage,
		wire:            wire,
		Store:           store,
		KeyCount:        sizeInfo.KeyCount,
		ValueSize:       sizeInfo.ValueSize,
		KeySize:         sizeInfo.KeySize,
		Split:           sizeInfo.IsSplit,
		VersionedInline: sizeInfo.VersionedInline,
		Version:         version,
	}

	return stored, nil
}

// The load timer counts the caller's issue and wait, not time queued behind earlier rows.
func (l *recordLoad) finish() (*FDBStoredRecord[proto.Message], error) {
	l.load()
	start := time.Now()
	defer func() {
		l.store.context.Timer().Record(EventLoadRecord, (l.issueCost + l.loadCost + time.Since(start)).Nanoseconds())
	}()
	if l.loadErr != nil || l.record == nil {
		return l.record, l.loadErr
	}
	version, err := l.first.finishVersion(l.record.Version)
	if err != nil {
		return nil, fmt.Errorf("load record version for %v: %w", l.primaryKey, err)
	}
	l.record.Version = version
	return l.record, nil
}

// RecordExists checks if a record exists with the given primary key.
// Handles both split and unsplit records via SplitHelper.
//
// Java equivalent: FDBRecordStore.recordExistsAsync(Tuple primaryKey, IsolationLevel isolationLevel)
func (store *FDBRecordStore) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	recordsSubspace := store.recordsSubspace

	var tx fdb.ReadTransaction
	if isolationLevel.IsSnapshot() {
		tx = store.context.Transaction().Snapshot()
	} else {
		tx = store.context.Transaction()
	}

	return recordExistsWithSplit(tx, recordsSubspace, primaryKey, store.metaData.IsSplitLongRecords(), store.omitUnsplitRecordSuffix())
}

// RecordStoreState captures the mutable state of a record store at a point in time.
// Matches Java's RecordStoreState.
type RecordStoreState struct {
	StoreHeader *metadatapb.DataStoreInfo
	IndexStates map[string]IndexState
}

// GetRecordStoreState returns the current state of the store (header + index states).
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getRecordStoreState().
func (store *FDBRecordStore) GetRecordStoreState() *RecordStoreState {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	states := store.snapshotIndexStatesLocked()
	return &RecordStoreState{
		StoreHeader: store.storeHeader,
		IndexStates: states,
	}
}

// ReloadRecordStoreState forces a reload of the store state from FDB.
// Refreshes transaction-visible state, including explicit raw state mutations;
// it does not advance the transaction's read version. The loaded state is shared
// with other handles in this context.
// Matches Java's FDBRecordStore.loadRecordStoreStateAsync() force reload path.
func (store *FDBRecordStore) ReloadRecordStoreState() error {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return err
	}
	// Reload excludes maintenance through other handles before taking the
	// lower-level registry/view locks used to publish the refreshed state.
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	store.indexStateView.maintenance.Lock()
	defer store.indexStateView.maintenance.Unlock()
	store.context.indexStateMu.Lock()
	defer store.context.indexStateMu.Unlock()
	store.indexStateView.mu.Lock()
	defer store.indexStateView.mu.Unlock()
	exists, header, err := store.checkStoreExists()
	if err != nil {
		return err
	}
	if !exists {
		return &RecordStoreDoesNotExistError{}
	}
	states, err := readIndexStates(store.context.Transaction(), store.subspace)
	if err != nil {
		return err
	}
	if !maps.Equal(store.indexStateView.states, states) {
		store.context.SetDirtyStoreState(true)
		if header.GetCacheable() {
			store.context.SetMetaDataVersionStamp()
		}
	}
	store.storeHeader = header
	store.indexStates = states
	store.indexStateView.states = maps.Clone(states)
	return nil
}

// loadStoreState loads store state via the cache or directly from FDB.
// When bypassFullStoreLockReason is set, the cache is bypassed entirely
// (matching Java's checkVersion which skips cache on lock bypass).
// Called during store Build() before concurrent access, but uses stateMu
// for consistency.
func (store *FDBRecordStore) loadStoreState(existenceCheck StoreExistenceCheck, bypassReason *string) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	// Register before returning the loaded handle, and exclude clears for the
	// entire read-to-publication interval, including cache lookups.
	store.context.indexStateMu.Lock()
	defer store.context.indexStateMu.Unlock()
	if bypassReason != nil {
		// Bypass cache when using lock bypass — need fresh state to validate lock.
		state, err := loadRecordStoreState(store, existenceCheck)
		if err != nil {
			return err
		}
		store.storeHeader = state.StoreHeader
		store.indexStates = state.IndexStates
		store.indexStateView = store.context.indexStateViewLocked(store.subspace, store.indexStates)
		return nil
	}

	entry, err := store.storeStateCache.Get(store, existenceCheck)
	if err != nil {
		return err
	}

	cachedState := entry.GetRecordStoreState()
	if entry.shared {
		// Clone cached state so store mutations don't corrupt the shared cache entry.
		// Matches Java's RecordStoreState.toImmutable() which returns an unmodifiable copy.
		store.storeHeader = proto.Clone(cachedState.StoreHeader).(*metadatapb.DataStoreInfo)
		store.indexStates = make(map[string]IndexState, len(cachedState.IndexStates))
		for k, v := range cachedState.IndexStates {
			store.indexStates[k] = v
		}
	} else {
		// Non-shared entry (e.g., PassThrough cache) — safe to use directly.
		store.storeHeader = cachedState.StoreHeader
		store.indexStates = cachedState.IndexStates
	}
	store.indexStateView = store.context.indexStateViewLocked(store.subspace, store.indexStates)
	return nil
}

// SetStateCacheability sets whether this store's state can be cached across
// transactions. When enabled, the metadata version stamp is initialized so
// cache invalidation can work. Requires FormatVersion >= CACHEABLE_STATE (7).
// Returns true if the cacheability was changed, false if already at the desired state.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setStateCacheabilityAsync().
func (store *FDBRecordStore) SetStateCacheability(cacheable bool) (bool, error) {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return false, &RecordStoreStateNotLoadedError{}
	}
	if store.storeHeader.GetFormatVersion() < formatVersionCacheableState {
		return false, fmt.Errorf("store format version %d does not support cacheability (requires >= %d)",
			store.storeHeader.GetFormatVersion(), formatVersionCacheableState)
	}
	if store.storeHeader.GetCacheable() == cacheable {
		return false, nil
	}
	header := proto.Clone(store.storeHeader).(*metadatapb.DataStoreInfo)
	header.Cacheable = &cacheable
	if err := store.writeStoreHeader(header); err != nil {
		return false, err
	}
	store.storeHeader = header
	return true, nil
}

// IsStateCacheable returns whether this store's state is currently cacheable.
// Goroutine-safe via stateMu (read lock).
func (store *FDBRecordStore) IsStateCacheable() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.storeHeader != nil && store.storeHeader.GetCacheable()
}

// readStoredRecord is the union message a stored record's bytes hold: Java's
// TransformedRecordSerializer.deserialize before its inner serializer, which
// decodes the transformation prefix, decrypts and decompresses, and passes a
// bare union message through. Every store reads through it, decrypting with its
// own serializer's key manager; a store that writes with none reads every
// prefix but cannot decrypt.
func (store *FDBRecordStore) readStoredRecord(stored []byte) ([]byte, error) {
	reader := store.serializer
	if reader == nil {
		reader = plainTransformedReader
	}
	out, err := reader.untransform(stored)
	if err != nil {
		var rse *RecordSerializationError
		if errors.As(err, &rse) && rse.RetryResult == "success" && store.metaData != nil {
			rse.MetaDataVersion = store.metaData.Version()
		}
	}
	return out, err
}

// deserializeAndDiscover reads the union wire format tag to discover the record type,
// then unmarshals the inner bytes directly into the concrete message type.
// Skips allocating/parsing a full UnionDescriptor. The returned wire keeps the
// inner bytes when the type reaches a map field (internal/recordlayer/rlwire/map_order.go); a
// FDBStoredRecord built from the message carries it. data is the stored bytes,
// which readStoredRecord decodes first.
func (store *FDBRecordStore) deserializeAndDiscover(data []byte) (*RecordType, proto.Message, *recordWire, error) {
	data, err := store.readStoredRecord(data)
	if err != nil {
		return nil, nil, nil, err
	}
	return store.discoverUnion(data)
}

// discoverUnion is deserializeAndDiscover over a union message's bytes, the
// transformation already undone: the record they hold, its type and wire.
// It is Java's DynamicMessageRecordSerializer.deserializeUnion: the union must
// hold exactly one known field and no unknown one, else "Could not deserialize
// union message because there are unknown fields" (or "extra known fields",
// or "no fields"). A field the bytes repeat is one field, its occurrences
// merged, as protobuf parses them (a message field's occurrences concatenate).
func (store *FDBRecordStore) discoverUnion(data []byte) (*RecordType, proto.Message, *recordWire, error) {
	var (
		rt      *RecordType
		field   protowire.Number
		inner   []byte
		repeats bool
		known   int
		unknown int
	)
	remaining := data
	for len(remaining) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(remaining)
		if n < 0 {
			return nil, nil, nil, fmt.Errorf("failed to read union tag")
		}
		remaining = remaining[n:]
		if t := store.metaData.fieldNumberToRecordType[fieldNum]; t != nil && wireType == protowire.BytesType {
			b, m := protowire.ConsumeBytes(remaining)
			if m < 0 {
				return nil, nil, nil, fmt.Errorf("failed to read field %d bytes", fieldNum)
			}
			remaining = remaining[m:]
			switch {
			case rt == nil:
				rt, field, inner, known = t, fieldNum, b, 1
			case fieldNum == field:
				// A repeated occurrence merges into the first.
				if !repeats {
					inner = append([]byte(nil), inner...)
					repeats = true
				}
				inner = append(inner, b...)
			default:
				known++
			}
			continue
		}
		// A field the union does not declare, or one whose wire type does
		// not match its declaration, which protobuf keeps as unknown.
		m := protowire.ConsumeFieldValue(fieldNum, wireType, remaining)
		if m < 0 {
			return nil, nil, nil, fmt.Errorf("failed to skip field %d", fieldNum)
		}
		remaining = remaining[m:]
		unknown++
	}
	switch {
	case unknown > 0:
		return nil, nil, nil, serializationError("Could not deserialize union message because there are unknown fields")
	case known > 1:
		return nil, nil, nil, serializationError("Could not deserialize union message because there are extra known fields")
	case rt == nil:
		return nil, nil, nil, serializationError("Could not deserialize union message because there are no fields")
	}
	msg, err := rt.unmarshalRecord(inner)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to unmarshal %s: %w", rt.Name, err)
	}
	return rt, msg, newRecordWire(rt, inner), nil
}

// priorRecordInner is the record bytes a save writes its maps over: the
// existing record's, as its decode found them (deserializeAndDiscover, which
// keeps them as the record's wire when its type reaches a map field), when the
// record is of the type being saved; nil otherwise, for which a save writes
// its maps in key order. It reads the decode every write path has already
// done, so the stored bytes are decrypted once.
func priorRecordInner(oldRT *RecordType, oldWire *recordWire, recordType *RecordType) []byte {
	if oldWire == nil || oldRT != recordType {
		return nil
	}
	return oldWire.Bytes()
}

// unionInner is the inner bytes of the union field numbered field in data, nil
// when data is nil or holds no such field.
func unionInner(data []byte, field protowire.Number) []byte {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		if num == field && typ == protowire.BytesType {
			inner, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return nil
			}
			return inner
		}
		m := protowire.ConsumeFieldValue(num, typ, data)
		if m < 0 {
			return nil
		}
		data = data[m:]
	}
	return nil
}

// deserializeRecord unmarshals the inner bytes of a union-wrapped record directly
// into the known record type, skipping UnionDescriptor allocation.
func (store *FDBRecordStore) deserializeRecord(data []byte, recordType *RecordType) (proto.Message, error) {
	if recordType.unionFieldNumber == 0 {
		return nil, fmt.Errorf("no union field number for record type: %s", recordType.Name)
	}
	rt, msg, _, err := store.deserializeAndDiscover(data)
	if err != nil {
		return nil, err
	}
	if rt != recordType {
		return nil, fmt.Errorf("union descriptor does not contain %s record", recordType.Name)
	}
	return msg, nil
}
