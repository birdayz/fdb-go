package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/directory"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Java's LocatableResolver family (keyspace/LocatableResolver,
// ScopedDirectoryLayer, layers/interning/ScopedInterningLayer,
// StringInterningLayer, HighContentionAllocator, FDBReverseDirectoryCache):
// a scope that maps strings to compact longs, persisted byte-for-byte as Java
// persists them, so a key space path resolved by Go and by Java lands on the
// same keys. Go ports the resolution path a DirectoryLayerDirectory uses
// (resolveWithMetadata, reverseLookup) with the default create hooks; the
// administrative operations (locking, migration, setMapping, setWindow) are
// not ported.

// ResolverResult is Java's ResolverResult: the mapped value and the metadata
// stored with it.
type ResolverResult struct {
	Value    int64
	Metadata []byte
}

// LocatableResolver is a scope that maps names to longs.
type LocatableResolver interface {
	// scopeKey identifies the scope (Java's resolver equality: its class and
	// path) for the process caches.
	scopeKey() string
	read(rctx *FDBRecordContext, name string) (*ResolverResult, error)
	create(rctx *FDBRecordContext, name string, metadata []byte) (ResolverResult, error)
	readReverse(rctx *FDBRecordContext, value int64) (string, bool, error)
	stateKey() fdb.Key
}

// LocatableResolverLockedError is Java's LocatableResolverLockedException.
type LocatableResolverLockedError struct{ Message string }

func (e *LocatableResolverLockedError) Error() string { return e.Message }

// resolverCache is FDBDatabase's directory cache: committed mappings per
// scope. Java keys it additionally by the resolver state's version, which
// only the unported migration operations change.
type resolverCache struct {
	mu      sync.RWMutex
	forward map[string]ResolverResult
	reverse map[string]string
}

func (d *FDBDatabase) resolvers() *resolverCache {
	d.resolverCacheOnce.Do(func() {
		d.resolverCache = &resolverCache{forward: map[string]ResolverResult{}, reverse: map[string]string{}}
	})
	return d.resolverCache
}

func (c *resolverCache) get(key string) (ResolverResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.forward[key]
	return r, ok
}

func (c *resolverCache) put(key string, r ResolverResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forward[key] = r
}

// ResolveWithMetadata is LocatableResolver.resolveWithMetadata: the cached
// mapping, else the stored one, else a new one, read and created in a child
// transaction that commits on its own (runAsyncBorrowingReadVersion), so the
// mapping survives the caller's rollback. Without a database (a context over
// an external transaction) it resolves in the caller's transaction.
func ResolveWithMetadata(rctx *FDBRecordContext, r LocatableResolver, name string) (ResolverResult, error) {
	db := rctx.GetDatabase()
	if db == nil {
		return readOrCreate(rctx, r, name)
	}
	key := r.scopeKey() + "\x00" + name
	if res, ok := db.resolvers().get(key); ok {
		return res, nil
	}
	ctx := rctx.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out, err := db.Run(ctx, func(child *FDBRecordContext) (any, error) {
		return readOrCreate(child, r, name)
	})
	if err != nil {
		return ResolverResult{}, err
	}
	res := out.(ResolverResult)
	db.resolvers().put(key, res)
	return res, nil
}

// ReadInTransaction is LocatableResolver.readInTransaction: the cached or
// stored mapping, read in the caller's transaction, never created. ok is
// false when the name has no mapping.
func ReadInTransaction(rctx *FDBRecordContext, r LocatableResolver, name string) (ResolverResult, bool, error) {
	db := rctx.GetDatabase()
	key := r.scopeKey() + "\x00" + name
	if db != nil {
		if res, ok := db.resolvers().get(key); ok {
			return res, true, nil
		}
	}
	res, err := r.read(rctx, name)
	if err != nil || res == nil {
		return ResolverResult{}, false, err
	}
	if db != nil {
		c, found := db.resolvers(), *res
		rctx.AddPostCommit(func() { c.put(key, found) })
	}
	return *res, true, nil
}

// ReverseLookup is LocatableResolver.reverseLookup: the name a value maps to,
// or NoSuchElementException "reverse lookup of <value>".
func ReverseLookup(rctx *FDBRecordContext, r LocatableResolver, value int64) (string, error) {
	db := rctx.GetDatabase()
	key := fmt.Sprintf("%s\x00%d", r.scopeKey(), value)
	if db != nil {
		c := db.resolvers()
		c.mu.RLock()
		name, ok := c.reverse[key]
		c.mu.RUnlock()
		if ok {
			return name, nil
		}
	}
	name, ok, err := r.readReverse(rctx, value)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &NoSuchElementError{Message: fmt.Sprintf("reverse lookup of %d", value)}
	}
	if db != nil {
		c := db.resolvers()
		rctx.AddPostCommit(func() {
			c.mu.Lock()
			c.reverse[key] = name
			c.mu.Unlock()
		})
	}
	return name, nil
}

// NoSuchElementError is Java's NoSuchElementException.
type NoSuchElementError struct{ Message string }

func (e *NoSuchElementError) Error() string { return e.Message }

// readOrCreate is readOrCreateValue and createIfNotLocked with the default
// hooks (no pre-write checks, no metadata): the stored mapping, else the
// resolver state is read serializably and must be UNLOCKED, then the mapping
// is created.
func readOrCreate(rctx *FDBRecordContext, r LocatableResolver, name string) (ResolverResult, error) {
	res, err := r.read(rctx, name)
	if err != nil || res != nil {
		if res != nil {
			return *res, nil
		}
		return ResolverResult{}, err
	}
	raw, err := rctx.Transaction().Get(r.stateKey()).Get()
	if err != nil {
		return ResolverResult{}, err
	}
	state := &gen.State{}
	if raw != nil {
		if err := proto.Unmarshal(raw, state); err != nil {
			return ResolverResult{}, &RecordCoreError{Message: "invalid state value", Cause: err}
		}
	}
	if state.GetLock() != gen.WriteLock_UNLOCKED {
		return ResolverResult{}, &LocatableResolverLockedError{Message: "locatable resolver is not writable"}
	}
	return r.create(rctx, name, nil)
}

// ---- HighContentionAllocator (layers/interning) ----

// Java's record-layer HighContentionAllocator. It differs from the FDB
// directory layer's: windows of 64, 1024 and 4096, the window advances when
// count * 2 exceeds it, a candidate's allocation key holds the packed name,
// and a root-level allocator refuses a candidate whose tuple prefix holds
// keys.
type highContentionAllocator struct {
	counter, allocation subspace.Subspace
	root                bool
}

var littleEndianLongOne = []byte{1, 0, 0, 0, 0, 0, 0, 0}

func hcaWindowSize(start int64) int64 {
	switch {
	case start < 255:
		return 1 << 6
	case start < 65_535:
		return 1 << 10
	}
	return 1 << 12
}

func (h highContentionAllocator) allocate(rctx *FDBRecordContext, name string) (int64, error) {
	tr := rctx.Transaction()
	kvs, err := tr.Snapshot().GetRange(h.counter, fdb.RangeOptions{Limit: 1, Reverse: true}).GetSliceWithError()
	if err != nil {
		return 0, err
	}
	var start int64
	if len(kvs) == 1 {
		t, err := h.counter.Unpack(kvs[0].Key)
		if err != nil {
			return 0, err
		}
		start = t[0].(int64)
	}
	wipe := false
	for {
		counterKey := h.counter.Pack(tuple.Tuple{start})
		if wipe {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(h.counter.Bytes()), End: counterKey})
		}
		tr.Add(counterKey, littleEndianLongOne)
		raw, err := tr.Snapshot().Get(counterKey).Get()
		if err != nil {
			return 0, err
		}
		var count int64
		if len(raw) > 0 {
			buf := make([]byte, 8)
			copy(buf, raw)
			count = int64(binary.LittleEndian.Uint64(buf))
		}
		if count*2 > hcaWindowSize(start) {
			start += hcaWindowSize(start)
			wipe = true
			continue
		}
		break
	}
	window := hcaWindowSize(start)
	value := tuple.Tuple{name}.Pack()
	for {
		candidate := start + int64(envUint64(rctx)%uint64(window))
		allocationKey := h.allocation.Pack(tuple.Tuple{candidate})
		good := true
		if h.root {
			// hasConflictAtRoot: no key may start with the candidate's tuple.
			r, err := fdb.PrefixRange(tuple.Tuple{candidate}.Pack())
			if err != nil {
				return 0, err
			}
			kvs, err := tr.Snapshot().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return 0, err
			}
			good = len(kvs) == 0
		}
		previous, err := tr.Get(allocationKey).Get()
		if err != nil {
			return 0, err
		}
		if err := tr.Options().SetNextWriteNoWriteConflictRange(); err != nil {
			return 0, err
		}
		tr.Set(allocationKey, []byte{})
		if previous == nil {
			if !good {
				tr.Set(allocationKey, []byte{0xFD})
				return 0, &IllegalStateError{Message: "database already has keys in allocation range"}
			}
			tr.Set(allocationKey, value)
			return candidate, nil
		}
		if len(previous) != 0 {
			// We overwrote a real allocation: put it back.
			if err := tr.Options().SetNextWriteNoWriteConflictRange(); err != nil {
				return 0, err
			}
			tr.Set(allocationKey, previous)
		}
	}
}

// IllegalStateError is Java's IllegalStateException.
type IllegalStateError struct{ Message string }

func (e *IllegalStateError) Error() string { return e.Message }

// ---- ScopedInterningLayer / StringInterningLayer ----

// ScopedInterningLayer is Java's ScopedInterningLayer over a resolved path:
// its StringInterningLayer keeps the counters at base/0, the reverse mapping
// (the allocator's allocation subspace) at base/1, the mapping at base/2, and
// the resolver state at base/-10.
type ScopedInterningLayer struct {
	base subspace.Subspace
}

// NewScopedInterningLayer is the interning layer scoped to the path whose
// resolved subspace is base.
func NewScopedInterningLayer(base subspace.Subspace) *ScopedInterningLayer {
	return &ScopedInterningLayer{base: base}
}

func (s *ScopedInterningLayer) scopeKey() string { return "IL" + string(s.base.Bytes()) }

func (s *ScopedInterningLayer) mapping() subspace.Subspace { return s.base.Sub(int64(2)) }

func (s *ScopedInterningLayer) stateKey() fdb.Key {
	return fdb.Key(s.base.Sub(int64(-10)).Bytes())
}

func (s *ScopedInterningLayer) read(rctx *FDBRecordContext, name string) (*ResolverResult, error) {
	raw, err := rctx.Transaction().Get(s.mapping().Pack(tuple.Tuple{name})).Get()
	if err != nil || raw == nil {
		return nil, err
	}
	data := &gen.Data{}
	if err := proto.Unmarshal(raw, data); err != nil {
		return nil, &RecordCoreError{Message: "invalid interned value", Cause: err}
	}
	return &ResolverResult{Value: int64(data.GetInternedValue()), Metadata: data.GetMetadata()}, nil
}

func (s *ScopedInterningLayer) create(rctx *FDBRecordContext, name string, metadata []byte) (ResolverResult, error) {
	key := s.mapping().Pack(tuple.Tuple{name})
	existing, err := rctx.Transaction().Get(key).Get()
	if err != nil {
		return ResolverResult{}, err
	}
	if existing != nil {
		return ResolverResult{}, &RecordCoreError{Message: "value already exists in interning layer"}
	}
	hca := highContentionAllocator{counter: s.base.Sub(int64(0)), allocation: s.base.Sub(int64(1))}
	value, err := hca.allocate(rctx, name)
	if err != nil {
		return ResolverResult{}, err
	}
	data := &gen.Data{InternedValue: proto.Uint64(uint64(value))}
	if metadata != nil {
		data.Metadata = metadata
	}
	raw, err := proto.Marshal(data)
	if err != nil {
		return ResolverResult{}, err
	}
	rctx.Transaction().Set(key, raw)
	return ResolverResult{Value: value, Metadata: metadata}, nil
}

func (s *ScopedInterningLayer) readReverse(rctx *FDBRecordContext, value int64) (string, bool, error) {
	raw, err := rctx.Transaction().Get(s.base.Sub(int64(1)).Pack(tuple.Tuple{value})).Get()
	if err != nil || raw == nil {
		return "", false, err
	}
	t, err := tuple.Unpack(raw)
	if err != nil {
		return "", false, err
	}
	name, ok := t[0].(string)
	return name, ok, nil
}

// ---- ScopedDirectoryLayer (global) ----

// GlobalDirectoryLayer is ScopedDirectoryLayer.global: the FDB directory layer
// at the default node subspace (0xFE) with an empty content subspace, whose
// one-element directories' prefixes are tuple longs, plus the reverse
// directory cache entry each read or create writes.
type GlobalDirectoryLayer struct{}

// reverseDirectoryCacheEntry is FDBReverseDirectoryCache's directory name.
const reverseDirectoryCacheEntry = "recdb_rd_cache"

var nodeSubspaceGlobal = subspace.FromBytes([]byte{0xFE})

// globalDirectoryLayer draws its prefix candidates from the context's
// randomness (the DST seam), so a simulation allocates the same prefixes on
// every run; production draws from crypto/rand.
func globalDirectoryLayer(rctx *FDBRecordContext) directory.Directory {
	return directory.NewDirectoryLayerWithRandom(nodeSubspaceGlobal, subspace.FromBytes(nil), false,
		func(n int64) int64 { return int64(envUint64(rctx) % uint64(n)) })
}

func envUint64(rctx *FDBRecordContext) uint64 {
	var b [8]byte
	if _, err := rctx.Env().Read(b[:]); err != nil {
		panic(err) // crypto/rand and the DST sources do not fail
	}
	return binary.LittleEndian.Uint64(b[:])
}

// inTransaction runs a directory operation in the caller's transaction.
type inTransaction struct{ fdb.WritableTransaction }

func (t inTransaction) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	return f(t.WritableTransaction)
}

func (GlobalDirectoryLayer) scopeKey() string { return "DL" }

func (GlobalDirectoryLayer) stateKey() fdb.Key {
	return fdb.Key(nodeSubspaceGlobal.Sub(int64(-10)).Bytes())
}

func directoryValue(prefix []byte) (int64, error) {
	t, err := tuple.Unpack(prefix)
	if err != nil {
		return 0, err
	}
	v, ok := t[0].(int64)
	if !ok || len(t) != 1 {
		return 0, &RecordCoreError{Message: fmt.Sprintf("directory prefix %x is not a tuple long", prefix)}
	}
	return v, nil
}

func (g GlobalDirectoryLayer) read(rctx *FDBRecordContext, name string) (*ResolverResult, error) {
	dl := globalDirectoryLayer(rctx)
	tr := inTransaction{rctx.Transaction()}
	exists, err := dl.Exists(tr, []string{name})
	if err != nil || !exists {
		return nil, err
	}
	ds, err := dl.Open(tr, []string{name}, nil)
	if err != nil {
		return nil, err
	}
	v, err := directoryValue(ds.Bytes())
	if err != nil {
		return nil, err
	}
	if err := g.putReverseIfNotExists(rctx, name, v); err != nil {
		return nil, err
	}
	return &ResolverResult{Value: v}, nil
}

func (g GlobalDirectoryLayer) create(rctx *FDBRecordContext, name string, metadata []byte) (ResolverResult, error) {
	if metadata != nil {
		return ResolverResult{}, &IllegalArgumentError{Message: "cannot set metadata in ScopedDirectoryLayer"}
	}
	ds, err := globalDirectoryLayer(rctx).Create(inTransaction{rctx.Transaction()}, []string{name}, nil)
	if err != nil {
		return ResolverResult{}, err
	}
	v, err := directoryValue(ds.Bytes())
	if err != nil {
		return ResolverResult{}, err
	}
	if err := g.putReverseIfNotExists(rctx, name, v); err != nil {
		return ResolverResult{}, err
	}
	return ResolverResult{Value: v}, nil
}

func (g GlobalDirectoryLayer) readReverse(rctx *FDBRecordContext, value int64) (string, bool, error) {
	ss, err := reverseCacheSubspace(rctx)
	if err != nil {
		return "", false, err
	}
	raw, err := rctx.Transaction().Snapshot().Get(ss.Pack(tuple.Tuple{value})).Get()
	if err != nil || raw == nil {
		return "", false, err
	}
	t, err := tuple.Unpack(raw)
	if err != nil {
		return "", false, err
	}
	name, ok := t[0].(string)
	return name, ok, nil
}

// putReverseIfNotExists is FDBReverseDirectoryCache.putIfNotExists: a stored
// entry must name the same string; an absent one is written.
func (g GlobalDirectoryLayer) putReverseIfNotExists(rctx *FDBRecordContext, name string, value int64) error {
	ss, err := reverseCacheSubspace(rctx)
	if err != nil {
		return err
	}
	key := ss.Pack(tuple.Tuple{value})
	raw, err := rctx.Transaction().Snapshot().Get(key).Get()
	if err != nil {
		return err
	}
	if raw != nil {
		t, err := tuple.Unpack(raw)
		if err != nil {
			return err
		}
		if s, _ := t[0].(string); s != name {
			return &RecordCoreError{Message: "Provided value for path key does not match existing value in reverse directory layer cache"}
		}
		return nil
	}
	rctx.Transaction().Set(key, tuple.Tuple{name}.Pack())
	return nil
}

// reverseCacheSubspace is the global scope's reverse cache: the base subspace
// (empty) under the long of the "recdb_rd_cache" directory, which
// FDBReverseDirectoryCache creates or opens once per database in its own
// transaction.
func reverseCacheSubspace(rctx *FDBRecordContext) (subspace.Subspace, error) {
	entry, err := reverseCacheEntry(rctx)
	if err != nil {
		return nil, err
	}
	return subspace.Sub(entry), nil
}

func reverseCacheEntry(rctx *FDBRecordContext) (int64, error) {
	create := func(rctx *FDBRecordContext) (int64, error) {
		ds, err := globalDirectoryLayer(rctx).CreateOrOpen(inTransaction{rctx.Transaction()}, []string{reverseDirectoryCacheEntry}, nil)
		if err != nil {
			return 0, err
		}
		return directoryValue(ds.Bytes())
	}
	db := rctx.GetDatabase()
	if db == nil {
		return create(rctx)
	}
	db.reverseCacheMu.Lock()
	defer db.reverseCacheMu.Unlock()
	if db.reverseCacheEntry != nil {
		return *db.reverseCacheEntry, nil
	}
	ctx := rctx.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out, err := db.Run(ctx, func(child *FDBRecordContext) (any, error) { return create(child) })
	if err != nil {
		return 0, err
	}
	v := out.(int64)
	db.reverseCacheEntry = &v
	return v, nil
}
