// Portions derived from FoundationDB Record Layer (
// RelationalKeyspaceProvider.java, ScopedDirectoryLayer.java,
// FDBRecordStoreKeyspace.java, FDBRecordContext.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package keyspace defines the relational-layer FDB key structure, Java's
// RelationalKeyspaceProvider byte for byte:
//
//	catalog store:  (NULL, NULL, 0)                  __SYS / __SYS / CATALOG
//	user schema:    (domain, database, schema)        three longs
//
// The domain is a directory of the FDB directory layer at the root (Java's
// ScopedDirectoryLayer.global); the database and the schema are names interned
// in the domain's interning layer at (domain, "IL") (ScopedInterningLayer).
// A store written by Go is the store Java opens, and the reverse.
package keyspace

import (
	"context"
	"strings"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

const (
	SysName     = "__SYS"
	CatalogName = "CATALOG"
	// StatsName roots collected planner statistics (RFC-236). It is a SIBLING
	// of every schema subspace, never a child, because a schema subspace IS a
	// record store subspace and that namespace belongs to Java —
	// FDBRecordStoreKeyspace defines 0-10 and is (UNSTABLE). Statistics are
	// a Go-side optimization and must not add a byte inside it.
	StatsName = "__STATS"
)

// RelationalKeyspace provides FDB subspace resolution for the relational layer.
// Construct one with a root subspace (e.g. the empty subspace or a tenant-specific
// prefix), then call CatalogSubspace / SchemaSubspace to get per-object subspaces.
type RelationalKeyspace struct {
	root subspace.Subspace
	// resolved caches committed schema resolutions: an interned name never
	// changes its value (Java's directory cache relies on the same).
	resolved sync.Map // dbPath "\x00" schema -> subspace.Subspace
}

// New returns a RelationalKeyspace rooted at root.
func New(root subspace.Subspace) *RelationalKeyspace {
	return &RelationalKeyspace{root: root}
}

// CatalogSubspace returns the subspace for the system catalog record store:
// __SYS (NULL) / __SYS (NULL) / CATALOG (LONG 0).
func (k *RelationalKeyspace) CatalogSubspace() subspace.Subspace {
	return k.root.Sub(nil, nil, int64(0))
}

// SchemaSubspace resolves a schema's record store subspace, Java's
// toDatabasePath(dbUri).schemaPath(schemaName) resolved: the domain's
// directory-layer long, then the database and schema names interned in the
// domain's scope (created on first use, each in a transaction of its own, as
// Java's resolveWithMetadata does).
func (k *RelationalKeyspace) SchemaSubspace(rctx *recordlayer.FDBRecordContext, dbPath, schemaName string) (subspace.Subspace, error) {
	if ss, ok := k.resolved.Load(dbPath + "\x00" + schemaName); ok {
		return ss.(subspace.Subspace), nil
	}
	if dbPath == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "dbPath must not be empty")
	}
	if schemaName == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "schemaName must not be empty")
	}
	// Java resolves a schema's store through toDatabasePath(dbUri).schemaPath
	// (create/drop schema, every store open), so a database outside the
	// registered domains is INVALID_PATH there.
	path, err := ToDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}
	if path.System {
		// __SYS's only store is the catalog (CATALOG, a LONG constant 0).
		if schemaName == CatalogName {
			return k.CatalogSubspace(), nil
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidPath, "<%s/%s> is an invalid database path", dbPath, schemaName)
	}
	domain, err := recordlayer.ResolveWithMetadata(rctx, recordlayer.GlobalDirectoryLayer{}, path.Domain)
	if err != nil {
		return nil, err
	}
	scope := recordlayer.NewScopedInterningLayer(k.root.Sub(domain.Value, InterningLayerValue))
	db, err := recordlayer.ResolveWithMetadata(rctx, scope, path.Database)
	if err != nil {
		return nil, err
	}
	schema, err := recordlayer.ResolveWithMetadata(rctx, scope, schemaName)
	if err != nil {
		return nil, err
	}
	ss := k.root.Sub(domain.Value, db.Value, schema.Value)
	// Only a resolution its own committed transactions made is cacheable; over
	// an external transaction it may still roll back.
	if rctx.GetDatabase() != nil {
		k.resolved.Store(dbPath+"\x00"+schemaName, ss)
	}
	return ss, nil
}

// LookupSchemaSubspace resolves a schema's store subspace without creating
// any mapping (readInTransaction): a domain, database or schema name that was
// never interned names no store, and is UNDEFINED_SCHEMA.
func (k *RelationalKeyspace) LookupSchemaSubspace(ctx context.Context, db *recordlayer.FDBDatabase, dbPath, schemaName string) (subspace.Subspace, error) {
	if ss, ok := k.resolved.Load(dbPath + "\x00" + schemaName); ok {
		return ss.(subspace.Subspace), nil
	}
	path, err := ToDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}
	if path.System {
		if schemaName == CatalogName {
			return k.CatalogSubspace(), nil
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidPath, "<%s/%s> is an invalid database path", dbPath, schemaName)
	}
	out, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		missing := api.NewErrorf(api.ErrCodeUndefinedSchema, "Schema <%s/%s> has no record store", dbPath, schemaName)
		domain, ok, err := recordlayer.ReadInTransaction(rctx, recordlayer.GlobalDirectoryLayer{}, path.Domain)
		if err != nil || !ok {
			return nil, firstErr(err, missing)
		}
		scope := recordlayer.NewScopedInterningLayer(k.root.Sub(domain.Value, InterningLayerValue))
		dbv, ok, err := recordlayer.ReadInTransaction(rctx, scope, path.Database)
		if err != nil || !ok {
			return nil, firstErr(err, missing)
		}
		sv, ok, err := recordlayer.ReadInTransaction(rctx, scope, schemaName)
		if err != nil || !ok {
			return nil, firstErr(err, missing)
		}
		return k.root.Sub(domain.Value, dbv.Value, sv.Value), nil
	})
	if err != nil {
		return nil, err
	}
	return out.(subspace.Subspace), nil
}

func firstErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// Runner runs fn in an auto-commit transaction: an *recordlayer.FDBDatabase, or
// a connection's runner that configures each transaction from its options.
type Runner interface {
	Run(ctx context.Context, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error)
}

// SchemaSubspaceIn is SchemaSubspace for a caller outside a transaction: a
// resolved schema comes from the keyspace's cache, otherwise it is resolved
// in a transaction of the database's.
func (k *RelationalKeyspace) SchemaSubspaceIn(ctx context.Context, db Runner, dbPath, schemaName string) (subspace.Subspace, error) {
	if ss, ok := k.resolved.Load(dbPath + "\x00" + schemaName); ok {
		return ss.(subspace.Subspace), nil
	}
	out, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return k.SchemaSubspace(rctx, dbPath, schemaName)
	})
	if err != nil {
		return nil, err
	}
	return out.(subspace.Subspace), nil
}

// ParseDBPath breaks a URI-style database path like "/domain/db" into its
// path components, stripping the leading slash. Returns an error if the
// path is invalid.
func ParseDBPath(dbPath string) ([]string, error) {
	if dbPath == "" || dbPath[0] != '/' {
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
			"database path must start with '/': %q", dbPath)
	}
	parts := strings.Split(dbPath[1:], "/")
	for _, p := range parts {
		if p == "" {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
				"database path has empty segment: %q", dbPath)
		}
	}
	return parts, nil
}

// StatisticsSubspace returns the root under which collected planner statistics
// live (RFC-236). Statistics for a given store are keyed BELOW this by that
// store's own subspace prefix, which is what makes the scheme layout-agnostic:
// a store is its prefix, whether it came from a relational schema, a hand-built
// store, or a Java-authored layout.
//
// It deliberately takes no (dbPath, schema): the per-store keying happens one
// level down, so one root serves every store in this keyspace and the collector
// needs no relational concepts at all.
//
// TENANTS: FDB tenants are separate keyspaces, so a store's prefix is only
// meaningful within its tenant — two tenants may hold byte-identical prefixes
// for different stores. A tenant-scoped deployment must therefore build its
// RelationalKeyspace on a root inside that tenant, which it already does, since
// `root` is whatever the caller opened. Nothing here can mix tenants that the
// caller has not already mixed.
func (k *RelationalKeyspace) StatisticsSubspace() subspace.Subspace {
	return k.root.Sub(tuple.Tuple{StatsName})
}
