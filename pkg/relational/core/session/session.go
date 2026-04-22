// Package session holds per-connection resource state for the
// relational SQL engine. Mirrors the role of
// EmbeddedRelationalConnection's resource fields in Java's
// fdb-relational-core/recordlayer/EmbeddedRelationalConnection.java —
// specifically the FDB database handle, the catalog, the keyspace
// navigator, the metadata-ops factory, the current database + schema
// identifiers, and the default-schema fallback used by ResetSession.
//
// A Session is the "bound execution context" that a query.Generator
// needs to plan and execute SQL. One Session per logical SQL session:
//   - one per database/sql driver.Conn today
//   - one per RPC stream in the future gRPC frontend
//
// The Session type deliberately starts minimal — only the long-lived
// resource handles + durable session identifiers migrate here in
// Phase 1b of RFC 021. Statement-scoped state (CTE map, scalar
// subquery cache, outer-scope stack, per-statement time, valid-
// qualifier set) and the driver-level transaction handle stay on
// EmbeddedConnection during 1b and follow in Phase 1c as the exec*
// bodies move behind the Plan seam.
package session

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
)

// Session carries the resource handles and session identifiers a SQL
// engine needs to plan and run statements. Fields are exported so
// collaborators in pkg/relational/core/embedded / core/query can
// access them without a layer of getters; there is no Session-level
// invariant to protect beyond single-goroutine access (driver.Conn
// contract).
type Session struct {
	// DB is the FDB database handle every transaction opens against.
	DB *recordlayer.FDBDatabase

	// Catalog is the FDB-backed catalog (databases / schemas / templates).
	Catalog *catalog.RecordLayerStoreCatalog

	// Keyspace navigates the relational subspaces under the catalog.
	Keyspace *keyspace.RelationalKeyspace

	// Factory builds metadata operations (DDL). Injected at session
	// construction so the embedded engine and a future remote engine
	// can plug in different factories.
	Factory apiddl.MetadataOperationsFactory

	// DBPath is the current database URI (e.g. "/mydb"). Set at
	// Conn/Session construction and updated by USE DATABASE (not yet
	// implemented).
	DBPath string

	// Schema is the current schema name, set via USE SCHEMA or the
	// driver's SetSchema call.
	Schema string

	// DefaultSchema is the schema name set at Conn creation time. Used
	// by ResetSession to restore the original after the pool hands the
	// connection back.
	DefaultSchema string
}

// New builds a Session with the given resource handles. DBPath,
// Schema and DefaultSchema can be set directly by the caller post-
// construction — they are not validated here (the catalog init
// elsewhere does that).
func New(
	db *recordlayer.FDBDatabase,
	cat *catalog.RecordLayerStoreCatalog,
	ks *keyspace.RelationalKeyspace,
	factory apiddl.MetadataOperationsFactory,
) *Session {
	return &Session{
		DB:       db,
		Catalog:  cat,
		Keyspace: ks,
		Factory:  factory,
	}
}
