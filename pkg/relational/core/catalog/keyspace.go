package catalog

// Keyspace-path constants for the FDB-backed catalog. Mirrors Java's
// com.apple.foundationdb.relational.recordlayer.RelationalKeyspaceProvider
// byte-for-byte so the on-FDB layout is cross-language compatible.
//
// Layout:
//
//	/                              — root
//	└── __SYS                      — system schema (SysConstant)
//	    └── CATALOG                — catalog schema (CatalogConstant)
//	└── <domain name>              — user domains (naïve today per Java)
//	    └── <dbName>               — DB_NAME_DIR child
//	        └── <schema>           — SCHEMA_DIR child
//
// Used by the (not-yet-landed) FDB-backed RecordLayerStoreCatalog to
// construct KeySpacePaths. Defining the constants here now keeps the
// whole catalog package as the single source of truth for the
// SQL-layer-side of the layout; the FDB impl will import these.
const (
	// SysConstant is the root system namespace ("__SYS"). Contains
	// the catalog's own record store.
	SysConstant = "__SYS"

	// CatalogConstant is the schema name inside __SYS that holds the
	// catalog records (DATABASE_INFO, SCHEMA, SCHEMA_TEMPLATE).
	CatalogConstant = "CATALOG"

	// DBNameDir is the KeySpaceDirectory name for a database entry
	// under a domain.
	DBNameDir = "dbName"

	// SchemaDir is the KeySpaceDirectory name for a schema entry
	// under a database.
	SchemaDir = "schema"

	// DefaultSchemaDir marks the "default" schema child directory
	// (used for convenience-resolution today per Java).
	DefaultSchemaDir = "defaultSchema"

	// InterningLayer is the dedicated subspace name for the
	// interning-layer string resolver (turns long domain / db names
	// into short integer IDs).
	InterningLayer = "__internedStrings"

	// InterningLayerValue is the fixed scalar stored at the
	// interning-layer subspace (two-byte sentinel "IL").
	InterningLayerValue = "IL"
)
