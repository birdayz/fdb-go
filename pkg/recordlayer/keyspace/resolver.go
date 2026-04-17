package keyspace

import (
	"context"
	"fmt"
	"sync"
)

// LocatableResolver maps string names to compact int64 values and back.
// Used by DirectoryLayerDirectory for string→long key compression.
//
// Phase 2: this interface defines the contract. Phase 3 will add
// ScopedDirectoryLayer that uses FDB's directory layer for persistent
// resolution with metadata.
//
// Matches Java's LocatableResolver (abstract class).
type LocatableResolver interface {
	// Resolve maps a name to a compact int64. Creates the mapping if absent.
	Resolve(ctx context.Context, name string) (int64, error)

	// ReverseLookup maps a value back to its name. Returns ("", false, nil)
	// if the value is not in the resolver.
	ReverseLookup(ctx context.Context, value int64) (string, bool, error)
}

// MemoryResolver is an in-memory LocatableResolver for testing.
// Not persistent — data is lost on restart. Safe for concurrent use.
type MemoryResolver struct {
	mu         sync.RWMutex
	forward    map[string]int64 // name → value
	reverse    map[int64]string // value → name
	nextValue  int64
	startValue int64 // initial value for first resolution
}

// NewMemoryResolver creates an in-memory resolver starting at startValue.
func NewMemoryResolver(startValue int64) *MemoryResolver {
	return &MemoryResolver{
		forward:    make(map[string]int64),
		reverse:    make(map[int64]string),
		nextValue:  startValue,
		startValue: startValue,
	}
}

// Resolve returns the value for name, allocating a new one if needed.
func (r *MemoryResolver) Resolve(_ context.Context, name string) (int64, error) {
	r.mu.RLock()
	if v, ok := r.forward[name]; ok {
		r.mu.RUnlock()
		return v, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock
	if v, ok := r.forward[name]; ok {
		return v, nil
	}
	v := r.nextValue
	r.nextValue++
	r.forward[name] = v
	r.reverse[v] = name
	return v, nil
}

// ReverseLookup returns the name for a value, or false if not found.
func (r *MemoryResolver) ReverseLookup(_ context.Context, value int64) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name, ok := r.reverse[value]
	return name, ok, nil
}

// Size returns the number of mappings stored.
func (r *MemoryResolver) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.forward)
}

// ResolverDirectory creates a Directory node whose values are resolved
// through a LocatableResolver — strings in, int64 values stored in FDB.
//
// Input must be a string (the logical name). Output stored in FDB is int64.
// Matches Java's DirectoryLayerDirectory.isValueValid which rejects non-strings.
//
// The declared KeyType is LONG because that's how the value is stored,
// but input validation accepts only strings (matching Java).
func ResolverDirectory(name string, resolver LocatableResolver) *Directory {
	d := NewDirectory(name, KeyTypeLong)
	// Override validation: accept strings only (not longs).
	// Resolution happens via the Resolver hook below.
	d.inputValidator = func(value any) error {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("resolver directory %q: expected string, got %T", name, value)
		}
		return nil
	}
	d.Resolver = func(value any) (any, error) {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("resolver directory %q: expected string, got %T", name, value)
		}
		return resolver.Resolve(context.Background(), s)
	}
	return d
}
