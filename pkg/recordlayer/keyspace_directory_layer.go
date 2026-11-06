// Portions derived from FoundationDB Record Layer (ExtendedDirectoryLayer.java,
// LocatableResolver.java, DirectoryLayerDirectory.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/keyspace"
)

// ---- ExtendedDirectoryLayer (global) ----

// ExtendedDirectoryLayerGlobal shares root mappings with ScopedDirectoryLayer.global,
// but allocates via Record Layer HighContentionAllocator and writes no other directory metadata.
type ExtendedDirectoryLayerGlobal struct{}

// edlRootNode is the directory layer's root node at the default node
// subspace: nodeSubspace.get(nodeSubspace.getKey()).
var edlRootNode = nodeSubspaceGlobal.Sub(nodeSubspaceGlobal.Bytes())

func (ExtendedDirectoryLayerGlobal) scopeKey() string { return "EDL" }

// stateKey is the node subspace's -10 (STATE_SUBSPACE_KEY_SUFFIX), shared
// with ScopedDirectoryLayer.global.
func (ExtendedDirectoryLayerGlobal) stateKey() fdb.Key {
	return fdb.Key(nodeSubspaceGlobal.Sub(int64(-10)).Bytes())
}

// mapping is getMappingSubspaceAsync: the root node's subdirectories, 0.
func (ExtendedDirectoryLayerGlobal) mapping() subspace.Subspace { return edlRootNode.Sub(int64(0)) }

// hca is getHca at the root level: counters and allocations under the root
// node's "hca" bytes, where the directory layer's own allocator keeps them.
func (ExtendedDirectoryLayerGlobal) hca() highContentionAllocator {
	hca := edlRootNode.Sub([]byte("hca"))
	return highContentionAllocator{counter: hca.Sub(int64(0)), allocation: hca.Sub(int64(1)), root: true}
}

// deserializeValue unpacks the empty content subspace's (value) or
// (value, metadata).
func (ExtendedDirectoryLayerGlobal) deserializeValue(raw []byte) (ResolverResult, error) {
	t, err := tuple.Unpack(raw)
	if err != nil {
		return ResolverResult{}, err
	}
	if len(t) == 0 {
		return ResolverResult{}, &RecordCoreError{Message: "empty directory layer value"}
	}
	v, ok := t[0].(int64)
	if !ok {
		return ResolverResult{}, &RecordCoreError{Message: "directory layer value is not a long"}
	}
	if len(t) == 1 {
		return ResolverResult{Value: v}, nil
	}
	metadata, ok := t[1].([]byte)
	if !ok {
		return ResolverResult{}, &RecordCoreError{Message: "directory layer metadata is not bytes"}
	}
	return ResolverResult{Value: v, Metadata: metadata}, nil
}

// read is readInternal: the mapping, recorded in the reverse cache.
func (e ExtendedDirectoryLayerGlobal) read(rctx *FDBRecordContext, name string) (*ResolverResult, error) {
	raw, err := rctx.Transaction().Get(e.mapping().Pack(tuple.Tuple{name})).Get()
	if err != nil || raw == nil {
		return nil, err
	}
	res, err := e.deserializeValue(raw)
	if err != nil {
		return nil, err
	}
	if err := putReverseIfNotExists(rctx, e, name, res.Value); err != nil {
		return nil, err
	}
	return &res, nil
}

// create is createInternal: allocate, write the mapping, then the reverse
// cache entry.
func (e ExtendedDirectoryLayerGlobal) create(rctx *FDBRecordContext, name string, metadata []byte) (ResolverResult, error) {
	value, err := e.hca().allocate(rctx, name)
	if err != nil {
		return ResolverResult{}, err
	}
	stored := tuple.Tuple{value}
	if metadata != nil {
		stored = tuple.Tuple{value, metadata}
	}
	rctx.Transaction().Set(e.mapping().Pack(tuple.Tuple{name}), stored.Pack())
	if err := putReverseIfNotExists(rctx, e, name, value); err != nil {
		return ResolverResult{}, err
	}
	return ResolverResult{Value: value, Metadata: metadata}, nil
}

// readReverse is the reverse directory cache entry of the global scope.
func (ExtendedDirectoryLayerGlobal) readReverse(rctx *FDBRecordContext, value int64) (string, bool, error) {
	return GlobalDirectoryLayer{}.readReverse(rctx, value)
}

// ---- LocatableResolver bound to a database, for KeySpace directories ----

// KeySpaceResolver binds a keyspace.LocatableResolver scope to its database;
// each call uses its own context, matching Java’s database-level calls.
type KeySpaceResolver struct {
	db        *FDBDatabase
	scope     LocatableResolver
	configure TransactionConfigurer
}

var _ keyspace.LocatableResolver = (*KeySpaceResolver)(nil)

// NewKeySpaceResolver binds scope (ExtendedDirectoryLayerGlobal,
// GlobalDirectoryLayer, or a ScopedInterningLayer) to db.
func NewKeySpaceResolver(db *FDBDatabase, scope LocatableResolver) *KeySpaceResolver {
	return &KeySpaceResolver{db: db, scope: scope}
}

// WithTransactionConfigurer returns a resolver whose parent and child transactions
// use configure, including retries. The original resolver is unchanged.
func (r *KeySpaceResolver) WithTransactionConfigurer(configure TransactionConfigurer) *KeySpaceResolver {
	copy := *r
	copy.configure = configure
	return &copy
}

// openContext is FDBDatabase.openContext: a context over a fresh transaction,
// cancelled by close.
func (r *KeySpaceResolver) openContext(ctx context.Context) (*FDBRecordContext, func(), error) {
	tx, err := r.db.CreateWritableTransaction()
	if err != nil {
		return nil, nil, err
	}
	rctx := r.db.NewRecordContextIn(ctx, tx)
	if err := rctx.Configure(r.configure); err != nil {
		tx.Cancel()
		return nil, nil, err
	}
	return rctx, tx.Cancel, nil
}

// Resolve creates absent mappings in an independently committed context
// (LocatableResolver.resolveWithMetadata, LocatableResolver.java:256-338).
func (r *KeySpaceResolver) Resolve(ctx context.Context, name string) (int64, error) {
	rctx, closeContext, err := r.openContext(ctx)
	if err != nil {
		return 0, err
	}
	defer closeContext()
	res, err := ResolveWithMetadata(rctx, r.scope, name)
	if err != nil {
		return 0, err
	}
	return res.Value, nil
}

// ReverseLookup uses the reverse cache or borrows a read version, retrying misses once at a newer one.
// It caches results; ok=false corresponds to Java NoSuchElementException (LocatableResolver.java:416-470).
func (r *KeySpaceResolver) ReverseLookup(ctx context.Context, value int64) (string, bool, error) {
	key := reverseCacheKey(r.scope, value)
	c := r.db.resolvers()
	c.mu.RLock()
	name, ok := c.reverse[key]
	c.mu.RUnlock()
	if ok {
		return name, true, nil
	}
	rctx, closeContext, err := r.openContext(ctx)
	if err != nil {
		return "", false, err
	}
	defer closeContext()
	name, err = reverseLookupFromDatabase(rctx, r.scope, value)
	var missing *NoSuchElementError
	if errors.As(err, &missing) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// reverseLookupFromDatabase is LocatableResolver.reverseLookupFromDatabase
// with runAsyncBorrowingReadVersion.
func reverseLookupFromDatabase(parent *FDBRecordContext, r LocatableResolver, value int64) (string, error) {
	type lookup struct {
		name string
		ok   bool
	}
	ctx := parent.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	first, maybeStale := true, true
	out, err := parent.runChild(ctx, func(child *FDBRecordContext) (any, error) {
		if first {
			// The first attempt borrows the parent's read version.
			first = false
			rv, err := parent.Transaction().GetReadVersion().Get()
			if err != nil {
				return nil, err
			}
			child.Transaction().SetReadVersion(rv)
		}
		name, ok, err := r.readReverse(child, value)
		if err != nil {
			return nil, err
		}
		if maybeStale {
			maybeStale = false
			if !ok {
				// The borrowed version may predate the mapping: retry at a
				// newer read version.
				return nil, &RecordCoreRetriableTransactionError{Message: "reverse lookup failed on potentially stale read"}
			}
		}
		return lookup{name, ok}, nil
	})
	if err != nil {
		return "", err
	}
	found := out.(lookup)
	if !found.ok {
		return "", &NoSuchElementError{Message: fmt.Sprintf("reverse lookup of %d", value)}
	}
	if db := parent.GetDatabase(); db != nil {
		c := db.resolvers()
		c.mu.Lock()
		c.reverse[reverseCacheKey(r, value)] = found.name
		c.mu.Unlock()
	}
	return found.name, nil
}

// DirectoryLayerDirectory resolves names to longs through ExtendedDirectoryLayer.global.
// Go resolves paths when built, so db is explicit rather than taken from Java’s context.
func DirectoryLayerDirectory(name string, db *FDBDatabase) *keyspace.Directory {
	return keyspace.ResolverDirectory(name, NewKeySpaceResolver(db, ExtendedDirectoryLayerGlobal{}))
}
