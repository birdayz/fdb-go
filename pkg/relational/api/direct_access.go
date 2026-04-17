package api

import "context"

// DirectAccessStatement provides primary-key-direct access to tables
// that bypasses the SQL compiler — Scan, Get, Insert, Delete on a
// named table identified by a KeySet. Mirrors Java's
// RelationalDirectAccessStatement.
//
// Java's RelationalStatement extends this interface (statements are
// also direct-access) but in Go we keep the two concerns split; a
// Statement impl can optionally expose DirectAccessStatement via
// type assertion.
type DirectAccessStatement interface {
	// ExecuteScan scans a contiguous range of rows in tableName,
	// using keyPrefix as a PK-prefix. keyPrefix may be EmptyKeySet to
	// scan the full table. Fails if keyPrefix columns are not a
	// prefix of the table's primary key.
	ExecuteScan(ctx context.Context, tableName string, keyPrefix *KeySet, opts *Options) (ResultSet, error)

	// ExecuteGet returns a single row by its primary key. Returns an
	// empty ResultSet (Next()=false) if the row does not exist.
	ExecuteGet(ctx context.Context, tableName string, key *KeySet, opts *Options) (ResultSet, error)

	// ExecuteInsert inserts one or more rows. Returns the number of
	// rows actually inserted (may be 0 for IF-NOT-EXISTS semantics
	// with REPLACE_ON_DUPLICATE_PK=false).
	ExecuteInsert(ctx context.Context, tableName string, data []Struct, opts *Options) (int64, error)

	// ExecuteDelete deletes a single row by primary key. Returns the
	// number of rows deleted (0 or 1).
	ExecuteDelete(ctx context.Context, tableName string, key *KeySet, opts *Options) (int64, error)

	// ExecuteDeleteRange bulk-deletes every row matching the
	// keyPrefix. Equivalent to DELETE FROM tableName WHERE PK-prefix
	// matches. Returns the number of rows removed.
	ExecuteDeleteRange(ctx context.Context, tableName string, keyPrefix *KeySet, opts *Options) (int64, error)
}
