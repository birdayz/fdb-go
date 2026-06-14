package keyspace

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// FDBResolver is a persistent LocatableResolver backed by an FDB subspace.
// It stores name→int64 mappings directly (no DirectoryLayer complexity),
// providing a simpler alternative to Java's ScopedDirectoryLayer.
//
// Storage layout under the given subspace:
//
//	subspace.Pack({"n", name}) → int64 value (big-endian)
//	subspace.Pack({"r", value}) → name (reverse lookup)
//	subspace.Pack({"c"})       → int64 counter (next value to allocate)
//
// An in-memory cache fronts the FDB reads for hot keys. The cache is
// transactional-aware: newly allocated mappings are only cached after commit.
type FDBResolver struct {
	db       fdb.Database
	subspace subspace.Subspace

	mu      sync.RWMutex
	forward map[string]int64 // name → value (committed only)
	reverse map[int64]string // value → name (committed only)
}

// NewFDBResolver creates a persistent resolver under the given subspace.
// The database is used for transactions; the subspace holds the mappings.
func NewFDBResolver(db fdb.Database, ss subspace.Subspace) *FDBResolver {
	return &FDBResolver{
		db:       db,
		subspace: ss,
		forward:  make(map[string]int64),
		reverse:  make(map[int64]string),
	}
}

// Resolve returns the value for name, allocating and persisting a new one if absent.
// Safe for concurrent use — FDB transactions handle cross-process contention.
//
// ctx is accepted for LocatableResolver interface compliance but is NOT forwarded
// to the FDB transaction — db.Transact uses the database's internal context.
// Use NewFDBResolver with a database configured for the desired deadline.
func (r *FDBResolver) Resolve(ctx context.Context, name string) (int64, error) {
	// Fast path: in-memory cache hit.
	r.mu.RLock()
	if v, ok := r.forward[name]; ok {
		r.mu.RUnlock()
		return v, nil
	}
	r.mu.RUnlock()

	// Slow path: transactional read + allocate if absent.
	result, err := r.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		nameKey := r.subspace.Pack(tuple.Tuple{"n", name})
		// Check if name is already mapped. Use .Get() (explicit error) rather than
		// .MustGet() (panic→panicToError): a routine transaction conflict (1020) on
		// this read is an expected, retryable event, not an invariant violation — it
		// should flow back as an error for db.Transact to retry, not via a panic.
		existing, err := tx.Get(fdb.Key(nameKey)).Get()
		if err != nil {
			return nil, err
		}
		if len(existing) == 8 {
			return int64(binary.BigEndian.Uint64(existing)), nil
		}

		// Allocate a new value from the counter
		counterKey := r.subspace.Pack(tuple.Tuple{"c"})
		counterBytes, err := tx.Get(fdb.Key(counterKey)).Get()
		if err != nil {
			return nil, err
		}
		var next int64
		if len(counterBytes) == 8 {
			next = int64(binary.BigEndian.Uint64(counterBytes))
		}

		// Write forward and reverse mappings. Use separate buffers because
		// tx.Set takes a slice — sharing a single array across writes would
		// cause the second write to corrupt the first at serialization time.
		nameVal := make([]byte, 8)
		binary.BigEndian.PutUint64(nameVal, uint64(next))
		tx.Set(fdb.Key(nameKey), nameVal)

		revKey := r.subspace.Pack(tuple.Tuple{"r", next})
		tx.Set(fdb.Key(revKey), []byte(name))

		// Increment counter (separate buffer)
		counterVal := make([]byte, 8)
		binary.BigEndian.PutUint64(counterVal, uint64(next+1))
		tx.Set(fdb.Key(counterKey), counterVal)

		return next, nil
	})
	if err != nil {
		return 0, fmt.Errorf("fdb_resolver: resolve %q: %w", name, err)
	}
	v := result.(int64)

	// Populate cache after commit
	r.mu.Lock()
	r.forward[name] = v
	r.reverse[v] = name
	r.mu.Unlock()

	return v, nil
}

// InvalidateCache clears the in-memory cache. Useful for testing or when
// you know the resolver mappings have been externally modified.
func (r *FDBResolver) InvalidateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forward = make(map[string]int64)
	r.reverse = make(map[int64]string)
}

// CacheSize returns the number of mappings currently cached in memory.
func (r *FDBResolver) CacheSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.forward)
}

// revResult carries both the name and a found flag to avoid using empty
// string as a sentinel — the name "" is a valid value.
type revResult struct {
	name  string
	found bool
}

// ReverseLookup returns the name for a value, reading from FDB if not cached.
//
// ctx is accepted for LocatableResolver interface compliance but is NOT forwarded
// to the FDB transaction — db.ReadTransact uses the database's internal context.
func (r *FDBResolver) ReverseLookup(ctx context.Context, value int64) (string, bool, error) {
	// Fast path: in-memory cache hit.
	r.mu.RLock()
	if name, ok := r.reverse[value]; ok {
		r.mu.RUnlock()
		return name, true, nil
	}
	r.mu.RUnlock()

	// Slow path: read from FDB.
	result, err := r.db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		revKey := r.subspace.Pack(tuple.Tuple{"r", value})
		data, err := tx.Get(fdb.Key(revKey)).Get()
		if err != nil {
			return revResult{}, err
		}
		if data == nil {
			return revResult{}, nil
		}
		return revResult{name: string(data), found: true}, nil
	})
	if err != nil {
		return "", false, fmt.Errorf("fdb_resolver: reverse lookup %d: %w", value, err)
	}
	res := result.(revResult)
	if !res.found {
		return "", false, nil
	}

	// Populate cache
	r.mu.Lock()
	r.forward[res.name] = value
	r.reverse[value] = res.name
	r.mu.Unlock()

	return res.name, true, nil
}
