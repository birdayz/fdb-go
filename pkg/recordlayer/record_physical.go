package recordlayer

import (
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Physical ownership excludes only raw record mutations, never user callbacks.
// Entries and generations live until the context ends, preventing ABA on reuse.
type recordPhysicalState struct {
	mu     sync.Mutex
	stores map[string]*recordPhysicalStore
	active int
	idle   chan struct{}
	sealed bool
}

type recordPhysicalStore struct {
	gate  sync.RWMutex
	epoch uint64
	mu    sync.Mutex
	keys  map[string]*recordPhysicalKey
}

type recordPhysicalKey struct {
	mu         sync.Mutex
	generation uint64
}

type recordPhysicalStamp struct{ generation, epoch uint64 }

type recordPhysicalRecord struct {
	store               *FDBRecordStore
	scope               *recordPhysicalStore
	key                 *recordPhysicalKey
	prefix              []byte
	records             subspace.Subspace
	primaryKey          tuple.Tuple
	versionKey          fdb.Key
	split, omit, legacy bool
}

func (rc *FDBRecordContext) recordWriteContext() context.Context {
	if rc.ctx != nil {
		if rc.directCommitOwned.Load() {
			return context.WithoutCancel(rc.ctx)
		}
		return rc.ctx
	}
	return context.Background()
}

func (rc *FDBRecordContext) physicalHook(stage string, key []byte) {
	if rc.recordPhysicalHook != nil {
		rc.recordPhysicalHook(stage, key)
	}
}

func (rc *FDBRecordContext) beginRecordWrite() (func(), error) {
	if err := rc.recordWriteContext().Err(); err != nil {
		return nil, err
	}
	s := &rc.recordPhysical
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		return nil, errTransactionNotActive()
	}
	if s.active == 0 {
		s.idle = make(chan struct{})
	}
	s.active++
	s.mu.Unlock()
	rc.physicalHook("admitted", nil)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.active--
		if s.active == 0 {
			close(s.idle)
		}
	}, nil
}

// Drain pre-commit checks while writes remain admissible, then atomically seal
// once every admitted operation and newly registered check has completed.
func (rc *FDBRecordContext) drainCommitChecksAndSealWrites() error {
	s := &rc.recordPhysical
	checked := 0
	for {
		if err := rc.runPendingCommitChecks(&checked); err != nil {
			return err
		}
		if err := rc.recordWriteContext().Err(); err != nil {
			return err
		}
		// Registration precedes admission in this lock order; suppliers may
		// admit writes. No check or user hook runs with either mutex held.
		rc.commitMu.Lock()
		s.mu.Lock()
		if checked != len(rc.commitChecks) {
			s.mu.Unlock()
			rc.commitMu.Unlock()
			continue
		}
		if s.active == 0 {
			s.sealed = true
			s.mu.Unlock()
			rc.commitMu.Unlock()
			rc.physicalHook("sealed", nil)
			return nil
		}
		idle := s.idle
		s.mu.Unlock()
		rc.commitMu.Unlock()
		select {
		case <-idle:
		case <-rc.recordWriteContext().Done():
			return rc.recordWriteContext().Err()
		}
	}
}

func (store *FDBRecordStore) physicalScope() *recordPhysicalStore {
	s := &store.context.recordPhysical
	key := string(store.recordsSubspace.Bytes())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stores == nil {
		s.stores = make(map[string]*recordPhysicalStore)
	}
	if s.stores[key] == nil {
		s.stores[key] = &recordPhysicalStore{keys: make(map[string]*recordPhysicalKey)}
	}
	return s.stores[key]
}

func (store *FDBRecordStore) physicalRecord(primaryKey tuple.Tuple) *recordPhysicalRecord {
	// Snapshot the address before ownership; a custom Subspace is not called
	// from inside the physical group.
	records := subspace.FromBytes(store.recordsSubspace.Bytes())
	prefix := records.Pack(primaryKey)
	scope := store.physicalScope()
	scope.mu.Lock()
	key := scope.keys[string(prefix)]
	if key == nil {
		key = &recordPhysicalKey{}
		scope.keys[string(prefix)] = key
	}
	scope.mu.Unlock()
	return &recordPhysicalRecord{
		store: store, scope: scope, key: key, prefix: prefix, records: records, primaryKey: primaryKey,
		versionKey: store.versionKey(primaryKey), split: store.metaData.IsSplitLongRecords(), omit: store.omitUnsplitRecordSuffix(), legacy: store.useOldVersionFormat(),
	}
}

func (r *recordPhysicalRecord) lock() (func(), error) {
	r.store.context.physicalHook("before-lock", r.prefix)
	if err := r.store.context.recordWriteContext().Err(); err != nil {
		return nil, err
	}
	r.scope.gate.RLock()
	r.key.mu.Lock()
	unlock := func() { r.key.mu.Unlock(); r.scope.gate.RUnlock() }
	if err := r.store.context.recordWriteContext().Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (r *recordPhysicalRecord) stamp() recordPhysicalStamp {
	return recordPhysicalStamp{generation: r.key.generation, epoch: r.scope.epoch}
}

func (r *recordPhysicalRecord) startLoad() pendingStoreRecordLoad {
	var load pendingStoreRecordLoad
	if r.legacy {
		load.oldVersion = r.store.loadRecordVersionAtKeyAsync(r.versionKey, false, true)
	}
	load.raw = r.startRawLoad()
	return load
}

func (r *recordPhysicalRecord) startRawLoad() pendingRecordLoad {
	return startLoadWithSplit(r.store.context.Transaction(), r.records, r.primaryKey, r.split, r.omit, r.store.context)
}

func (store *FDBRecordStore) startAuthoritativeRecordLoad(tx fdb.ReadTransaction, primaryKey tuple.Tuple) pendingStoreRecordLoad {
	r := store.physicalRecord(primaryKey)
	unlock, err := r.lock()
	if err != nil {
		return pendingStoreRecordLoad{physical: r, physicalErr: err}
	}
	stamp := r.stamp()
	unlock()
	load := store.startRecordLoad(tx, primaryKey)
	load.physical, load.stamp = r, stamp
	return load
}

// Validate raw observations before deserialization, including structural errors
// caused by a mutation between range pages. Backend errors are never discarded.
func (load *pendingStoreRecordLoad) finishAuthoritative(sizes *sizeInfo) ([]byte, *FDBRecordVersion, error) {
	if load.physicalErr != nil {
		return nil, nil, load.physicalErr
	}
	value, version, err := load.raw.finish(sizes)
	if load.raw.backendErr {
		return value, version, err
	}
	r := load.physical
	unlock, lockErr := r.lock()
	if lockErr != nil {
		return nil, nil, lockErr
	}
	defer unlock()
	if load.stamp == r.stamp() {
		return value, version, err
	}
	fresh := r.startLoad()
	fresh.physical, fresh.stamp = r, r.stamp()
	fresh.retiredVersions = load.retiredVersions
	if read, ok := load.oldVersion.(*recordVersionRead); ok {
		fresh.retiredVersions = append(fresh.retiredVersions, read)
	}
	*load = fresh
	return load.raw.finish(sizes)
}

// Only physical sizing is refreshed after opaque user work. The original
// logical old image and already-prepared bytes are not decoded or replayed.
func (load *pendingStoreRecordLoad) beginPhysicalMutation(present bool, sizes *sizeInfo) (bool, func(), error) {
	r := load.physical
	unlock, err := r.lock()
	if err != nil {
		return false, nil, err
	}
	if load.stamp != r.stamp() {
		fresh := r.startRawLoad()
		value, _, err := fresh.finish(sizes)
		if err != nil {
			unlock()
			return false, nil, err
		}
		present = value != nil
	}
	if err := r.store.context.recordWriteContext().Err(); err != nil {
		unlock()
		return false, nil, err
	}
	return present, func() {
		// Even an error after the first physical effect invalidates old images.
		r.key.generation++
		load.stamp = r.stamp()
		unlock()
	}, nil
}

func (store *FDBRecordStore) beginPhysicalBulk() (func(), error) {
	s := store.physicalScope()
	store.context.physicalHook("before-bulk", store.recordsSubspace.Bytes())
	if err := store.context.recordWriteContext().Err(); err != nil {
		return nil, err
	}
	s.gate.Lock()
	if err := store.context.recordWriteContext().Err(); err != nil {
		s.gate.Unlock()
		return nil, err
	}
	return func() { s.epoch++; s.gate.Unlock() }, nil
}

// Removing a body must drop the current deferred version, not merely the
// version observed before a serializer or another writer changed the record.
func (r *recordPhysicalRecord) clearVersion() {
	rc := r.store.context
	// Metadata-disabled legacy versions remain intentionally ignored; remove
	// current pending/local state without changing that committed-byte policy.
	if r.legacy && r.store.metaData.IsStoreRecordVersions() {
		rc.Transaction().Clear(r.versionKey)
	}
	rc.RemoveVersionMutation(r.versionKey)
	rc.RemoveLocalVersion(r.versionKey)
}
