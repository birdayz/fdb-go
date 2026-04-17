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
func (r *FDBResolver) Resolve(ctx context.Context, name string) (int64, error) {
	// Fast path: in-memory cache hit.
	r.mu.RLock()
	if v, ok := r.forward[name]; ok {
		r.mu.RUnlock()
		return v, nil
	}
	r.mu.RUnlock()

	// Slow path: transactional read + allocate if absent.
	result, err := r.db.Transact(func(tx fdb.Transaction) (any, error) {
		nameKey := r.subspace.Pack(tuple.Tuple{"n", name})
		// Check if name is already mapped
		existing := tx.Get(fdb.Key(nameKey)).MustGet()
		if len(existing) == 8 {
			return int64(binary.BigEndian.Uint64(existing)), nil
		}

		// Allocate a new value from the counter
		counterKey := r.subspace.Pack(tuple.Tuple{"c"})
		counterBytes := tx.Get(fdb.Key(counterKey)).MustGet()
		var next int64
		if len(counterBytes) == 8 {
			next = int64(binary.BigEndian.Uint64(counterBytes))
		}

		// Write forward and reverse mappings
		var valueBuf [8]byte
		binary.BigEndian.PutUint64(valueBuf[:], uint64(next))
		tx.Set(fdb.Key(nameKey), valueBuf[:])

		revKey := r.subspace.Pack(tuple.Tuple{"r", next})
		tx.Set(fdb.Key(revKey), []byte(name))

		// Increment counter
		binary.BigEndian.PutUint64(valueBuf[:], uint64(next+1))
		tx.Set(fdb.Key(counterKey), valueBuf[:])

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

// ReverseLookup returns the name for a value, reading from FDB if not cached.
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
		data := tx.Get(fdb.Key(revKey)).MustGet()
		if data == nil {
			return "", nil
		}
		return string(data), nil
	})
	if err != nil {
		return "", false, fmt.Errorf("fdb_resolver: reverse lookup %d: %w", value, err)
	}
	name := result.(string)
	if name == "" {
		return "", false, nil
	}

	// Populate cache
	r.mu.Lock()
	r.forward[name] = value
	r.reverse[value] = name
	r.mu.Unlock()

	return name, true, nil
}
