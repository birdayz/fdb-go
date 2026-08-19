// Package keyspace defines the relational-layer FDB key structure.
//
// The relational layer stores data under a two-level path:
//
//	catalog store:  [root]["__SYS"]["__SYS"]["CATALOG"]
//	user schemas:   [root][domain][dbPath][schemaName]
//
// This mirrors the Java RelationalKeyspaceProvider layout conceptually,
// but uses plain tuple keys instead of DirectoryLayerDirectory —
// Go-to-Go relational stores do not need to share keyspace with Java
// relational stores. Only the RECORD LAYER data format (records,
// indexes, catalog protos) must be Java-compatible, not the path
// prefix scheme.
package keyspace

import (
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
}

// New returns a RelationalKeyspace rooted at root.
func New(root subspace.Subspace) *RelationalKeyspace {
	return &RelationalKeyspace{root: root}
}

// CatalogSubspace returns the subspace for the system catalog record store
// (the __SYS/CATALOG schema).
func (k *RelationalKeyspace) CatalogSubspace() subspace.Subspace {
	return k.root.Sub(tuple.Tuple{SysName, SysName, CatalogName})
}

// SchemaSubspace returns the subspace for a user schema identified by
// (dbPath, schemaName). dbPath is the full database path (e.g. "/my/db").
// Returns an error if dbPath or schemaName is empty.
func (k *RelationalKeyspace) SchemaSubspace(dbPath, schemaName string) (subspace.Subspace, error) {
	if dbPath == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "dbPath must not be empty")
	}
	if schemaName == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "schemaName must not be empty")
	}
	return k.root.Sub(tuple.Tuple{dbPath, schemaName}), nil
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
