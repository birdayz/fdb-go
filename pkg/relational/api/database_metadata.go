package api

import "context"

// DatabaseMetaData is a read-only view over the database's catalog
// suitable for exposing to tools (schema browsers, JDBC introspection,
// etc.). Mirrors Java's RelationalDatabaseMetaData — lean Go subset.
//
// We keep only the essential discovery surface (Schemas, Tables,
// Columns, IndexInfo, PrimaryKeys) plus product-identification
// getters. The java.sql.DatabaseMetaData inheritance chain has ~100
// additional methods that Java throws
// SQLFeatureNotSupportedException on; Go has no equivalent, so those
// don't exist here.
//
// All discovery methods return a ResultSet whose column shape matches
// JDBC's schemas for the same method name — client tools assume that
// layout, and Java↔Go cross-compatibility requires we match it
// byte-for-byte. See each method's doc comment for the column list.
type DatabaseMetaData interface {
	// Schemas returns every schema in every catalog.
	//
	// Columns:
	//   1 TABLE_SCHEM   string
	//   2 TABLE_CATALOG string
	Schemas(ctx context.Context) (ResultSet, error)

	// SchemasFiltered returns schemas matching the SQL LIKE
	// catalog + schemaPattern filters. Empty patterns match anything.
	SchemasFiltered(ctx context.Context, catalog, schemaPattern string) (ResultSet, error)

	// Tables returns tables matching the patterns. types is an
	// optional list of table types ("TABLE", "VIEW", "SYSTEM TABLE");
	// nil or empty returns all types.
	//
	// Columns (matching JDBC's DatabaseMetaData.getTables):
	//   1 TABLE_CAT   string  2 TABLE_SCHEM string  3 TABLE_NAME string
	//   4 TABLE_TYPE  string  5 REMARKS string      6 TYPE_CAT string
	//   7 TYPE_SCHEM  string  8 TYPE_NAME string    9 SELF_REFERENCING_COL_NAME string
	//  10 REF_GENERATION string
	Tables(ctx context.Context, catalog, schemaPattern, tableNamePattern string, types []string) (ResultSet, error)

	// Columns returns column metadata matching the patterns.
	//
	// Columns (matching JDBC's DatabaseMetaData.getColumns):
	//   1 TABLE_CAT  2 TABLE_SCHEM  3 TABLE_NAME  4 COLUMN_NAME
	//   5 DATA_TYPE (int, JDBC type code)  6 TYPE_NAME
	//   7 COLUMN_SIZE  8 BUFFER_LENGTH  9 DECIMAL_DIGITS 10 NUM_PREC_RADIX
	//  11 NULLABLE  12 REMARKS  13 COLUMN_DEF  14 SQL_DATA_TYPE
	//  15 SQL_DATETIME_SUB  16 CHAR_OCTET_LENGTH  17 ORDINAL_POSITION
	//  18 IS_NULLABLE  19 SCOPE_CATALOG  20 SCOPE_SCHEMA  21 SCOPE_TABLE
	//  22 SOURCE_DATA_TYPE  23 IS_AUTOINCREMENT  24 IS_GENERATEDCOLUMN
	Columns(ctx context.Context, catalog, schemaPattern, tableNamePattern, columnNamePattern string) (ResultSet, error)

	// IndexInfo returns secondary-index metadata for a table. If
	// unique is true, only unique indexes are returned. The
	// approximate flag tells the driver whether it may return stale
	// statistics (speed vs. freshness).
	//
	// Columns: 1 TABLE_CAT  2 TABLE_SCHEM  3 TABLE_NAME
	//          4 NON_UNIQUE  5 INDEX_QUALIFIER  6 INDEX_NAME
	//          7 TYPE  8 ORDINAL_POSITION  9 COLUMN_NAME
	//         10 ASC_OR_DESC  11 CARDINALITY  12 PAGES  13 FILTER_CONDITION
	IndexInfo(ctx context.Context, catalog, schema, table string, unique, approximate bool) (ResultSet, error)

	// PrimaryKeys returns the primary-key columns of the given table.
	//
	// Columns: 1 TABLE_CAT  2 TABLE_SCHEM  3 TABLE_NAME
	//          4 COLUMN_NAME  5 KEY_SEQ  6 PK_NAME
	PrimaryKeys(ctx context.Context, catalog, schema, table string) (ResultSet, error)

	// ---- Product / driver identification ----

	// URL returns the JDBC-style URL this connection was opened with.
	URL() string
	// UserName returns the authenticated user, or empty if none.
	UserName() string
	// IsReadOnly reports whether the underlying database is read-only.
	IsReadOnly() bool

	// DatabaseProductName returns a human-readable database name
	// ("FoundationDB Relational"). Tools use this to route
	// dialect-specific quirks.
	DatabaseProductName() string
	// DatabaseProductVersion is the database version string.
	DatabaseProductVersion() string
	// DriverName is the relational-layer driver name.
	DriverName() string
	// DriverVersion is the driver version string.
	DriverVersion() string
}
