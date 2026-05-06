# KeySpace/KeySpacePath Design — Go Port

## Overview

KeySpace is a logical directory tree abstraction over FDB. It lets you define a schema (`KeySpace`) and navigate it (`KeySpacePath`) to get FDB-ready subspaces. `LocatableResolver` provides string-to-long mapping for compact key encoding.

## Java Class Hierarchy (25 files, ~6,900 LOC)

```
KeySpace                         # Root schema tree
KeySpacePath (interface)         # Path traversal handle
KeySpacePathImpl                 # Main implementation
ResolvedKeySpacePath             # Path with resolved FDB values

KeySpaceDirectory                # Tree node (1,010 LOC)
  DirectoryLayerDirectory        # Node backed by FDB DirectoryLayer
  
LocatableResolver (abstract)     # String<->Long resolver (1,102 LOC)
  ScopedDirectoryLayer           # Resolver using FDB DirectoryLayer (234 LOC)
  ExtendedDirectoryLayer         # Alternative resolver (285 LOC)
```

## Dependency Graph

```
FDBReverseDirectoryCache
  └── LocatableResolver
        └── ScopedDirectoryLayer
              └── FDB DirectoryLayer (already implemented in Go)

KeySpacePath
  └── KeySpaceDirectory
        └── DirectoryLayerDirectory
              └── LocatableResolver
```

## Proposed Go Implementation Order

### Phase 1: Core KeySpace (no resolver, ~3-4 shifts)

```go
// KeySpace defines the tree schema
type KeySpace struct {
    root *KeySpaceDirectory
}

// KeySpaceDirectory is a tree node with type + children
type KeySpaceDirectory struct {
    Name     string
    KeyType  KeyType  // STRING, LONG, NULL, UUID, BYTES
    Value    any      // constant value (optional)
    Children []*KeySpaceDirectory
}

// KeySpacePath navigates the tree
type KeySpacePath struct {
    directory *KeySpaceDirectory
    parent    *KeySpacePath
    value     any
}

// Resolve to FDB subspace
func (p *KeySpacePath) ToSubspace(ctx *FDBRecordContext) (subspace.Subspace, error)
```

**Deliverables:**
- KeySpace/KeySpaceDirectory/KeySpacePath types
- Path construction: `ks.Path("state", "CA").Add("office", 1234)`
- Forward resolution: path -> tuple -> subspace
- Type validation (STRING, LONG, etc.)
- Constant enforcement
- Proto serialization

### Phase 2: LocatableResolver + ScopedDirectoryLayer (~2-3 shifts)

```go
// LocatableResolver maps strings to compact longs
type LocatableResolver interface {
    Resolve(ctx context.Context, tx fdb.Transaction, name string) (int64, error)
    ReverseLookup(ctx context.Context, tx fdb.Transaction, value int64) (string, error)
}

// ScopedDirectoryLayer uses FDB's DirectoryLayer for resolution
type ScopedDirectoryLayer struct {
    dirLayer *directory.DirectoryLayer
    // ... caching
}
```

**Deliverables:**
- LocatableResolver interface
- ScopedDirectoryLayer using our existing Go DirectoryLayer
- Bi-directional cache
- DirectoryLayerDirectory (KeySpaceDirectory backed by resolver)

### Phase 3: FDBReverseDirectoryCache (~1 shift)

```go
// FDBReverseDirectoryCache provides persistent reverse lookups
type FDBReverseDirectoryCache struct {
    subspace subspace.Subspace  // persistent cache storage
    inMemory *lru.Cache         // in-memory layer
}

func (c *FDBReverseDirectoryCache) Get(tx fdb.Transaction, value int64) (string, error)
func (c *FDBReverseDirectoryCache) Put(tx fdb.Transaction, name string, value int64) error
func (c *FDBReverseDirectoryCache) Rebuild(ctx context.Context) error
```

### Phase 4: Reverse Resolution + Full API (~1-2 shifts)

- Tuple -> ResolvedKeySpacePath (reverse navigation)
- KeySpaceTreeResolver
- Full test suite (parity with Java's 17 test files)
- Conformance tests

## Key Decisions Needed

1. **Cache implementation**: Use `sync.Map` or `github.com/hashicorp/golang-lru`?
2. **Async model**: Java uses `CompletableFuture`. Go uses goroutines + error returns. Direct translation.
3. **Proto serialization**: Need to decide if we support KeySpace proto round-trip (adds ~200 LOC).
4. **Scoping**: Do we need multi-resolver support or just single ScopedDirectoryLayer?

## Estimated Total: 7-10 shifts

Not a single-shift task. Start with Phase 1 (pure tree traversal, no FDB), then add resolver layers incrementally.
