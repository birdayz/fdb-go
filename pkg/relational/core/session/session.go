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
// The Session type deliberately starts minimal — resource handles
// and durable session identifiers migrated in Phase 1b of RFC 021.
// Phase 1d adds connection-lifetime caches that follow Session
// identity rather than statement execution: schema lookup cache and
// catalog-initialisation flag. Statement-scoped state (CTE map,
// scalar subquery cache, outer-scope stack, per-statement time,
// valid-qualifier set) and the driver-level transaction handle
// still live on EmbeddedConnection — those follow Phase 1c as the
// exec* bodies move behind the Plan seam.
package session

import (
	"sync"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
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

	// SchemaCache memoises catalog schema lookups for the session's
	// lifetime. Key is SchemaCacheKey(dbPath, schemaName). Safe for
	// single-goroutine use — matches database/sql's per-Conn threading
	// contract. Invalidated on DDL via InvalidateSchema and on
	// ResetSession.
	SchemaCache map[string]api.Schema

	// CatalogMu + CatalogReady gate the first successful catalog init.
	// Lock on access; set CatalogReady=true after init succeeds so
	// later calls short-circuit. Transient init failures leave
	// CatalogReady false so the next caller retries.
	CatalogMu    sync.Mutex
	CatalogReady bool

	// StatementTime is the timestamp captured at the top of the
	// current statement. SQL standard requires every reference to
	// CURRENT_TIMESTAMP / CURRENT_DATE / CURRENT_TIME / LOCALTIME
	// within a single statement to return the same value; callers
	// read this field through StatementNow so the deterministic
	// value is used while a statement is executing, and time.Now()
	// is used outside. Zero value means "no statement in flight".
	StatementTime time.Time
}

// StatementNow returns the timestamp captured at the top of the
// current statement, or time.Now().UTC() when no statement is in
// flight. Callers that need deterministic CURRENT_TIMESTAMP-family
// values within a statement use this instead of time.Now() directly.
func (s *Session) StatementNow() time.Time {
	if s == nil || s.StatementTime.IsZero() {
		return time.Now().UTC()
	}
	return s.StatementTime
}

// BeginStatement captures the statement-start timestamp and returns
// a cleanup function that restores the previous value. Callers use
// `defer sess.BeginStatement()()` at every public Query/Exec entry
// point to ensure CURRENT_TIMESTAMP-family calls see a stable value
// for the life of the statement.
func (s *Session) BeginStatement() func() {
	prior := s.StatementTime
	s.StatementTime = time.Now().UTC()
	return func() { s.StatementTime = prior }
}

// SchemaCacheKey builds the canonical key under which SchemaCache
// stores a lookup for (dbPath, schemaName). Callers must use this
// helper rather than hand-concatenating so both stores/loads agree.
func SchemaCacheKey(dbPath, schemaName string) string {
	return dbPath + "\x00" + schemaName
}

// InvalidateSchema removes the cached api.Schema for (dbPath,
// schemaName) if present. No-op when not cached. Callers hold this
// method's contract: call after any DDL that changes the schema's
// metadata or proto descriptor.
func (s *Session) InvalidateSchema(dbPath, schemaName string) {
	delete(s.SchemaCache, SchemaCacheKey(dbPath, schemaName))
}

// ResetSchemaCache drops every cached schema. Used by ResetSession
// when the driver-layer hands the connection back to the pool.
func (s *Session) ResetSchemaCache() {
	s.SchemaCache = make(map[string]api.Schema)
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
		DB:          db,
		Catalog:     cat,
		Keyspace:    ks,
		Factory:     factory,
		SchemaCache: make(map[string]api.Schema),
	}
}
