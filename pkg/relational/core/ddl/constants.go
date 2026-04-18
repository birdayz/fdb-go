package ddl

const (
	// SysDatabasePath is the protected system database. DDL operations
	// that could break the catalog are rejected for this path.
	SysDatabasePath = "/__SYS"

	colSchemaName = "SCHEMA_NAME"
)
